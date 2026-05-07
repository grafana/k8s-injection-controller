/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/grafana/beyla-k8s-injector/internal/registry"
)

// InjectedEnvName / InjectedEnvValue are intentionally exported so the
// controller's "already injected" check stays in sync with the webhook.
const (
	InjectedEnvName  = "FOO"
	InjectedEnvValue = "bar"
)

var podlog = logf.Log.WithName("pod-webhook")

// SetupPodWebhookWithManager registers the mutating webhook for Pod.
func SetupPodWebhookWithManager(mgr ctrl.Manager, reg *registry.Registry) error {
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(&PodCustomDefaulter{Registry: reg}).
		Complete()
}

// failurePolicy=Ignore is deliberate: a broken injector must not block pod
// creation cluster-wide. The controller's own namespace must also be excluded
// at install time via namespaceSelector to avoid a self-deadlock if the
// injector pod is itself recreated while the webhook is failing.
//
// +kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=mpod-v1.beyla.grafana.com,admissionReviewVersions=v1

// PodCustomDefaulter injects the configured env var into pods in registered
// namespaces.
type PodCustomDefaulter struct {
	Registry *registry.Registry
}

func (d *PodCustomDefaulter) Default(_ context.Context, obj *corev1.Pod) error {
	if !d.Registry.Has(obj.Namespace) {
		return nil
	}
	mutated := injectInto(obj.Spec.Containers)
	if injectInto(obj.Spec.InitContainers) {
		mutated = true
	}
	if mutated {
		podlog.Info("injected env var", "namespace", obj.Namespace, "name", obj.Name)
	}
	return nil
}

// injectInto appends the env var to every container that doesn't already have
// it. Returns true if at least one container was modified.
func injectInto(containers []corev1.Container) bool {
	mutated := false
	for i := range containers {
		c := &containers[i]
		if hasEnv(c.Env) {
			continue
		}
		c.Env = append(c.Env, corev1.EnvVar{Name: InjectedEnvName, Value: InjectedEnvValue})
		mutated = true
	}
	return mutated
}

func hasEnv(env []corev1.EnvVar) bool {
	for _, e := range env {
		if e.Name == InjectedEnvName {
			return true
		}
	}
	return false
}
