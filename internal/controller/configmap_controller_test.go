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

	"github.com/grafana/beyla/v3/pkg/webhook/configmap"

	webhookv1 "github.com/grafana/beyla-k8s-injector/internal/webhook/v1"
)

func ensureNS(ns string) {
	err := k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

func mkRS(ns, name string) *appsv1.ReplicaSet {
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

func mkDeployment(ns, name string) *appsv1.Deployment {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "worker"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
	return deployment
}

func mkPod(ns, name string, rs *appsv1.ReplicaSet, mutate func(*corev1.Pod)) {
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

func mkCM(ns, name, selectionYAML, restartYAML string) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: map[string]string{configmap.SelectorAnnotation: ""},
		},
		Data: map[string]string{
			configmap.KeyInstrumentation:    selectionYAML,
			configmap.KeyEligibleForRestart: restartYAML,
		},
	}
	Expect(k8sClient.Create(ctx, cm)).To(Succeed())
}

// expectKept asserts the pod stays present for `window`. Any eviction
// the controller decides to do happens within a single Reconcile pass
// (the positive test confirms that takes well under a second), so 3s
// is plenty of margin without slowing the suite down.
func expectKept(ns, name string) {
	Consistently(func() error {
		return k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &corev1.Pod{})
	}, 3*time.Second, 100*time.Millisecond).Should(Succeed(),
		"pod %s/%s was unexpectedly evicted", ns, name)
}

func expectAnnotated(ns, depName string) {
	Eventually(func() string {
		var d appsv1.Deployment
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: depName}, &d); err != nil {
			return ""
		}
		return d.Spec.Template.Annotations["beyla.grafana.com/restartedAt"]
	}, 30*time.Second, 50*time.Millisecond).ShouldNot(BeEmpty(),
		"deployment %s/%s did not get restartedAt annotation", ns, depName)
}

// This suite spins up a real apiserver+etcd via envtest and the
// ConfigMapReconciler against it. envtest has no scheduler or kubelet, so the
// pods we create stay Pending — but the apiserver still serves the
// pods/eviction subresource.
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

	It("does not patch when the pod's owner is a standalone ReplicaSet with no Deployment parent", func() {
		By("creating a standalone ReplicaSet (no Deployment owner)")
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

		By("creating a pod owned by that ReplicaSet")
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

		By("creating the selector ConfigMap targeting the RS")
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "beyla-selector",
				Namespace:   ns,
				Annotations: map[string]string{configmap.SelectorAnnotation: ""},
			},
			Data: map[string]string{
				// Selection criterion is the dual gate: without it, rolloutMatching
				// would skip the pod even if the restart target matched.
				configmap.KeyInstrumentation: "discovery:\n  - k8s_namespace: " + ns + "\n",
				configmap.KeyEligibleForRestart: "- namespace: " + ns + "\n" +
					"  kind: ReplicaSet\n" +
					"  name: " + rsName + "\n",
			},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		By("asserting the pod is not restarted - standalone RS cannot be gracefully rolled")
		expectKept(ns, podName)
	})
})

// Negative cases: situations in which the controller must NOT evict the
// matched pod. Each spec uses its own namespace because the in-memory
// Registry is package-global from the suite and persists across tests.
var _ = Describe("ConfigMap controller eviction skip cases", func() {
	It("does not evict when only eligible_for_restart matches (selection_criteria misses)", func() {
		const ns = "evict-skip-1"
		ensureNS(ns)
		rs := mkRS(ns, "worker-1")
		mkPod(ns, "worker-1-001", rs, nil)

		// Selection criterion targets a different namespace, so Registry.Match
		// returns false and the pod is skipped despite the restart-target match.
		mkCM(ns, "cm-1",
			"discovery:\n  - k8s_namespace: somewhere-else\n",
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
			"discovery:\n  - k8s_namespace: "+ns+"\n",
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
			"discovery:\n  - k8s_namespace: "+ns+"\n",
			"- namespace: "+ns+"\n  kind: ReplicaSet\n  name: worker-3\n")

		expectKept(ns, "worker-3-001")
	})

	It("does not evict pods already annotated at the current SDK version", func() {
		const ns = "evict-skip-4"
		ensureNS(ns)
		rs := mkRS(ns, "worker-4")
		mkPod(ns, "worker-4-001", rs, func(p *corev1.Pod) {
			p.Annotations = map[string]string{
				webhookv1.InjectedAnnotation: testSDKConfig.PackageVersion(),
			}
		})

		// Both gates would pass; version-matched annotation is the skip reason.
		mkCM(ns, "cm-4",
			"discovery:\n  - k8s_namespace: "+ns+"\n",
			"- namespace: "+ns+"\n  kind: ReplicaSet\n  name: worker-4\n")

		expectKept(ns, "worker-4-001")
	})

	It("evicts pods annotated with a stale SDK version so the webhook can re-inject", func() {
		const ns = "evict-stale-5"
		ensureNS(ns)

		dep := mkDeployment(ns, "worker-5")
		ctrlTrue := true

		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-5-rs", Namespace: ns,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1",
					Kind:       "Deployment", Name: dep.Name, UID: dep.UID, Controller: &ctrlTrue,
				}},
			},
			Spec: appsv1.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "worker"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		mkPod(ns, "worker-5-001", rs, func(p *corev1.Pod) {
			p.Annotations = map[string]string{
				webhookv1.InjectedAnnotation: "stale-version-digest",
			}
		})

		mkCM(ns, "cm-5",
			"discovery:\n  - k8s_namespace: "+ns+"\n",
			"- namespace: "+ns+"\n  kind: Deployment\n  name: worker-5\n")

		By("asserting the pod is NOT individually evicted")
		expectKept(ns, "worker-5-001")

		By("asserting the pod is annotated")
		expectAnnotated(ns, "worker-5")
	})

	It("does not evict pods in a protected namespace even when both gates match", func() {
		const ns = "kube-system"
		ensureNS(ns)
		rs := mkRS(ns, "system-worker")
		mkPod(ns, "system-worker-001", rs, nil)

		// Both gates would pass if we didn't have denylist:
		// - discovery matches kube-system
		// - eligible_for_restart targets kube-system/system-worker
		mkCM(ns, "cm-protected",
			"discovery:\n - k8s_namespace: "+ns+"\n",
			"- namespace: "+ns+"\n  kind: ReplicaSet\n  name: system-worker\n")

		expectKept(ns, "system-worker-001")
	})
})

