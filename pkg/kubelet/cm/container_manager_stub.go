// +build !linux

/*
Copyright 2015 The Kubernetes Authors.

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

package cm

import (
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/util/mount"
)

type stubContainerManager struct{}

func (stubContainerManager) Start() error {
	return nil
}

func (stubContainerManager) SystemCgroupsLimit() api.ResourceList {
	return api.ResourceList{}
}

func (stubContainerManager) GetNodeConfig() NodeConfig {
	return NodeConfig{}
}

func (cm *stubContainerManager) Status() Status {
	return Status{}
}

func NewContainerManager(_ mount.Interface, _ cadvisor.Interface, _ NodeConfig) (ContainerManager, error) {
	return &stubContainerManager{}, nil
}
