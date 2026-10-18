# Quickstart: sharing Models and Skills across namespaces

An Agent in one namespace can use a Model or Skill owned by another, but only after the owner says yes. The owner-side yes is an **AccessGrant**, which works like Gateway API's `ReferenceGrant` (or VPC peering if you think in networks).

K8s target only; docker has no namespaces.

## The flow

```bash
# team-a owns a Model
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s --name qwen --namespace team-a

# team-b references it as <namespace>/<name>
bairav agent deploy ghcr.io/team-b/bot:v1 --target k8s --namespace team-b --model team-a/qwen
#   phase=WaitingForDeps
#   no AccessGrant in team-a lets team-b use Model qwen
#   (owner runs: bairav grant team-b --model qwen -n team-a)

# team-a (or a platform admin) approves
bairav grant team-b --model qwen -n team-a
# ✓ team-a/allow-team-b: team-b may now use Model/qwen
# team-b's Agent picks it up within seconds and rolls; no redeploy needed.
```

The agent container sees the same env var as for a local Model: `BAIRAV_MODEL_QWEN_ENDPOINT=http://qwen.team-a.svc.cluster.local:8080/v1`. Status shows the ref as `team-a/qwen`, and `bairav visualize` draws the edge across namespace boxes.

## Managing grants

```bash
bairav grant team-b --model qwen --skill echo -n team-a   # extends allow-team-b
bairav grant team-c --all-models -n shared-models         # every Model in the namespace
bairav grants -A                                          # who can use what
bairav revoke team-b --model qwen -n team-a               # drop one entry
bairav revoke team-b -n team-a                            # drop the whole grant
```

On revoke, the consumer's Agent moves to `WaitingForDeps` with a message naming the missing grant. Its Pod keeps running, the same as when a dependency is deleted.

The grant object, if you'd rather apply YAML:

```yaml
apiVersion: orch.bairavs.com/v1alpha1
kind: AccessGrant
metadata: { namespace: team-a, name: allow-team-b }
spec:
  from: [{ namespace: team-b }]
  to:
    - { kind: Model, name: qwen }
    - { kind: Skill }          # no name = every Skill in team-a
```

## Who decides: owner or admin?

Your cluster's RBAC decides, not BAIRAVS. To let team leads approve their own resources, give them `create/update/delete` on `accessgrants` in their namespace. To keep it admin-only, don't. Either way, the operator itself only reads grants.

## Rules worth knowing

- **Same-namespace refs never need a grant.** Every existing Agent behaves exactly as before.
- **An ungranted Agent learns nothing about the target**, not even whether it exists. The grant is checked before the Model is read.
- **Ref names must stay distinct per Agent.** `team-a/qwen` and `qwen` would both become `BAIRAV_MODEL_QWEN_*`, so the Agent is held in `WaitingForDeps` with a message saying so.
- **`dlpRef` stays same-namespace.** The DLP sidecar mounts the policy's ConfigMap, and a Pod can't mount one from another namespace. Copy the policy into your namespace instead.
- **`--cascade-agents` reaches other namespaces.** Undeploying team-a's Model with it lists and deletes dependent Agents in team-b too, provided your kubeconfig user is allowed to delete them there.
- **Not yet: rate limits.** A granted team can still overload a shared Model. Per-consumer limits on the grant (enforced by the DLP proxy) are planned.
