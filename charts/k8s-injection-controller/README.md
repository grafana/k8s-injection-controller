# k8s-injection-controller

Helm chart for the **k8s-injection-controller**, a Kubernetes mutating-admission
controller that injects Grafana Beyla / OpenTelemetry SDK auto-instrumentation
into application pods selected by Beyla-managed ConfigMaps.

The chart installs the controller and everything it needs: ServiceAccount, RBAC
(cluster + namespaced), the pod mutating webhook and ConfigMap validating
webhook, their Service, the webhook serving certificate (self-signed by default,
or via cert-manager), and (optionally) the default SDK injection config. It does
**not** install Beyla or any demo application.

## Prerequisites

- Kubernetes **1.35+** (required for `image` injection mode via
  `ImageVolumeSource`; older clusters work with `sdkConfig.injectionMode:
  init_container`).
- **No cert-manager required by default.** `webhook.certManager.mode` defaults to
  `auto`: the chart uses cert-manager if the `cert-manager.io/v1` API is present,
  otherwise the controller provisions and rotates its own self-signed serving
  cert in-process. See [Webhook certificate](#webhook-certificate-provisioning)
  below to force a strategy.

## Install

```bash
# auto: cert-manager if present on the cluster, otherwise self-signed.
helm install kic ./k8s-injection-controller
```

## Webhook certificate provisioning

`webhook.certManager.mode` selects how the webhook's TLS serving cert is
provisioned:

| `mode` | Behavior |
|--------|----------|
| `auto` (default) | cert-manager if the `cert-manager.io/v1` API is present, otherwise the in-process self-signed rotator. Never installs cert-manager. |
| `self-signed` | Always the in-process self-signed rotator. cert-manager is ignored even if present. No prerequisites. |
| `cert-manager` | Always cert-manager. If the API is **absent**, the chart installs cert-manager on the fly via the pre-install hook below (instead of failing). |

### cert-manager pre-install hook

The hook runs **only when it is needed**: `webhook.certManager.mode=cert-manager`
is forced *and* the `cert-manager.io/v1` API is absent. In `auto` mode a missing
cert-manager falls back to self-signed and the hook never runs; to install
cert-manager on the fly, set the mode to `cert-manager`. When triggered, a
short-lived `pre-install` hook Pod runs
[`hooks/install-cert-manager.sh`](./hooks/install-cert-manager.sh) before the rest
of the chart is applied. It:

1. checks whether cert-manager is already present
   (`kubectl get crd certificates.cert-manager.io`) and does nothing if so;
2. otherwise `helm install`s cert-manager into the `cert-manager` namespace
   (`--set crds.enabled=true`) and waits for its webhook to become Ready.

The hook Pod runs from an image containing both `kubectl` and `helm`
(`ghcr.io/grafana/k8s-injection-controller-helm-kubectl`, built from
[`images/helm-kubectl`](../../images/helm-kubectl/Dockerfile), by default) under a **temporary** ServiceAccount +
ClusterRole/ClusterRoleBinding. Installing cert-manager touches cluster-scoped
objects (CRDs, ClusterRoles, webhook configs, a Namespace), so that role is
cluster-admin–equivalent — but it lives only for the duration of the hook and
is deleted as soon as the hook succeeds (`helm.sh/hook-delete-policy`).

### How the cert-manager serving certificate is provisioned (hook path)

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

If you prefer to manage cert-manager out of band, install it once per cluster
beforehand. The install hook only fires when cert-manager is *absent*, so with
cert-manager already present it never runs (in either `auto` or `cert-manager`
mode):

```bash
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true
kubectl -n cert-manager rollout status deployment/cert-manager-webhook

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
| `webhook.port` | `9443` | Port the webhook server listens on; the webhook Service maps 443 to it. Non-default values need controller image 0.4.0 or newer; the chart refuses to render otherwise unless `image.tag` is set. |
| `hostNetwork` | `false` | Run the controller on the node network so the API server can reach the webhooks on clusters with a non-VPC CNI (see below). |
| `dnsPolicy` | `""` | Pod DNS policy; empty picks `ClusterFirstWithHostNet` when `hostNetwork` is true. |
| `strategy` | `{}` | Deployment update strategy; empty means RollingUpdate, or Recreate when `hostNetwork` is true. |
| `metrics.enabled` / `metrics.port` | `true` / `8080` | Plain-HTTP Prometheus metrics, advertised via pod annotations (no Prometheus Operator required). |
| `certManager.installHook.image.*` | `ghcr.io/grafana/k8s-injection-controller-helm-kubectl:<appVersion>` | Image (with `kubectl` + `helm`) used by the cert-manager install hook Pod (runs only when `webhook.certManager.mode=cert-manager` and cert-manager is absent). |

## Clusters where the API server cannot reach pod IPs

On managed clusters whose control plane runs outside the pod network, the API
server dials admission webhooks through the webhook Service and needs a route
to the backing pod IP. With the default CNI that works. With an overlay CNI it
does not: on EKS with Cilium in tunnel mode (or Calico overlay) every call to
the webhooks fails with `Address is not allowed`, and because the ConfigMap
validating webhook is fail-closed, every ConfigMap create or update in the
watched namespace is rejected, including Helm upgrades of other releases there.

Run the controller on the node network instead:

```bash
helm upgrade --install kic ./k8s-injection-controller \
  --set hostNetwork=true
```

The chart then sets `dnsPolicy: ClusterFirstWithHostNet` so Service names keep
resolving, and the webhook Service forwards 443 to `<node IP>:9443`. The
serving certificate is still verified by the API server against the webhook
`caBundle`, so another process on the node cannot impersonate the webhook; it
can only take the port. That is enough for an outage: a process that binds the
port first keeps the controller from starting, and the fail-closed ConfigMap
webhook then rejects the writes it covers until the port is free again. Before
enabling it:

- Allow TCP 9443 from the control plane to the nodes if your node security
  group is restricted. Pick another `webhook.port` if 9443 is already taken on
  the nodes, for example by another host-network webhook.
- Set a hostname `podAntiAffinity` when `replicaCount` is greater than one;
  two controller pods on one node collide on the port.
- Rollouts use `Recreate` unless `strategy` is set: a surging replacement pod
  needs another node with the port free, so a rolling update stalls when only
  one eligible node exists. The webhooks are down for the restart; pods
  admitted then start without injection and ConfigMap writes covered by the
  validating webhook are rejected until the controller is back.
- Allow the pod in the namespace's Pod Security or policy-engine rules;
  `hostNetwork` is rejected by the Pod Security "baseline" profile and by the
  usual Gatekeeper and Kyverno baseline policies.
- The health endpoint on 8081 and the metrics endpoint on `metrics.port` are
  plain HTTP without authentication. On the host network they are reachable
  from anything that reaches the node IP, and standard NetworkPolicy does not
  cover host-network pods on most CNIs, so restrict them with security groups
  or disable `metrics.enabled`.
- The pod reaches the cloud metadata service the same way the node does, so a
  compromised controller could read the node's instance credentials; the IMDS
  hop limit that keeps pod-network pods out does not help here. Keep IMDSv2
  required and the node role minimal. The controller needs no cloud credentials.

Where the CNI can be switched to a mode with routable pod IPs, such as Cilium
ENI mode on EKS, prefer that over `hostNetwork`.

## Uninstall

```bash
helm uninstall kic
```

cert-manager (a shared cluster prerequisite) is left untouched; remove it
separately if nothing else uses it.

## Local development mode deployment with kind

1. Build the controller image as `beyla-k8s-injector:dev`
```sh
IMG=beyla-k8s-injector:dev mise run docker-build
```
2. Create your cluster (if you don't already have one)
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

## Disable automatic workload restarts

Set `sdkConfig.disableAutoRestart: true` to prevent controller-triggered
workload rollouts. Newly created pods are still injected. This renders
`disable_auto_restart: true` into the controller's SDK configuration; it is
not a Beyla setting. Restart the controller after changing its mounted SDK
configuration so the process loads the new value.

See [Node state and SDK upgrades](../../../README.md#node-state-and-sdk-upgrades)
for registry migration and upgrade precautions.
