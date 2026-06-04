/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package registry holds the in-memory model of which pods should be touched
// by the injector. Selector ConfigMaps contribute an InjectConfig (a list of
// rules plus the CM-level image version); the webhook and controller test pods
// against the rules via Match.
package registry

import (
	"sort"
	"sync"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
)

// Instrumentation is one selector ConfigMap's contribution to the registry:
// the parsed InjectConfig Beyla wrote, carrying the ordered rules the webhook
// evaluates against each pod plus the CM-level image version.
type Instrumentation struct {
	InjectConfig configmap.InjectConfig
}

// PodInfo is the projection of a Pod that Match consumes. OwnerChain is
// pre-resolved by podinfo.Resolve so the match hot path needs no API calls.
type PodInfo struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	// OwnerChain is the resolved ownership chain, starting with the pod itself
	// (Kind: "Pod") followed by its controller owners in ascending order
	// (e.g. ReplicaSet, then Deployment). Built by podinfo.Resolve.
	OwnerChain []configmap.Owner
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

// Set replaces this CM's contribution. An empty Rules slice is equivalent
// to Delete: a CM with no rules can't match anything.
func (r *Registry) Set(cmKey string, inst Instrumentation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(inst.InjectConfig.Rules) == 0 {
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

// Match returns the first rule whose selector matches the pod, the InjectConfig
// of the owning ConfigMap (for the CM-level image version), and a boolean
// indicating any match. CMs are evaluated in sorted key order for determinism;
// within each CM, rules are evaluated in order and first-match wins.
func (r *Registry) Match(p PodInfo) (configmap.Rule, configmap.InjectConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.instruments))
	for k := range r.instruments {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	input := buildMatchInput(p)
	for _, k := range keys {
		inst := r.instruments[k]
		for _, rule := range inst.InjectConfig.Rules {
			if !rule.Selector.Match(input) {
				continue
			}
			// First matching rule wins. A skip rule means the pod is explicitly
			// excluded — return no match so it is not instrumented (and do not
			// fall through to later install rules, which is how
			// "instrument everything except X" works).
			if rule.Config.Mode == configmap.ModeSkip {
				return configmap.Rule{}, configmap.InjectConfig{}, false
			}
			return rule, inst.InjectConfig, true
		}
	}
	return configmap.Rule{}, configmap.InjectConfig{}, false
}

func buildMatchInput(p PodInfo) configmap.MatchInput {
	return configmap.MatchInput{
		Namespace:   p.Namespace,
		OwnerChain:  p.OwnerChain,
		Labels:      p.Labels,
		Annotations: p.Annotations,
	}
}
