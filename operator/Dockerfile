# bairav/operator:laptop — controller-runtime manager image.
#
# Runs the Model reconciler (Skill + DLPPolicy reconcilers join in Weeks 2-3).
# Distroless static base — small, no shell, no package manager.

# Builder tracks a supported Go release (stdlib security fixes, net/http
# in particular); go.mod stays at 1.22 so older local toolchains still work.
FROM golang:1.27@sha256:ec47ea3725fea0cc88f6de94919aef19f40f12fc1ec0e2d1b3426dbf15ecca04 AS builder

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

FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
WORKDIR /
COPY --from=builder /workspace/bairav-operator /bairav-operator
USER 65532:65532

ENTRYPOINT ["/bairav-operator"]
