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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/podinfo"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
)

// Status values for the status label on beyla_injection_pods.
const (
	StatusInstrumented   = "instrumented"
	StatusPendingRestart = "pending_restart"
	StatusSkipped        = "skipped"
	StatusUnmatched      = "unmatched"
)

// SkipReasonConflict is the skip_reason label value set when status=skipped
// because the pod carries a foreign LD_PRELOAD. It is the only skip reason the
// state gauge emits: an already-instrumented pod is reported as its own
// status=instrumented, not as a skip.
const SkipReasonConflict = "conflict"

// systemNamespaces are excluded from state metrics regardless of selector
// scope, matching beyla's state collector.
var systemNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-node-lease": true,
	"kube-public":     true,
}

var collectorLog = logf.Log.WithName("metrics-collector")

// PodStateCollector is a prometheus.Collector that, on each scrape, lists pods
// from the manager cache and classifies each into an injection state.
type PodStateCollector struct {
	reader client.Reader
	reg    *registry.Registry
	cfg    config.SDKInject
	desc   *prometheus.Desc
}

// NewPodStateCollector builds the collector. reader should be the manager's
// (cached) client, reg the live selector registry, and cfg the controller-wide
// SDK default (per-ConfigMap overrides are layered on at classification time).
func NewPodStateCollector(reader client.Reader, reg *registry.Registry, cfg config.SDKInject) *PodStateCollector {
	return &PodStateCollector{
		reader: reader,
		reg:    reg,
		cfg:    cfg,
		desc: prometheus.NewDesc(
			metricPrefix+"_injection_pods",
			"Current number of pods in each SDK injection state.",
			[]string{"k8s_namespace_name", "k8s_workload_kind", "k8s_workload_name", "k8s_node_name", "status", "skip_reason"},
			nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *PodStateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// labelTuple is the aggregation key for one gauge sample.
type labelTuple struct {
	namespace    string
	workloadKind string
	workloadName string
	nodeName     string
	status       string
	skipReason   string
}

// collectTimeout bounds a single scrape's cache reads so a pathological
// Collect can't stall the whole /metrics response indefinitely.
const collectTimeout = 10 * time.Second

// Collect implements prometheus.Collector.
func (c *PodStateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	var pods corev1.PodList
	if err := c.reader.List(ctx, &pods); err != nil {
		collectorLog.Error(err, "listing pods for state metrics")
		return
	}

	counts := map[labelTuple]int{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if systemNamespaces[pod.Namespace] {
			continue
		}
		cl := c.classify(ctx, pod)
		counts[labelTuple{
			namespace:    pod.Namespace,
			workloadKind: cl.kind,
			workloadName: cl.name,
			nodeName:     pod.Spec.NodeName,
			status:       cl.status,
			skipReason:   cl.skipReason,
		}]++
	}

	for lt, count := range counts {
		ch <- prometheus.MustNewConstMetric(
			c.desc,
			prometheus.GaugeValue,
			float64(count),
			lt.namespace,
			lt.workloadKind,
			lt.workloadName,
			lt.nodeName,
			lt.status,
			lt.skipReason,
		)
	}
}

// classification is the per-pod result of classify.
type classification struct {
	status     string
	skipReason string
	kind       string
	name       string
}

// classify determines a pod's injection state using the same predicates the
// webhook applies at admission, so the gauge and the request counter never
// disagree. Order mirrors PodCustomDefaulter.Default:
//   - no registry match            -> unmatched
//   - our annotation at wantVer    -> instrumented
//   - foreign LD_PRELOAD           -> skipped / conflict
//   - otherwise (matched, not yet) -> pending_restart
//
// AlreadyInstrumented is checked before the LD_PRELOAD conflict (matching the
// webhook): a pod we already instrumented carries our own LD_PRELOAD, not a
// foreign one, so the conflict predicate is false for it anyway. The order only
// matters for the rare pod that is both annotated at the wanted version and
// carries a foreign LD_PRELOAD on another container; reporting it as
// instrumented keeps the gauge consistent with the admission counter.
func (c *PodStateCollector) classify(ctx context.Context, pod *corev1.Pod) classification {
	info := podinfo.Resolve(ctx, c.reader, pod)
	kind, name := podinfo.Workload(info)

	match, cfg, ok := c.reg.Match(info)
	if !ok {
		return classification{status: StatusUnmatched, kind: kind, name: name}
	}

	effective := c.cfg.WithConfigMapOverrides(cfg)
	want := config.PodConfigHash(&effective, &match.Config)
	if webhookv1.IsInstrumentedWithWantedConfig(&pod.Spec, &pod.ObjectMeta, want) {
		return classification{status: StatusInstrumented, kind: kind, name: name}
	}
	if webhookv1.PreloadsSomethingElse(pod) {
		return classification{status: StatusSkipped, skipReason: SkipReasonConflict, kind: kind, name: name}
	}
	return classification{status: StatusPendingRestart, kind: kind, name: name}
}
