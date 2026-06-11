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
  deployed. The chart's resources are applied — the chart creates
  `Issuer`/`Certificate` objects, so cert-manager's CRDs and webhook must
  already exist. cert-manager is intentionally **not** bundled as a subchart:
  its CRDs and validating webhook cannot be created and consumed within a single
  `helm install`. By default the chart satisfies this prerequisite for you with
  a pre-install hook (see below); you can also install cert-manager yourself.

## Install

```bash
helm install kic ./k8s-injection-controller
```

### cert-manager pre-install hook (default)

With `certManager.installHook.enabled=true` (the default), a short-lived
`pre-install` hook Pod runs [`hooks/install-cert-manager.sh`](./hooks/install-cert-manager.sh)
before the rest of the chart is applied. It:

1. checks whether cert-manager is already present
   (`kubectl get crd certificates.cert-manager.io`) and does nothing if so;
2. otherwise `helm install`s cert-manager into the `cert-manager` namespace
   (`--set crds.enabled=true`) and waits for its webhook to become Ready.

The hook Pod runs from an image containing both `kubectl` and `helm`
(`dtzar/helm-kubectl` by default) under a **temporary** ServiceAccount +
ClusterRole/ClusterRoleBinding. Installing cert-manager touches cluster-scoped
objects (CRDs, ClusterRoles, webhook configs, a Namespace), so that role is
cluster-admin–equivalent — but it lives only for the duration of the hook and
is deleted as soon as the hook succeeds (`helm.sh/hook-delete-policy`).

### How the webhook serving certificate is provisioned

Because cert-manager is installed on the fly by the pre-install hook above, its
`cert-manager.io/v1` CRDs do not exist when Helm validates the main manifest.
The `Issuer` and `Certificate` are therefore rendered as **`post-install`
hooks**: they are created only after the pre-install Pod has registered the CRDs
and waited for the cert-manager webhook to be Ready, so they always resolve.

> **`--wait` / `--atomic` caveat:** on the **first install** these flags do not
> work. Helm waits for the controller Deployment to be Ready *before* running
> post-install hooks, but the Deployment cannot start until the hook issues the
> serving-cert Secret it mounts. Install **without** `--wait` (the default): the
> pod waits a few seconds in `ContainerCreating` until the Secret appears, then
> starts. Upgrades are unaffected — the Secret already exists by then.
>
> If you require `--wait` on first install, install cert-manager beforehand (see
> below) so the CRDs pre-exist; the cert resources can then be applied inline
> instead of via the post-install hook.

### Installing cert-manager yourself

If you prefer to manage cert-manager out of band, disable the hook and install
it once per cluster beforehand:

```bash
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true
kubectl -n cert-manager rollout status deployment/cert-manager-webhook

helm install kic ./k8s-injection-controller \
  --set certManager.installHook.enabled=false
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
| `certManager.installHook.enabled` | `true` | Run a pre-install hook Pod that installs cert-manager if it is missing. Disable if you manage cert-manager yourself. |
| `certManager.installHook.image.*` | `dtzar/helm-kubectl:3.16` | Image (with `kubectl` + `helm`) used by the cert-manager install hook Pod. |

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