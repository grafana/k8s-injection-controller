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

package main

import (
	"context"
	"encoding/json"
	"net"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// hardenConfigMapWebhook flips the ConfigMap ValidatingWebhookConfiguration from
// the failurePolicy=Ignore it ships with to failurePolicy=Fail, once this
// controller's webhook server is serving and reachable.
//
// Why it ships Ignore: the webhook runs scoped (by namespaceSelector) to the
// namespace the controller watches, which is usually the controller's own
// namespace. With Fail, the apiserver would reject the kube-controller-manager's
// attempt to publish kube-root-ca.crt into that namespace while the backend
// (this pod) is still starting - but the pod cannot start without
// kube-root-ca.crt (its ServiceAccount-token volume needs it). Shipping Ignore
// lets the apiserver fail open during that window so the pod starts; this then
// hardens the policy to Fail (a security control: once the validator is up, an
// outage must not silently re-open the injection-steering hole).
//
// It runs only after the server is serving and its Service is reachable, so the
// apiserver never starts failing closed against an unreachable backend. It only
// touches failurePolicy (the single webhook at index 0), so it never races the
// caBundle that cert-manager's cainjector or the in-process cert rotator write
// into the same object. On restart the patch is a harmless no-op (already Fail).
func hardenConfigMapWebhook(
	ctx context.Context,
	serverStarted healthz.Checker,
	clientset kubernetes.Interface,
	webhookName, serviceAddr string,
) {
	log := ctrl.Log.WithName("webhook-activation")

	// 1. Wait for the local webhook server to be listening.
	if err := pollUntil(ctx, time.Second, func() bool { return serverStarted(nil) == nil }); err != nil {
		log.Info("shutting down before the webhook server started; webhook left at failurePolicy=Ignore",
			"webhook", webhookName)
		return
	}

	// 2. Wait until our webhook Service is reachable (endpoints programmed) so the
	//    apiserver can call us the instant we switch to Fail. Best-effort.
	if serviceAddr != "" {
		_ = pollUntil(ctx, 2*time.Second, func() bool {
			c, err := net.DialTimeout("tcp", serviceAddr, 2*time.Second)
			if err != nil {
				return false
			}
			_ = c.Close()
			return true
		})
	}

	// 3. Harden failurePolicy to Fail on the (single) webhook.
	patch, err := json.Marshal([]map[string]any{{
		"op":    "replace",
		"path":  "/webhooks/0/failurePolicy",
		"value": admissionregistrationv1.Fail,
	}})
	if err != nil {
		log.Error(err, "failed to build webhook hardening patch")
		return
	}

	if err := pollUntil(ctx, 2*time.Second, func() bool {
		_, perr := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().
			Patch(ctx, webhookName, types.JSONPatchType, patch, metav1.PatchOptions{})
		if perr != nil {
			log.Info("retrying webhook hardening", "webhook", webhookName, "error", perr.Error())
			return false
		}
		return true
	}); err != nil {
		log.Info("shutting down before the webhook could be hardened to Fail", "webhook", webhookName)
		return
	}
	log.Info("hardened ConfigMap validating webhook to failurePolicy=Fail", "webhook", webhookName)
}

// pollUntil calls cond every interval until it returns true or ctx is done.
func pollUntil(ctx context.Context, interval time.Duration, cond func() bool) error {
	return wait.PollUntilContextCancel(ctx, interval, true, func(context.Context) (bool, error) {
		return cond(), nil
	})
}
