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
				ImageVolumePath: "ghcr.io/grafana/beyla/inject-sdk-image:0.0.9",
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

		It("Should annotate the Pod as instrumented", func() {
			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())
			Expect(obj.Annotations).To(HaveKeyWithValue(InjectedAnnotation, InjectedAnnotValue))
		})
	})

	Context("When creating a pod that is already annotated as instrumented", func() {
		It("Should not modify anything", func() {
			obj.Annotations = map[string]string{InjectedAnnotation: InjectedAnnotValue}
			before := obj.DeepCopy()

			Expect(defaulter.Default(context.Background(), obj)).To(Succeed())

			// Identical to the pre-call snapshot: no env vars, no volumes,
			// no extra annotations.
			Expect(obj).To(Equal(before))
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
