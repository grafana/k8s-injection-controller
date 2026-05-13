# Demo: Beyla → k8s-injection-controller

This demo wires Beyla to the injection controller via the per-node ConfigMap
contract: Beyla scans local processes, decides which workloads should be
instrumented, and writes a ConfigMap that the controller-manager consumes.

## What gets deployed

- The injection controller (from `config/test`, namespace `beyla-k8s-injector`),
  including its sample SDK config (`config/test/sdk-inject.yaml`).
- Beyla as a DaemonSet (namespace `beyla-k8s-injector`), with its mutating webhook
  delegated to the controller via `injector.webhook.external_deployment_name`.
- A sample Node.js workload (namespace `demo`) that matches Beyla's
  `injector.instrument` criteria.

## Prerequisites

- A running Kubernetes cluster reachable via the current kubecontext.
  `kind`, `minikube`, or any real cluster all work.
- Cluster Kubernetes version **1.31+** (the controller's sample SDK config
  uses `ImageVolumeSource`, which requires 1.31).
- `kubectl` and `make` on your PATH.
- Two container images pushed to a registry your cluster can pull from:
  - the injection controller image (`IMG`)
  - a Beyla image (`BEYLA_IMG`) — use your local dev build to demo the
    in-flight changes

## Deploy

From the repo root (`k8s-injection-controller/`):

```sh
#installs a dev certificate manager
export IMG=beyla-k8s-injector:dev
export BEYLA_IMG=your-repo/beyla:dev
make docker-build docker-push # can replace docker-push by '&& kind load docker-image $IMG'
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
make install
make demo-deploy
```

The target runs three steps:

1. `kustomize build config/test | kubectl apply -f -` — controller +
   sample SDK config in `beyla-k8s-injector`.
2. `sed BEYLA_IMG into examples/demo/beyla.yaml | kubectl apply -f -` —
   Beyla DaemonSet, RBAC, and config in `beyla-k8s-injector`.
3. `kubectl apply -f examples/demo/sample-app.yaml` — the `demo` namespace
   and the `hello-node` Deployment.

If you only want to refresh Beyla after rebuilding its image:

```sh
sed "s|BEYLA_IMAGE_PLACEHOLDER|your-registry/beyla:dev|g" \
  examples/demo/beyla.yaml | kubectl apply -f -
kubectl -n beyla-k8s-injector rollout restart daemonset/beyla
```

## Verify

```sh
# Beyla pods are running, one per node:
kubectl -n beyla-k8s-injector get pods -o wide

# Controller manager is running:
kubectl -n beyla-k8s-injector get pods

# The sample workload:
kubectl -n demo get pods
```

The interesting bit — Beyla's per-node state ConfigMaps:

```sh
kubectl -n beyla-k8s-injector get configmaps -l app.kubernetes.io/managed-by=beyla
kubectl -n beyla-k8s-injector get configmap <name> -o yaml
```

Each ConfigMap carries the `beyla.grafana.com/node` annotation (that's the
watch-predicate the controller filters on) and two data keys:

- `instrumentation.yaml` — the `discovery` criteria and the `otel_export`
  destination (endpoint + protocol from Beyla's `otel_traces_export`).
- `eligible_for_restart.yaml` — workloads Beyla matched on this node. After
  the sample app is up you should see an entry like:

  ```yaml
  - namespace: demo
    kind: Deployment
    name: hello-node
    language: nodejs
  ```

Controller-side reaction:

```sh
kubectl -n beyla-k8s-injector logs deploy/controller-manager -f
```

## Override the OTLP destination

The endpoint and protocol that show up under `otel_export` in the per-node
ConfigMap come from Beyla's standard OTLP traces config. The bundled
`beyla-config.yaml` defaults to
`http://otel-collector.observability.svc.cluster.local:4318`; if you don't
run a collector there, point Beyla elsewhere — either edit the ConfigMap or
set the standard env vars in the DaemonSet:

```yaml
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://my-collector.my-ns.svc.cluster.local:4318
  - name: OTEL_EXPORTER_OTLP_PROTOCOL
    value: http/protobuf
```

## Tear down

```sh
make demo-undeploy
```

This removes the sample workload, the Beyla DaemonSet (and the per-node
ConfigMaps it owns, via the DaemonSet ownerReference), and the controller.

## Troubleshooting

- **`kubectl get configmaps -n beyla-k8s-injector` shows no Beyla-owned ConfigMap.**
  Check `kubectl -n beyla-k8s-injector logs daemonset/beyla` for either
  `disabling injector state ConfigMap writer` (RBAC or env issue —
  `NODE_NAME` must be set) or
  `failed to write injector state ConfigMap` (transient API error).
- **`image volume mounts require Kubernetes 1.31 or later`** in the
  controller logs: your cluster is too old for the sample SDK image volume.
  Upgrade or swap `image_volume_path` for a host-path setup in
  `config/test/sdk-inject.yaml`.
- **Beyla pods CrashLoopBackOff with permission errors.** Re-check the
  `securityContext.capabilities` block — `BPF`, `SYS_PTRACE`, `PERFMON`,
  `SYS_ADMIN` are all required for the local process scanner.
- **Workload not appearing under `eligible_for_restart`.** Confirm it
  actually runs on the same node as a Beyla pod; Beyla only records
  processes it sees locally. Also confirm the workload's namespace matches
  the `injector.instrument` criteria in `beyla-config`.
