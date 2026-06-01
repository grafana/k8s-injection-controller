/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
)

// TestParseConfigMapPodLabels verifies that a discovery entry narrowing a
// namespace by k8s_pod_labels survives translation into a SelectionCriterion.
func TestParseConfigMapPodLabels(t *testing.T) {
	const yaml = `discovery:
  - k8s_namespace: test-unmatched
    k8s_pod_labels:
      inject: "true"
`
	inst, _, err := parseConfigMap(map[string]string{configmap.KeyInstrumentation: yaml})
	if err != nil {
		t.Fatalf("parseConfigMap returned error: %v", err)
	}
	if len(inst.Criteria) != 1 {
		t.Fatalf("expected 1 criterion, got %d", len(inst.Criteria))
	}
	crit := inst.Criteria[0]
	if crit.K8sNamespace == nil || !crit.K8sNamespace.MatchString("test-unmatched") {
		t.Errorf("namespace not populated: %+v", crit.K8sNamespace)
	}
	g, ok := crit.K8sPodLabels["inject"]
	if !ok || g == nil {
		t.Fatalf("k8s_pod_labels[inject] not populated: %+v", crit.K8sPodLabels)
	}
	if !g.MatchString("true") {
		t.Errorf("k8s_pod_labels[inject] does not match \"true\"")
	}
	if g.MatchString("false") {
		t.Errorf("k8s_pod_labels[inject] should not match \"false\"")
	}
}

// TestParseConfigMapPodAnnotations verifies the same for k8s_pod_annotations.
func TestParseConfigMapPodAnnotations(t *testing.T) {
	const yaml = `discovery:
  - k8s_namespace: demo
    k8s_pod_annotations:
      team: obs
`
	inst, _, err := parseConfigMap(map[string]string{configmap.KeyInstrumentation: yaml})
	if err != nil {
		t.Fatalf("parseConfigMap returned error: %v", err)
	}
	if len(inst.Criteria) != 1 {
		t.Fatalf("expected 1 criterion, got %d", len(inst.Criteria))
	}
	g, ok := inst.Criteria[0].K8sPodAnnotations["team"]
	if !ok || g == nil || !g.MatchString("obs") {
		t.Fatalf("k8s_pod_annotations[team] not populated/matching: %+v", inst.Criteria[0].K8sPodAnnotations)
	}
}

// TestParseConfigMapPodLabelsOnlyIsNotEmpty guards that a discovery entry whose
// ONLY selector is a pod-label clause still produces a non-empty criterion (so
// it isn't silently dropped by the IsEmpty filter in parseConfigMap).
func TestParseConfigMapPodLabelsOnlyIsNotEmpty(t *testing.T) {
	const yaml = `discovery:
  - k8s_pod_labels:
      inject: "true"
`
	inst, _, err := parseConfigMap(map[string]string{configmap.KeyInstrumentation: yaml})
	if err != nil {
		t.Fatalf("parseConfigMap returned error: %v", err)
	}
	if len(inst.Criteria) != 1 {
		t.Fatalf("expected 1 criterion (label-only entry must not be dropped), got %d", len(inst.Criteria))
	}
}
