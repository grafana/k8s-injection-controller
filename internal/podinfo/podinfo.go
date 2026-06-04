/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package podinfo resolves a Pod's ownership chain into a registry.PodInfo so
// the controller and webhook can reuse the same lookup logic. It walks
// pod -> ReplicaSet -> Deployment when applicable; other owner kinds are
// appended directly after the pod itself.
package podinfo

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/grafana/beyla-k8s-injector/internal/registry"
	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
)

// kindDeployment is the owner kind we resolve the RS chain up to.
const kindDeployment = "Deployment"

// Resolve builds a registry.PodInfo from the given pod. The pod itself is
// always the first link in OwnerChain (Kind: "Pod"), followed by its
// controller owners. If the pod is owned by a ReplicaSet, Resolve fetches
// that RS to add any Deployment ancestor. Lookup errors are logged and treated
// as "chain truncated here" — the pod still matches criteria that only require
// earlier chain entries.
func Resolve(ctx context.Context, c client.Reader, pod *corev1.Pod) registry.PodInfo {
	info := registry.PodInfo{
		Name:        pod.Name,
		Namespace:   pod.Namespace,
		Labels:      pod.Labels,
		Annotations: pod.Annotations,
		OwnerChain:  []configmap.Owner{{Kind: "Pod", Name: pod.Name}},
	}
	owner := controllerRef(pod.OwnerReferences)
	if owner == nil {
		return info
	}
	info.OwnerChain = append(info.OwnerChain, configmap.Owner{Kind: owner.Kind, Name: owner.Name})

	if owner.Kind == "ReplicaSet" {
		var rs appsv1.ReplicaSet
		key := types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}
		if err := c.Get(ctx, key, &rs); err != nil {
			if !apierrors.IsNotFound(err) {
				log.FromContext(ctx).Error(err, "failed to resolve ReplicaSet for pod owner chain",
					"pod", pod.Name, "namespace", pod.Namespace, "replicaset", owner.Name)
			}
			return info
		}
		if rsOwner := controllerRef(rs.OwnerReferences); rsOwner != nil && rsOwner.Kind == kindDeployment {
			info.OwnerChain = append(info.OwnerChain, configmap.Owner{Kind: kindDeployment, Name: rsOwner.Name})
		}
	}
	return info
}

// Workload reduces a resolved PodInfo to the (kind, name) pair used as metric
// labels. It reports the top-most owner in the resolved chain — a pod owned
// (transitively) by a Deployment reports as Deployment; a StatefulSet/DaemonSet
// pod reports as that kind; a bare pod reports as ("Pod", pod name).
func Workload(info registry.PodInfo) (kind, name string) {
	if n := len(info.OwnerChain); n > 0 {
		top := info.OwnerChain[n-1]
		return top.Kind, top.Name
	}
	return "Pod", info.Name
}

// controllerRef picks the OwnerReference flagged Controller=true; otherwise
// the first ref, if any. K8s only allows a single controller ref per object,
// so this is unambiguous in practice.
func controllerRef(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}
