/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
)

// A nil *SDKInjectionMetrics is a valid no-op recorder so call sites need no
// guards when metrics are disabled.
func TestNilRecorderIsNoOp(t *testing.T) {
	var m *SDKInjectionMetrics
	m.RecordRequest("demo", "Deployment", "hello", webhookv1.OutcomeSuccess)
	m.RecordRestart("demo", "Deployment", "hello")
}

func TestMustRegister(t *testing.T) {
	m := NewSDKInjectionMetrics()
	reg := prometheus.NewRegistry()
	m.MustRegister(reg)
	m.RecordRequest("demo", "Deployment", "hello", webhookv1.OutcomeSuccess)

	count, err := testutil.GatherAndCount(reg, "beyla_sdk_injection_requests_total")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if count != 1 {
		t.Fatalf("series count = %d, want 1", count)
	}
}
