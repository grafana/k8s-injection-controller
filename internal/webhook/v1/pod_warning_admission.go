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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type admissionWarningCollector struct {
	warnings []string
}

type admissionWarningContextKey struct{}

type warningAdmissionHandler struct {
	next admission.Handler
}

var _ admission.Handler = (*warningAdmissionHandler)(nil)

func newWarningAdmissionHandler(next admission.Handler) admission.Handler {
	return &warningAdmissionHandler{next: next}
}

func (h *warningAdmissionHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	collector := &admissionWarningCollector{}
	ctx = context.WithValue(ctx, admissionWarningContextKey{}, collector)
	response := h.next.Handle(ctx, req)
	return response.WithWarnings(collector.warnings...)
}

func addAdmissionWarnings(ctx context.Context, pod *corev1.Pod, warnings []string) {
	for _, warning := range warnings {
		podlog.Info("Pod admission warning", "namespace", pod.Namespace, "name", pod.Name, "warning", warning)
	}
	collector, ok := ctx.Value(admissionWarningContextKey{}).(*admissionWarningCollector)
	if !ok {
		return
	}
	collector.warnings = append(collector.warnings, warnings...)
}
