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
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
)

const beylaSA = "system:serviceaccount:beyla-k8s-injector:beyla"

func newValidator(t *testing.T, allowed ...string) *ConfigMapValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	return NewConfigMapValidator(scheme, allowed)
}

func cmRequest(t *testing.T, annotations map[string]string, user authenticationv1.UserInfo) admission.Request {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "beyla-state-node-1",
			Namespace:   "beyla-k8s-injector",
			Annotations: annotations,
		},
		Data: map[string]string{"instrumentation.yaml": "[]"},
	}
	raw, err := json.Marshal(cm)
	if err != nil {
		t.Fatalf("marshal configmap: %v", err)
	}
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: cm.Namespace,
			Name:      cm.Name,
			UserInfo:  user,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

func TestConfigMapValidator_Handle(t *testing.T) {
	annotated := map[string]string{configmap.SelectorAnnotation: "node-1"}

	tests := []struct {
		name        string
		annotations map[string]string
		user        authenticationv1.UserInfo
		allowed     []string
		wantAllowed bool
	}{
		{
			name:        "unannotated configmap passes regardless of user",
			annotations: map[string]string{"unrelated": "x"},
			user:        authenticationv1.UserInfo{Username: "system:serviceaccount:default:random"},
			allowed:     []string{beylaSA},
			wantAllowed: true,
		},
		{
			name:        "configmap with no annotations at all passes",
			annotations: nil,
			user:        authenticationv1.UserInfo{Username: "system:serviceaccount:default:random"},
			allowed:     []string{beylaSA},
			wantAllowed: true,
		},
		{
			name:        "annotated configmap from allowlisted user is allowed",
			annotations: annotated,
			user:        authenticationv1.UserInfo{Username: beylaSA},
			allowed:     []string{beylaSA},
			wantAllowed: true,
		},
		{
			name:        "annotated configmap from unauthorized user is denied",
			annotations: annotated,
			user:        authenticationv1.UserInfo{Username: "system:serviceaccount:default:attacker"},
			allowed:     []string{beylaSA},
			wantAllowed: false,
		},
		{
			name:        "annotated configmap from system:masters break-glass is allowed",
			annotations: annotated,
			user: authenticationv1.UserInfo{
				Username: "kubernetes-admin",
				Groups:   []string{"system:authenticated", breakGlassGroup},
			},
			allowed:     []string{beylaSA},
			wantAllowed: true,
		},
		{
			name:        "annotated configmap denied when allowlist empty and not break-glass",
			annotations: annotated,
			user:        authenticationv1.UserInfo{Username: beylaSA},
			allowed:     nil,
			wantAllowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator(t, tt.allowed...)
			resp := v.Handle(context.Background(), cmRequest(t, tt.annotations, tt.user))
			if resp.Allowed != tt.wantAllowed {
				t.Fatalf("Handle allowed = %v, want %v (message: %q)",
					resp.Allowed, tt.wantAllowed, resp.Result.Message)
			}
		})
	}
}
