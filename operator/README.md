# `operator/` — BAIRAVS Kubernetes operator

A controller-runtime-based operator for the `orch.bairavs.com/v1alpha1` API group. v1alpha1 ships the **Model** kind; **Skill** and **DLPPolicy** join in Weeks 2-3.

## What it does today

Reconciles each `Model` CR into:

- a `Deployment` running `bairav/serving:laptop` (which wraps `llama-server`)
- a `Service` exposing the OpenAI-compatible HTTP endpoint
- a status update with a `phase` (Pending → Loading → Ready) and the reachable in-cluster `endpoint`

Pod restarts re-pull the GGUF (emptyDir for v1alpha1; PVC support is the next iteration).

## Layout (kubebuilder-style)

```
operator/
├── cmd/main.go                                    # manager entrypoint
├── api/v1alpha1/                                  # CR Go types (+ DeepCopy)
├── internal/controller/model_controller.go        # reconcile loop
├── config/
│   ├── crd/bases/orch.bairavs.com_models.yaml     # CRD schema
│   ├── rbac/rbac.yaml                             # ClusterRole + Binding + SA
│   ├── manager/manager.yaml                       # operator Deployment + Namespace
│   └── samples/orch_v1alpha1_model.yaml           # try-me CR
└── Dockerfile                                     # distroless static
```

## Local install on k3d

From the orch-engine root:

```bash
# One-time prereqs (only k3d if not installed)
brew install k3d
k3d cluster create bairavs        # if not already created

# Build images + load into k3d, install CRDs + RBAC + operator Deployment
make image-serving
make install-operator             # builds image-operator, applies CRDs/RBAC/Deployment

# Apply the sample Model and watch it come up
kubectl apply -f operator/config/samples/orch_v1alpha1_model.yaml
kubectl get models -w
# Phase should walk Pending → Loading → Ready as the GGUF pulls + loads.

# Hit the endpoint from inside the cluster
kubectl run curl --rm -it --image=curlimages/curl --restart=Never -- \
    curl -s http://qwen-tiny.default.svc.cluster.local:8080/v1/chat/completions \
        -H 'Content-Type: application/json' \
        -d '{"model":"qwen","messages":[{"role":"user","content":"hello"}]}'
```

## Running the operator outside the cluster (dev mode)

For tight iteration, run the binary against the cluster with your local kubeconfig:

```bash
make install-crds                 # CRDs only
make build-operator
./bin/bairav-operator --leader-elect=false

# In another terminal:
kubectl apply -f operator/config/samples/orch_v1alpha1_model.yaml
```

The operator uses your `~/.kube/config` to talk to k3d while the binary runs on your laptop. Faster than rebuilding the operator image every change.

## Versioning

CRDs ship under `v1alpha1` until we cut three releases without breaking changes, then promote to `v1beta1` / `v1`. See `docs/ARCHITECTURE.md` for the broader stance.

## Scope cuts (v1alpha1, intentional)

- **emptyDir, not PVC** — re-pulls on pod restart. Add PVC support in v1alpha2.
- **Single replica per Model** — no HPA, no multi-replica load balancing yet.
- **No webhooks** — validation is purely declarative in the CRD schema.
- **No leader election by default** — single-replica operator on a laptop k3d.
