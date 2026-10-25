# Quickstart — BAIRAVS on local Kubernetes (k3d)

End-to-end: from a fresh `k3d` cluster to a `Model` CR serving an OpenAI-compatible endpoint inside the cluster, in ~5 minutes plus model download time.

## Prerequisites

Kubernetes **1.28 or later** (the CRDs use validation rules that older API servers reject with `unknown field "x-kubernetes-validations"`). Any recent k3d, kind, EKS, GKE or AKS cluster qualifies.

```bash
brew install docker k3d kubectl
```

If you already have a k3d cluster, skip the create step. Otherwise:

```bash
k3d cluster create bairavs
kubectl get nodes               # should show k3d-bairavs-server-0 Ready
```

## Install a release

```bash
kubectl apply -f https://github.com/bairav-foundation/orch-engine/releases/latest/download/install.yaml
kubectl -n bairav-system rollout status deploy/bairav-operator
```

That's the CRDs, RBAC and the operator in the `bairav-system` namespace, with images from `ghcr.io/bairav-foundation`. You also need the CLI: `curl -fsSL https://bairavs.com/install.sh | sh`.

Verify:

```bash
kubectl -n bairav-system get pods
# NAME                              READY   STATUS    RESTARTS   AGE
# bairav-operator-7d9c8b5f4-2xkqp   1/1     Running   0          12s
# bairav-operator-7d9c8b5f4-9mzlt   1/1     Running   0          12s
```

Two replicas run: one is the leader and does the work, the other takes over within seconds if the leader's Pod or node goes away (`kubectl -n bairav-system get lease bairav-operator-leader` shows who leads). See the [production guide](./production.md) for sizing and hardening.

## Deploy your first Model

```bash
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s --name qwen-tiny
```

Or with plain kubectl:

```bash
kubectl apply -f - <<'YAML'
apiVersion: orch.bairavs.com/v1alpha1
kind: Model
metadata:
  name: qwen-tiny
  namespace: default
spec:
  repo: Qwen/Qwen2.5-0.5B-Instruct-GGUF
  resources:
    requests: { cpu: "500m", memory: "1Gi" }
    limits: { memory: "2Gi" }
YAML
kubectl get models -w
# NAME        PHASE     REPO                                ENDPOINT
# qwen-tiny   Pending   Qwen/Qwen2.5-0.5B-Instruct-GGUF
# qwen-tiny   Loading   Qwen/Qwen2.5-0.5B-Instruct-GGUF
# qwen-tiny   Ready     Qwen/Qwen2.5-0.5B-Instruct-GGUF     http://qwen-tiny.default.svc.cluster.local:8080/v1
```

Loading takes ~30–60s for the 0.5B model: the pod boots, the entrypoint pulls the GGUF (~400 MB), llama-server loads it, the readiness probe goes green. Bigger models take proportionally longer.

## Hit the endpoint

From inside the cluster:

```bash
kubectl run curl --rm -it --image=curlimages/curl --restart=Never -- \
    curl -s http://qwen-tiny.default.svc.cluster.local:8080/v1/chat/completions \
        -H 'Content-Type: application/json' \
        -d '{"model":"qwen","messages":[{"role":"user","content":"hi"}]}'
```

From your laptop (port-forward):

```bash
kubectl port-forward svc/qwen-tiny 8080:8080
# In another terminal:
bairav chat
```

(`bairav chat` defaults to `http://localhost:8080/v1` — the port-forward routes that into the cluster.)

## Inspecting reconciler activity

```bash
kubectl describe model qwen-tiny
# Look at Status.Conditions for the reconciler's view
# Look at Events at the bottom for what happened when

kubectl -n bairav-system logs -f deploy/bairav-operator
# Real-time reconciler logs
```

## Tearing down

```bash
bairav model undeploy qwen-tiny --target k8s     # or: kubectl delete model qwen-tiny
kubectl delete -f https://github.com/bairav-foundation/orch-engine/releases/latest/download/install.yaml
# or delete the whole local cluster: k3d cluster delete bairavs
```

## Not in this release yet

- **PVC-backed cache** — pods re-download the GGUF on restart (emptyDir). Planned.
- **Multi-replica serving** — one replica per Model. Autoscaling is planned.
- **Helm chart.** For now `install.yaml` is plain manifests for `kubectl apply`.

## For contributors: build from source

From the repo root:

```bash
make image-serving               # builds bairav/serving:laptop, imports into k3d
make install-operator            # builds operator image, applies CRDs/RBAC/Deployment
```

`install-operator` does three things:

1. `make image-operator` — builds the distroless operator image and `k3d image import`s it.
2. `kubectl apply -f operator/config/crd/bases/` — installs the `Model` CRD.
3. `kubectl apply -f operator/config/rbac/` and `operator/config/manager/` — RBAC + the operator Deployment in the `bairav-system` namespace.

### Iteration loop (no operator image rebuild)

If you're changing the operator and want to skip the docker-build-import-deploy loop:

```bash
# In the operator's namespace, scale the in-cluster operator down so two
# managers don't fight over the same CRs.
kubectl -n bairav-system scale deploy bairav-operator --replicas=0

# Run the operator on your laptop, against the k3d cluster:
make build-operator
./bin/bairav-operator --leader-elect=false
```

When done:

```bash
kubectl -n bairav-system scale deploy bairav-operator --replicas=2
```
