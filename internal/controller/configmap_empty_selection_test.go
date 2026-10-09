package controller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
)

func TestEmptySelectionRestartsInjectedWorkloadsAcrossNodes(t *testing.T) {
	for name, payload := range map[string]string{
		"empty":          "{}\n",
		"exclusion_only": "rules:\n- k8s_selector:\n    namespaces: [excluded]\n  config:\n    mode: skip\n",
	} {
		for _, coldRegistry := range []bool{false, true} {
			t.Run(name+fmt.Sprintf("/cold_registry=%v", coldRegistry), func(t *testing.T) {
				testEmptySelectionRestartsInjectedWorkloadsAcrossNodes(t, payload, coldRegistry)
			})
		}
	}
}

func testEmptySelectionRestartsInjectedWorkloadsAcrossNodes(t *testing.T, payload string, coldRegistry bool) {
	t.Helper()
	const lastNode = "node-2"
	ctx := t.Context()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	defaults := config.SDKInject{ImageVersion: "test"}
	oldRules := "rules:\n- k8s_selector:\n    namespaces: [apps]\n"
	reg := registry.New()
	inst, _, err := parseConfigMap(map[string]string{configmap.KeyInstrumentation: oldRules})
	require.NoError(t, err)
	workloads := []struct {
		namespace string
		name      string
		injected  bool
	}{
		{"apps", "java-node-1", true},
		{"apps", "java-node-2", true},
		{"apps", "uninjected", false},
		{"active", "keep-instrumented", true},
		{"kube-system", "protected", true},
	}
	objects := make([]runtime.Object, 0, len(workloads)*3+3)
	for _, workload := range workloads {
		name, namespace := workload.name, workload.namespace
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name + "-abc",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: name}},
		}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name + "-abc-1",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name}},
		}}
		if workload.injected {
			pod.Annotations = map[string]string{
				webhookv1.InjectedAnnotation: config.PodConfigHash(&defaults, &inst.InjectConfig.Rules[0].Config),
			}
		}
		if namespace == "active" {
			// Cleanup must not reinstrument a still-selected workload, even
			// when its injected hash differs from the current configuration.
			pod.Annotations[webhookv1.InjectedAnnotation] = "another-config"
		}
		objects = append(objects, deployment, rs, pod)
	}
	active, _, err := parseConfigMap(map[string]string{configmap.KeyInstrumentation: "rules:\n- k8s_selector:\n    namespaces: [active]\n"})
	require.NoError(t, err)
	reg.Set("monitoring/active", active)
	objects = append(objects, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "active", Annotations: map[string]string{configmap.SelectorAnnotation: ""}},
		Data:       map[string]string{configmap.KeyInstrumentation: "rules:\n- k8s_selector:\n    namespaces: [active]\n"},
	})
	for _, node := range []string{"node-1", lastNode} {
		reg.Set("monitoring/"+node, inst)
		currentPayload := payload
		if node == lastNode {
			currentPayload = oldRules
		}
		// Beyla publishes only the empty selection. There are no restart
		// targets from its informers, which may be restricted to one node.
		objects = append(objects, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "monitoring", Name: node,
				Annotations: map[string]string{configmap.SelectorAnnotation: node},
			},
			Data: map[string]string{
				configmap.KeyInstrumentation: currentPayload,
			},
		})
	}
	if coldRegistry {
		reg = registry.New()
	}
	clientset := fake.NewSimpleClientset(objects...)
	reconciler := &ConfigMapReconciler{
		Client:    clientfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build(),
		Clientset: clientset, Registry: reg, DefaultSDKConfig: defaults,
	}
	for _, node := range []string{"node-1", lastNode} {
		if node == lastNode {
			var cm corev1.ConfigMap
			require.NoError(t, reconciler.Get(ctx, client.ObjectKey{Namespace: "monitoring", Name: node}, &cm))
			cm.Data[configmap.KeyInstrumentation] = payload
			require.NoError(t, reconciler.Update(ctx, &cm))
		}
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "monitoring", Name: node}})
		require.NoError(t, err)
		for _, workload := range workloads {
			deployment, err := clientset.AppsV1().Deployments(workload.namespace).Get(ctx, workload.name, metav1.GetOptions{})
			require.NoError(t, err)
			if node == lastNode && workload.namespace == "apps" && workload.injected {
				require.NotEmpty(t, deployment.Spec.Template.Annotations["beyla.grafana.com/restartedAt"])
			} else {
				require.Empty(t, deployment.Spec.Template.Annotations["beyla.grafana.com/restartedAt"])
			}
		}
	}
	_, _, matched := reg.Match(registry.PodInfo{Namespace: "apps"})
	require.False(t, matched, "empty selection must stop future SDK injection")
}
