# Quickstart — deploy a model with `bairav model deploy`

One command, three targets. Pick the one that matches what you have on your machine.

```
bairav model deploy <hf-repo>                    # → docker compose (default)
bairav model deploy <hf-repo> --target k8s       # → active kubeconfig context
bairav model serve <hf-repo>                     # → bare process on this host
```

Docker and Kubernetes run the same serving image, `ghcr.io/bairav-foundation/serving`, at the version of your CLI. The bare-process target runs `llama-server` directly. All three expose an OpenAI-compatible endpoint and keep downloaded GGUFs in `~/.bairav/models/`, so later runs skip the download.

This guide uses **`Qwen/Qwen2.5-0.5B-Instruct-GGUF`**, a 400 MB model that runs on any laptop.

## Step 0: install the CLI

macOS or Linux, with Docker running:

```bash
curl -fsSL https://bairavs.com/install.sh | sh
bairav version
```

The script downloads the release for your OS and CPU from GitHub, checks its SHA-256 against the release's `checksums.txt`, and installs `bairav`. To check it yourself first, see [Install](https://tech.bairavs.com/orch-engine/#install). Building from source is covered in [CONTRIBUTING.md](../CONTRIBUTING.md).

## Not sure which model? `bairav model discover`

New open-weight models land every week, and most never reach the news. `discover` shows a curated catalog grouped by what each model is good at, with the ones bairav can run today first:

```bash
bairav model discover                    # every group
bairav model discover --for coding       # laptop | general | capable | coding | reasoning | indic | vision | embedding
bairav model discover --json             # for scripts
```

Sample output (your list and times will differ):

```
Open-weight models worth running, updated 3h ago

RUNS ON A LAPTOP  ·  Up to 4B parameters: about 3 GB of RAM at 4-bit. Start here.
  Qwen/Qwen2.5-0.5B-Instruct-GGUF ★   -               -           ✓ Qwen/Qwen2.5-0.5B-Instruct-GGUF
    The quickstart model. About 400 MB, runs anywhere.
  ...
INDIAN LANGUAGES  ·  Models trained for Hindi, Tamil, Telugu and other Indian languages.
  ...
```

The catalog is rebuilt every 12 hours from Hugging Face (first-party releases only: fine-tunes, re-uploads and "uncensored" variants are filtered out) plus an editorial list, [`catalog/curated.yaml`](../catalog/curated.yaml), where we pin models worth knowing (★) and block bad ones. The CLI caches it for 12 hours in `~/.bairav/catalog.json`, uses the cached copy when it can't refresh, and falls back to the live ranking when there's nothing cached. Set `BAIRAV_CATALOG_URL` to use a mirror.

For the live Hugging Face ranking instead:

```bash
bairav model discover --live                   # trending text models
bairav model discover --live --sort new        # newest first
bairav model discover --task embedding         # text | vision | embedding | image | speech
bairav model discover --all                    # raw HF ranking, unfiltered
```

How to read the columns:

- **SIZE** is the parameter count. The `~Q4` figure is roughly the 4-bit download, which is also about the RAM the model needs.
- **The ✓ repo** is the most-downloaded GGUF build bairav can pull. The model's own org wins first, then well-known quantizers.
- **"no GGUF yet"** means only safetensors weights exist so far. Community GGUF builds usually follow within days.

## What's running? `bairav model status`

`bairav model list` is the download cache. `status` shows deployments on every target (local, docker and k8s): the model each one really serves, any upgrade in flight, and AGE.

```
NAME           TARGET  NAMESPACE  SERVING                          PHASE  UPGRADE                    AGE
qwen           docker  -          Qwen/Qwen2.5-0.5B-Instruct-GGUF  Ready  -                          3h
support-model  k8s     team-a     Qwen/Qwen2.5-1.5B-Instruct-GGUF  Ready  from Qwen2.5-0.5B-… 12m ago  2d
```

`bairav model status support-model` shows the details for one deployment:

- serving vs wanted model
- previous model and the exact rollback command
- strategy, endpoint and conditions

