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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.opentelemetry.io/obi/pkg/appolly/services"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
)

var _ = Describe("Pod Webhook", func() {
	const testNamespace = "webhook-test"

	var (
		obj       *corev1.Pod
		defaulter *PodCustomDefaulter
	)

	BeforeEach(func() {
		// Registry with a single criterion that matches every pod in
		// testNamespace; that's enough to exercise the mutator path.
		ns := services.NewGlob(testNamespace)
		reg := registry.New()
		reg.Set("test-cm", registry.Instrumentation{
			Criteria: []registry.SelectionCriterion{{K8sNamespace: &ns}},
			// OTLP destination now travels with the matched ConfigMap, not
			// with the startup --config.
			InjectConfig: configmap.InjectConfig{
				OtelExport: configmap.OtelExport{
					Endpoint: "http://otel-collector:4318",
					Protocol: "http/protobuf",
				},
			},
		})

		defaulter = &PodCustomDefaulter{
			Registry: reg,
			// k8sClient comes from the envtest suite. It's only consulted by
			// podinfo.Resolve when the pod has a ReplicaSet owner; the test
			// pods below have no owner refs, so the reader is effectively a
			// no-op here.
			Reader: k8sClient,
			Mutator: &PodMutator{Cfg: config.SDKInject{
				// TODO: replace from some auto-updating source
				ImageVolumeRoot: "ghcr.io/grafana/beyla/inject-sdk-image",
				ImageVersion:    "0.0.9",
				Propagators:     []string{"tracecontext"},
			}},
		}

		obj = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "demo",
				Namespace: testNamespace,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
			},
		}
	})

	Context("When creating an uninstrumented Pod under Defaulting Webhook", func() {
		It("Should add the requirement environment variables", func() {
			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			Expect(obj.Spec.Containers).To(HaveLen(1))
			names := envNames(obj.Spec.Containers[0].Env)
			Expect(names).To(ContainElements(
				"LD_PRELOAD",
				"OTEL_INJECTOR_CONFIG_FILE",
				"OTEL_EXPORTER_OTLP_ENDPOINT",
				"OTEL_EXPORTER_OTLP_PROTOCOL",
				"BEYLA_INJECTOR_SDK_PKG_VERSION",
			))
			Expect(envValue(obj.Spec.Containers[0].Env, "OTEL_EXPORTER_OTLP_ENDPOINT")).
				To(Equal("http://otel-collector:4318"))
			// PackageVersion is a SHA-224 hex digest of the image reference: 56 chars.
			Expect(envValue(obj.Spec.Containers[0].Env, "BEYLA_INJECTOR_SDK_PKG_VERSION")).
				To(HaveLen(56))
		})

		It("Should mount the volume with the injectors", func() {
			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			Expect(obj.Spec.Volumes).To(HaveLen(1))
			Expect(obj.Spec.Volumes[0].Name).To(Equal(injectVolumeName))
			Expect(obj.Spec.Volumes[0].Image).NotTo(BeNil(),
				"expected an ImageVolumeSource since that's the only supported mode")
			Expect(obj.Spec.Volumes[0].Image.Reference).To(Equal("ghcr.io/grafana/beyla/inject-sdk-image:0.0.9"))

			mounts := obj.Spec.Containers[0].VolumeMounts
			Expect(mounts).To(HaveLen(1))
			Expect(mounts[0].Name).To(Equal(injectVolumeName))
			Expect(mounts[0].MountPath).To(Equal(internalMountPath))
			Expect(mounts[0].ReadOnly).To(BeTrue())
		})

		It("Should annotate the Pod with the current SDK package version", func() {
			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())
			Expect(obj.Annotations).To(HaveKeyWithValue(InjectedAnnotation, defaulter.Mutator.Cfg.PackageVersion()))
		})
	})

	Context("When the matched ConfigMap carries SDK overrides", func() {
		// Helper: re-register the test CM so its InjectConfig overrides the
		// controller-wide defaults set in BeforeEach. The criteria stay the
		// same so the pod still matches.
		setOverride := func(cfg configmap.InjectConfig) {
			ns := services.NewGlob(testNamespace)
			cfg.OtelExport = configmap.OtelExport{
				Endpoint: "http://otel-collector:4318",
				Protocol: "http/protobuf",
			}
			defaulter.Registry.Set("test-cm", registry.Instrumentation{
				Criteria:     []registry.SelectionCriterion{{K8sNamespace: &ns}},
				InjectConfig: cfg,
			})
		}

		It("Should override the mounted image and its derived package version", func() {
			setOverride(configmap.InjectConfig{
				ImageVersion: "override",
			})

			// Capture the package-version env the default path would produce, then
			// run injection and confirm both the volume reference and the env
			// reflect the override (not the controller default seeded in BeforeEach).
			defaultPV := (&config.SDKInject{ImageVersion: "0.0.9"}).PackageVersion()

			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			Expect(obj.Spec.Volumes).To(HaveLen(1))
			Expect(obj.Spec.Volumes[0].Image).NotTo(BeNil())
			Expect(obj.Spec.Volumes[0].Image.Reference).
				To(Equal("ghcr.io/grafana/beyla/inject-sdk-image:override"))

			gotPV := envValue(obj.Spec.Containers[0].Env, "BEYLA_INJECTOR_SDK_PKG_VERSION")
			Expect(gotPV).NotTo(BeEmpty())
			Expect(gotPV).NotTo(Equal(defaultPV),
				"package-version env should reflect the overridden ImageVersion, not the controller default")
		})

		It("Should honor Resources flags from the ConfigMap", func() {
			setOverride(configmap.InjectConfig{
				Resources: configmap.SDKResource{
					AddK8sUIDAttributes: true,
					AddK8sIPAttribute:   true,
				},
			})

			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			names := envNames(obj.Spec.Containers[0].Env)
			// AddK8sUIDAttributes gates this env var entirely; the default
			// (false) leaves it absent. Its presence proves the override took
			// effect.
			Expect(names).To(ContainElement("OTEL_INJECTOR_K8S_POD_UID"))
			// Same gate for the IP attribute.
			Expect(names).To(ContainElement("OTEL_RESOURCE_ATTRIBUTES_POD_IP"))
		})

		It("Should override propagators while preserving other defaults", func() {
			setOverride(configmap.InjectConfig{
				Propagators: []string{"b3", "baggage"},
			})

			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			Expect(envValue(obj.Spec.Containers[0].Env, "OTEL_PROPAGATORS")).
				To(Equal("b3,baggage"))
			// Volume reference should still come from the controller default —
			// only propagators was overridden.
			Expect(obj.Spec.Volumes[0].Image.Reference).
				To(Equal("ghcr.io/grafana/beyla/inject-sdk-image:0.0.9"))
		})
	})

	Context("When a pod is already annotated with the current SDK version", func() {
		It("Should not modify anything", func() {
			obj.Annotations = map[string]string{
				InjectedAnnotation: defaulter.Mutator.Cfg.PackageVersion(),
			}
			before := obj.DeepCopy()

			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			// Identical to the pre-call snapshot: no env vars, no volumes,
			// no extra annotations.
			Expect(obj).To(Equal(before))
		})
	})

	Context("When a pod is annotated with a stale SDK version", func() {
		It("Should re-instrument and update the annotation to the current version", func() {
			obj.Annotations = map[string]string{
				InjectedAnnotation: "stale-version-digest",
			}

			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			// Re-instrumentation went through: env vars present, volume mounted,
			// annotation refreshed to the current package version.
			want := defaulter.Mutator.Cfg.PackageVersion()
			Expect(obj.Annotations).To(HaveKeyWithValue(InjectedAnnotation, want))
			Expect(envValue(obj.Spec.Containers[0].Env, "BEYLA_INJECTOR_SDK_PKG_VERSION")).To(Equal(want))
			Expect(obj.Spec.Volumes).To(HaveLen(1))
		})
	})
})

func envNames(env []corev1.EnvVar) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		out = append(out, e.Name)
	}
	return out
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
