/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package registry tracks which namespaces should have the injection webhook
// applied. Multiple ConfigMaps may select the same namespace; we refcount by
// the set of ConfigMap keys (namespace/name) that requested it so removing one
// ConfigMap does not unwatch a namespace another still wants.
package registry

import "sync"

// Registry is safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	// namespace -> set of configmap keys ("ns/name") that selected it
	refs map[string]map[string]struct{}
}

func New() *Registry {
	return &Registry{refs: map[string]map[string]struct{}{}}
}

// Set records that the given ConfigMap selects the given namespaces, replacing
// any previous selection from the same ConfigMap. Returns the namespaces that
// became newly watched (had no refs before this call) so the caller can
// reconcile pre-existing pods in them.
func (r *Registry) Set(cmKey string, namespaces []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	desired := make(map[string]struct{}, len(namespaces))
	for _, ns := range namespaces {
		if ns == "" {
			continue
		}
		desired[ns] = struct{}{}
	}

	// Remove stale references from this CM.
	for ns, owners := range r.refs {
		if _, want := desired[ns]; want {
			continue
		}
		if _, had := owners[cmKey]; had {
			delete(owners, cmKey)
			if len(owners) == 0 {
				delete(r.refs, ns)
			}
		}
	}

	var newlyWatched []string
	for ns := range desired {
		owners, exists := r.refs[ns]
		if !exists {
			owners = map[string]struct{}{}
			r.refs[ns] = owners
			newlyWatched = append(newlyWatched, ns)
		}
		owners[cmKey] = struct{}{}
	}
	return newlyWatched
}

// Delete drops all references owned by the given ConfigMap.
func (r *Registry) Delete(cmKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ns, owners := range r.refs {
		if _, had := owners[cmKey]; !had {
			continue
		}
		delete(owners, cmKey)
		if len(owners) == 0 {
			delete(r.refs, ns)
		}
	}
}

// Has reports whether the namespace is currently selected by any ConfigMap.
func (r *Registry) Has(namespace string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.refs[namespace]
	return ok
}
