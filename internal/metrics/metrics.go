/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package metrics exposes Prometheus metrics describing SDK injection activity.
//
// It re-homes the three metrics originally added on beyla's injection-metrics
// branch into this controller's structure. All metrics keep their beyla_*
// names for continuity with existing dashboards and the Instrumentation Hub.
// They register on controller-runtime's global registry, so they are served on
// the manager's --metrics-bind-address endpoint and scraped by the existing
// ServiceMonitor — no separate exporter or HTTP server.
//
//   - beyla_sdk_injection_requests_total  (CounterVec) — webhook admission
//     outcomes, recorded inline by the pod webhook.
//   - beyla_sdk_injection_restarts_total  (CounterVec) — workload rollouts the
//     controller triggers, recorded inline by the ConfigMap reconciler.
//   - beyla_injection_pods                (Gauge via PodStateCollector) — the
//     current cluster-wide instrumentation state, computed at scrape time.
//
// beyla's counters also carried a "language" label; this codebase has no
// per-pod language signal (injection is language-agnostic), so that label is
// dropped rather than emitted permanently empty.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metricPrefix is beyla's vendor prefix, kept verbatim so the metric names
// match the original injection-metrics branch.
const metricPrefix = "beyla"

// Outcome constants for beyla_sdk_injection_requests_total. Only the values
// reachable in this codebase's admission path are defined; beyla's
// patch-generation outcomes do not apply because the controller-runtime
// defaulter mutates the pod in place rather than building a JSON patch.
const (
	OutcomeSuccess             = "success"
	OutcomeNoMatchingSelector  = "no_matching_selector"
	OutcomeNoSDKConfig         = "no_sdk_config"
	OutcomeAlreadyInstrumented = "already_instrumented"
	OutcomeLDPreloadConflict   = "ld_preload_conflict"
)

// SDKInjectionMetrics holds the injection event counters.
type SDKInjectionMetrics struct {
	requests *prometheus.CounterVec
	restarts *prometheus.CounterVec
}

// NewSDKInjectionMetrics builds the counters. Register them with MustRegister
// before use.
func NewSDKInjectionMetrics() *SDKInjectionMetrics {
	return &SDKInjectionMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "_sdk_injection_requests_total",
			Help: "SDK injection admission requests by outcome",
		}, []string{"k8s_namespace_name", "k8s_workload_kind", "k8s_workload_name", "outcome"}),
		restarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "_sdk_injection_restarts_total",
			Help: "Workload rollouts triggered for SDK injection",
		}, []string{"k8s_namespace_name", "k8s_workload_kind", "k8s_workload_name"}),
	}
}

// MustRegister registers the counters on reg. The caller passes
// controller-runtime's metrics.Registry.
func (m *SDKInjectionMetrics) MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(m.requests, m.restarts)
}

// RecordRequest records one admission request with its outcome.
func (m *SDKInjectionMetrics) RecordRequest(namespace, workloadKind, workloadName, outcome string) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(namespace, workloadKind, workloadName, outcome).Inc()
}

// RecordRestart records one workload rollout triggered for SDK injection.
func (m *SDKInjectionMetrics) RecordRestart(namespace, workloadKind, workloadName string) {
	if m == nil {
		return
	}
	m.restarts.WithLabelValues(namespace, workloadKind, workloadName).Inc()
}
