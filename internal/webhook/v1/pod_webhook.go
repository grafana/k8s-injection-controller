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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/grafana/beyla-k8s-injector/internal/podinfo"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
)

// InjectedAnnotation is the marker we set on every pod we mutate. It's used
// both as an idempotency check inside the webhook and by the controller to
// avoid evicting already-injected pods.
const (
	InjectedAnnotation = "beyla.grafana.com/inject"
	InjectedAnnotValue = "true"
)

var podlog = logf.Log.WithName("pod-webhook")

// SetupPodWebhookWithManager registers the mutating webhook for Pod. The
// reader is used to walk pod -> ReplicaSet -> Deployment when a criterion
// targets a Deployment. mutator may be nil — in that case the webhook only
// records a match log line and does not mutate (useful when no SDK config
// has been provided).
func SetupPodWebhookWithManager(mgr ctrl.Manager, reg *registry.Registry, reader client.Reader, mutator *PodMutator) error {
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(&PodCustomDefaulter{Registry: reg, Reader: reader, Mutator: mutator}).
		Complete()
}

// failurePolicy=Ignore is deliberate: a broken injector must not block pod
// creation cluster-wide. The controller's own namespace must also be excluded
// at install time via namespaceSelector to avoid a self-deadlock if the
// injector pod is itself recreated while the webhook is failing.
//
// +kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=mpod-v1.beyla.grafana.com,admissionReviewVersions=v1

// PodCustomDefaulter applies the OTel SDK auto-instrumentation to pods that
// match a registry criterion.
type PodCustomDefaulter struct {
	Registry *registry.Registry
	Reader   client.Reader
	// Mutator is nil when the operator runs without an SDK config; in that
	// mode the webhook is a no-op even for matching pods.
	Mutator *PodMutator
}

func (d *PodCustomDefaulter) Default(ctx context.Context, obj *corev1.Pod) error {
	info := podinfo.Resolve(ctx, d.Reader, obj)
	if !d.Registry.Match(info) {
		return nil
	}
	if d.Mutator == nil {
		podlog.Info("pod matches but no SDK config loaded; skipping injection",
			"namespace", obj.Namespace, "name", obj.Name)
		return nil
	}
	if AlreadyInstrumentedByOther(&obj.Spec, &obj.ObjectMeta) {
		return nil
	}
	if PreloadsSomethingElse(obj) {
		podlog.Info("skipping injection: pod has a conflicting LD_PRELOAD",
			"namespace", obj.Namespace, "name", obj.Name)
		return nil
	}

	d.Mutator.mountVolume(&obj.Spec, &obj.ObjectMeta)
	for i := range obj.Spec.Containers {
		d.Mutator.instrumentContainer(&obj.ObjectMeta, &obj.Spec.Containers[i])
	}
	for i := range obj.Spec.InitContainers {
		d.Mutator.instrumentContainer(&obj.ObjectMeta, &obj.Spec.InitContainers[i])
	}

	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations[InjectedAnnotation] = InjectedAnnotValue

	podlog.Info("instrumented pod", "namespace", obj.Namespace, "name", obj.Name)
	return nil
}
