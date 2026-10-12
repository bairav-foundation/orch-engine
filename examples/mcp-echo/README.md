# mcp-echo — reference MCP server for BAIRAVS Skill demos

The `bairav/mcp-echo` image is a **real, buildable MCP server** you can deploy as a Skill to prove your BAIRAVS install end-to-end. It's not meant to do anything useful in production — it's meant to run.

Speaks MCP over SSE (Server-Sent Events) per the 2025-06-18 protocol version. One tool called `echo` that returns whatever text you send in. Written in pure Python stdlib — no `mcp` SDK, no pip install, one file (`server.py`).

Why this exists: earlier quickstarts referenced a fictional `mcp/filesystem:v1` image, so following the docs meant substituting `nginx:alpine` and hoping. This image is the substitute that actually speaks MCP.

## Build

```bash
docker build -t bairav/mcp-echo:v1 examples/mcp-echo
```

Image is Alpine + Python 3.12, ~55MB. No external deps.

## Push to the k3d dev registry (if you're using `make dev`)

```bash
docker tag bairav/mcp-echo:v1 k3d-bairavs-registry:5000/bairav/mcp-echo:v1
docker push k3d-bairavs-registry:5000/bairav/mcp-echo:v1
```

## Deploy as a Skill

```bash
bin/bairav skill deploy k3d-bairavs-registry:5000/bairav/mcp-echo:v1 --name echo --target k8s
```

Expected output:

```
Namespace:  default
Name:       echo
Image:      k3d-bairavs-registry:5000/bairav/mcp-echo:v1
Transport:  sse
Port:       3000

✓ Skill default/echo is Ready — endpoint: http://echo.default.svc.cluster.local:3000
```

## Verify the MCP handshake

Port-forward and curl the SSE endpoint — you should see an `endpoint` event within a few hundred milliseconds:

```bash
kubectl port-forward svc/echo 3000:3000
curl -N http://127.0.0.1:3000/sse
```

Output (streams — hit Ctrl-C to stop):

```
event: endpoint
data: /messages?session_id=<hex>
```

Grab that `session_id`, then call `tools/list` in another shell:

```bash
curl -X POST "http://127.0.0.1:3000/messages?session_id=<paste>" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

The reply arrives back on the SSE stream you left running — that's how MCP over SSE works. You should see one tool called `echo`.

## Layout

```
examples/mcp-echo/
├── Dockerfile     # Alpine + Python 3.12, ~55MB, non-root
├── server.py      # ~180 lines, stdlib only
└── README.md      # you are here
```

## What it doesn't do

- No auth. Do NOT run this in a public cluster.
- No stdio transport (v1 BAIRAVS Skill = SSE or HTTP only, per Week 3 scope).
- No keepalive on the SSE stream. Real MCP servers should send `event: ping` every ~15s; this one relies on the caller's request cycle to keep the socket live.
- One tool. That's the whole point.

For a Skill that actually does something (filesystem, search, DB), swap the image ref on `bairav skill deploy` — no other BAIRAVS-side change needed. The substrate is the value; the MCP content is yours.
