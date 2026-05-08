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
