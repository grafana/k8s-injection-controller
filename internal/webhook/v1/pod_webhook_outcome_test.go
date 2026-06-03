/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.opentelemetry.io/obi/pkg/appolly/services"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
)

// fakeRecorder captures the outcome of the last recorded request so a test can
// assert which branch of Default fired.
type fakeRecorder struct {
	namespace, kind, name, outcome string
	calls                          int
}

func (f *fakeRecorder) RecordRequest(namespace, workloadKind, workloadName, outcome string) {
	f.namespace, f.kind, f.name, f.outcome = namespace, workloadKind, workloadName, outcome
	f.calls++
}

// outcomeTestNS is the namespace every outcome-test pod and criterion uses.
const outcomeTestNS = "outcome-test"

// outcomePod is a bare pod (no owner refs) in outcomeTestNS with the supplied
// env and annotations; podinfo resolves it as a ("Pod", name) workload.
func outcomePod(name string, env []corev1.EnvVar, ann map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: outcomeTestNS, Name: name, Annotations: ann},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx", Env: env}}},
	}
}

// matchAll returns a registry whose single criterion matches every pod in
// outcomeTestNS.
func matchAll() *registry.Registry {
	g := services.NewGlob(outcomeTestNS)
	r := registry.New()
	r.Set("test-cm", registry.Instrumentation{
		Criteria: []registry.SelectionCriterion{{K8sNamespace: &g}},
	})
	return r
}

// TestDefaultRecordsOutcome asserts that each terminal branch of Default
// records the matching outcome label. These outcomes are the producer side of
// beyla_sdk_injection_requests_total, so a wrong constant would silently split
// a metric series — this guards against that.
func TestDefaultRecordsOutcome(t *testing.T) {
	cfg := config.SDKInject{ImageVolumeRoot: "ghcr.io/grafana/beyla/inject-sdk-image", ImageVersion: "0.0.11"}
	wantVersion := cfg.PackageVersion()

	tests := []struct {
		name        string
		registry    *registry.Registry
		mutatorCfg  config.SDKInject
		pod         *corev1.Pod
		wantOutcome string
	}{
		{
			name:        "no criterion matches",
			registry:    registry.New(),
			mutatorCfg:  cfg,
			pod:         outcomePod("p1", nil, nil),
			wantOutcome: OutcomeNoMatchingSelector,
		},
		{
			name:        "matched but no SDK config loaded",
			registry:    matchAll(),
			mutatorCfg:  config.SDKInject{}, // empty ImageVolumePath
			pod:         outcomePod("p2", nil, nil),
			wantOutcome: OutcomeNoSDKConfig,
		},
		{
			name:        "already instrumented at current version",
			registry:    matchAll(),
			mutatorCfg:  cfg,
			pod:         outcomePod("p3", nil, map[string]string{InjectedAnnotation: wantVersion}),
			wantOutcome: OutcomeAlreadyInstrumented,
		},
		{
			name:        "conflicting LD_PRELOAD",
			registry:    matchAll(),
			mutatorCfg:  cfg,
			pod:         outcomePod("p4", []corev1.EnvVar{{Name: "LD_PRELOAD", Value: "/opt/other.so"}}, nil),
			wantOutcome: OutcomeLDPreloadConflict,
		},
		{
			name:        "successful injection",
			registry:    matchAll(),
			mutatorCfg:  cfg,
			pod:         outcomePod("p5", nil, nil),
			wantOutcome: OutcomeSuccess,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeRecorder{}
			d := &PodCustomDefaulter{
				Registry: tc.registry,
				Reader:   nil, // bare pods have no owner refs, so Resolve never reads
				Mutator:  &PodMutator{Cfg: tc.mutatorCfg},
				Metrics:  rec,
			}
			if err := d.Default(context.Background(), tc.pod); err != nil {
				t.Fatalf("Default() error = %v", err)
			}
			if rec.calls != 1 {
				t.Fatalf("recorder calls = %d, want exactly 1", rec.calls)
			}
			if rec.outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q", rec.outcome, tc.wantOutcome)
			}
			if rec.namespace != outcomeTestNS {
				t.Fatalf("namespace label = %q, want %q", rec.namespace, outcomeTestNS)
			}
			// Bare pod: workload resolves to ("Pod", pod name).
			if rec.kind != "Pod" || rec.name != tc.pod.Name {
				t.Fatalf("workload labels = (%q, %q), want (%q, %q)", rec.kind, rec.name, "Pod", tc.pod.Name)
			}
		})
	}
}
