# Quickstart — deploy a model with `bairav model deploy`

One command, three targets. Pick the one that matches what you have on your machine.

```
bairav model deploy <hf-repo>                    # → docker compose (default)
bairav model deploy <hf-repo> --target k8s       # → active kubeconfig context
bairav model serve <hf-repo>                     # → bare process on this host
```

All three run the same serving image (`ghcr.io/bairav-foundation/serving:<version>` with a release binary, or the local `bairav/serving:laptop` when you build from source) (the bare-process target runs `llama-server` directly), expose an OpenAI-compatible endpoint, and persist downloaded GGUFs in `~/.bairav/models/` so subsequent runs skip the pull.

Example we'll use throughout: **`Qwen/Qwen2.5-0.5B-Instruct-GGUF`** (~400 MB, runs on any laptop).

## Not sure which model? `bairav model discover`

New open-weight models land every week, and most never reach the news. `discover` lists the top 10 on Hugging Face right now. It only shows first-party releases: fine-tunes, re-uploads and "uncensored" variants are filtered out, and each model family appears once. Each row says whether bairav can run the model today.

```bash
bin/bairav model discover                      # trending text models
bin/bairav model discover --sort new           # newest first
bin/bairav model discover --task embedding     # text | vision | embedding | image | speech
bin/bairav model discover --all                # raw HF ranking, unfiltered
```

```
#  MODEL                     RELEASED  SIZE                LICENSE     LIKES  RUN WITH BAIRAV
1  poolside/Laguna-S-2.1     82d ago   ?                   openmdw-1.1 689    ✓ unsloth/Laguna-S-2.1-GGUF
2  Aleph-Alpha/Kolibri-1     1d ago    78B (~43 GiB Q4)    apache-2.0  178    no GGUF yet
...
```

How to read the columns:

- **SIZE** is the parameter count. The `~Q4` figure is roughly the 4-bit download, which is also about the RAM the model needs.
- **RUN WITH BAIRAV** shows the most-downloaded GGUF build. The model's own org wins first, then well-known quantizers.
- **"no GGUF yet"** means only safetensors/vLLM weights exist so far. Community GGUF builds usually follow within days.

Coding / reasoning / image specialisations come later as a curated layer.

## What's running? `bairav model status`

`bairav model list` is the download cache. `status` shows deployments on every target (local, docker and k8s): the model each one really serves, any upgrade in flight, and AGE.

```
NAME           TARGET  NAMESPACE  SERVING                          PHASE  UPGRADE                    AGE
qwen           docker  -          Qwen/Qwen2.5-0.5B-Instruct-GGUF  Ready  -                          3h
support-model  k8s     team-a     Qwen/Qwen2.5-1.5B-Instruct-GGUF  Ready  from Qwen2.5-0.5B-… 12m ago  2d
```

`bin/bairav model status support-model` shows the details for one deployment:

- serving vs wanted model
- previous model and the exact rollback command
- strategy, endpoint and conditions

To swap models safely, see [quickstart-model-upgrade.md](./quickstart-model-upgrade.md).

---

## Path 1: Docker Compose (the default, no cluster)

Ports are published on `127.0.0.1` only, so your model isn't reachable from the network you're on. Set `BAIRAV_PUBLISH_ADDR=0.0.0.0` before deploying if you do want LAN access.

Best when you have Docker Desktop / Colima and don't want to bring up Kubernetes.

**Prerequisites:** `docker` (with the v2 `compose` plugin — comes with Docker Desktop).

```bash
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF
bairav ps
```

Built from source? Run `make image-serving` once first, and use `bin/bairav` (the release images are only pulled by release binaries).

What you'll see:

```
Target:    docker (compose)
Name:      qwen2-5-0-5b-instruct-gguf
Repo:      Qwen/Qwen2.5-0.5B-Instruct-GGUF
Endpoint:  http://127.0.0.1:8080/v1
StackDir:  ~/.bairav/compose/qwen2-5-0-5b-instruct-gguf
…
✓ Stack up. First boot pulls the GGUF inside the container — give it a minute.
```

