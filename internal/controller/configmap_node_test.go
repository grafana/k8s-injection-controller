/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/grafana/beyla-k8s-injector/internal/config"
	"github.com/grafana/beyla-k8s-injector/internal/registry"
)

func TestNodeStateLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: "beyla", Annotations: map[string]string{configmap.SelectorAnnotation: "node-1"}},
		Data:       map[string]string{configmap.KeyInstrumentation: "image_version: old\nrules:\n  - k8s_selector:\n      namespaces: [app]\n"},
	}
	otherNode := cm.DeepCopy()
	otherNode.Name = "other-node-state"
	otherNode.Annotations[configmap.SelectorAnnotation] = "node-2"
	independent := cm.DeepCopy()
	independent.Name = "node-independent-state"
	independent.Annotations[configmap.SelectorAnnotation] = ""
	unannotated := cm.DeepCopy()
	unannotated.Name = "unrelated-config"
	unannotated.Annotations = nil
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cm, otherNode, independent, unannotated).
		Build()
	r := &ConfigMapReconciler{Client: c, Registry: registry.New()}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cm)}
	pod := registry.PodInfo{Namespace: "app"}
	reconcile := func(wantMatch bool) {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := r.Registry.Match(pod); ok != wantMatch {
			t.Fatalf("matched = %v, want %v", ok, wantMatch)
		}
	}
	reconcile(false) // An orphan present at controller startup cannot contribute.
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	if err := c.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	if requests := r.configMapsForNode(ctx, node); len(requests) != 1 || requests[0] != req {
		t.Fatalf("node requests: %v", requests)
	}
	reconcile(true)
	if err := c.Delete(ctx, node); err != nil {
		t.Fatal(err)
	}
	reconcile(false) // Previously loaded state must be removed too.
	// A node waiting for finalizers is already ineligible, even before deletion.
	node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Finalizers: []string{"test.example/hold"}}}
	if err := c.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	reconcile(true)
	if err := c.Delete(ctx, node); err != nil {
		t.Fatal(err)
	}
	reconcile(false)
	// Empty annotations are still valid for manually managed selectors.
	if err := c.Get(ctx, req.NamespacedName, cm); err != nil {
		t.Fatal(err)
	}
	cm.Annotations[configmap.SelectorAnnotation] = ""
	if err := c.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	reconcile(true)
	if requests := r.configMapsForNode(ctx, node); len(requests) != 0 {
		t.Fatalf("node lookup retained a map after its node annotation was cleared: %v", requests)
	}
}

func TestOrphanDoesNotOverrideLiveState(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "live-node"}}
	makeState := func(name, nodeName, version string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "beyla", Annotations: map[string]string{configmap.SelectorAnnotation: nodeName}},
			Data:       map[string]string{configmap.KeyInstrumentation: "image_version: " + version + "\nrules:\n  - k8s_selector:\n      namespaces: [app]\n"},
		}
	}
	// The orphan sorts before the live map, reproducing the stale-version selection.
	orphan := makeState("a-orphan", "gone-node", "old")
	live := makeState("z-live", node.Name, "wanted")
	r := &ConfigMapReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, orphan, live).Build(),
		Registry: registry.New(),
	}
	for _, cm := range []*corev1.ConfigMap{live, orphan} {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cm)}); err != nil {
			t.Fatal(err)
		}
	}
	_, cfg, ok := r.Registry.Match(registry.PodInfo{Namespace: "app"})
	if !ok || cfg.ImageVersion != "wanted" {
		t.Fatalf("selected stale state: %+v, matched=%v", cfg, ok)
	}
}