To swap models safely, see [Swap a model with bairav model upgrade](./quickstart-model-upgrade.md).

---

## Path 1: Docker Compose (the default, no cluster)

Ports are published on `127.0.0.1` only, so your model isn't reachable from the network you're on. Set `BAIRAV_PUBLISH_ADDR=0.0.0.0` before deploying if you do want LAN access.

Best when you have Docker Desktop / Colima and don't want to bring up Kubernetes.

**Prerequisites:** `docker` (with the v2 `compose` plugin — comes with Docker Desktop).

```bash
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF
bairav ps
```

Sample output:

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
bairav chat --endpoint http://127.0.0.1:8080/v1
# or curl directly
curl http://127.0.0.1:8080/v1/models
```

Tear it down:

```bash
bairav model undeploy qwen2-5-0-5b-instruct-gguf
```

---

## Path 2: Kubernetes (k3d on the laptop, or any real cluster)

Best when you want the operator and the `Model` resource, or to see what a real cluster will run.

**Prerequisites:** `kubectl` pointed at a cluster. For a local one, Docker plus [`k3d`](https://k3d.io): `k3d cluster create bairavs`.

Install the operator and CRDs once, then deploy:

```bash
kubectl apply -f https://github.com/bairav-foundation/orch-engine/releases/latest/download/install.yaml
kubectl -n bairav-system rollout status deploy/bairav-operator
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s
bairav ps
```

Sample `ps` output:

```
KUBERNETES  (context: k3d-bairavs)
  NAMESPACE  NAME                        REPO                             PHASE    ENDPOINT
  default    qwen2-5-0-5b-instruct-gguf  Qwen/Qwen2.5-0.5B-Instruct-GGUF  Loading  http://qwen2-5-0-5b-instruct-gguf.default.svc.cluster.local:8080/v1
```

The phase moves `Pending → Pulling → Loading → Ready` as the operator reconciles the pod. `--wait` (default true) polls until `Ready`; pass `--wait=false` to return immediately.

Talk to it from inside the cluster (the in-cluster DNS name above), or `kubectl port-forward` to reach it from the laptop:

```bash
kubectl port-forward svc/qwen2-5-0-5b-instruct-gguf 8080:8080
bairav chat --endpoint http://127.0.0.1:8080/v1
```

Tear it down:

```bash
bairav model undeploy qwen2-5-0-5b-instruct-gguf --target k8s
# or delete the local cluster entirely:
k3d cluster delete bairavs
```

---

## Path 3: Bare process (`bairav model serve`)

No container and no cluster: the CLI starts `llama-server` directly. Useful on machines without Docker, or when you want to tune `llama-server` flags by hand.

**Prerequisites:** `llama-server` on your PATH (we don't bundle it for this path — the container image is what handles the install for the other two).

```bash
bairav model pull Qwen/Qwen2.5-0.5B-Instruct-GGUF
bairav model serve Qwen/Qwen2.5-0.5B-Instruct-GGUF
bairav ps         # LOCAL section shows it
bairav chat       # auto-detects the running endpoint
```

State lives at `~/.bairav/run/port-<N>.json`; `ps` prunes stale entries on every invocation.

---

## Which target should I use?

| Situation | Target |
|-----------|--------|
| "I just want to talk to an LLM on my laptop" | **docker** (default) |
| "I want the Kubernetes resources, grants and dashboard" | **k8s** |
| "I have no Docker, or I want to tune llama-server myself" | **serve** (bare process) |
| "I'm going to ship this to a real cluster" | **k8s** (same code path as production) |

The docker and k8s targets run the same serving image with the same `BAIRAV_REPO / BAIRAV_PORT / BAIRAV_FILE / BAIRAV_CTX_SIZE` env contract. A model that works on docker will work on k8s and vice versa.

---

## Cross-references

- [`quickstart-k8s.md`](./quickstart-k8s.md) — the Kubernetes path in more detail: what the operator creates, and how to check it.
- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — why the Backend interface exists and how vLLM/Ollama plug in later.
