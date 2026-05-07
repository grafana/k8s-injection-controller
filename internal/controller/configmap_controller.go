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
	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
)

// SelectorAnnotation marks a ConfigMap as a Beyla injection selector. Its
// value is ignored — presence is what matters.
const SelectorAnnotation = "beyla.grafana.com/node"

// Keys we read from ConfigMap.Data. Anything else is ignored.
const (
	// SelectionCriteriaKey holds a YAML list of namespaces eligible for
	// injection by the webhook. Schema: `- k8s_namespace: <name>`.
	SelectionCriteriaKey = "selection_criteria.yaml"
	// EligibleForRestartKey holds a YAML list of restart targets. Each entry
	// names a workload whose pods should be evicted so the webhook can
	// re-intercept them on recreation. Schema:
	//   - namespace: foo        # required
	//     kind: Deployment      # required: Deployment | ReplicaSet | StatefulSet | DaemonSet
	//     name: frontend        # optional; empty means "any of that kind in the namespace"
	//     language: nodejs      # parsed but currently unused
	EligibleForRestartKey = "eligible_for_restart.yaml"
)

// restartCriterion is one entry in eligible_for_restart.yaml.
type restartCriterion struct {
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Language  string `json:"language,omitempty"` // currently unused
}

// restartKinds is the set of workload kinds we know how to match against a
// pod's owner chain.
var restartKinds = map[string]struct{}{
	"Deployment":  {},
	"ReplicaSet":  {},
	"StatefulSet": {},
	"DaemonSet":   {},
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

	criteria, restartTargets, err := parseConfigMap(cm.Data)
	if err != nil {
		logger.Error(err, "ignoring ConfigMap with invalid payload")
		r.Registry.Delete(cmKey)
		return ctrl.Result{}, nil
	}
	r.Registry.Set(cmKey, criteria)
	if len(restartTargets) > 0 {
		if err := r.evictMatching(ctx, restartTargets); err != nil {
			logger.Error(err, "failed to evict pre-existing pods")
		}
	}
	return ctrl.Result{}, nil
}

// parseConfigMap extracts the selection criteria (from selection_criteria.yaml)
// and the eligible-for-restart targets (from eligible_for_restart.yaml).
// Either key may be absent. Restart entries missing the required namespace
// or kind, or naming an unknown kind, are dropped with no error.
func parseConfigMap(data map[string]string) ([]registry.SelectionCriterion, []restartCriterion, error) {
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
	var restartTargets []restartCriterion
	if raw, ok := data[EligibleForRestartKey]; ok {
		var parsed []restartCriterion
		if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", EligibleForRestartKey, err)
		}
		for _, c := range parsed {
			if c.Namespace == "" || c.Kind == "" {
				continue
			}
			if _, ok := restartKinds[c.Kind]; !ok {
				continue
			}
			restartTargets = append(restartTargets, c)
		}
	}
	return criteria, restartTargets, nil
}

// evictMatching processes the restart targets from a single ConfigMap. For
// each target it lists pods in target.Namespace and evicts those whose owner
// chain matches target.Kind (and optionally target.Name) AND that match a
// selection criterion in the registry — no point evicting pods the webhook
// won't inject anyway. Bare pods are skipped (no controller to recreate them).
func (r *ConfigMapReconciler) evictMatching(ctx context.Context, targets []restartCriterion) error {
	logger := log.FromContext(ctx)

	// One LIST per distinct namespace, regardless of how many entries name it.
	byNamespace := map[string][]restartCriterion{}
	for _, t := range targets {
		byNamespace[t.Namespace] = append(byNamespace[t.Namespace], t)
	}

	for namespace, nsTargets := range byNamespace {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
			return fmt.Errorf("list pods in %s: %w", namespace, err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.DeletionTimestamp != nil {
				continue
			}
			if len(pod.OwnerReferences) == 0 {
				continue
			}
			info := podinfo.Resolve(ctx, r.Client, pod)
			if !matchesAnyTarget(info, nsTargets) {
				continue
			}
			if !r.Registry.Match(info) {
				continue
			}
			if webhookv1.AlreadyInstrumentedByOther(&pod.Spec, &pod.ObjectMeta) {
				continue
			}
			if webhookv1.PreloadsSomethingElse(pod) {
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
				// PDB blocked it; log and move on. The pod will be picked up
				// by the webhook whenever it's eventually replaced.
				logger.Info("eviction blocked by PDB; leaving pod in place", "namespace", pod.Namespace, "pod", pod.Name)
			default:
				logger.Error(err, "eviction failed", "namespace", pod.Namespace, "pod", pod.Name)
			}
		}
	}
	return nil
}

// matchesAnyTarget reports whether the pod's owner chain satisfies any of the
// supplied restart targets. All targets here have already been validated to
// share the pod's namespace.
func matchesAnyTarget(info podinfoMatcher, targets []restartCriterion) bool {
	for _, t := range targets {
		if matchesTarget(info, t) {
			return true
		}
	}
	return false
}

// podinfoMatcher decouples matchesTarget from the registry.PodInfo concrete
// type so it stays trivially testable.
type podinfoMatcher = registry.PodInfo

func matchesTarget(info podinfoMatcher, t restartCriterion) bool {
	switch t.Kind {
	case "Deployment":
		// Pod is owned (transitively) by a Deployment when podinfo.Resolve
		// populates DeploymentName via the RS chain — or directly, if the
		// pod's controller ref is itself a Deployment.
		if info.DeploymentName == "" {
			return false
		}
		return t.Name == "" || t.Name == info.DeploymentName
	case "ReplicaSet", "StatefulSet", "DaemonSet":
		if info.OwnerKind != t.Kind {
			return false
		}
		return t.Name == "" || t.Name == info.OwnerName
	}
	return false
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
