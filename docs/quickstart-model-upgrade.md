# Quickstart — swap a model with `bairav model upgrade`

Running Qwen 0.5B and want 1.5B? Or the next Qwen release? `bairav model upgrade` changes the model behind a deployment. The name, endpoint, DLP setup and every Agent wired to it stay the same, so nothing else has to change or restart.

```
bairav model upgrade <name> --to <hf-repo> [--file <gguf>]     # swap
bairav model rollback <name>                                    # go back
```

Both work on `--target docker` (default) and `--target k8s`.

## What "safe" means here

1. **The new model has to prove itself first.** The CLI waits until the new model is serving, then sends it a smoke prompt (`--smoke-prompt`, or skip with `--no-smoke`).
2. **Automatic rollback.** If the rollout stalls, the container goes unhealthy, the smoke prompt fails or `--rollout-timeout` (default 30m) passes, the previous model is restored. Use `--no-rollback` to leave the broken state in place for debugging.
3. **Optional eval gate.** `--eval suite.yaml` runs a [`bairav eval`](quickstart-eval.md) suite against the new model after the smoke prompt. Below the suite's threshold counts as a failure and rolls back.
4. **One-command undo.** The previous model is recorded (an annotation on k8s, `previous-model.json` in the stack dir on docker), so `bairav model rollback <name>` works any time after a successful upgrade.

## k8s: zero downtime

```bash
bin/bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --name support-model --target k8s --namespace team-a
bin/bairav model upgrade support-model --to Qwen/Qwen2.5-1.5B-Instruct-GGUF --target k8s --namespace team-a
```

What you'll see:

```
Model:     team-a/support-model (context kind-bairav)
From:      Qwen/Qwen2.5-0.5B-Instruct-GGUF
To:        Qwen/Qwen2.5-1.5B-Instruct-GGUF
Strategy:  Surge
           (the old model keeps serving until the new one is Ready; both need memory meanwhile)

✓ Spec updated; waiting for the new model to take over ...
  … rolling out Qwen/Qwen2.5-1.5B-Instruct-GGUF; Qwen/Qwen2.5-0.5B-Instruct-GGUF keeps serving until it is Ready
  ✓ serving Qwen/Qwen2.5-1.5B-Instruct-GGUF
Smoke test through the model's Service ...
  ✓ model answered: "OK"

✓ team-a/support-model now serves Qwen/Qwen2.5-1.5B-Instruct-GGUF. Agents were not restarted; their endpoint is unchanged.
```

How it works:

- The Model gets `spec.upgradeStrategy`, which is `Surge` by default. The operator runs the Deployment as a rolling update with `maxUnavailable: 0, maxSurge: 1`. The new pod downloads and loads the model while the old one keeps answering. The Service only switches once the new pod's readiness probe passes.
- `status.servedRepo` / `status.servedFile` say which model is answering right now. They change only when the rollout finishes. During the swap the `Upgrading` condition is `True` with reason `RollingOut`.
- If Kubernetes gives up (`progressDeadlineSeconds: 1800`), the reason becomes `UpgradeStalled`. The CLI sees this and rolls back.
- The smoke prompt goes through the API server's service proxy, so no port-forward is needed. Your kube user needs `services/proxy` in the namespace.

**Memory.** Surge runs both models at once for a few minutes. If the node can't fit both, use `--strategy recreate`: the old pod stops first, so it needs half the memory but there is a short outage. The choice is saved on the Model, so later upgrades reuse it.

```bash
kubectl get model support-model -n team-a -o jsonpath='{.status.servedRepo}{"\n"}'
kubectl get model support-model -n team-a -o jsonpath='{.metadata.annotations}'   # previous-repo / previous-file / upgraded-at
```

## docker: short outage

```bash
bin/bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --name support-model --dlp
bin/bairav model upgrade support-model --to Qwen/Qwen2.5-1.5B-Instruct-GGUF
```

Compose has no rolling update, so the serving container is recreated. The CLI reads the existing stack's compose file, so port, `--dlp`, policy mount, context size and image are all kept. Only the repo and file change. It then waits for the container's `(healthy)` status and smoke-tests the host endpoint. If either fails, the old model comes back up.

## Tips

- **Deploy with a stable `--name`.** The default name comes from the repo, e.g. `qwen2-5-0-5b-instruct-gguf`, which reads oddly after an upgrade to 1.5B. Something like `support-model` ages better.
- **Agents don't care.** They reach the model through `BAIRAV_MODEL_<X>_ENDPOINT`, which points at the Model's Service (k8s) or stack (docker). Neither changes on upgrade.
- **Same model again?** The command says "nothing to do" and exits.
- **Pre-pull to shorten the swap.** On docker, `bairav model pull <repo>` first fills `~/.bairav/models/` so the new container starts loading right away.
