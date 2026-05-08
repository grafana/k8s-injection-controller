/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
)

// This suite spins up a real apiserver+etcd via envtest and the
// ConfigMapReconciler against it. envtest has no scheduler or kubelet, so the
// pods we create stay Pending — but the apiserver still serves the
// pods/eviction subresource, and "evicted" here really just means "deleted",
// which is exactly what we assert.
var _ = Describe("ConfigMap controller eviction sweep", func() {
	const (
		ns      = "demo-evict"
		rsName  = "worker-abc"
		podName = "worker-abc-001"
	)

	BeforeEach(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("evicts a pre-existing pod when a matching selector ConfigMap is created", func() {
		By("creating a pre-existing ReplicaSet (we never actually run pods through it; it's just an owner ref target)")
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: rsName, Namespace: ns},
			Spec: appsv1.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "worker"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		By("creating a pre-existing pod owned by that ReplicaSet")
		ctrlTrue := true
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: ns,
				Labels:    map[string]string{"app": "worker"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       rsName,
					UID:        rs.UID,
					Controller: &ctrlTrue,
				}},
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyAlways,
				Containers:    []corev1.Container{{Name: "app", Image: "nginx"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())

		By("creating the selector ConfigMap with the pod's RS in eligible_for_restart.yaml")
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "beyla-selector",
				Namespace:   ns,
				Annotations: map[string]string{SelectorAnnotation: ""},
			},
			Data: map[string]string{
				// Selection criterion is the dual gate: without it, evictMatching
				// would skip the pod even if the restart target matched.
				SelectionCriteriaKey: "- k8s_namespace: " + ns + "\n",
				EligibleForRestartKey: "- namespace: " + ns + "\n" +
					"  kind: ReplicaSet\n" +
					"  name: " + rsName + "\n",
			},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		By("waiting for the pod to be evicted (deleted)")
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: podName}, &corev1.Pod{})
			return apierrors.IsNotFound(err)
		}, 30*time.Second, 50*time.Millisecond).Should(BeTrue(),
			"pod %s/%s was not evicted within the timeout", ns, podName)
	})
})

// Negative cases: situations in which the controller must NOT evict the
// matched pod. Each spec uses its own namespace because the in-memory
// Registry is package-global from the suite and persists across tests.
var _ = Describe("ConfigMap controller eviction skip cases", func() {
	ensureNS := func(ns string) {
		err := k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}

	mkRS := func(ns, name string) *appsv1.ReplicaSet {
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: appsv1.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "worker"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		return rs
	}

	mkPod := func(ns, name string, rs *appsv1.ReplicaSet, mutate func(*corev1.Pod)) {
		ctrlTrue := true
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Labels:    map[string]string{"app": "worker"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       rs.Name,
					UID:        rs.UID,
					Controller: &ctrlTrue,
				}},
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyAlways,
				Containers:    []corev1.Container{{Name: "app", Image: "nginx"}},
			},
		}
		if mutate != nil {
			mutate(pod)
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	}

	mkCM := func(ns, name, selectionYAML, restartYAML string) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   ns,
				Annotations: map[string]string{SelectorAnnotation: ""},
			},
			Data: map[string]string{
				SelectionCriteriaKey:  selectionYAML,
				EligibleForRestartKey: restartYAML,
			},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
	}

	// expectKept asserts the pod stays present for `window`. Any eviction
	// the controller decides to do happens within a single Reconcile pass
	// (the positive test confirms that takes well under a second), so 3s
	// is plenty of margin without slowing the suite down.
	expectKept := func(ns, name string) {
		Consistently(func() error {
			return k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &corev1.Pod{})
		}, 3*time.Second, 100*time.Millisecond).Should(Succeed(),
			"pod %s/%s was unexpectedly evicted", ns, name)
	}

	It("does not evict when only eligible_for_restart matches (selection_criteria misses)", func() {
		const ns = "evict-skip-1"
		ensureNS(ns)
		rs := mkRS(ns, "worker-1")
		mkPod(ns, "worker-1-001", rs, nil)

		// Selection criterion targets a different namespace, so Registry.Match
		// returns false and the pod is skipped despite the restart-target match.
		mkCM(ns, "cm-1",
			"- k8s_namespace: somewhere-else\n",
			"- namespace: "+ns+"\n  kind: ReplicaSet\n  name: worker-1\n")

		expectKept(ns, "worker-1-001")
	})

	It("does not evict when only selection_criteria matches (eligible_for_restart misses)", func() {
		const ns = "evict-skip-2"
		ensureNS(ns)
		rs := mkRS(ns, "worker-2")
		mkPod(ns, "worker-2-001", rs, nil)

		// Restart target names a different RS in the same namespace, so
		// matchesAnyTarget returns false. Selection criterion would have
		// matched — but the dual gate blocks the eviction.
		mkCM(ns, "cm-2",
			"- k8s_namespace: "+ns+"\n",
			"- namespace: "+ns+"\n  kind: ReplicaSet\n  name: some-other-rs\n")

		expectKept(ns, "worker-2-001")
	})

	It("does not evict pods that already declare a foreign LD_PRELOAD", func() {
		const ns = "evict-skip-3"
		ensureNS(ns)
		rs := mkRS(ns, "worker-3")
		mkPod(ns, "worker-3-001", rs, func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{
				{Name: "LD_PRELOAD", Value: "/opt/other.so"},
			}
		})

		// Both gates would pass; PreloadsSomethingElse is the skip reason.
		mkCM(ns, "cm-3",
			"- k8s_namespace: "+ns+"\n",
			"- namespace: "+ns+"\n  kind: ReplicaSet\n  name: worker-3\n")

		expectKept(ns, "worker-3-001")
	})

	It("does not evict pods already annotated with our injection marker", func() {
		const ns = "evict-skip-4"
		ensureNS(ns)
		rs := mkRS(ns, "worker-4")
		mkPod(ns, "worker-4-001", rs, func(p *corev1.Pod) {
			p.Annotations = map[string]string{
				webhookv1.InjectedAnnotation: webhookv1.InjectedAnnotValue,
			}
		})

		// Both gates would pass; AlreadyInstrumentedByOther is the skip reason.
		mkCM(ns, "cm-4",
			"- k8s_namespace: "+ns+"\n",
			"- namespace: "+ns+"\n  kind: ReplicaSet\n  name: worker-4\n")

		expectKept(ns, "worker-4-001")
	})
})
