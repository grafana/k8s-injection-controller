# k8s-injection-controller

Helm chart for the **k8s-injection-controller**, a Kubernetes mutating-admission
controller that injects Grafana Beyla / OpenTelemetry SDK auto-instrumentation
into application pods selected by Beyla-managed ConfigMaps.

The chart installs the controller and everything it needs: ServiceAccount, RBAC
(cluster + namespaced), the pod mutating webhook and ConfigMap validating
webhook, their Service, the cert-manager Issuer/Certificate for the webhook TLS,
and (optionally) the default SDK injection config. It does **not** install Beyla
or any demo application.

## Prerequisites

- Kubernetes **1.31+** (required for `image` injection mode via
  `ImageVolumeSource`; older clusters work with `sdkConfig.injectionMode:
  init_container`).
- **[cert-manager](https://cert-manager.io/)** must already be installed and
  Ready. The chart creates `Issuer`/`Certificate` resources, so cert-manager's
  CRDs and validating webhook must exist *before* you install this chart.
  cert-manager is intentionally **not** bundled as a subchart: its CRDs and
  webhook cannot be created and consumed within a single `helm install`.

## Install

cert-manager is a one-time, cluster-wide prerequisite — install it once per
cluster, not per release:

```bash
# 1. Install cert-manager (once per cluster) and wait until it is Ready.
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true
kubectl -n cert-manager rollout status deployment/cert-manager-webhook

# 2. Install this chart.
helm install kic ./k8s-injection-controller
```

## Configuration

See [`values.yaml`](./values.yaml) for the full list. Common knobs:

| Value | Default | Description |
|-------|---------|-------------|
| `namespace.name` | `beyla-k8s-injector` | Namespace the controller is installed into. |
| `namespace.create` | `true` | Whether the chart renders the Namespace object. |
| `watchNamespace` | `""` | Namespace whose ConfigMaps Beyla writes; empty = the install namespace. |
| `image.repository` / `image.tag` | `ghcr.io/grafana/k8s-injection-controller` / chart `appVersion` | Controller image. |
| `allowedConfigMapWriters` | `system:serviceaccount:beyla-k8s-injector:beyla` | Identities allowed to write injection ConfigMaps. |
| `sdkConfig.*` | see values | Default SDK auto-instrumentation config; `sdkConfig.enabled=false` selects but does not mutate. |
| `webhook.excludedNamespaces` | system/infra namespaces | Namespaces the mutating webhook never touches (install namespace is always added). |
| `metrics.enabled` / `metrics.port` | `true` / `8080` | Plain-HTTP Prometheus metrics, advertised via pod annotations (no Prometheus Operator required). |

## Uninstall

```bash
helm uninstall kic
```

cert-manager (a shared cluster prerequisite) is left untouched; remove it
separately if nothing else uses it.
