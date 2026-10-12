# Quickstart — BAIRAVS on local Kubernetes (k3d)

End-to-end: from a fresh `k3d` cluster to a `Model` CR serving an OpenAI-compatible endpoint inside the cluster, in ~5 minutes plus model download time.

## Prerequisites

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

That's the CRDs, RBAC and the operator, with images from `ghcr.io/bairav-foundation`. Then jump to "Verify" below.

## Or build + install from source

From the repo root:

```bash
make image-serving               # builds bairav/serving:laptop, imports into k3d
make install-operator            # builds operator image, applies CRDs/RBAC/Deployment
```

`install-operator` does three things:

1. `make image-operator` — builds the distroless operator image and `k3d image import`s it.
2. `kubectl apply -f operator/config/crd/bases/` — installs the `Model` CRD.
3. `kubectl apply -f operator/config/rbac/` and `operator/config/manager/` — RBAC + the operator Deployment in the `bairav-system` namespace.

Verify:

```bash
kubectl -n bairav-system get pods
# NAME                              READY   STATUS    RESTARTS   AGE
# bairav-operator-xxxxxxxxxx-yyyyy  1/1     Running   0          12s
```

## Deploy your first Model

```bash
kubectl apply -f operator/config/samples/orch_v1alpha1_model.yaml
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
./bin/bairav chat
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

## Iteration loop (developer mode — no operator image rebuild)

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
kubectl -n bairav-system scale deploy bairav-operator --replicas=1
```

## Tearing down

```bash
kubectl delete -f operator/config/samples/orch_v1alpha1_model.yaml
make uninstall-operator
# (Or nuke everything: k3d cluster delete bairavs)
```

## What's NOT in v1alpha1

- **PVC-backed cache** — pods re-pull the GGUF on restart (emptyDir). Coming in v1alpha2.
- **Multi-replica serving** — one replica per Model. HPA support comes with the inference-side concurrency story.
- **Helm chart.** For now `make install-operator` applies the manifests with kubectl.
