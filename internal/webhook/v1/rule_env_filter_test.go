package v1

import (
	"reflect"
	"testing"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRuleEnvironmentForSDKFiltersOnlyRuleMetadata(t *testing.T) {
	ruleEnv := []corev1.EnvVar{
		{Name: "KEEP", Value: "rule"},
		{Name: config.JavaExtensionImagesEnv, Value: "example.test/ext:v1"},
		{Name: injectorJavaExtensionsEnv, Value: "/configured/path"},
	}
	wantOriginal := append([]corev1.EnvVar(nil), ruleEnv...)

	got := ruleEnvironmentForSDK(ruleEnv)
	if !reflect.DeepEqual(got, []corev1.EnvVar{{Name: "KEEP", Value: "rule"}}) {
		t.Fatalf("filtered env = %#v", got)
	}
	if !reflect.DeepEqual(ruleEnv, wantOriginal) {
		t.Fatalf("filter modified input: got %#v, want %#v", ruleEnv, wantOriginal)
	}

	workloadEnv := []corev1.EnvVar{
		{Name: config.JavaExtensionImagesEnv, Value: "workload-image-metadata"},
		{Name: injectorJavaExtensionsEnv, Value: "/workload/extensions"},
	}
	container := &corev1.Container{Name: "app", Env: append([]corev1.EnvVar(nil), workloadEnv...)}
	pm := newModeMutator(config.InjectionModeImage)
	pm.addEnvVars(&metav1.ObjectMeta{}, container, ruleEnv, false)
	if len(container.Env) < len(workloadEnv) || !reflect.DeepEqual(container.Env[:len(workloadEnv)], workloadEnv) {
		t.Fatalf("addEnvVars changed workload-owned extension env: got %#v, want %#v", container.Env, workloadEnv)
	}
}
