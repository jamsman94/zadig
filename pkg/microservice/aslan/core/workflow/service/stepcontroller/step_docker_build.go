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

package stepcontroller

import (
	"context"
	"fmt"

	"github.com/koderover/zadig/pkg/microservice/aslan/config"
	commonmodels "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb"
	"github.com/koderover/zadig/pkg/types/step"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

type dockerBuildCtl struct {
	step            *commonmodels.StepTask
	dockerBuildSpec *step.StepDockerBuildSpec
	log             *zap.SugaredLogger
}

func NewDockerBuildCtl(stepTask *commonmodels.StepTask, log *zap.SugaredLogger) (*dockerBuildCtl, error) {
	yamlString, err := yaml.Marshal(stepTask.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal docker build spec error: %v", err)
	}
	dockerBuildSpec := &step.StepDockerBuildSpec{}
	if err := yaml.Unmarshal(yamlString, &dockerBuildSpec); err != nil {
		return nil, fmt.Errorf("unmarshal docker build spec error: %v", err)
	}
	if dockerBuildSpec.Proxy == nil {
		dockerBuildSpec.Proxy = &step.Proxy{}
	}
	return &dockerBuildCtl{dockerBuildSpec: dockerBuildSpec, log: log, step: stepTask}, nil
}

func (s *dockerBuildCtl) PreRun(ctx context.Context) error {
	if s.dockerBuildSpec.DockerRegistry != nil && s.dockerBuildSpec.DockerRegistry.DockerRegistryID != "" {
		reg, _ := mongodb.NewRegistryNamespaceColl().Find(&mongodb.FindRegOps{ID: s.dockerBuildSpec.DockerRegistry.DockerRegistryID})
		s.dockerBuildSpec.DockerRegistry.UserName = reg.AccessKey
		s.dockerBuildSpec.DockerRegistry.Password = reg.SecretKey
		s.dockerBuildSpec.DockerRegistry.Namespace = reg.Namespace
		s.dockerBuildSpec.DockerRegistry.Host = reg.RegAddr
	} else {
		s.dockerBuildSpec.DockerRegistry = &step.DockerRegistry{
			UserName:  config.RegistryAccessKey(),
			Password:  config.RegistrySecretKey(),
			Namespace: config.RegistryNamespace(),
			Host:      config.RegistryAddress(),
		}
	}

	proxies, _ := mongodb.NewProxyColl().List(&mongodb.ProxyArgs{})
	if len(proxies) != 0 {
		s.dockerBuildSpec.Proxy.Address = proxies[0].Address
		s.dockerBuildSpec.Proxy.EnableApplicationProxy = proxies[0].EnableApplicationProxy
		s.dockerBuildSpec.Proxy.EnableRepoProxy = proxies[0].EnableRepoProxy
		s.dockerBuildSpec.Proxy.NeedPassword = proxies[0].NeedPassword
		s.dockerBuildSpec.Proxy.Password = proxies[0].Password
		s.dockerBuildSpec.Proxy.Port = proxies[0].Port
		s.dockerBuildSpec.Proxy.Type = proxies[0].Type
		s.dockerBuildSpec.Proxy.Username = proxies[0].Username
	}
	s.step.Spec = s.dockerBuildSpec
	return nil
}

func (s *dockerBuildCtl) Run(ctx context.Context) (config.Status, error) {
	return config.StatusPassed, nil
}

func (s *dockerBuildCtl) AfterRun(ctx context.Context) error {
	return nil
}
