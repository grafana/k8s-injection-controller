/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
	bservices "github.com/grafana/beyla/v3/pkg/services"
	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/obi/pkg/appolly/app/svc"
	"go.opentelemetry.io/obi/pkg/appolly/services"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const warningTestNamespace = "warning-test"

func TestPodAdmissionReturnsJavaExtensionWarnings(t *testing.T) {
	tests := []struct {
		name             string
		images           []string
		mode             config.InjectionMode
		ruleEnv          []corev1.EnvVar
		volumes          []corev1.Volume
		wantWarning      string
		wantExtension    bool
		wantSDKInjection bool
		alreadyInjected  bool
	}{
		{
			name:             "unsupported controller defaults warn but keep SDK injection",
			images:           []string{"example/extensions:default"},
			mode:             config.InjectionModeInitContainer,
			wantWarning:      "Java extension images will not be injected: image injection mode (Kubernetes 1.35+) is required",
			wantSDKInjection: true,
		},
		{
			name:            "unsupported mode warns before already-instrumented return",
			images:          []string{"example/extensions:default"},
			mode:            config.InjectionModeInitContainer,
			wantWarning:     "Java extension images will not be injected: image injection mode (Kubernetes 1.35+) is required",
			alreadyInjected: true,
		},
		{
			name:             "unsupported per-rule images warn",
			mode:             config.InjectionModeInitContainer,
			ruleEnv:          []corev1.EnvVar{{Name: config.JavaExtensionImagesEnv, Value: "example/extensions:rule"}},
			wantWarning:      "Java extension images will not be injected: image injection mode (Kubernetes 1.35+) is required",
			wantSDKInjection: true,
		},
		{
			name:             "per-rule extension images are applied",
			mode:             config.InjectionModeImage,
			ruleEnv:          []corev1.EnvVar{{Name: config.JavaExtensionImagesEnv, Value: "example/extensions:rule"}},
			wantExtension:    true,
			wantSDKInjection: true,
		},
		{
			name:             "per-rule collision returns helper warning",
			mode:             config.InjectionModeImage,
			ruleEnv:          []corev1.EnvVar{{Name: config.JavaExtensionImagesEnv, Value: "example/extensions:rule"}},
			volumes:          []corev1.Volume{{Name: javaExtensionVolumeName(0)}},
			wantWarning:      "Java extension configuration could not be applied safely",
			wantSDKInjection: true,
		},
		{
			name:             "explicit empty per-rule value disables controller defaults",
			images:           []string{"example/extensions:default"},
			mode:             config.InjectionModeInitContainer,
			ruleEnv:          []corev1.EnvVar{{Name: config.JavaExtensionImagesEnv, Value: ""}},
			wantSDKInjection: true,
		},
		{
			name:             "compatible controller defaults do not warn",
			images:           []string{"example/extensions:default"},
			mode:             config.InjectionModeImage,
			wantExtension:    true,
			wantSDKInjection: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.SDKInject{
				ImageVolumeRoot:     "ghcr.io/grafana/beyla/inject-sdk-image",
				ImageVersion:        "test-version",
				InjectionMode:       tc.mode,
				JavaExtensionImages: tc.images,
				EnabledSDKs: []bservices.InstrumentableType{{
					InstrumentableType: svc.InstrumentableJava,
				}},
			}
			reg := registry.New()
			reg.Set("test", registry.Instrumentation{InjectConfig: configmap.InjectConfig{
				Rules: []configmap.Rule{{
					Selector: configmap.K8sSelector{Namespaces: []services.GlobAttr{services.NewGlob(warningTestNamespace)}},
					Config:   configmap.RuleConfig{Env: tc.ruleEnv},
				}},
			}})
			defaulter := &PodCustomDefaulter{
				Registry: reg,
				Mutator:  &PodMutator{Cfg: cfg},
			}
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			handler := newWarningAdmissionHandler(admission.WithDefaulter[*corev1.Pod](scheme, defaulter).Handler)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: warningTestNamespace},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
					Volumes:    tc.volumes,
				},
			}
			if tc.alreadyInjected {
				pod.Annotations = map[string]string{
					InjectedAnnotation: config.PodConfigHash(&cfg, &configmap.RuleConfig{Env: tc.ruleEnv}),
				}
			}
			raw, err := json.Marshal(pod)
			require.NoError(t, err)
			response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
				Object:    runtime.RawExtension{Raw: raw},
			}})

			require.True(t, response.Allowed)
			if tc.wantWarning == "" {
				require.Empty(t, response.Warnings)
			} else {
				require.Len(t, response.Warnings, 1)
				require.Contains(t, response.Warnings[0], tc.wantWarning)
			}
			patch, err := json.Marshal(response.Patches)
			require.NoError(t, err)
			require.Equal(t, tc.wantSDKInjection, strings.Contains(string(patch), "LD_PRELOAD"))
			require.Equal(t, tc.wantExtension, strings.Contains(string(patch), "example/extensions:"))
		})
	}
}
