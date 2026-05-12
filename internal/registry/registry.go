/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package registry holds the in-memory model of which pods should be touched
// by the injector. Selector ConfigMaps contribute lists of SelectionCriterion;
// the webhook and controller test pods against the union via Match.
package registry

import (
	"sort"
	"sync"

	"go.opentelemetry.io/obi/pkg/appolly/services"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
)

// Instrumentation is one selector ConfigMap's contribution to the registry:
// the criteria that decide which pods to touch, and the export config the
// webhook should stamp onto those pods. Criteria are the controller's
// translation of the on-wire Discovery globs into typed match fields.
type Instrumentation struct {
	Criteria   []SelectionCriterion
	OtelExport configmap.OtelExport
}

// SelectionCriterion is one entry from a selector ConfigMap's
// selection_criteria.yaml. Within a criterion all populated fields must
// match (AND); a nil field is a wildcard. Across criteria the registry
// applies OR.
//
// Each field is a *services.GlobAttr (obi's glob wrapper, reused so the
// injector and Beyla agree on syntax) so values like "hello-*" match a
// family of names.
type SelectionCriterion struct {
	K8sPodName         *services.GlobAttr `json:"k8s_pod_name,omitempty"`
	K8sNamespace       *services.GlobAttr `json:"k8s_namespace,omitempty"`
	K8sDeploymentName  *services.GlobAttr `json:"k8s_deployment_name,omitempty"`
	K8sReplicaSetName  *services.GlobAttr `json:"k8s_replicaset_name,omitempty"`
	K8sStatefulSetName *services.GlobAttr `json:"k8s_statefulset_name,omitempty"`
	K8sDaemonSetName   *services.GlobAttr `json:"k8s_daemonset_name,omitempty"`
	// K8sOwnerName matches the pod's direct owner name (RS / STS / DS) or
	// the resolved Deployment name reached via the RS chain. Combinable with
	// the typed fields above (AND).
	K8sOwnerName *services.GlobAttr `json:"k8s_owner_name,omitempty"`
}

// IsEmpty reports whether no field is populated. An empty criterion would
// match every pod, which is almost always a misconfiguration; the parser
// rejects these.
func (c SelectionCriterion) IsEmpty() bool {
	return c.K8sPodName == nil &&
		c.K8sNamespace == nil &&
		c.K8sDeploymentName == nil &&
		c.K8sReplicaSetName == nil &&
		c.K8sStatefulSetName == nil &&
		c.K8sDaemonSetName == nil &&
		c.K8sOwnerName == nil
}

// PodInfo is the projection of a Pod that Match consumes. The caller is
// responsible for resolving DeploymentName by walking the pod's RS owner
// (see internal/podinfo).
type PodInfo struct {
	Name      string
	Namespace string
	// OwnerKind is the kind of the controller OwnerReference on the pod, if
	// any. Expected values: "ReplicaSet", "StatefulSet", "DaemonSet",
	// "Deployment", or "".
	OwnerKind string
	OwnerName string
	// DeploymentName is set if the pod's RS owner is itself owned by a
	// Deployment (or if the pod is directly owned by a Deployment).
	DeploymentName string
}

// Registry is safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	// instruments holds the parsed instrumentation.yaml of each tracked CM,
	// keyed by "namespace/name".
	instruments map[string]Instrumentation
}

func New() *Registry {
	return &Registry{instruments: map[string]Instrumentation{}}
}

// Set replaces this CM's contribution. An empty Criteria slice is equivalent
// to Delete: a CM with no criteria can't match anything regardless of its
// OtelExport config.
func (r *Registry) Set(cmKey string, inst Instrumentation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(inst.Criteria) == 0 {
		delete(r.instruments, cmKey)
		return
	}
	r.instruments[cmKey] = inst
}

// Delete drops all of this CM's contribution.
func (r *Registry) Delete(cmKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.instruments, cmKey)
}

// Match returns the first injection record whose criteria match the pod, plus
// a boolean indicating any match. Iteration is in sorted cmKey order so when
// multiple CMs select the same pod, the chosen OtelExport is deterministic.
func (r *Registry) Match(p PodInfo) (Instrumentation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.instruments))
	for k := range r.instruments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		inst := r.instruments[k]
		for _, c := range inst.Criteria {
			if criterionMatches(c, p) {
				return inst, true
			}
		}
	}
	return Instrumentation{}, false
}

func criterionMatches(c SelectionCriterion, p PodInfo) bool {
	if c.K8sPodName != nil && !c.K8sPodName.MatchString(p.Name) {
		return false
	}
	if c.K8sNamespace != nil && !c.K8sNamespace.MatchString(p.Namespace) {
		return false
	}
	if c.K8sReplicaSetName != nil && (p.OwnerKind != "ReplicaSet" || !c.K8sReplicaSetName.MatchString(p.OwnerName)) {
		return false
	}
	if c.K8sStatefulSetName != nil && (p.OwnerKind != "StatefulSet" || !c.K8sStatefulSetName.MatchString(p.OwnerName)) {
		return false
	}
	if c.K8sDaemonSetName != nil && (p.OwnerKind != "DaemonSet" || !c.K8sDaemonSetName.MatchString(p.OwnerName)) {
		return false
	}
	if c.K8sDeploymentName != nil {
		if p.DeploymentName == "" || !c.K8sDeploymentName.MatchString(p.DeploymentName) {
			return false
		}
	}
	if c.K8sOwnerName != nil && !ownerNameMatches(c.K8sOwnerName, p) {
		return false
	}
	// An entirely empty criterion (no field set) matches every pod by design;
	// callers are expected to validate at parse time if they want to forbid that.
	return true
}

func ownerNameMatches(g *services.GlobAttr, p PodInfo) bool {
	switch p.OwnerKind {
	case "ReplicaSet", "StatefulSet", "DaemonSet", "Deployment":
		if g.MatchString(p.OwnerName) {
			return true
		}
	}
	if p.DeploymentName != "" && g.MatchString(p.DeploymentName) {
		return true
	}
	return false
}
