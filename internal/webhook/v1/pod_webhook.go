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

// InjectedAnnotation marks every pod we mutate. Its value is the SHA-224
// digest returned by SDKInject.PackageVersion(), so a later admission can
// tell whether the pod is already instrumented with the current SDK image
// (skip) or with an older one (re-instrument). Both the webhook and the
// controller use the annotation's presence to decide whether to evict a
// pre-existing pod for re-interception.
const (
	InjectedAnnotation = "beyla.grafana.com/inject"
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
	podlog.Info("admission received", "namespace", obj.Namespace, "name", obj.Name, "generateName", obj.GenerateName)
	info := podinfo.Resolve(ctx, d.Reader, obj)
	inst, ok := d.Registry.Match(info)
	if !ok {
		podlog.Info("no criterion matched; skipping", "namespace", obj.Namespace, "name", obj.Name)
		return nil
	}
	if d.Mutator.Cfg.ImageVolumePath == "" {
		podlog.Info("pod matches but no SDK config loaded; skipping injection",
			"namespace", obj.Namespace, "name", obj.Name)
		return nil
	}

	// Per-request mutator with any per-ConfigMap overrides layered on top of
	// the controller-wide SDK defaults. Mutator methods are pm.Cfg-driven, so
	// a shallow copy is enough to scope the override. Compute the resolved
	// package version up front: it depends on the (possibly-overridden)
	// ImageVolumePath, and both the version-skew check and the annotation we
	// stamp need it.
	mutator := *d.Mutator
	mutator.Cfg = mutator.Cfg.WithConfigMapOverrides(inst.InjectConfig)
	wantVersion := mutator.Cfg.PackageVersion()

	if AlreadyInstrumented(&obj.Spec, &obj.ObjectMeta, wantVersion) {
		podlog.Info("already instrumented at current SDK version; skipping",
			"namespace", obj.Namespace, "name", obj.Name, "version", wantVersion)
		return nil
	}
	if PreloadsSomethingElse(obj) {
		podlog.Info("skipping injection: pod has a conflicting LD_PRELOAD",
			"namespace", obj.Namespace, "name", obj.Name)
		return nil
	}

	mutator.mountVolume(&obj.Spec)
	for i := range obj.Spec.Containers {
		mutator.instrumentContainer(&obj.ObjectMeta, &obj.Spec.Containers[i], inst.InjectConfig.OtelExport)
	}
	for i := range obj.Spec.InitContainers {
		mutator.instrumentContainer(&obj.ObjectMeta, &obj.Spec.InitContainers[i], inst.InjectConfig.OtelExport)
	}

	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations[InjectedAnnotation] = wantVersion

	podlog.Info("instrumented pod", "namespace", obj.Namespace, "name", obj.Name)
	return nil
}
