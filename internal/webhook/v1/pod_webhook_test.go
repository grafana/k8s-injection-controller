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

	// nsRule builds an Instrumentation with a single rule matching every pod in
	// testNamespace. The rule carries the OTLP destination as env vars (as Beyla
	// would write them). imageVersion is the CM-level override ("" = use the
	// controller default).
	nsRule := func(imageVersion string, env ...corev1.EnvVar) registry.Instrumentation {
		if env == nil {
			env = []corev1.EnvVar{
				{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://otel-collector:4318"},
				{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "http/protobuf"},
			}
		}
		return registry.Instrumentation{InjectConfig: configmap.InjectConfig{
			ImageVersion: imageVersion,
			Rules: []configmap.Rule{{
				Selector: configmap.K8sSelector{
					Namespaces: []services.GlobAttr{services.NewGlob(testNamespace)},
				},
				Config: configmap.RuleConfig{Env: env},
			}},
		}}
	}

	setRule := func(inst registry.Instrumentation) {
		reg := registry.New()
		reg.Set("test-cm", inst)
		defaulter.Registry = reg
	}

	BeforeEach(func() {
		reg := registry.New()
		reg.Set("test-cm", nsRule(""))

		defaulter = &PodCustomDefaulter{
			Registry: reg,
			// k8sClient comes from the envtest suite. It's only consulted by
			// podinfo.Resolve when the pod has a ReplicaSet owner; the test
			// pods below have no owner refs, so the reader is effectively a
			// no-op here.
			Reader: k8sClient,
			Mutator: &PodMutator{Cfg: config.SDKInject{
				ImageVolumeRoot: "ghcr.io/grafana/beyla/inject-sdk-image",
				ImageVersion:    "0.0.12",
				// These specs assert direct ImageVolumeSource behavior. In
				// production "auto" is resolved to a concrete mode at boot; the
				// tests construct the mutator directly, so set it explicitly.
				InjectionMode: config.InjectionModeImage,
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
			Expect(obj.Spec.Volumes[0].Image.Reference).To(Equal("ghcr.io/grafana/beyla/inject-sdk-image:0.0.12"))

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

	Context("When a rule env var conflicts with a fixed injector var", func() {
		It("should not let the rule override LD_PRELOAD or OTEL_SEMCONV_STABILITY_OPT_IN", func() {
			setRule(nsRule("",
				corev1.EnvVar{Name: "LD_PRELOAD", Value: "/attacker/lib.so"},
				corev1.EnvVar{Name: "OTEL_SEMCONV_STABILITY_OPT_IN", Value: "database"},
				corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://my-collector:4318"},
			))

			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			env := obj.Spec.Containers[0].Env
			Expect(envValue(env, "LD_PRELOAD")).To(Equal(envVarLdPreloadValue),
				"fixed LD_PRELOAD must win over rule-supplied value")
			Expect(envValue(env, "OTEL_SEMCONV_STABILITY_OPT_IN")).To(Equal("http"),
				"fixed semconv opt-in must win over rule-supplied value")
			Expect(envValue(env, "OTEL_EXPORTER_OTLP_ENDPOINT")).To(Equal("http://my-collector:4318"),
				"non-conflicting rule env var must be applied")
		})
	})

	Context("When the ConfigMap overrides the image version", func() {
		It("Should override the mounted image and its derived package version", func() {
			setRule(nsRule("override"))

			// Capture the package-version the controller default would produce,
			// then confirm both the volume reference and the env reflect the
			// per-ConfigMap override instead.
			defaultPV := defaulter.Mutator.Cfg.PackageVersion()

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
	})

	Context("When injection_mode is init_container", func() {
		BeforeEach(func() {
			defaulter.Mutator.Cfg.InjectionMode = config.InjectionModeInitContainer
		})

		It("Should provision an ephemeral volume and a copy init container, and not instrument the copy container", func() {
			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			// Ephemeral emptyDir volume rather than an image volume.
			Expect(obj.Spec.Volumes).To(HaveLen(1))
			Expect(obj.Spec.Volumes[0].Name).To(Equal(injectVolumeName))
			Expect(obj.Spec.Volumes[0].EmptyDir).NotTo(BeNil())
			Expect(obj.Spec.Volumes[0].Image).To(BeNil())

			// Exactly one init container: our copy container.
			Expect(obj.Spec.InitContainers).To(HaveLen(1))
			copyC := obj.Spec.InitContainers[0]
			Expect(copyC.Name).To(Equal(injectInitContainerName))
			Expect(copyC.Image).To(Equal("ghcr.io/grafana/beyla/inject-sdk-image:0.0.12"))

			// The copy container must NOT be instrumented: no LD_PRELOAD, and
			// its mount is read-write so it can populate the volume.
			Expect(envNames(copyC.Env)).NotTo(ContainElement("LD_PRELOAD"))
			Expect(copyC.VolumeMounts).To(HaveLen(1))
			Expect(copyC.VolumeMounts[0].ReadOnly).To(BeFalse())

			// The app container is still instrumented with a read-only mount.
			Expect(envNames(obj.Spec.Containers[0].Env)).To(ContainElement("LD_PRELOAD"))
			appMounts := obj.Spec.Containers[0].VolumeMounts
			Expect(appMounts).To(HaveLen(1))
			Expect(appMounts[0].ReadOnly).To(BeTrue())
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

	Context("When the matched rule is narrowed by a pod label", func() {
		// Register a rule whose selector ALSO requires an inject=true label,
		// exercising the K8sSelector.PodLabels match path.
		BeforeEach(func() {
			setRule(registry.Instrumentation{InjectConfig: configmap.InjectConfig{
				Rules: []configmap.Rule{{
					Selector: configmap.K8sSelector{
						Namespaces: []services.GlobAttr{services.NewGlob(testNamespace)},
						PodLabels:  map[string]services.GlobAttr{"inject": services.NewGlob("true")},
					},
					Config: configmap.RuleConfig{Env: []corev1.EnvVar{
						{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://otel-collector:4318"},
					}},
				}},
			}})
		})

		It("Should NOT mutate a pod missing the inject label", func() {
			before := obj.DeepCopy()
			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())
			Expect(obj).To(Equal(before))
		})

		It("Should mutate a pod carrying inject=true", func() {
			obj.Labels = map[string]string{"inject": "true"}
			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())
			Expect(obj.Annotations).To(HaveKeyWithValue(InjectedAnnotation, defaulter.Mutator.Cfg.PackageVersion()))
			Expect(obj.Spec.Volumes).To(HaveLen(1))
		})
	})
})

var _ = Describe("IsInstrumented", func() {
	It("is false for a clean pod", func() {
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		}}
		Expect(IsInstrumented(&pod.Spec, &pod.ObjectMeta)).To(BeFalse())
	})

	It("is true when the inject annotation is present, regardless of version", func() {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{InjectedAnnotation: "any-stale-or-current-digest"},
		}}
		Expect(IsInstrumented(&pod.Spec, &pod.ObjectMeta)).To(BeTrue())
	})

	It("ignores an empty annotation value", func() {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{InjectedAnnotation: ""},
		}}
		Expect(IsInstrumented(&pod.Spec, &pod.ObjectMeta)).To(BeFalse())
	})

	It("is true when a container carries the SDK version env var", func() {
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "nginx",
				Env:   []corev1.EnvVar{{Name: envVarSDKVersion, Value: "some-digest"}},
			}},
		}}
		Expect(IsInstrumented(&pod.Spec, &pod.ObjectMeta)).To(BeTrue())
	})

	It("is true when only an init container carries the SDK version env var", func() {
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name:  "init",
				Image: "busybox",
				Env:   []corev1.EnvVar{{Name: envVarSDKVersion, Value: "some-digest"}},
			}},
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		}}
		Expect(IsInstrumented(&pod.Spec, &pod.ObjectMeta)).To(BeTrue())
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
