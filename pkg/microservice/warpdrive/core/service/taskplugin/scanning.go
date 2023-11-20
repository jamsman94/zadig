/*
Copyright 2022 The KodeRover Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package taskplugin

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/koderover/zadig/pkg/types"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zadigconfig "github.com/koderover/zadig/pkg/config"
	"github.com/koderover/zadig/pkg/microservice/warpdrive/config"
	"github.com/koderover/zadig/pkg/microservice/warpdrive/core/service/types/task"
	"github.com/koderover/zadig/pkg/setting"
	krkubeclient "github.com/koderover/zadig/pkg/tool/kube/client"
	"github.com/koderover/zadig/pkg/tool/kube/label"
	"github.com/koderover/zadig/pkg/tool/kube/updater"
)

func InitializeScanningTaskPlugin(taskType config.TaskType) TaskPlugin {
	return &ScanPlugin{
		Name:       taskType,
		kubeClient: krkubeclient.Client(),
		clientset:  krkubeclient.Clientset(),
		restConfig: krkubeclient.RESTConfig(),
		apiReader:  krkubeclient.APIReader(),
	}
}

type ScanPlugin struct {
	Name          config.TaskType
	KubeNamespace string
	JobName       string
	FileName      string
	kubeClient    client.Client
	clientset     kubernetes.Interface
	restConfig    *rest.Config
	apiReader     client.Reader
	Task          *task.Scanning
	Log           *zap.SugaredLogger
	Timeout       <-chan time.Time
}

func (p *ScanPlugin) SetAckFunc(func()) {
}

const (
	ScanningTaskTimeout = 60 * 60 // 60 minutes
	ScanningTypeSonar   = "sonarQube"
	ScanningTypeOther   = "other"
)

func (p *ScanPlugin) Init(jobname, filename string, xl *zap.SugaredLogger) {
	p.JobName = jobname
	p.FileName = filename
	p.Log = xl
}

func (p *ScanPlugin) Type() config.TaskType {
	return p.Name
}

func (p *ScanPlugin) Status() config.Status {
	return p.Task.Status
}

func (p *ScanPlugin) SetStatus(status config.Status) {
	p.Task.Status = status
}

func (p *ScanPlugin) TaskTimeout() int {
	if p.Task.Timeout == 0 {
		p.Task.Timeout = ScanningTaskTimeout
	}
	return int(p.Task.Timeout)
}

func (p *ScanPlugin) Run(ctx context.Context, pipelineTask *task.Task, pipelineCtx *task.PipelineCtx, serviceName string) {
	switch p.Task.ClusterID {
	case setting.LocalClusterID:
		p.KubeNamespace = zadigconfig.Namespace()
	default:
		p.KubeNamespace = setting.AttachedClusterNamespace

		crClient, clientset, restConfig, apiReader, err := GetK8sClients(pipelineTask.ConfigPayload.HubServerAddr, p.Task.ClusterID)
		if err != nil {
			p.Log.Error(err)
			p.Task.Status = config.StatusFailed
			p.Task.Error = err.Error()
			return
		}

		p.kubeClient = crClient
		p.clientset = clientset
		p.restConfig = restConfig
		p.apiReader = apiReader
	}

	repoList := make([]*task.Repository, 0)

	for _, taskRepo := range p.Task.Repos {
		repo := &task.Repository{
			Source:             taskRepo.Source,
			RepoOwner:          taskRepo.RepoNamespace,
			RepoName:           taskRepo.RepoName,
			Branch:             taskRepo.Branch,
			PR:                 taskRepo.PR,
			PRs:                taskRepo.PRs,
			Tag:                taskRepo.Tag,
			OauthToken:         taskRepo.OauthToken,
			Address:            taskRepo.Address,
			Username:           taskRepo.Username,
			Password:           taskRepo.Password,
			EnableProxy:        taskRepo.EnableProxy,
			RemoteName:         taskRepo.RemoteName,
			SubModules:         taskRepo.SubModules,
			CheckoutPath:       taskRepo.CheckoutPath,
			AuthType:           taskRepo.AuthType,
			SSHKey:             taskRepo.SSHKey,
			PrivateAccessToken: taskRepo.PrivateAccessToken,
		}
		if repo.RemoteName == "" {
			repo.RemoteName = "origin"
		}
		repoList = append(repoList, repo)
	}

	// ============== environment variables ============================
	envVars := make([]*task.KeyVal, 0)

	envVars = append(envVars, PrepareDefaultWorkflowTaskEnvs(pipelineTask)...)

	envVars = append(envVars, CreateEnvsFromRepoInfo(repoList)...)

	for _, env := range p.Task.Envs {
		envVars = append(envVars, &task.KeyVal{
			Key:          env.Key,
			Value:        env.Value,
			IsCredential: env.IsCredential,
		})
	}

	if len(repoList) > 0 {
		envVars = append(envVars, &task.KeyVal{
			Key:   "BRANCH",
			Value: repoList[0].Branch,
		})
	}

	if p.Task.SonarInfo != nil {
		envVars = append(envVars, &task.KeyVal{
			Key:          "SONAR_TOKEN",
			Value:        p.Task.SonarInfo.Token,
			IsCredential: true,
		})

		envVars = append(envVars, &task.KeyVal{
			Key:          "SONAR_URL",
			Value:        p.Task.SonarInfo.ServerAddress,
			IsCredential: false,
		})
	}

	jobCtx := JobCtxBuilder{
		JobName:     p.JobName,
		PipelineCtx: pipelineCtx,
		Installs:    p.Task.InstallCtx,
		JobCtx: task.JobCtx{
			Builds:  repoList,
			EnvVars: envVars,
		},
	}

	reaperContext := jobCtx.BuildReaperContext(pipelineTask, serviceName)

	// if the scanning task is of sonar type, then we add the sonar parameter to the context
	if p.Task.SonarInfo != nil {
		reaperContext.SonarServer = p.Task.SonarInfo.ServerAddress
		reaperContext.SonarLogin = p.Task.SonarInfo.Token
		reaperContext.ScannerType = ScanningTypeSonar
		reaperContext.SonarCheckQualityGate = p.Task.CheckQualityGate
	} else {
		reaperContext.ScannerType = ScanningTypeOther
	}
	reaperContext.Scripts = append(reaperContext.Scripts, strings.Split(replaceWrapLine(p.Task.Script), "\n")...)

	if p.Task.CacheEnable {
		// since a prefix of $WORKSPACE is added to the user path in the product, we add this prefix to the cacheUserDir parameter
		cacheDir := fmt.Sprintf("%s/%s", "/workspace", p.Task.CacheUserDir)

		// job creation usage
		pipelineCtx.CacheEnable = true
		pipelineCtx.Cache = p.Task.Cache
		pipelineCtx.CacheDirType = p.Task.CacheDirType
		pipelineCtx.CacheUserDir = cacheDir
		// job usage
		reaperContext.CacheEnable = true
		reaperContext.CacheDirType = p.Task.CacheDirType
		reaperContext.CacheUserDir = cacheDir
		reaperContext.Cache = p.Task.Cache

		// Since we allow users to use custom environment variables, variable resolution is required.
		if pipelineCtx.CacheEnable && pipelineCtx.Cache.MediumType == types.NFSMedium {
			pipelineCtx.CacheUserDir = p.renderEnv(cacheDir, envVars)
			pipelineCtx.Cache.NFSProperties.Subpath = p.renderEnv(pipelineCtx.Cache.NFSProperties.Subpath, envVars)
		}
	}

	reaperContext.SonarEnableScanner = p.Task.EnableScanner
	reaperContext.SonarParameter = p.Task.Parameter

	jobCtxBytes, err := yaml.Marshal(reaperContext)
	if err != nil {
		msg := fmt.Sprintf("cannot reaper.Context data: %v", err)
		p.Log.Error(msg)
		p.Task.Status = config.StatusFailed
		p.Task.Error = msg
		return
	}

	jobLabel := &label.JobLabel{
		PipelineName: pipelineTask.PipelineName,
		ServiceName:  serviceName,
		TaskID:       pipelineTask.TaskID,
		TaskType:     string(p.Type()),
		PipelineType: string(pipelineTask.Type),
	}

	jobObj, jobExist, err := checkJobExists(ctx, p.KubeNamespace, jobLabel, p.kubeClient)
	if err != nil {
		msg := fmt.Sprintf("failed to check whether Job exist for %s:%d: %s", pipelineTask.PipelineName, pipelineTask.TaskID, err)
		p.Log.Error(msg)
		p.Task.Status = config.StatusFailed
		p.Task.Error = msg
		return
	}

	if jobExist {
		p.Log.Infof("Job %s:%d eixsts.", pipelineTask.PipelineName, pipelineTask.TaskID)

		p.JobName = jobObj.Name

		// If the code is executed at this point, it indicates that the `wd` instance that executed the Job has been restarted and the
		// Job timeout period needs to be corrected.
		//
		// Rule of reseting timeout: `timeout - (now - start_time_of_job) + compensate_duration`
		// For now, `compensate_duration` is 2min.
		taskTimeout := p.TaskTimeout()
		elaspedTime := time.Now().Unix() - jobObj.Status.StartTime.Time.Unix()
		timeout := taskTimeout + 120 - int(elaspedTime)
		p.Log.Infof("Timeout before normalization: %d seconds", timeout)
		if timeout < 0 {
			// That shouldn't happen in theory, but a protection is needed.
			timeout = 0
		}

		p.Log.Infof("Timeout after normalization: %d seconds", timeout)
		p.tmpSetTaskTimeout(timeout)
	}

	_, cmExist, err := checkConfigMapExists(ctx, p.KubeNamespace, jobLabel, p.kubeClient)
	if err != nil {
		msg := fmt.Sprintf("failed to check whether ConfigMap exist for %s:%d: %s", pipelineTask.PipelineName, pipelineTask.TaskID, err)
		p.Log.Error(msg)
		p.Task.Status = config.StatusFailed
		p.Task.Error = msg
		return
	}

	if !cmExist {
		p.Log.Infof("ConfigMap for Job %s:%d does not exist. Create.", pipelineTask.PipelineName, pipelineTask.TaskID)

		err = createJobConfigMap(p.KubeNamespace, p.JobName, jobLabel, string(jobCtxBytes), p.kubeClient)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			msg := fmt.Sprintf("createJobConfigMap error: %v", err)
			p.Log.Error(msg)
			p.Task.Status = config.StatusFailed
			p.Task.Error = msg
			return
		}
	}
	p.Log.Infof("succeed to create cm for build job %s", p.JobName)

	if !jobExist {
		p.Log.Infof("Job %s:%d does not exist. Create.", pipelineTask.PipelineName, pipelineTask.TaskID)

		p.Task.Registries = getMatchedRegistries(p.Task.ImageInfo, p.Task.Registries)
		// search namespace should also include desired namespace
		job, err := buildJobWithLinkedNs(
			p.Type(), p.Task.ImageInfo, p.JobName, serviceName, p.Task.ClusterID, p.Task.StrategyID, pipelineTask.ConfigPayload.Test.KubeNamespace, p.Task.ResReq, p.Task.ResReqSpec, pipelineCtx, pipelineTask, p.Task.Registries,
		)

		job.Namespace = p.KubeNamespace

		if err != nil {
			msg := fmt.Sprintf("create scanning job context error: %v", err)
			p.Log.Error(msg)
			p.Task.Status = config.StatusFailed
			p.Task.Error = msg
			return
		}

		// 将集成到KodeRover的私有镜像仓库的访问权限设置到namespace中
		if err := createOrUpdateRegistrySecrets(p.KubeNamespace, pipelineTask.ConfigPayload.RegistryID, p.Task.Registries, p.kubeClient); err != nil {
			msg := fmt.Sprintf("create secret error: %v", err)
			p.Log.Errorf(msg)
			p.Task.Status = config.StatusFailed
			p.Task.Error = msg
			return
		}
		if err := updater.CreateJob(job, p.kubeClient); err != nil {
			msg := fmt.Sprintf("create testing job error: %v", err)
			p.Log.Error(msg)
			p.Task.Status = config.StatusFailed
			p.Task.Error = msg
			return
		}
	}

	p.Log.Infof("succeed to create build job %s", p.JobName)
	p.Timeout = time.After(time.Duration(p.TaskTimeout()) * time.Second)
	p.Task.Status, err = waitJobReady(ctx, p.KubeNamespace, p.JobName, p.kubeClient, p.apiReader, p.Timeout, p.Log)
	if err != nil {
		p.Task.Error = err.Error()
	}
}

func (p *ScanPlugin) Wait(ctx context.Context) {
	status, err := waitJobEndWithFile(ctx, p.TaskTimeout(), p.Timeout, p.KubeNamespace, p.JobName, true, p.kubeClient, p.clientset, p.restConfig, p.Log)
	p.SetStatus(status)
	if err != nil {
		p.Task.Error = err.Error()
	}
}

func (p *ScanPlugin) tmpSetTaskTimeout(durationInSeconds int) {
	p.Task.Timeout = int64(math.Ceil(float64(durationInSeconds) / 60.0))
}

func (p *ScanPlugin) Complete(ctx context.Context, pipelineTask *task.Task, serviceName string) {
	jobLabel := &label.JobLabel{
		PipelineName: pipelineTask.PipelineName,
		ServiceName:  serviceName,
		TaskID:       pipelineTask.TaskID,
		TaskType:     string(p.Type()),
		PipelineType: string(pipelineTask.Type),
	}

	// Clean up tasks that user canceled or timed out.
	defer func() {
		go func() {
			if err := ensureDeleteJob(p.KubeNamespace, jobLabel, p.kubeClient); err != nil {
				p.Log.Error(err)
			}

			if err := ensureDeleteConfigMap(p.KubeNamespace, jobLabel, p.kubeClient); err != nil {
				p.Log.Error(err)
			}
		}()
	}()

	err := saveContainerLog(pipelineTask, p.KubeNamespace, p.Task.ClusterID, p.FileName, jobLabel, p.kubeClient)
	if err != nil {
		p.Log.Error(err)
		if p.Task.Error == "" {
			p.Task.Error = err.Error()
		}
		return
	}
}

func (p *ScanPlugin) SetTask(t map[string]interface{}) error {
	task, err := ToScanningTask(t)
	if err != nil {
		return err
	}
	p.Task = task

	return nil
}

func (p *ScanPlugin) GetTask() interface{} {
	return p.Task
}

func (p *ScanPlugin) IsTaskDone() bool {
	if p.Task.Status != config.StatusCreated && p.Task.Status != config.StatusRunning {
		return true
	}
	return false
}

func (p *ScanPlugin) IsTaskFailed() bool {
	if p.Task.Status == config.StatusFailed || p.Task.Status == config.StatusTimeout || p.Task.Status == config.StatusCancelled {
		return true
	}
	return false
}

// since scan is a standalone job, a subtask should not have startTime and endtime

func (p *ScanPlugin) SetStartTime() {
	//p.Task.S = time.Now().Unix()
}

func (p *ScanPlugin) SetEndTime() {
	//p.Task.EndTime = time.Now().Unix()
}

// scan job is a standalone job so it is always enabled
func (p *ScanPlugin) IsTaskEnabled() bool {
	return true
}

func (p *ScanPlugin) ResetError() {
	p.Task.Error = ""
}

// Note: Since there are few environment variables and few variables to be replaced,
// this method is temporarily used.
func (p *ScanPlugin) renderEnv(data string, envs []*task.KeyVal) string {
	mapper := func(data string) string {
		for _, envar := range envs {
			if data != envar.Key {
				continue
			}

			return envar.Value
		}

		return fmt.Sprintf("$%s", data)
	}

	return os.Expand(data, mapper)
}
