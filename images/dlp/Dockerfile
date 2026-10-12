# bairav/dlp:laptop — the BAIRAVS DLP proxy packaged as a container.
#
# Same binary as the host CLI; runs `bairav dlp serve` as PID 1. All flag
# defaults fall back to BAIRAV_DLP_* env vars (see dlp.go), so this image
# takes zero args in normal use — the compose stack sets the env vars
# via renderDLPService (compose/template.go).
#
# Build context is the orch-engine root (not images/dlp/) because we
# need cli/, operator/, and go.work all accessible to the Go compiler.
# See the `image-dlp` Makefile target.

# ---------- 1. Builder ----------
# Builder tracks a supported Go release (stdlib security fixes, net/http
# in particular); go.mod stays at 1.22 so older local toolchains still work.
FROM golang:1.26 AS builder

WORKDIR /workspace

# Multi-module workspace: copy the workspace file, go.sum, and both
# modules. Mirrors the operator/Dockerfile pattern so this stays self-
# contained and doesn't need external artefacts.
COPY go.work go.work.sum ./
COPY cli/ cli/
COPY operator/ operator/

WORKDIR /workspace/cli
# VERSION is stamped into the binary so `docker logs <dlp container>` shows
# which build is running — stale-image bugs become diagnosable at a glance.
# `make image-dlp` passes the same VERSION the CLI gets.
ARG VERSION=v0.0.0-dev
# CGO off so the binary is fully static and runs on the distroless base.
# -trimpath + -ldflags stripping cut ~2MB and remove build-machine paths.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" -o /workspace/bairav ./

# ---------- 2. Runtime ----------
FROM gcr.io/distroless/static:nonroot

ARG VERSION=v0.0.0-dev
LABEL org.opencontainers.image.version="${VERSION}"

WORKDIR /

COPY --from=builder /workspace/bairav /usr/local/bin/bairav

USER nonroot:nonroot

# The CLI defaults to loopback-only (127.0.0.1:8090) so a laptop gateway
# isn't reachable from the LAN; in a container the proxy must listen on all
# interfaces of its own network namespace.
ENV BAIRAV_DLP_LISTEN=:8090
EXPOSE 8090

# Distroless has no shell — we call the binary directly. All flags fall
# back to BAIRAV_DLP_* env vars (see runDLPServe in cli/cmd/dlp.go), so
# the compose stack configures behaviour purely through environment.
ENTRYPOINT ["/usr/local/bin/bairav", "dlp", "serve"]
