/*
Copyright 2026.

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

package podinfo

import (
	"testing"

	"github.com/grafana/beyla-k8s-injector/internal/registry"
)

func TestWorkload(t *testing.T) {
	tests := []struct {
		name     string
		info     registry.PodInfo
		wantKind string
		wantName string
	}{
		{
			name:     "deployment-backed pod reports as Deployment",
			info:     registry.PodInfo{Name: "hello-abc", OwnerKind: "ReplicaSet", OwnerName: "hello-abc", DeploymentName: "hello"},
			wantKind: kindDeployment,
			wantName: "hello",
		},
		{
			name:     "direct deployment owner reports as Deployment",
			info:     registry.PodInfo{Name: "hello-abc", OwnerKind: "Deployment", OwnerName: "hello", DeploymentName: "hello"},
			wantKind: kindDeployment,
			wantName: "hello",
		},
		{
			name:     "statefulset-owned pod reports the owner kind/name",
			info:     registry.PodInfo{Name: "db-0", OwnerKind: "StatefulSet", OwnerName: "db"},
			wantKind: "StatefulSet",
			wantName: "db",
		},
		{
			name:     "daemonset-owned pod reports the owner kind/name",
			info:     registry.PodInfo{Name: "agent-xyz", OwnerKind: "DaemonSet", OwnerName: "agent"},
			wantKind: "DaemonSet",
			wantName: "agent",
		},
		{
			name:     "bare pod with no owner reports as Pod with its own name",
			info:     registry.PodInfo{Name: "standalone"},
			wantKind: "Pod",
			wantName: "standalone",
		},
		{
			name:     "DeploymentName wins over a non-empty OwnerKind",
			info:     registry.PodInfo{Name: "hello-abc", OwnerKind: "ReplicaSet", OwnerName: "hello-abc", DeploymentName: "hello"},
			wantKind: kindDeployment,
			wantName: "hello",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, name := Workload(tc.info)
			if kind != tc.wantKind || name != tc.wantName {
				t.Fatalf("Workload() = (%q, %q), want (%q, %q)", kind, name, tc.wantKind, tc.wantName)
			}
		})
	}
}
