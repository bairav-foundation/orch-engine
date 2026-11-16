# bairav/dlp-ocr:laptop — the OCR server for DLP `attachments: scan`.
#
# Runs `bairav dlp ocr` (same binary as the CLI) with Tesseract. The DLP
# proxy next to it posts images over loopback and scans the text; images
# and text stay in the Pod and are never logged. Kept out of the DLP image
# so that one stays a small static distroless binary.
#
# Build context is the orch-engine root, like images/dlp:
#   docker build -t bairav/dlp-ocr:laptop -f images/dlp-ocr/Dockerfile .

# ---------- 1. Builder ----------
FROM golang:1.27@sha256:ec47ea3725fea0cc88f6de94919aef19f40f12fc1ec0e2d1b3426dbf15ecca04 AS builder
WORKDIR /workspace
COPY go.work go.work.sum ./
COPY cli/ cli/
COPY operator/ operator/
WORKDIR /workspace/cli
ARG VERSION=v0.0.0-dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" -o /workspace/bairav ./

# ---------- 2. Runtime ----------
# Debian's Tesseract 5 with the English model. Other languages: build with
# --build-arg LANGS="eng hin tam" and set BAIRAV_OCR_LANGS=eng+hin+tam.
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
ARG VERSION=v0.0.0-dev
ARG LANGS="eng"
LABEL org.opencontainers.image.version="${VERSION}"
RUN apt-get update \
 && apt-get install -y --no-install-recommends tesseract-ocr $(for l in ${LANGS}; do echo tesseract-ocr-$l; done) \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -u 10003 -r -s /usr/sbin/nologin ocr
COPY --from=builder /workspace/bairav /usr/local/bin/bairav
USER 10003:10003
# Loopback only by default; on Kubernetes the proxy is in the same Pod.
ENV BAIRAV_OCR_LISTEN=127.0.0.1:8095
EXPOSE 8095
ENTRYPOINT ["/usr/local/bin/bairav", "dlp", "ocr"]