The compose file lands at `~/.bairav/compose/<name>/docker-compose.yaml`. The container bind-mounts `~/.bairav/models/` to `/models`, so once the GGUF is pulled it persists across `down → up`.

Talk to it:

```bash
bin/bairav chat --endpoint http://127.0.0.1:8080/v1
# or curl directly
curl http://127.0.0.1:8080/v1/models
```

Tear it down:

```bash
bin/bairav model undeploy qwen2-5-0-5b-instruct-gguf
```

---

## Path 2: Kubernetes (k3d on the laptop, or any real cluster)

Best when you want to exercise the full operator + Model CR path, or when you're testing what the user's cluster will see.

**Prerequisites:** `docker`, [`k3d`](https://k3d.io), `kubectl`.

One command bootstraps everything:

```bash
make dev                         # = make dev-k8s
```

`make dev` is idempotent and does the full onboarding:

1. Creates the `bairavs` k3d cluster if it doesn't already exist.
2. Builds + imports `bairav/serving:laptop` and `bairav/operator:laptop` into k3d (no registry needed).
3. Installs the `Model` CRD, RBAC, and the operator Deployment.
4. Waits up to 120 s for the operator to report `Available`.
5. Tails the operator logs (Ctrl-C stops the tail; the operator keeps running).

Then, from another terminal:

```bash
bin/bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s
bin/bairav ps
```

What you'll see in `ps`:

```
KUBERNETES  (context: k3d-bairavs)
  NAMESPACE  NAME                        REPO                             PHASE    ENDPOINT
  default    qwen2-5-0-5b-instruct-gguf  Qwen/Qwen2.5-0.5B-Instruct-GGUF  Loading  http://qwen2-5-0-5b-instruct-gguf.default.svc.cluster.local:8080/v1
```

The phase moves `Pending → Pulling → Loading → Ready` as the operator reconciles the pod. `--wait` (default true) polls until `Ready`; pass `--wait=false` to return immediately.

Talk to it from inside the cluster (the in-cluster DNS name above), or `kubectl port-forward` to reach it from the laptop:

```bash
kubectl port-forward svc/qwen2-5-0-5b-instruct-gguf 8080:8080
bin/bairav chat --endpoint http://127.0.0.1:8080/v1
```

Tear it down:

```bash
bin/bairav model undeploy qwen2-5-0-5b-instruct-gguf --target k8s
# or destroy the cluster entirely:
make dev-down
```

---

## Path 3: Bare process (`bairav model serve`)

Best for the fastest iteration loop — no container, no cluster, talk straight to `llama-server`. Useful when you're debugging the CLI or the OpenAI shim itself.

**Prerequisites:** `llama-server` on your PATH (we don't bundle it for this path — the container image is what handles the install for the other two).

```bash
bin/bairav model pull Qwen/Qwen2.5-0.5B-Instruct-GGUF
bin/bairav model serve Qwen/Qwen2.5-0.5B-Instruct-GGUF
bin/bairav ps         # LOCAL section shows it
bin/bairav chat       # auto-detects the running endpoint
```

State lives at `~/.bairav/run/port-<N>.json`; `ps` prunes stale entries on every invocation.

---

## Which target should I use?

| Situation | Target |
|-----------|--------|
| "I just want to talk to an LLM on my laptop" | **docker** (default) |
| "I'm validating the operator / CRD behaviour" | **k8s** |
| "I'm debugging the BAIRAVS CLI itself" | **serve** (bare process) |
| "I'm going to ship this to a real cluster" | **k8s** (same code path as production) |

The docker and k8s targets run the same `bairav/serving:laptop` image with the same `BAIRAV_REPO / BAIRAV_PORT / BAIRAV_FILE / BAIRAV_CTX_SIZE` env contract. A model that works on docker will work on k8s and vice versa.

---

## Cross-references

- [`quickstart-k8s.md`](./quickstart-k8s.md) — deeper dive on the k3d path (cluster internals, operator install steps).
- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — why the Backend interface exists and how vLLM/Ollama plug in later.
