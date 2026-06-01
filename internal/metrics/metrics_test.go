/*
Copyright 2026.
*/

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
)

func TestRecordRequest(t *testing.T) {
	m := NewSDKInjectionMetrics()
	m.RecordRequest("demo", "Deployment", "hello", webhookv1.OutcomeSuccess)
	m.RecordRequest("demo", "Deployment", "hello", webhookv1.OutcomeSuccess)
	m.RecordRequest("demo", "Deployment", "hello", webhookv1.OutcomeNoMatchingSelector)

	if got := testutil.ToFloat64(m.requests.WithLabelValues("demo", "Deployment", "hello", webhookv1.OutcomeSuccess)); got != 2 {
		t.Fatalf("success count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("demo", "Deployment", "hello", webhookv1.OutcomeNoMatchingSelector)); got != 1 {
		t.Fatalf("no_matching_selector count = %v, want 1", got)
	}
}

func TestRecordRestart(t *testing.T) {
	m := NewSDKInjectionMetrics()
	m.RecordRestart("demo", "StatefulSet", "db")
	m.RecordRestart("demo", "StatefulSet", "db")
	if got := testutil.ToFloat64(m.restarts.WithLabelValues("demo", "StatefulSet", "db")); got != 2 {
		t.Fatalf("restart count = %v, want 2", got)
	}
}

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