func TestNoAutoRestartStillRegistersRules(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: "beyla", Annotations: map[string]string{configmap.SelectorAnnotation: ""}},
		Data: map[string]string{
			configmap.KeyInstrumentation:    "image_version: new\nrules:\n  - k8s_selector:\n      namespaces: [app]\n",
			configmap.KeyEligibleForRestart: "- namespace: app\n  kind: Deployment\n  name: web\n",
		},
	}
	// A rollout would reach WebhookReady. Disabling restarts must skip it,
	// while publishing the rules used to inject newly admitted pods.
	r := &ConfigMapReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build(),
		Registry: registry.New(), DefaultSDKConfig: config.SDKInject{NoAutoRestart: true},
		WebhookReady: func(_ *http.Request) error { t.Fatal("restart sweep reached readiness check"); return nil },
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}}); err != nil {
		t.Fatal(err)
	}
	if _, cfg, ok := r.Registry.Match(registry.PodInfo{Namespace: "app"}); !ok || cfg.ImageVersion != "new" {
		t.Fatalf("admission rules not registered: %+v, %v", cfg, ok)
	}
}

func TestRefreshRegistryRejectsDepartedNodes(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		name := "absent"
		if terminating {
			name = "terminating"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			makeState := func(name, node, namespace, version string) *corev1.ConfigMap {
				return &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "beyla", Annotations: map[string]string{configmap.SelectorAnnotation: node}},
					Data:       map[string]string{configmap.KeyInstrumentation: "image_version: " + version + "\nrules:\n- k8s_selector:\n    namespaces: [" + namespace + "]\n"},
				}
			}
			stale := makeState("a-stale", "departed-node", "app", "old")
			live := makeState("z-live", "live-node", "app", "wanted")
			manual := makeState("manual", "", "manual-app", "manual")
			unannotated := makeState("unrelated", "", "unrelated-app", "unrelated")
			unannotated.Annotations = nil
			objects := []client.Object{stale, live, manual, unannotated, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "live-node"}}}
			if terminating {
				objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
					Name: "departed-node", DeletionTimestamp: &metav1.Time{Time: time.Now()}, Finalizers: []string{"test.example/hold"},
				}})
			}
			r := &ConfigMapReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), Registry: registry.New(),
			}
			// Reject stale state through normal reconciliation, then verify that
			// the cleanup snapshot cannot restore its old image version.
			for _, cm := range []*corev1.ConfigMap{stale, live, manual} {
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cm)})
				require.NoError(t, err)
			}
			require.NoError(t, r.refreshRegistry(ctx))
			_, cfg, matched := r.Registry.Match(registry.PodInfo{Namespace: "app"})
			require.True(t, matched)
			require.Equal(t, "wanted", cfg.ImageVersion)
			_, cfg, matched = r.Registry.Match(registry.PodInfo{Namespace: "manual-app"})
			require.True(t, matched)
			require.Equal(t, "manual", cfg.ImageVersion)
			_, _, matched = r.Registry.Match(registry.PodInfo{Namespace: "unrelated-app"})
			require.False(t, matched)
		})
	}
}

// Exercise the real informer wiring: node deletion must invalidate state
// without any ConfigMap update, including after a previously absent node joins.
var _ = Describe("Node state lifecycle", func() {
	It("reconciles existing ConfigMaps on node creation and deletion", func() {
		const ns = "node-state-lifecycle"
		ensureNS(ns)
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: ns, Annotations: map[string]string{configmap.SelectorAnnotation: "state-test-node"}},
			Data:       map[string]string{configmap.KeyInstrumentation: "image_version: wanted\nrules:\n  - k8s_selector:\n      namespaces: [node-state-lifecycle]\n"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, cm)).To(Succeed()) })
		pod := registry.PodInfo{Namespace: ns}
		matched := func() bool { _, _, ok := reg.Match(pod); return ok }
		Consistently(matched, time.Second, 50*time.Millisecond).Should(BeFalse())
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "state-test-node"}}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		Eventually(matched, 10*time.Second, 50*time.Millisecond).Should(BeTrue())
		Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		Eventually(matched, 10*time.Second, 50*time.Millisecond).Should(BeFalse())
	})
})
