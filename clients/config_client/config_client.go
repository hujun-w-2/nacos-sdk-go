/*
 * Copyright 1999-2020 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package config_client

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/kms"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/cache"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/nacos_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/common/logger"
	"github.com/nacos-group/nacos-sdk-go/v2/common/monitor"
	"github.com/nacos-group/nacos-sdk-go/v2/common/nacos_error"
	"github.com/nacos-group/nacos-sdk-go/v2/common/remote/rpc/rpc_request"
	"github.com/nacos-group/nacos-sdk-go/v2/common/remote/rpc/rpc_response"
	"github.com/nacos-group/nacos-sdk-go/v2/inner/uuid"
	"github.com/nacos-group/nacos-sdk-go/v2/model"
	"github.com/nacos-group/nacos-sdk-go/v2/util"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
	"github.com/pkg/errors"
)

const (
	perTaskConfigSize = 3000
	executorErrDelay  = 5 * time.Second
)

type ConfigClient struct {
	ctx    context.Context
	cancel context.CancelFunc
	nacos_client.INacosClient
	kmsClient       *kms.Client
	localConfigs    []vo.ConfigParam
	mutex           sync.Mutex
	configProxy     IConfigProxy
	configCacheDir  string
	lastAllSyncTime time.Time
	cacheMap        cache.ConcurrentMap
	uid             string
	listenExecute   chan struct{}
}

type cacheData struct {
	isInitializing    bool
	dataId            string
	group             string
	content           string
	contentType       string
	tenant            string
	cacheDataListener *cacheDataListener
	md5               string
	appName           string
	taskId            int
	configClient      *ConfigClient
	isSyncWithServer  bool
}

type cacheDataListener struct {
	listener vo.Listener
	lastMd5  string
}

func (cacheData *cacheData) executeListener() {
	cacheData.cacheDataListener.lastMd5 = cacheData.md5
	cacheData.configClient.cacheMap.Set(util.GetConfigCacheKey(cacheData.dataId, cacheData.group, cacheData.tenant), *cacheData)

	decryptedContent, err := cacheData.configClient.decrypt(cacheData.dataId, cacheData.content)
	if err != nil {
		logger.Errorf("decrypt content fail ,dataId=%s,group=%s,tenant=%s,err:%+v ", cacheData.dataId,
			cacheData.group, cacheData.tenant, err)
		return
	}
	go cacheData.cacheDataListener.listener(cacheData.tenant, cacheData.group, cacheData.dataId, decryptedContent)
}

func NewConfigClient(nc nacos_client.INacosClient) (*ConfigClient, error) {
	config := &ConfigClient{}
	config.ctx, config.cancel = context.WithCancel(context.Background())
	config.INacosClient = nc
	clientConfig, err := nc.GetClientConfig()
	if err != nil {
		return nil, err
	}
	serverConfig, err := nc.GetServerConfig()
	if err != nil {
		return nil, err
	}
	httpAgent, err := nc.GetHttpAgent()
	if err != nil {
		return nil, err
	}

	if err = initLogger(clientConfig); err != nil {
		return nil, err
	}
	clientConfig.CacheDir = clientConfig.CacheDir + string(os.PathSeparator) + "config"
	config.configCacheDir = clientConfig.CacheDir

	if config.configProxy, err = NewConfigProxy(config.ctx, serverConfig, clientConfig, httpAgent); err != nil {
		return nil, err
	}

	if clientConfig.OpenKMS {
		kmsClient, err := kms.NewClientWithAccessKey(clientConfig.RegionId, clientConfig.AccessKey, clientConfig.SecretKey)
		if err != nil {
			return nil, err
		}
		config.kmsClient = kmsClient
	}

	uid, err := uuid.NewV4()
	if err != nil {
		return nil, err
	}

	config.uid = uid.String()
	config.cacheMap = cache.NewConcurrentMap()
	config.listenExecute = make(chan struct{})
	config.startInternal()
	return config, err
}

func initLogger(clientConfig constant.ClientConfig) error {
	return logger.InitLogger(logger.BuildLoggerConfig(clientConfig))
}

func (client *ConfigClient) GetConfig(param vo.ConfigParam) (content string, err error) {
	content, err = client.getConfigInner(param)
	if err != nil {
		return "", err
	}
	return client.decrypt(param.DataId, content)
}

func (client *ConfigClient) decrypt(dataId, content string) (string, error) {
	if client.kmsClient != nil && strings.HasPrefix(dataId, "cipher-") {
		request := kms.CreateDecryptRequest()
		request.Method = "POST"
		request.Scheme = "https"
		request.AcceptFormat = "json"
		request.CiphertextBlob = content
		response, err := client.kmsClient.Decrypt(request)
		if err != nil {
			return "", fmt.Errorf("kms decrypt failed: %v", err)
		}
		content = response.Plaintext
	}
	return content, nil
}

func (client *ConfigClient) encrypt(dataId, content string) (string, error) {
	if client.kmsClient != nil && strings.HasPrefix(dataId, "cipher-") {
		request := kms.CreateEncryptRequest()
		request.Method = "POST"
		request.Scheme = "https"
		request.AcceptFormat = "json"
		request.KeyId = "alias/acs/mse" // use default key
		request.Plaintext = content
		response, err := client.kmsClient.Encrypt(request)
		if err != nil {
			return "", fmt.Errorf("kms encrypt failed: %v", err)
		}
		content = response.CiphertextBlob
	}
	return content, nil
}

func (client *ConfigClient) getConfigInner(param vo.ConfigParam) (content string, err error) {
	if len(param.DataId) <= 0 {
		err = errors.New("[client.GetConfig] param.dataId can not be empty")
		return "", err
	}
	if len(param.Group) <= 0 {
		param.Group = constant.DEFAULT_GROUP
	}

	clientConfig, _ := client.GetClientConfig()
	cacheKey := util.GetConfigCacheKey(param.DataId, param.Group, clientConfig.NamespaceId)
	content = cache.GetFailover(cacheKey, client.configCacheDir)
	if len(content) > 0 {
		logger.Warnf("%s %s %s is using failover content!", clientConfig.NamespaceId, param.Group, param.DataId)
		return content, nil
	}
	response, err := client.configProxy.queryConfig(param.DataId, param.Group, clientConfig.NamespaceId,
		clientConfig.TimeoutMs, false, client)
	if err != nil {
		logger.Errorf("get config from server error:%v, dataId=%s, group=%s, namespaceId=%s", err,
			param.DataId, param.Group, clientConfig.NamespaceId)

		if clientConfig.DisableUseSnapShot {
			return "", errors.Errorf("get config from remote nacos server fail, and is not allowed to read local file, err:%v", err)
		}

		cacheContent, cacheErr := cache.ReadConfigFromFile(cacheKey, client.configCacheDir)
		if cacheErr != nil {
			return "", errors.Errorf("read config from both server and cache fail, err=%v，dataId=%s, group=%s, namespaceId=%s",
				cacheErr, param.DataId, param.Group, clientConfig.NamespaceId)
		}

		logger.Warnf("read config from cache success, dataId=%s, group=%s, namespaceId=%s", param.DataId, param.Group, clientConfig.NamespaceId)
		return cacheContent, nil
	}
	return response.Content, nil
}

func (client *ConfigClient) PublishConfig(param vo.ConfigParam) (published bool, err error) {
	if len(param.DataId) <= 0 {
		err = errors.New("[client.PublishConfig] param.dataId can not be empty")
		return
	}
	if len(param.Content) <= 0 {
		err = errors.New("[client.PublishConfig] param.content can not be empty")
		return
	}

	if len(param.Group) <= 0 {
		param.Group = constant.DEFAULT_GROUP
	}
	if param.Content, err = client.encrypt(param.DataId, param.Content); err != nil {
		return
	}

	clientConfig, _ := client.GetClientConfig()
	request := rpc_request.NewConfigPublishRequest(param.Group, param.DataId, clientConfig.NamespaceId, param.Content, param.CasMd5)
	request.AdditionMap["tag"] = param.Tag
	request.AdditionMap["appName"] = param.AppName
	request.AdditionMap["betaIps"] = param.BetaIps
	request.AdditionMap["type"] = param.Type
	request.AdditionMap["src_user"] = param.SrcUser
	request.AdditionMap["encryptedDataKey"] = param.EncryptedDataKey
	rpcClient := client.configProxy.getRpcClient(client)
	response, err := client.configProxy.requestProxy(rpcClient, request, constant.DEFAULT_TIMEOUT_MILLS)
	if response != nil {
		return response.IsSuccess(), err
	}
	return false, err
}

func (client *ConfigClient) DeleteConfig(param vo.ConfigParam) (deleted bool, err error) {
	if len(param.DataId) <= 0 {
		err = errors.New("[client.DeleteConfig] param.dataId can not be empty")
	}
	if len(param.Group) <= 0 {
		param.Group = constant.DEFAULT_GROUP
	}
	if err != nil {
		return false, err
	}
	clientConfig, _ := client.GetClientConfig()
	request := rpc_request.NewConfigRemoveRequest(param.Group, param.DataId, clientConfig.NamespaceId)
	rpcClient := client.configProxy.getRpcClient(client)
	response, err := client.configProxy.requestProxy(rpcClient, request, constant.DEFAULT_TIMEOUT_MILLS)
	if response != nil {
		return response.IsSuccess(), err
	}
	return false, err
}

// Cancel Listen Config
func (client *ConfigClient) CancelListenConfig(param vo.ConfigParam) (err error) {
	clientConfig, err := client.GetClientConfig()
	if err != nil {
		logger.Errorf("[checkConfigInfo.GetClientConfig] failed,err:%+v", err)
		return
	}
	client.cacheMap.Remove(util.GetConfigCacheKey(param.DataId, param.Group, clientConfig.NamespaceId))
	logger.Infof("Cancel listen config DataId:%s Group:%s", param.DataId, param.Group)
	return err
}

func (client *ConfigClient) ListenConfig(param vo.ConfigParam) (err error) {
	if len(param.DataId) <= 0 {
		err = errors.New("[client.ListenConfig] DataId can not be empty")
		return err
	}
	if len(param.Group) <= 0 {
		err = errors.New("[client.ListenConfig] Group can not be empty")
		return err
	}
	clientConfig, err := client.GetClientConfig()
	if err != nil {
		err = errors.New("[checkConfigInfo.GetClientConfig] failed")
		return err
	}

	key := util.GetConfigCacheKey(param.DataId, param.Group, clientConfig.NamespaceId)
	var cData cacheData
	if v, ok := client.cacheMap.Get(key); ok {
		cData = v.(cacheData)
		cData.isInitializing = true
	} else {
		var (
			content string
			md5Str  string
		)
		content, _ = cache.ReadConfigFromFile(key, client.configCacheDir)
		if len(content) > 0 {
			md5Str = util.Md5(content)
		}
		listener := &cacheDataListener{
			listener: param.OnChange,
			lastMd5:  md5Str,
		}

		cData = cacheData{
			isInitializing:    true,
			dataId:            param.DataId,
			group:             param.Group,
			tenant:            clientConfig.NamespaceId,
			content:           content,
			md5:               md5Str,
			cacheDataListener: listener,
			taskId:            client.cacheMap.Count() / perTaskConfigSize,
			configClient:      client,
		}
	}
	client.cacheMap.Set(key, cData)
	return
}

func (client *ConfigClient) SearchConfig(param vo.SearchConfigParam) (*model.ConfigPage, error) {
	return client.searchConfigInner(param)
}

func (client *ConfigClient) CloseClient() {
	client.configProxy.getRpcClient(client).Shutdown()
	client.cancel()
}

func (client *ConfigClient) searchConfigInner(param vo.SearchConfigParam) (*model.ConfigPage, error) {
	if param.Search != "accurate" && param.Search != "blur" {
		return nil, errors.New("[client.searchConfigInner] param.search must be accurate or blur")
	}
	if param.PageNo <= 0 {
		param.PageNo = 1
	}
	if param.PageSize <= 0 {
		param.PageSize = 10
	}
	clientConfig, _ := client.GetClientConfig()
	configItems, err := client.configProxy.searchConfigProxy(param, clientConfig.NamespaceId, clientConfig.AccessKey, clientConfig.SecretKey)
	if err != nil {
		logger.Errorf("search config from server error:%+v ", err)
		if _, ok := err.(*nacos_error.NacosError); ok {
			nacosErr := err.(*nacos_error.NacosError)
			if nacosErr.ErrorCode() == "404" {
				return nil, errors.New("config not found")
			}
			if nacosErr.ErrorCode() == "403" {
				return nil, errors.New("get config forbidden")
			}
		}
		return nil, err
	}
	return configItems, nil
}

func (client *ConfigClient) startInternal() {
	go func() {
		timer := time.NewTimer(executorErrDelay)
		defer timer.Stop()
		for {
			select {
			case <-client.listenExecute:
				client.executeConfigListen()
			case <-timer.C:
				client.executeConfigListen()
			case <-client.ctx.Done():
				return
			}
			timer.Reset(executorErrDelay)
		}
	}()
}

func (client *ConfigClient) executeConfigListen() {
	listenCachesMap := make(map[int][]cacheData, 16)
	needAllSync := time.Since(client.lastAllSyncTime) >= constant.ALL_SYNC_INTERNAL
	for _, v := range client.cacheMap.Items() {
		cache, ok := v.(cacheData)
		if !ok {
			continue
		}

		if cache.isSyncWithServer {
			if cache.md5 != cache.cacheDataListener.lastMd5 {
				cache.executeListener()
			}
			if !needAllSync {
				continue
			}
		}

		cacheDatas := listenCachesMap[cache.taskId]
		cacheDatas = append(cacheDatas, cache)
		listenCachesMap[cache.taskId] = cacheDatas
	}
	hasChangedKeys := false
	if len(listenCachesMap) > 0 {
		for taskId, listenCaches := range listenCachesMap {
			request := buildConfigBatchListenRequest(listenCaches)
			rpcClient := client.configProxy.createRpcClient(client.ctx, fmt.Sprintf("%d", taskId), client)
			iResponse, err := client.configProxy.requestProxy(rpcClient, request, 3000)
			if err != nil {
				logger.Warnf("ConfigBatchListenRequest failure,err:%+v", err)
				continue
			}
			if iResponse == nil {
				logger.Warnf("ConfigBatchListenRequest failure, response is nil")
				continue
			}
			if !iResponse.IsSuccess() {
				logger.Warnf("ConfigBatchListenRequest failure, error code:%+v", iResponse.GetErrorCode())
				continue
			}
			changeKeys := make(map[string]struct{})
			if response, ok := iResponse.(*rpc_response.ConfigChangeBatchListenResponse); ok {
				if len(response.ChangedConfigs) > 0 {
					hasChangedKeys = true
					for _, v := range response.ChangedConfigs {
						changeKey := util.GetConfigCacheKey(v.DataId, v.Group, v.Tenant)
						changeKeys[changeKey] = struct{}{}
						if cache, ok := client.cacheMap.Get(changeKey); !ok {
							continue
						} else {
							cacheData := cache.(cacheData)
							client.refreshContentAndCheck(cacheData, !cacheData.isInitializing)
						}
					}
				}

				for _, v := range listenCaches {
					changeKey := util.GetConfigCacheKey(v.dataId, v.group, v.tenant)
					if _, ok := changeKeys[changeKey]; !ok {
						v.isSyncWithServer = true
						continue
					}
					v.isInitializing = true
				}
			}
		}
	}
	if needAllSync {
		client.lastAllSyncTime = time.Now()
	}

	if hasChangedKeys {
		client.asyncNotifyListenConfig()
	}
	monitor.GetListenConfigCountMonitor().Set(float64(client.cacheMap.Count()))
}

func buildConfigBatchListenRequest(caches []cacheData) *rpc_request.ConfigBatchListenRequest {
	request := rpc_request.NewConfigBatchListenRequest(len(caches))
	for _, cache := range caches {
		request.ConfigListenContexts = append(request.ConfigListenContexts,
			model.ConfigListenContext{Group: cache.group, Md5: cache.md5, DataId: cache.dataId, Tenant: cache.tenant})
	}
	return request
}

func (client *ConfigClient) refreshContentAndCheck(cacheData cacheData, notify bool) {
	configQueryResponse, err := client.configProxy.queryConfig(cacheData.dataId, cacheData.group, cacheData.tenant,
		constant.DEFAULT_TIMEOUT_MILLS, notify, client)
	if err != nil {
		logger.Errorf("refresh content and check md5 fail ,dataId=%s,group=%s,tenant=%s ", cacheData.dataId,
			cacheData.group, cacheData.tenant)
		return
	}
	cacheData.content = configQueryResponse.Content
	cacheData.contentType = configQueryResponse.ContentType
	if notify {
		logger.Infof("[config_rpc_client] [data-received] dataId=%s, group=%s, tenant=%s, md5=%s, content=%s, type=%s",
			cacheData.dataId, cacheData.group, cacheData.tenant, cacheData.md5,
			util.TruncateContent(cacheData.content), cacheData.contentType)
	}
	cacheData.md5 = util.Md5(cacheData.content)
	if cacheData.md5 != cacheData.cacheDataListener.lastMd5 {
		cacheDataPtr := &cacheData
		cacheDataPtr.executeListener()
	}
}

func (client *ConfigClient) asyncNotifyListenConfig() {
	go func() {
		client.listenExecute <- struct{}{}
	}()
}
