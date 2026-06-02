/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package metrics

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.opentelemetry.io/obi/pkg/appolly/services"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
)

func glob(p string) *services.GlobAttr {
	a := services.NewGlob(p)
	return &a
}

func deployOwner(name string) metav1.OwnerReference {
	ctrl := true
	return metav1.OwnerReference{Kind: "Deployment", Name: name, Controller: &ctrl}
}

// testPod builds a pod with one container.
func testPod(ns, name string, owners []metav1.OwnerReference, env []corev1.EnvVar, ann map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			Annotations:     ann,
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Env: env}},
		},
	}
}

// demoRegistry matches everything in namespace "demo".
func demoRegistry() *registry.Registry {
	r := registry.New()
	r.Set("test/cm", registry.Instrumentation{
		Criteria: []registry.SelectionCriterion{{K8sNamespace: glob("demo")}},
	})
	return r
}

func TestClassify(t *testing.T) {
	cfg := config.SDKInject{ImageVolumePath: "registry.example/img:1"}
	wantVersion := cfg.PackageVersion()

	tests := []struct {
		name           string
		pod            *corev1.Pod
		wantStatus     string
		wantSkipReason string
		wantKind       string
		wantName       string
	}{
		{
			name:       "unmatched pod (other namespace)",
			pod:        testPod("other", "p1", []metav1.OwnerReference{deployOwner("hello")}, nil, nil),
			wantStatus: StatusUnmatched,
			wantKind:   "Deployment",
			wantName:   "hello",
		},
		{
			name:       "matched but not yet instrumented -> pending_restart",
			pod:        testPod("demo", "p2", []metav1.OwnerReference{deployOwner("hello")}, nil, nil),
			wantStatus: StatusPendingRestart,
			wantKind:   "Deployment",
			wantName:   "hello",
		},
		{
			name: "matched + our annotation at current version -> instrumented",
			pod: testPod("demo", "p3", []metav1.OwnerReference{deployOwner("hello")}, nil,
				map[string]string{webhookv1.InjectedAnnotation: wantVersion}),
			wantStatus: StatusInstrumented,
			wantKind:   "Deployment",
			wantName:   "hello",
		},
		{
			name: "matched + foreign LD_PRELOAD -> skipped/conflict",
			pod: testPod("demo", "p4", []metav1.OwnerReference{deployOwner("hello")},
				[]corev1.EnvVar{{Name: "LD_PRELOAD", Value: "/opt/other.so"}}, nil),
			wantStatus:     StatusSkipped,
			wantSkipReason: SkipReasonConflict,
			wantKind:       "Deployment",
			wantName:       "hello",
		},
		{
			name:       "standalone matched pod reports as Pod workload",
			pod:        testPod("demo", "p5", nil, nil, nil),
			wantStatus: StatusPendingRestart,
			wantKind:   "Pod",
			wantName:   "p5",
		},
	}

	c := NewPodStateCollector(nil, demoRegistry(), cfg)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := c.classify(context.Background(), tc.pod)
			if got.status != tc.wantStatus || got.skipReason != tc.wantSkipReason {
				t.Fatalf("status=%q skipReason=%q, want status=%q skipReason=%q",
					got.status, got.skipReason, tc.wantStatus, tc.wantSkipReason)
			}
			if got.kind != tc.wantKind || got.name != tc.wantName {
				t.Fatalf("workload=(%q,%q), want (%q,%q)", got.kind, got.name, tc.wantKind, tc.wantName)
			}
		})
	}
}

func TestCollectAggregatesAndExcludesSystemNamespaces(t *testing.T) {
	cfg := config.SDKInject{ImageVolumePath: "registry.example/img:1"}

	// Two pods of the same workload -> one aggregated series with value 2.
	p1 := testPod("demo", "hello-a", []metav1.OwnerReference{deployOwner("hello")}, nil, nil)
	p2 := testPod("demo", "hello-b", []metav1.OwnerReference{deployOwner("hello")}, nil, nil)
	// System-namespace pod must be excluded entirely.
	sys := testPod("kube-system", "kube-thing", []metav1.OwnerReference{deployOwner("kube-thing")}, nil, nil)

	cl := fake.NewClientBuilder().WithObjects(p1, p2, sys).Build()
	c := NewPodStateCollector(cl, demoRegistry(), cfg)

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var found bool
	var series int
	var value float64
	for _, mf := range mfs {
		if mf.GetName() != "beyla_injection_pods" {
			continue
		}
		found = true
		for _, m := range mf.Metric {
			series++
			// every emitted series must be in the demo namespace (system excluded)
			for _, l := range m.Label {
				if l.GetName() == "k8s_namespace_name" && l.GetValue() != "demo" {
					t.Fatalf("unexpected namespace in series: %q", l.GetValue())
				}
			}
			value = m.GetGauge().GetValue()
		}
	}
	if !found {
		t.Fatal("beyla_injection_pods family not found")
	}
	if series != 1 {
		t.Fatalf("series = %d, want 1 (the two demo pods aggregate into one)", series)
	}
	if value != 2 {
		t.Fatalf("gauge value = %v, want 2", value)
	}
}
