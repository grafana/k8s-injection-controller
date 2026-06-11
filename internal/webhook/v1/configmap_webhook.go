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
	"fmt"
	"net/http"
	"slices"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
)

// ValidateConfigMapPath is the admission path the ValidatingWebhookConfiguration
// routes ConfigMap CREATE/UPDATE requests to. It must stay in sync with the
// kubebuilder webhook marker below (and therefore the generated manifest).
const ValidateConfigMapPath = "/validate--v1-configmap"

// breakGlassGroup is the Kubernetes group whose members may always write
// annotated ConfigMaps, regardless of the configured allowlist. cluster-admins
// (and any subject explicitly bound to it) belong to this group, giving
// operators a manual override path — e.g. if Beyla's ServiceAccount identity
// ever needs to change without a controller restart.
const breakGlassGroup = "system:masters"

var cmlog = logf.Log.WithName("configmap-webhook")

// The validator SHIPS with failurePolicy=ignore and is hardened to Fail at
// runtime by the controller (see cmd/webhook_activation.go). Shipping Ignore
// avoids a fresh-install bootstrap deadlock: this webhook is scoped (by
// namespaceSelector) to the namespace the controller watches, which is usually
// the controller's own namespace. If it shipped Fail, the apiserver would reject
// the kube-controller-manager's attempt to publish kube-root-ca.crt into that
// namespace while the webhook backend (this pod) is still starting — but the pod
// cannot start without kube-root-ca.crt (its ServiceAccount-token volume needs
// it). With Ignore the apiserver fails open during that window, the pod starts,
// and the controller patches failurePolicy to Fail. Fail is the steady state (a
// security control: an outage must not silently re-open the injection-steering
// hole); the blast radius is bounded by the namespaceSelector (see
// config/webhook/configmap_namespace_selector_patch.yaml).
//
// +kubebuilder:webhook:path=/validate--v1-configmap,mutating=false,failurePolicy=ignore,sideEffects=None,groups="",resources=configmaps,verbs=create;update,versions=v1,name=vconfigmap-v1.beyla.grafana.com,admissionReviewVersions=v1

// The controller hardens this webhook to failurePolicy=Fail at runtime by
// patching it (see cmd/webhook_activation.go), so it needs get/patch on the
// configuration.
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;patch

// ConfigMapValidator rejects CREATE/UPDATE of ConfigMaps carrying the Beyla
// selector annotation unless the requesting identity is authorized. Without it,
// any principal able to create a ConfigMap in the watched namespace could steer
// instrumentation injection (env vars, volumes, LD_PRELOAD) onto arbitrary
// pods by planting an annotated ConfigMap the controller would consume.
type ConfigMapValidator struct {
	decoder      admission.Decoder
	allowedUsers map[string]struct{}
}

// NewConfigMapValidator builds a validator. allowedUsers is the set of
// usernames permitted to write annotated ConfigMaps — typically Beyla's
// ServiceAccount, e.g. "system:serviceaccount:beyla-k8s-injector:beyla".
// Members of system:masters are always allowed (break-glass).
func NewConfigMapValidator(scheme *runtime.Scheme, allowedUsers []string) *ConfigMapValidator {
	set := make(map[string]struct{}, len(allowedUsers))
	for _, u := range allowedUsers {
		if u != "" {
			set[u] = struct{}{}
		}
	}
	return &ConfigMapValidator{
		decoder:      admission.NewDecoder(scheme),
		allowedUsers: set,
	}
}

func (v *ConfigMapValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	cm := &corev1.ConfigMap{}
	if err := v.decoder.Decode(req, cm); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Only ConfigMaps the controller actually consumes are gated; everything
	// else passes untouched. The controller keys off the annotation's presence
	// (its value is unused), so that is exactly the condition we guard. Labels
	// can't carry this signal to a namespace/objectSelector — annotations are
	// not selectable — so the check has to live here in the handler.
	if _, ok := cm.Annotations[configmap.SelectorAnnotation]; !ok {
		return admission.Allowed("")
	}

	if v.authorized(req.UserInfo) {
		return admission.Allowed("")
	}

	cmlog.Info("denying unauthorized write to annotated ConfigMap",
		"namespace", req.Namespace, "name", req.Name,
		"user", req.UserInfo.Username, "operation", req.Operation)
	return admission.Denied(fmt.Sprintf(
		"user %q is not authorized to create or modify Beyla injection ConfigMaps (annotation %q)",
		req.UserInfo.Username, configmap.SelectorAnnotation))
}

// authorized reports whether the requesting identity may write annotated
// ConfigMaps: either its username is on the allowlist, or it belongs to the
// break-glass group.
func (v *ConfigMapValidator) authorized(user authenticationv1.UserInfo) bool {
	if _, ok := v.allowedUsers[user.Username]; ok {
		return true
	}
	return slices.Contains(user.Groups, breakGlassGroup)
}
