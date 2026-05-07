# beyla-k8s-injector

A Kubernetes mutating-admission controller that injects an environment variable
into pods running in namespaces selected by annotated ConfigMaps. Built with
[Kubebuilder](https://book.kubebuilder.io/) but it does **not** define any CRD.

## How it works

1. The **ConfigMap controller** watches every `ConfigMap` carrying the
   annotation `beyla.grafana.com/node` (the value is ignored — presence is
   what matters).
2. Each selector ConfigMap holds a YAML payload in its `.data` values. Either
   form is accepted:
   ```yaml
   k8s_namespace: my-app
   ```
   or
   ```yaml
   k8s_namespaces:
     - my-app
     - other-app
   ```
   Multiple ConfigMaps targeting the same namespace are merged (refcounted), so
   removing one ConfigMap will not "unwatch" a namespace another still selects.
3. The **mutating webhook** intercepts pod CREATE requests. If the pod's
   namespace appears in the in-memory registry, the env var `FOO=bar` is
   appended to every container and initContainer (idempotent — already-set vars
   are left alone).
4. **Pre-existing pods**: when a namespace becomes newly watched, the controller
   submits an `Eviction` for every pod in that namespace that has an
   `OwnerReference` (so a Deployment/StatefulSet/DaemonSet/etc. will recreate
   it through the webhook). PDBs are honored. Bare pods (no owner) are logged
   and skipped — deleting them would lose the workload with no controller to
   bring them back.

## Prerequisites

- Go 1.26+
- Docker (or any OCI builder) for image builds
- `kubectl` configured against the target cluster
- [cert-manager](https://cert-manager.io/) installed in the cluster — the
  generated kustomize wires it up to issue the webhook serving certificate

To install a dev certificate manager:
```
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
```

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
  config.yaml: |
    k8s_namespace: my-app
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
