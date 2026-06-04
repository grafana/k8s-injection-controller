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

// TestParseConfigMapRules verifies that the instrumentation.yaml document is
// parsed into the InjectConfig (image version + rules), and that selector
// fields — including the pod-label clause — survive unmarshalling.
func TestParseConfigMapRules(t *testing.T) {
	const yaml = `image_version: "1.2.3"
rules:
- k8s_selector:
    namespaces:
    - test-unmatched
    podLabels:
      inject: "true"
`
	inst, _, err := parseConfigMap(map[string]string{configmap.KeyInstrumentation: yaml})
	if err != nil {
		t.Fatalf("parseConfigMap returned error: %v", err)
	}
	if inst.InjectConfig.ImageVersion != "1.2.3" {
		t.Errorf("ImageVersion = %q, want %q", inst.InjectConfig.ImageVersion, "1.2.3")
	}
	if len(inst.InjectConfig.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(inst.InjectConfig.Rules))
	}
	sel := inst.InjectConfig.Rules[0].Selector
	if len(sel.Namespaces) != 1 || !sel.Namespaces[0].MatchString("test-unmatched") {
		t.Errorf("namespace not populated/matching: %+v", sel.Namespaces)
	}
	g, ok := sel.PodLabels["inject"]
	if !ok {
		t.Fatalf("podLabels[inject] not populated: %+v", sel.PodLabels)
	}
	if !g.MatchString("true") {
		t.Errorf("podLabels[inject] does not match \"true\"")
	}
	if g.MatchString("false") {
		t.Errorf("podLabels[inject] should not match \"false\"")
	}
}

// TestParseConfigMapRestartTargets verifies that eligible_for_restart entries
// are parsed, and that entries with an unknown kind or missing required fields
// are dropped without error.
func TestParseConfigMapRestartTargets(t *testing.T) {
	const inj = `rules:
- k8s_selector:
    namespaces:
    - demo
`
	const restart = `- namespace: demo
  kind: Deployment
  name: web
- namespace: demo
  kind: BogusKind
  name: skip-me
- namespace: demo
  name: missing-kind
`
	inst, targets, err := parseConfigMap(map[string]string{
		configmap.KeyInstrumentation:    inj,
		configmap.KeyEligibleForRestart: restart,
	})
	if err != nil {
		t.Fatalf("parseConfigMap returned error: %v", err)
	}
	if len(inst.InjectConfig.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(inst.InjectConfig.Rules))
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 valid restart target (bogus kind + missing kind dropped), got %d: %+v", len(targets), targets)
	}
	if targets[0].Kind != "Deployment" || targets[0].Name != "web" || targets[0].Namespace != "demo" {
		t.Errorf("unexpected restart target: %+v", targets[0])
	}
}

// TestParseConfigMapInvalidYAML verifies that a malformed instrumentation.yaml
// is surfaced as an error so the reconciler can drop the ConfigMap.
func TestParseConfigMapInvalidYAML(t *testing.T) {
	_, _, err := parseConfigMap(map[string]string{configmap.KeyInstrumentation: "rules: [oops"})
	if err == nil {
		t.Fatalf("expected an error for malformed YAML")
	}
}
