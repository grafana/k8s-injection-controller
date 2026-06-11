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
- *Optional*: [cert-manager](https://cert-manager.io/) can be optionally
  deployed. The chart creates `Issuer`/`Certificate` resources, so cert-manager's
  CRDs and validating webhook must exist *before* you install this chart.
  cert-manager is intentionally **not** bundled as a subchart: its CRDs and
  webhook cannot be created and consumed within a single `helm install`.
  If `cert-manager` is not present, self-signed certificates will be used
  for the webhook, which is automatically created and rotated on every start
  of the controller.

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
| `webhook.certManager.mode` | auto | How the webhook serving certificate is provisioned: auto, cert-manager, self-signed. |
| `metrics.enabled` / `metrics.port` | `true` / `8080` | Plain-HTTP Prometheus metrics, advertised via pod annotations (no Prometheus Operator required). |

## Uninstall

```bash
helm uninstall kic
```

cert-manager (a shared cluster prerequisite) is left untouched; remove it
separately if nothing else uses it.

## Local development mode deployment with kind

1. Build the controller image as `beyla-k8s-injector:dev`
```sh
IMG=beyla-k8s-injector:dev make docker-build
```
2. Create your cluster (if you don't have one with)
```sh
kind create cluster
```
3. Load the local image into kind
```sh
kind load docker-image beyla-k8s-injector:dev
```
4. Install the controller through helm
```sh
helm install kic ./charts/k8s-injection-controller \
--set image.repository=beyla-k8s-injector \
--set image.tag=dev \
--set image.pullPolicy=IfNotPresent
```

The command above will auto-detect if you have `cert-manager` running and
if not it will use self-signed certificate. If you want to test the helm
install process with `cert-manager` install cert manager before step 4, 
and wait until it's up and running.