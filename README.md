# beyla-k8s-injector

A Kubernetes mutating-admission controller that injects an environment variable
into pods running in namespaces selected by annotated ConfigMaps. Built with
[Kubebuilder](https://book.kubebuilder.io/) but it does **not** define any CRD.

## How it works

1. The **ConfigMap controller** watches every `ConfigMap` carrying the
   annotation `beyla.grafana.com/node` (the value is ignored — presence is
   what matters).
2. Each selector ConfigMap carries two files in its `.data`:

   **`selection_criteria.yaml`** — list of pod selectors. Each entry may set
   any combination of:

   ```yaml
   - k8s_pod_name: my-pod
     k8s_namespace: my-app
     k8s_deployment_name: hello       # walks pod -> ReplicaSet -> Deployment
     k8s_replicaset_name: hello-abc
     k8s_statefulset_name: db
     k8s_daemonset_name: agent
     k8s_owner_name: hello            # any owner kind, including resolved Deployment
   ```

   Within one entry, all populated fields must match (**AND**); empty fields
   are wildcards. Across entries the registry applies **OR**. `k8s_namespace`
   is optional — entries without one match cluster-wide in the webhook, but
   do not trigger eviction of pre-existing pods. Multiple ConfigMaps are
   merged.

   **`eligible_for_restart.yml`** — list of `{image: <ref>, language: <id>}`
   entries (the `language` field is parsed but currently unused). Used as an
   image filter when deciding which pre-existing pods to evict.
3. The **mutating webhook** intercepts pod CREATE requests. If the pod
   matches any criterion, the env var `FOO=bar` is appended to every
   container and initContainer (idempotent — already-set vars are left alone).
4. **Pre-existing pods**: on every reconcile of a selector ConfigMap with
   non-empty criteria, the controller submits an `Eviction` for every pod
   that (a) has an `OwnerReference`, (b) matches a criterion in the
   registry, and (c) runs at least one image listed in
   `eligible_for_restart.yml`. PDBs are honored. Bare pods are skipped.

   Listing scope: if every criterion in the just-updated ConfigMap names a
   literal `k8s_namespace`, only those namespaces are listed; otherwise the
   controller lists pods cluster-wide. The image filter and criterion match
   keep the eviction set bounded, so cluster-wide just means "any pod in
   the cluster that the operator actually selected." ReplicaSets are kept
   in the manager cache so the pod → RS → Deployment lookup is local.

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
  selection_criteria.yaml: |
    - k8s_namespace: my-app
      k8s_deployment_name: hello
  eligible_for_restart.yml: |
    - image: nginx
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
