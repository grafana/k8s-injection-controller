# beyla-k8s-injector

A Kubernetes mutating-admission controller that injects an environment variable
into pods running in namespaces selected by annotated ConfigMaps. Built with
[Kubebuilder](https://book.kubebuilder.io/) but it does **not** define any CRD.

It's meant to be used with Grafana Beyla and/or Grafana Alloy.

## How it works

1. The **ConfigMap controller** watches every `ConfigMap` carrying the
   annotation `beyla.grafana.com/node` (the value is ignored — presence is
   what matters).
2. Each selector ConfigMap carries two files in its `.data`:

   **`instrumentation.yaml`** — a Beyla
   [`InjectConfig`](https://pkg.go.dev/github.com/grafana/beyla/v3/pkg/webhook/configmap)
   document: an `image_version` plus an ordered list of `rules`. Each rule pairs
   a `k8s_selector` with the `config` (env vars) to apply when it matches:

   ```yaml
   image_version: "0.0.11"            # OCI tag appended to the controller's image_volume_root
   rules:
     - k8s_selector:
         namespaces: [my-app]         # globs; empty = all namespaces
         ownerNames: [hello]          # globs matched against the pod's owner chain
         ownerKinds: [Deployment]     # Deployment | ReplicaSet | StatefulSet | DaemonSet | Pod
         podLabels:                   # all entries must match (AND); values are globs
           app: hello
         podAnnotations:
           team: obs
       config:
         env:                         # stamped onto matched containers
           - name: OTEL_EXPORTER_OTLP_ENDPOINT
             value: http://otel-collector:4318
           - name: OTEL_EXPORTER_OTLP_PROTOCOL
             value: http/protobuf
   ```

   The per-rule `config.env` is Beyla's: it derives the OTLP destination,
   exporters, sampler, and propagators from its own configuration and writes
   them here, so a single injector can fan pods out to different collectors.

   Within a `k8s_selector`, all populated fields must match (**AND**) and empty
   fields are wildcards; entries within `namespaces` / `ownerNames` are OR'd.
   Rules are evaluated in order and the **first match wins**. A matched rule
   whose `config.mode` is `skip` excludes the pod from instrumentation (Beyla
   emits its `exclude_instrument` selectors as leading `skip` rules, so
   "instrument everything except X" works). `namespaces` is optional — rules
   without one match cluster-wide in the webhook, but do not trigger a restart
   of pre-existing pods. Multiple ConfigMaps are merged (evaluated in sorted
   key order).

   **`eligible_for_restart.yaml`** — list of restart targets. Each entry:

   ```yaml
   - namespace: foo        # required
     kind: Deployment      # required: Deployment | ReplicaSet | StatefulSet | DaemonSet
     name: frontend        # optional; empty means "any of that kind in the namespace"
     language: nodejs      # parsed but currently unused
   ```

   For each entry, the controller lists pods in `namespace` and evicts those
   whose owner chain matches the kind (and name, if set). `kind: Deployment`
   walks pod → ReplicaSet → Deployment.
3. The **mutating webhook** intercepts pod CREATE requests. If the pod
   matches any criterion, the env var `FOO=bar` is appended to every
   container and initContainer (idempotent — already-set vars are left alone).
4. **Pre-existing pods**: on every reconcile of a selector ConfigMap, the
   controller groups its `eligible_for_restart.yaml` entries by namespace,
   lists pods in each, and evicts pods whose owner chain matches an entry
   AND whose `PodInfo` matches a selection criterion in the registry (no
   point evicting pods the webhook wouldn't inject). PDBs are honored;
   bare pods are skipped. ReplicaSets are kept in the manager cache so the
   pod → RS → Deployment lookup is local.

## Prerequisites

- Go 1.26+
- Docker (or any OCI builder) for image builds
- `kubectl` configured against the target cluster
- *Optional*: [cert-manager](https://cert-manager.io/) installed in the cluster — the
  generated kustomize wires it up to issue the webhook serving certificate

To install a dev certificate manager:
```
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
```

If you haven't installed `cert-manager`, the injection controller can use self-signed
certificates, rotated on each start. For an example deployment in this mode, check out
the `config/deploy` kustomize configuration. The Helm chart has a feature that auto detects
an installed `cert-manager`, and automatically chooses to use the `cert-manager` if it's there
or setup the self-signed certificates.

## Build

```sh
# Compile the manager binary into ./bin/manager
make build

# Run unit tests (downloads envtest binaries on first run)
make test

# Build the container image (override IMG to publish elsewhere)
make docker-build IMG=<registry>/beyla-k8s-injector:<tag>
make docker-push  IMG=<registry>/beyla-k8s-injector:<tag>
```

## Deploy

The default kustomize overlay installs everything into the `beyla-k8s-injector`
namespace and excludes that same namespace from the webhook (so the injector
can't deadlock its own restart).

```sh
# Deploy with the image you just pushed
make deploy IMG=<registry>/beyla-k8s-injector:<tag>

# Tear it down
make undeploy
```

For a quick end-to-end smoke test there's `make deploy-test`, which uses the
overlay in `config/test/`: same as `deploy`, plus a sample SDK config
(`config/test/sdk-inject.yaml`) mounted as a ConfigMap at
`/etc/beyla-injector/sdk-inject.yaml` with `--config` wired in, plus the
example selector ConfigMap from `examples/test_config_map.yaml`. Tear down
with `make undeploy-test`. Production users provide their own SDK config —
`make deploy` stays generic (the webhook still selects pods but doesn't
mutate them when no `--config` is given).

`make deploy` runs `kustomize build config/default | kubectl apply -f -`. If
you prefer to inspect the manifests first:

```sh
make build-installer IMG=<registry>/beyla-k8s-injector:<tag>
# -> dist/install.yaml
```

### Run locally against a remote cluster

For development you can run the manager outside the cluster (the webhook still
needs to be reachable, so this mode skips it):

```sh
ENABLE_WEBHOOKS=false make run
```

## Try it

```sh
# 1. Create a target namespace and a workload
kubectl create namespace my-app
kubectl create deployment hello --image=nginx -n my-app

# 2. Tell the injector to watch my-app
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: beyla-selector
  namespace: default
  annotations:
    beyla.grafana.com/node: ""
data:
  instrumentation.yaml: |
    image_version: "0.0.11"
    rules:
      - k8s_selector:
          namespaces: [my-app]
          ownerNames: [hello]
          ownerKinds: [Deployment]
        config:
          env:
            - name: OTEL_EXPORTER_OTLP_ENDPOINT
              value: http://otel-collector.observability.svc.cluster.local:4318
            - name: OTEL_EXPORTER_OTLP_PROTOCOL
              value: http/protobuf
  eligible_for_restart.yaml: |
    - namespace: my-app
      kind: Deployment
      name: hello
      language: nodejs
EOF

# 3. Pre-existing pods get evicted and recreated; verify the injection
kubectl get pod -n my-app -o jsonpath='{.items[*].spec.containers[*].env}' | jq
```

You should see `{"name":"FOO","value":"bar"}` on each container.

## Project layout

```
cmd/main.go                                  # manager entrypoint
internal/registry/                           # refcounted namespace -> CM keys map
internal/controller/configmap_controller.go  # watches selector ConfigMaps
internal/webhook/v1/pod_webhook.go           # mutating webhook
config/                                      # kustomize manifests
  default/                                   # top-level overlay (namespace + name prefix)
  webhook/namespace_selector_patch.yaml      # excludes the controller's own ns
  rbac/                                      # generated RBAC
  certmanager/                               # cert-manager Issuer + Certificate
```

## Operational notes

- **`failurePolicy: Ignore`** — a broken injector must not block pod creation
  cluster-wide. The cost is that pods may start un-injected if the webhook is
  unavailable. Switch to `Fail` only after pairing it with a tighter
  `namespaceSelector` so the blast radius stays bounded.
- **Single replica** — the registry is in-memory. Running multiple replicas is
  safe (each one builds its own view from the informer cache) but adds no HA
  benefit; leader election is disabled by default.
- **RBAC** — the generated role grants `get/list/watch` on configmaps and pods,
  plus `delete` on pods and `create` on `pods/eviction`.
