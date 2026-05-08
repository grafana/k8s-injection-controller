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

import "sync"

// SelectionCriterion is one entry from a selector ConfigMap's
// selection_criteria.yaml. Within a criterion all populated fields must
// match (AND); a nil field is a wildcard. Across criteria the registry
// applies OR.
//
// Each field is a *GlobAttr so values like "hello-*" match a family of names.
// JSON tags double as the YAML schema: sigs.k8s.io/yaml decodes through JSON,
// which invokes GlobAttr.UnmarshalText on each populated string.
type SelectionCriterion struct {
	K8sPodName         *GlobAttr `json:"k8s_pod_name,omitempty"`
	K8sNamespace       *GlobAttr `json:"k8s_namespace,omitempty"`
	K8sDeploymentName  *GlobAttr `json:"k8s_deployment_name,omitempty"`
	K8sReplicaSetName  *GlobAttr `json:"k8s_replicaset_name,omitempty"`
	K8sStatefulSetName *GlobAttr `json:"k8s_statefulset_name,omitempty"`
	K8sDaemonSetName   *GlobAttr `json:"k8s_daemonset_name,omitempty"`
	// K8sOwnerName matches the pod's direct owner name (RS / STS / DS) or
	// the resolved Deployment name reached via the RS chain. Combinable with
	// the typed fields above (AND).
	K8sOwnerName *GlobAttr `json:"k8s_owner_name,omitempty"`
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
	// criteria holds the parsed selection_criteria.yaml of each tracked CM,
	// keyed by "namespace/name".
	criteria map[string][]SelectionCriterion
}

func New() *Registry {
	return &Registry{criteria: map[string][]SelectionCriterion{}}
}

// Set replaces this CM's contribution with the supplied criteria. An empty
// criteria slice is equivalent to Delete.
func (r *Registry) Set(cmKey string, criteria []SelectionCriterion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(criteria) == 0 {
		delete(r.criteria, cmKey)
		return
	}
	r.criteria[cmKey] = criteria
}

// Delete drops all of this CM's criteria.
func (r *Registry) Delete(cmKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.criteria, cmKey)
}

// Match reports whether any criterion across any tracked ConfigMap matches the
// given pod.
func (r *Registry) Match(p PodInfo) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, list := range r.criteria {
		for _, c := range list {
			if criterionMatches(c, p) {
				return true
			}
		}
	}
	return false
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

func ownerNameMatches(g *GlobAttr, p PodInfo) bool {
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
