/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	"github.com/grafana/beyla-k8s-injector/internal/podinfo"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
)

// SelectorAnnotation marks a ConfigMap as a Beyla injection selector. Its
// value is ignored — presence is what matters.
const SelectorAnnotation = "beyla.grafana.com/node"

// Keys we read from ConfigMap.Data. Anything else is ignored.
const (
	// SelectionCriteriaKey holds a YAML list of namespaces eligible for
	// injection by the webhook. Schema: `- k8s_namespace: <name>`.
	SelectionCriteriaKey = "selection_criteria.yaml"
	// EligibleForRestartKey holds a YAML list of container images whose
	// running pods are eligible for eviction so the webhook can re-intercept
	// them. Schema: `- image: <ref>` (the `language` attribute is parsed but
	// currently ignored).
	EligibleForRestartKey = "eligible_for_restart.yml"
)

type restartCriterion struct {
	Image    string `json:"image,omitempty"`
	Language string `json:"language,omitempty"` // currently unused
}

// ConfigMapReconciler watches ConfigMaps carrying the SelectorAnnotation and
// keeps the in-memory Registry in sync with their k8s_namespace selections.
type ConfigMapReconciler struct {
	client.Client
	// Clientset is used for the Eviction subresource, which controller-runtime's
	// typed client does not expose.
	Clientset kubernetes.Interface
	Registry  *registry.Registry
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch

func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	cmKey := req.NamespacedName.String()

	var cm corev1.ConfigMap
	if err := r.Get(ctx, req.NamespacedName, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			r.Registry.Delete(cmKey)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Predicate already filters by annotation, but defend against races where
	// the annotation was removed.
	if _, ok := cm.Annotations[SelectorAnnotation]; !ok {
		r.Registry.Delete(cmKey)
		return ctrl.Result{}, nil
	}

	criteria, eligibleImages, err := parseConfigMap(cm.Data)
	if err != nil {
		logger.Error(err, "ignoring ConfigMap with invalid payload")
		r.Registry.Delete(cmKey)
		return ctrl.Result{}, nil
	}
	r.Registry.Set(cmKey, criteria)
	if len(criteria) > 0 {
		if err := r.evictMatching(ctx, criteria, eligibleImages); err != nil {
			logger.Error(err, "failed to evict pre-existing pods")
		}
	}
	return ctrl.Result{}, nil
}

// parseConfigMap extracts the selection criteria (from selection_criteria.yaml)
// and the set of eligible-for-restart container images (from
// eligible_for_restart.yml). Either key may be absent.
func parseConfigMap(data map[string]string) ([]registry.SelectionCriterion, map[string]struct{}, error) {
	var criteria []registry.SelectionCriterion
	if raw, ok := data[SelectionCriteriaKey]; ok {
		if err := yaml.Unmarshal([]byte(raw), &criteria); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", SelectionCriteriaKey, err)
		}
		// Drop blank entries: a fully-empty criterion would match every pod.
		filtered := criteria[:0]
		for _, c := range criteria {
			if c.IsEmpty() {
				continue
			}
			filtered = append(filtered, c)
		}
		criteria = filtered
	}
	eligibleImages := map[string]struct{}{}
	if raw, ok := data[EligibleForRestartKey]; ok {
		var crit []restartCriterion
		if err := yaml.Unmarshal([]byte(raw), &crit); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", EligibleForRestartKey, err)
		}
		for _, c := range crit {
			if c.Image == "" {
				continue
			}
			eligibleImages[c.Image] = struct{}{}
		}
	}
	return criteria, eligibleImages, nil
}

