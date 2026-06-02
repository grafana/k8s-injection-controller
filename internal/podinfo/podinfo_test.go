/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
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
			name:     "DeploymentName wins, reports as Deployment",
			info:     registry.PodInfo{Name: "hello-abc", OwnerKind: "ReplicaSet", OwnerName: "hello-abc", DeploymentName: "hello"},
			wantKind: kindDeployment,
			wantName: "hello",
		},
		{
			name:     "non-Deployment owner reports the owner kind/name",
			info:     registry.PodInfo{Name: "db-0", OwnerKind: "StatefulSet", OwnerName: "db"},
			wantKind: "StatefulSet",
			wantName: "db",
		},
		{
			name:     "bare pod with no owner reports as Pod with its own name",
			info:     registry.PodInfo{Name: "standalone"},
			wantKind: "Pod",
			wantName: "standalone",
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
