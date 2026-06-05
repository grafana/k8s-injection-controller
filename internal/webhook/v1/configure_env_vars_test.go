package v1

import (
	"strings"
	"testing"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newEnvMutator() *PodMutator {
	return &PodMutator{Cfg: config.SDKInject{}}
}

func podMeta() *metav1.ObjectMeta {
	return &metav1.ObjectMeta{Namespace: "my-ns", Name: "my-pod"}
}

func TestConfigureContainerEnvVarsSetsServiceIdentity(t *testing.T) {
	pm := newEnvMutator()
	c := &corev1.Container{Name: "app"}

	pm.configureContainerEnvVars(podMeta(), c, false)

	// Per-container identification env var.
	if got := envValue(c.Env, envInjectorOtelK8sContainerName); got != "app" {
		t.Fatalf("%s = %q, want %q", envInjectorOtelK8sContainerName, got, "app")
	}
	// With no labels/annotations the service name falls back to the pod name,
	// which is a downward-API reference rather than a literal value.
	if got := envValue(c.Env, envInjectorOtelServiceName); got != "$(OTEL_INJECTOR_K8S_POD_NAME)" {
		t.Fatalf("%s = %q, want %q", envInjectorOtelServiceName, got, "$(OTEL_INJECTOR_K8S_POD_NAME)")
	}

	// Service instance id is namespace.pod.container, with namespace and pod
	// coming from downward-API references, and lands in the extra resource
	// attributes blob.
	wantInstanceID := "service.instance.id=$(OTEL_INJECTOR_K8S_NAMESPACE_NAME).$(OTEL_INJECTOR_K8S_POD_NAME).app"
	resAttrs := envValue(c.Env, envInjectorOtelExtraResourceAttrs)
	if !strings.Contains(resAttrs, wantInstanceID) {
		t.Fatalf("%s = %q, want it to contain %q",
			envInjectorOtelExtraResourceAttrs, resAttrs, wantInstanceID)
	}
}

func TestConfigureContainerEnvVarsSpanMetricsSkip(t *testing.T) {
	wantAttr := string(attr.SkipSpanMetrics) + "=true"

	t.Run("enabled", func(t *testing.T) {
		pm := newEnvMutator()
		c := &corev1.Container{Name: "app"}

		pm.configureContainerEnvVars(podMeta(), c, true)

		resAttrs := envValue(c.Env, envInjectorOtelExtraResourceAttrs)
		if !strings.Contains(resAttrs, wantAttr) {
			t.Fatalf("%s = %q, want it to contain %q",
				envInjectorOtelExtraResourceAttrs, resAttrs, wantAttr)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		pm := newEnvMutator()
		c := &corev1.Container{Name: "app"}

		pm.configureContainerEnvVars(podMeta(), c, false)

		resAttrs := envValue(c.Env, envInjectorOtelExtraResourceAttrs)
		if strings.Contains(resAttrs, wantAttr) {
			t.Fatalf("%s = %q, want it to NOT contain %q",
				envInjectorOtelExtraResourceAttrs, resAttrs, wantAttr)
		}
	})
}

func TestConfigureContainerEnvVarsAppendsToExistingResourceAttrs(t *testing.T) {
	pm := newEnvMutator()
	// A rule may have already set static resource attributes; per-pod dynamic
	// attributes must be appended, not overwrite them.
	c := &corev1.Container{
		Name: "app",
		Env: []corev1.EnvVar{
			{Name: envInjectorOtelExtraResourceAttrs, Value: "static.attr=value"},
		},
	}

	pm.configureContainerEnvVars(podMeta(), c, true)

	resAttrs := envValue(c.Env, envInjectorOtelExtraResourceAttrs)
	if !strings.HasPrefix(resAttrs, "static.attr=value,") {
		t.Fatalf("%s = %q, want it to start with the pre-existing static.attr=value,",
			envInjectorOtelExtraResourceAttrs, resAttrs)
	}
	if !strings.Contains(resAttrs, string(attr.SkipSpanMetrics)+"=true") {
		t.Fatalf("%s = %q, want the per-pod attributes appended",
			envInjectorOtelExtraResourceAttrs, resAttrs)
	}
}
