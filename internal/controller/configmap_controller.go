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

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/yaml"

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

type selectionCriterion struct {
	K8sNamespace string `json:"k8s_namespace,omitempty"`
}

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

	namespaces, eligibleImages, err := parseConfigMap(cm.Data)
	if err != nil {
		logger.Error(err, "ignoring ConfigMap with invalid payload")
		r.Registry.Delete(cmKey)
		return ctrl.Result{}, nil
	}

	newlyWatched := r.Registry.Set(cmKey, namespaces)
	for _, ns := range newlyWatched {
		if err := r.evictExisting(ctx, ns, eligibleImages); err != nil {
			logger.Error(err, "failed to evict pre-existing pods", "namespace", ns)
		}
	}
	return ctrl.Result{}, nil
}

// parseConfigMap extracts the watched namespaces (from selection_criteria.yaml)
// and the set of eligible-for-restart container images (from
// eligible_for_restart.yml). Either key may be absent: a missing
// selection_criteria.yaml just means this CM contributes no namespaces; a
// missing eligible_for_restart.yml means no pre-existing pods will be evicted
// (new pods in selected namespaces are still injected by the webhook).
func parseConfigMap(data map[string]string) (namespaces []string, eligibleImages map[string]struct{}, err error) {
	if raw, ok := data[SelectionCriteriaKey]; ok {
		var crit []selectionCriterion
		if err := yaml.Unmarshal([]byte(raw), &crit); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", SelectionCriteriaKey, err)
		}
		seen := map[string]struct{}{}
		for _, c := range crit {
			if c.K8sNamespace == "" {
				continue
			}
			if _, dup := seen[c.K8sNamespace]; dup {
				continue
			}
			seen[c.K8sNamespace] = struct{}{}
			namespaces = append(namespaces, c.K8sNamespace)
		}
	}
	eligibleImages = map[string]struct{}{}
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
	return namespaces, eligibleImages, nil
}

// evictExisting walks pods in the given namespace and submits an Eviction for
// each one that (a) has an OwnerReference (so something will recreate it) and
// (b) runs at least one container whose image is listed in eligibleImages.
// Bare pods and pods that don't match the image filter are skipped.
func (r *ConfigMapReconciler) evictExisting(ctx context.Context, namespace string, eligibleImages map[string]struct{}) error {
	logger := log.FromContext(ctx).WithValues("namespace", namespace)
	if len(eligibleImages) == 0 {
		logger.Info("no eligible-for-restart images declared; skipping pre-existing pods")
		return nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if len(pod.OwnerReferences) == 0 {
			logger.Info("skipping bare pod (no owner to recreate it)", "pod", pod.Name)
			continue
		}
		if !podMatchesImage(pod, eligibleImages) {
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
			logger.Info("evicted pod for re-injection", "pod", pod.Name)
		case apierrors.IsNotFound(err):
			// already gone
		case apierrors.IsTooManyRequests(err):
			// PDB blocked it; log and move on. The pod will be picked up by the
			// webhook whenever it's eventually replaced.
			logger.Info("eviction blocked by PDB; leaving pod in place", "pod", pod.Name)
		default:
			logger.Error(err, "eviction failed", "pod", pod.Name)
		}
	}
	return nil
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
	return ctrl.NewControllerManagedBy(mgr).
		Named("configmap-selector").
		For(&corev1.ConfigMap{}, builder.WithPredicates(annotated)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