var _ = Describe("ConfigMap controller rollout sweep", func() {
	const (
		ns      = "demo-rollout"
		depName = "web-app"
		rsName  = "web-app-abc123"
		podName = "web-app-abc123-001"
	)

	BeforeEach(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("patches the Deployment annotation", func() {
		By("creating a deployment")
		dep := mkDeployment(ns, depName)

		By("creating a ReplicaSet owned by the Deployment")
		ctrlTrue := true
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rsName,
				Namespace: ns,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					UID:        dep.UID,
					Name:       dep.Name,
					Controller: &ctrlTrue,
				}},
			},
			Spec: appsv1.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "worker"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		By("creating a Pod owned by the ReplicaSet")
		mkPod(ns, podName, rs, nil)

		By("creating the selector ConfigMap with kind: Deployment in eligible_for_restart.yaml")
		mkCM(ns, "beyla-selector-rollout",
			"discovery:\n - k8s_namespace: "+ns+"\n",
			"- namespace: "+ns+"\n  kind: Deployment\n  name: "+depName+"\n")

		By("asserting the pod is NOT evicted")
		expectKept(ns, podName)

		By("asserting the Deployment gains the restartedAt annotation")
		Eventually(func() string {
			var d appsv1.Deployment
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: depName}, &d); err != nil {
				return ""
			}
			return d.Spec.Template.Annotations["beyla.grafana.com/restartedAt"]
		}, 30*time.Second, 100*time.Millisecond).ShouldNot(BeEmpty(),
			"Deployment %s/%s did not get restartedAt annotation", ns, depName)

	})

	It("restarts an instrumented Deployment whose pods no longer match (uninstrumentation)", func() {
		const (
			uns     = "demo-uninstrument"
			udep    = "legacy-app"
			urs     = "legacy-app-abc123"
			upod    = "legacy-app-abc123-001"
			otherNS = "somewhere-else"
		)
		ensureNS(uns)

		By("creating an instrumented Deployment/ReplicaSet/Pod")
		dep := mkDeployment(uns, udep)
		ctrlTrue := true
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      urs,
				Namespace: uns,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					UID:        dep.UID,
					Name:       dep.Name,
					Controller: &ctrlTrue,
				}},
			},
			Spec: appsv1.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "worker"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		// The pod carries our inject annotation, marking it as instrumented.
		mkPod(uns, upod, rs, func(p *corev1.Pod) {
			p.Annotations = map[string]string{
				webhookv1.InjectedAnnotation: testSDKConfig.PackageVersion(),
			}
		})

		By("creating a ConfigMap whose selection criteria no longer match the pod")
		// discovery targets a different namespace, so Registry.Match misses; the
		// workload is still listed in eligible_for_restart so the controller
		// re-evaluates it and, finding it instrumented-but-unmatched, rolls it.
		mkCM(uns, "beyla-selector-uninstrument",
			"discovery:\n - k8s_namespace: "+otherNS+"\n",
			"- namespace: "+uns+"\n  kind: Deployment\n  name: "+udep+"\n")

		By("asserting the pod is NOT individually evicted")
		expectKept(uns, upod)

		By("asserting the Deployment gains the restartedAt annotation")
		expectAnnotated(uns, udep)
	})

})
