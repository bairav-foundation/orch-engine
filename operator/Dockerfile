# bairav/operator:laptop — controller-runtime manager image.
#
# Runs the Model reconciler (Skill + DLPPolicy reconcilers join in Weeks 2-3).
# Distroless static base — small, no shell, no package manager.

# Builder tracks a supported Go release (stdlib security fixes, net/http
# in particular); go.mod stays at 1.22 so older local toolchains still work.
FROM golang:1.27@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS builder

WORKDIR /workspace

# Copy go.mod + go.sum first so the module cache is reused across rebuilds.
# go.sum is required: builds run -mod=readonly, so a missing go.sum makes
# `go build` fail with "missing go.sum entry" before it compiles anything.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CGO_ENABLED=0 so the static binary runs on the distroless base.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o bairav-operator ./cmd

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
WORKDIR /
COPY --from=builder /workspace/bairav-operator /bairav-operator
USER 65532:65532

ENTRYPOINT ["/bairav-operator"]
