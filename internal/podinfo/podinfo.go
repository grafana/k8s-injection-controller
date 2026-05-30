/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package podinfo resolves a Pod's owner chain into a registry.PodInfo so the
// controller and webhook can reuse the same lookup logic. It walks
// pod -> ReplicaSet -> Deployment when applicable; other owner kinds are
// reported directly.
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
)

// kindDeployment is the owner kind we resolve the RS chain up to and report as
// the workload kind for pods backed (directly or transitively) by a Deployment.
const kindDeployment = "Deployment"

// Resolve builds a registry.PodInfo from the given pod. If the pod is owned by
// a ReplicaSet, Resolve fetches that RS through the supplied reader and reads
// the RS's controller owner to populate DeploymentName. Lookup errors are
// logged and treated as "no Deployment resolvable" — the pod still matches
// criteria that don't require a deployment.
func Resolve(ctx context.Context, c client.Reader, pod *corev1.Pod) registry.PodInfo {
	info := registry.PodInfo{
		Name:      pod.Name,
		Namespace: pod.Namespace,
	}
	owner := controllerRef(pod.OwnerReferences)
	if owner == nil {
		return info
	}
	info.OwnerKind = owner.Kind
	info.OwnerName = owner.Name

	switch owner.Kind {
	case kindDeployment:
		info.DeploymentName = owner.Name
	case "ReplicaSet":
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
			info.DeploymentName = rsOwner.Name
		}
	}
	return info
}

// Workload reduces a resolved PodInfo to the (kind, name) pair used as metric
// labels. A pod owned (transitively) by a Deployment reports as Deployment;
// otherwise the direct owner kind/name is used; a bare pod reports as
// ("Pod", pod name). This mirrors resolveWorkload in the controller but yields
// plain strings suitable for label values.
func Workload(info registry.PodInfo) (kind, name string) {
	if info.DeploymentName != "" {
		return kindDeployment, info.DeploymentName
	}
	if info.OwnerKind != "" {
		return info.OwnerKind, info.OwnerName
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
