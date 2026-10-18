# Quickstart: the full graph on Docker (no cluster)

You can run Model, Skill and Agent on one laptop with only Docker. You use the same CLI verbs as on K8s. Docker is the default target for all three (pass `--target k8s` for the cluster). Agent code sees the same `BAIRAV_MODEL_*` / `BAIRAV_SKILL_*` env vars on both targets, so an agent image you tested on your laptop runs unchanged on the cluster.

## How it's wired

Every bairav stack joins one shared Docker network, `bairav`. The CLI creates it on first use. The container other stacks talk to gets a stable DNS name on that network:

| Stack | DNS name on `bairav` | Endpoint |
|-------|----------------------|----------|
| Model | `model-<name>` | `http://model-<name>:8080/v1`, or `:8090` when deployed with `--dlp` |
| Skill | `skill-<name>` | `http://skill-<name>:<port>` |
| Agent | `agent-<name>` | `http://agent-<name>:<port>` |

Each of those containers also carries a `bairav.endpoint` label. `agent deploy --target docker` reads the labels from `docker ps` to resolve `--model` / `--skill`, so there's no state file to drift out of sync.

A Model deployed with `--dlp` (enforce mode) only puts its DLP proxy on the shared network, under the `model-<name>` name. The model container itself stays on its own stack's private network. Another container can't bypass the proxy by dialling the model directly.

## Walkthrough

```bash
# 1. Model (add --dlp to make the DLP proxy the only way in)
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target docker --name qwen --dlp

# 2. Skill: the reference MCP server
bairav skill deploy ghcr.io/bairav-foundation/mcp-echo:v0.2.0 --name echo --target docker

# 3. Agent, wired to both, with its own DLP proxy per Model
bairav agent deploy ghcr.io/you/agent:v1 --target docker \
  --model qwen --skill echo \
  --dlp --dlp-pack global-safe,india --dlp-direction both

# 4. See it
bairav ps
# DOCKER  (compose)
#   KIND        NAME   SOURCE               STATUS        ENDPOINT
#   model       qwen   Qwen/Qwen2.5-...     Up (healthy)  —
#   model/dlp   qwen   Qwen/Qwen2.5-...     Up            http://127.0.0.1:8080/v1
#   skill       echo   ghcr.io/bairav-foundation/mcp-echo:v0.2.0   Up            http://127.0.0.1:55012
#   agent       <n>    ghcr.io/you/agent:v1 Up            http://127.0.0.1:55013
#   agent/dlp   <n>    ghcr.io/bairav-foundation/dlp:v0.2.0    Up            —
```

Skills and Agents publish on a host port that Docker picks, so several can run side by side. `bairav ps` shows which port each got. Pass `--host-port N` to pin one.

## DLP on a docker Agent

K8s has a `DLPPolicy` CR; docker has no API server to store one in, so you pass the policy as flags. Setting any `--dlp-*` flag turns on `--dlp`.

| Flag | Default | Meaning |
|------|---------|---------|
| `--dlp` | off | one `dlp-<model>` proxy service per Model inside the agent stack |
| `--dlp-pack` | none (built-ins only) | packs, e.g. `global-safe,india` |
| `--dlp-action` | `redact` | `redact` \| `block` \| `audit` |
| `--dlp-direction` | `both` | `request` \| `response` \| `both` (streaming covered) |
| `--dlp-policy-file` | — | YAML custom rules, mounted read-only |

The agent container gets `BAIRAV_MODEL_<X>_ENDPOINT=http://dlp-<x>:8090/v1`. Audit lines go to `docker logs -f bairav-agent-<name>-dlp-<model>`.

`--dlp-policy <name>` still means a K8s DLPPolicy CR. On docker it errors and points you at the flags above.

## Tear down

```bash
bairav agent undeploy <n> --target docker -f
bairav skill undeploy echo --target docker --cascade-agents -f
bairav model undeploy qwen --target docker --cascade-agents -f
```

As on K8s, undeploying a Model or Skill always lists the Agents that reference it, and only deletes them with `--cascade-agents`. The cascade still works when the Model/Skill is already gone.

## Differences from the K8s target

- **Default target.** `model`, `skill` and `agent` all default to docker and print a one-line banner when `--target` is omitted. Pass `--target k8s` for the cluster. Flags that only make sense on one target (`--namespace`, `--wait`, `--dlp-policy` on k8s; `--host-port`, `--dlp*` on docker) error out on the other one instead of being silently ignored.
- **Refs resolve once, at deploy time.** There's no controller on the laptop, so there's no live `WaitingForDeps`. If a referenced stack isn't up, the deploy fails with the command to fix it. If a Model is removed later, its Agents keep running with a dead endpoint until you redeploy them.
- **Model stacks from older versions** never joined the shared network. `agent deploy` detects them and tells you to redeploy the Model.
- **The agent container is on the shared network**, so it could dial `model-<n>` directly and skip its own agent-side DLP proxy. That's the same trust model as K8s, where an Agent Pod can dial the Model Service directly. If you need a hard guarantee, deploy the Model with `--dlp`.
- **`bairav visualize` is K8s-only for now.**
