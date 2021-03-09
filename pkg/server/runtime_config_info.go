/*
Copyright 2017-2020 The Kubernetes Authors.

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

package server

import (
	"golang.org/x/net/context"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

func (c *criService) GetRuntimeConfigInfo(_ context.Context, r *runtime.GetRuntimeConfigInfoRequest) (res *runtime.GetRuntimeConfigInfoResponse, err error) {
	linuxConfig := &runtime.LinuxUserNamespaceConfig{
		UidMappings: []*runtime.LinuxIDMapping{
			&runtime.LinuxIDMapping{
				ContainerId: c.config.NodeWideUIDMapping.ContainerID,
				HostId:      c.config.NodeWideUIDMapping.HostID,
				Size_:       c.config.NodeWideUIDMapping.Size,
			},
		},
		GidMappings: []*runtime.LinuxIDMapping{
			&runtime.LinuxIDMapping{
				ContainerId: c.config.NodeWideGIDMapping.ContainerID,
				HostId:      c.config.NodeWideGIDMapping.HostID,
				Size_:       c.config.NodeWideGIDMapping.Size,
			},
		},
	}
	activeRuntimeConfig := &runtime.ActiveRuntimeConfig{UserNamespaceConfig: linuxConfig}
	return &runtime.GetRuntimeConfigInfoResponse{RuntimeConfig: activeRuntimeConfig}, nil
}
