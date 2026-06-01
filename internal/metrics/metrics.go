/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

const metricPrefix = "beyla"

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