// evictMatching lists candidate pods and submits an Eviction for each that
// (a) has an OwnerReference, (b) runs an image in eligibleImages, and
// (c) matches any criterion currently in the registry.
//
// Pods require an owner reference because bare pods have no controller to recreate them.
//
// Listing scope: if every criterion in the just-reconciled CM has a literal
// k8s_namespace, list only those namespaces; otherwise list cluster-wide.
// The image filter and registry.Match further narrow the eviction set, so
// "cluster-wide" is bounded by what the operator actually selected.
func (r *ConfigMapReconciler) evictMatching(ctx context.Context, criteria []registry.SelectionCriterion, eligibleImages map[string]struct{}) error {
	logger := log.FromContext(ctx)
	if len(eligibleImages) == 0 {
		logger.Info("no eligible-for-restart images declared; skipping pre-existing pods")
		return nil
	}

	pods, err := r.collectCandidatePods(ctx, criteria)
	if err != nil {
		return err
	}

	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if len(pod.OwnerReferences) == 0 {
			continue
		}
		if !podMatchesImage(pod, eligibleImages) {
			continue
		}
		info := podinfo.Resolve(ctx, r.Client, pod)
		if !r.Registry.Match(info) {
			continue
		}
		if podHasInjection(pod) {
			continue
		}
		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		}
		err := r.Clientset.CoreV1().Pods(pod.Namespace).EvictV1(ctx, eviction)
		switch {
		case err == nil:
			logger.Info("evicted pod for re-injection", "namespace", pod.Namespace, "pod", pod.Name)
		case apierrors.IsNotFound(err):
			// already gone
		case apierrors.IsTooManyRequests(err):
			// PDB blocked it; log and move on. The pod will be picked up by the
			// webhook whenever it's eventually replaced.
			logger.Info("eviction blocked by PDB; leaving pod in place", "namespace", pod.Namespace, "pod", pod.Name)
		default:
			logger.Error(err, "eviction failed", "namespace", pod.Namespace, "pod", pod.Name)
		}
	}
	return nil
}

// collectCandidatePods lists pods within the smallest namespace scope that
// could contain matches: the union of literal namespaces in the criteria
// when every criterion specifies one, otherwise cluster-wide.
func (r *ConfigMapReconciler) collectCandidatePods(ctx context.Context, criteria []registry.SelectionCriterion) ([]corev1.Pod, error) {
	logger := log.FromContext(ctx)
	namespaces, allLiteral := literalNamespaces(criteria)
	if !allLiteral {
		logger.Info("listing pods cluster-wide for eviction sweep (criteria include non-literal or absent namespaces)")
		var list corev1.PodList
		if err := r.List(ctx, &list); err != nil {
			return nil, fmt.Errorf("list pods cluster-wide: %w", err)
		}
		return list.Items, nil
	}
	var out []corev1.Pod
	for _, ns := range namespaces {
		var list corev1.PodList
		if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return nil, fmt.Errorf("list pods in %s: %w", ns, err)
		}
		out = append(out, list.Items...)
	}
	return out, nil
}

// literalNamespaces returns the deduplicated set of literal k8s_namespace
// values across the criteria, plus a flag indicating whether every criterion
// had a literal namespace. If even one criterion is missing or uses a glob,
// allLiteral is false and the caller must list cluster-wide.
func literalNamespaces(criteria []registry.SelectionCriterion) (out []string, allLiteral bool) {
	seen := map[string]struct{}{}
	allLiteral = true
	for _, c := range criteria {
		if !c.K8sNamespace.IsLiteral() {
			allLiteral = false
			continue
		}
		ns := c.K8sNamespace.Pattern()
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out, allLiteral
}

// podMatchesImage reports whether any container or initContainer in the pod
// runs an image present in the eligible set. Image matching is exact on the
// reference string as it appears in the PodSpec.
func podMatchesImage(pod *corev1.Pod, eligible map[string]struct{}) bool {
	for _, c := range pod.Spec.Containers {
		if _, ok := eligible[c.Image]; ok {
			return true
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if _, ok := eligible[c.Image]; ok {
			return true
		}
	}
	return false
}

// podHasInjection avoids evicting pods that already carry our env var across
// every container — there's nothing for the webhook to add.
func podHasInjection(pod *corev1.Pod) bool {
	check := func(cs []corev1.Container) bool {
		for _, c := range cs {
			found := false
			for _, e := range c.Env {
				if e.Name == "FOO" && e.Value == "bar" {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	return check(pod.Spec.Containers) && check(pod.Spec.InitContainers)
}

// hasSelectorAnnotation is the predicate filter we apply to the ConfigMap
// watch so the controller only wakes up for objects we care about.
func hasSelectorAnnotation(obj client.Object) bool {
	_, ok := obj.GetAnnotations()[SelectorAnnotation]
	return ok
}

func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	annotated := predicate.NewPredicateFuncs(hasSelectorAnnotation)
	// The Watches on ReplicaSets exists purely to hydrate the manager cache so
	// podinfo.Resolve (called per pod during eviction) reads RSes from the
	// informer instead of making per-pod API calls. The handler returns no
	// reconcile requests — RS events do not trigger ConfigMap reconciles.
	return ctrl.NewControllerManagedBy(mgr).
		Named("configmap-selector").
		For(&corev1.ConfigMap{}, builder.WithPredicates(annotated)).
		Watches(&appsv1.ReplicaSet{}, handler.EnqueueRequestsFromMapFunc(noEnqueue)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func noEnqueue(_ context.Context, _ client.Object) []reconcile.Request {
	return nil
}
