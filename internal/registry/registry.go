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
// match (AND); empty fields are wildcards. Across criteria the registry
// applies OR.
//
// JSON tags double as the YAML schema: sigs.k8s.io/yaml decodes through JSON.
type SelectionCriterion struct {
	K8sPodName         string `json:"k8s_pod_name,omitempty"`
	K8sNamespace       string `json:"k8s_namespace,omitempty"`
	K8sDeploymentName  string `json:"k8s_deployment_name,omitempty"`
	K8sReplicaSetName  string `json:"k8s_replicaset_name,omitempty"`
	K8sStatefulSetName string `json:"k8s_statefulset_name,omitempty"`
	K8sDaemonSetName   string `json:"k8s_daemonset_name,omitempty"`
	// K8sOwnerName matches the pod's direct owner name (RS / STS / DS) or
	// the resolved Deployment name reached via the RS chain. Combinable with
	// the typed fields above (AND).
	K8sOwnerName string `json:"k8s_owner_name,omitempty"`
}

// IsEmpty reports whether no field is populated. An empty criterion would
// match every pod, which is almost always a misconfiguration; the parser
// rejects these.
func (c SelectionCriterion) IsEmpty() bool {
	return c == SelectionCriterion{}
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
	// nsRefs counts which ConfigMaps mention each namespace. Only used to
	// drive the eviction step (newly-watched namespaces); cluster-wide
	// criteria don't appear here.
	nsRefs map[string]map[string]struct{}
}

func New() *Registry {
	return &Registry{
		criteria: map[string][]SelectionCriterion{},
		nsRefs:   map[string]map[string]struct{}{},
	}
}

// Set replaces this CM's contribution with the supplied criteria. It returns
// the namespaces that became newly watched (had no nsRefs before this call)
// so the caller can reconcile pre-existing pods in them.
func (r *Registry) Set(cmKey string, criteria []SelectionCriterion) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	desiredNS := map[string]struct{}{}
	for _, c := range criteria {
		if c.K8sNamespace != "" {
			desiredNS[c.K8sNamespace] = struct{}{}
		}
	}

	// Drop stale namespace refs from this CM.
	for ns, owners := range r.nsRefs {
		if _, want := desiredNS[ns]; want {
			continue
		}
		if _, had := owners[cmKey]; had {
			delete(owners, cmKey)
			if len(owners) == 0 {
				delete(r.nsRefs, ns)
			}
		}
	}

	var newlyWatched []string
	for ns := range desiredNS {
		owners, exists := r.nsRefs[ns]
		if !exists {
			owners = map[string]struct{}{}
			r.nsRefs[ns] = owners
			newlyWatched = append(newlyWatched, ns)
		}
		owners[cmKey] = struct{}{}
	}

	if len(criteria) == 0 {
		delete(r.criteria, cmKey)
	} else {
		r.criteria[cmKey] = criteria
	}
	return newlyWatched
}

// Delete drops all of this CM's criteria and namespace refs.
func (r *Registry) Delete(cmKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.criteria, cmKey)
	for ns, owners := range r.nsRefs {
		if _, had := owners[cmKey]; !had {
			continue
		}
		delete(owners, cmKey)
		if len(owners) == 0 {
			delete(r.nsRefs, ns)
		}
	}
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
	if c.K8sPodName != "" && c.K8sPodName != p.Name {
		return false
	}
	if c.K8sNamespace != "" && c.K8sNamespace != p.Namespace {
		return false
	}
	if c.K8sReplicaSetName != "" && (p.OwnerKind != "ReplicaSet" || c.K8sReplicaSetName != p.OwnerName) {
		return false
	}
	if c.K8sStatefulSetName != "" && (p.OwnerKind != "StatefulSet" || c.K8sStatefulSetName != p.OwnerName) {
		return false
	}
	if c.K8sDaemonSetName != "" && (p.OwnerKind != "DaemonSet" || c.K8sDaemonSetName != p.OwnerName) {
		return false
	}
	if c.K8sDeploymentName != "" && c.K8sDeploymentName != p.DeploymentName {
		return false
	}
	if c.K8sOwnerName != "" && !ownerNameMatches(c.K8sOwnerName, p) {
		return false
	}
	// An entirely empty criterion (no field set) matches every pod by design;
	// callers are expected to validate at parse time if they want to forbid that.
	return true
}

func ownerNameMatches(name string, p PodInfo) bool {
	switch p.OwnerKind {
	case "ReplicaSet", "StatefulSet", "DaemonSet", "Deployment":
		if p.OwnerName == name {
			return true
		}
	}
	if p.DeploymentName != "" && p.DeploymentName == name {
		return true
	}
	return false
}
