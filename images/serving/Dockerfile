# bairav/serving:laptop — minimal LLM serving image used by the K8s operator.
#
# Two stages so the runtime image stays small:
#   1. builder — clone llama.cpp at a pinned tag, compile llama-server
#   2. runtime — debian-slim + curl + the single binary + our entrypoint
#
# Footprint goal: keep the runtime image under 200 MB. We track the size
# in CI (or manually for now: `docker images bairav/serving:laptop`).
#
# Build:
#   docker build -t bairav/serving:laptop -f images/serving/Dockerfile images/serving/
#
# Load into k3d (no registry needed):
#   k3d image import bairav/serving:laptop -c bairavs

# ---------- 1. Builder ----------
FROM debian:bookworm-slim AS builder

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        build-essential cmake git ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Pin a known-good llama.cpp release. Bump intentionally, not on every build.
ARG LLAMA_CPP_TAG=b4421
# GGML_NATIVE=OFF / GGML_AMX=OFF on purpose: this image must run on whatever
# CPU a k8s node happens to have. -march=native (the ggml default) would bake
# the BUILD machine's instruction set into the binary and SIGILL elsewhere, and
# the AMX intrinsic sources don't compile cleanly in this toolchain anyway.
# This trades a little throughput for a portable binary — correct for a shipped
# image. Revisit with per-arch builds if/when we publish optimized variants.
RUN git clone --depth 1 --branch "${LLAMA_CPP_TAG}" \
        https://github.com/ggml-org/llama.cpp.git . && \
    cmake -B build \
        -DCMAKE_BUILD_TYPE=Release \
        -DBUILD_SHARED_LIBS=OFF \
        -DLLAMA_CURL=OFF \
        -DLLAMA_BUILD_TESTS=OFF \
        -DLLAMA_BUILD_EXAMPLES=ON \
        -DLLAMA_BUILD_SERVER=ON \
        -DGGML_NATIVE=OFF \
        -DGGML_AMX=OFF && \
    cmake --build build --config Release --target llama-server -j"$(nproc)" && \
    strip build/bin/llama-server

# ---------- 2. Runtime ----------
FROM debian:bookworm-slim

# curl is needed by entrypoint.sh to pull GGUF files from HF on first boot.
# ca-certificates so HTTPS works.
# libgomp1 is the GCC OpenMP runtime — llama.cpp's CPU backend links against
# libgomp.so.1 for parallel matmul. The builder stage has it via build-essential
# but the runtime base does not; without it llama-server exits at startup with
# "error while loading shared libraries: libgomp.so.1". Could alternatively
# build with -DGGML_OPENMP=OFF, but OpenMP is the easiest path to multi-core
# CPU throughput so we keep it and pay the ~50KB runtime cost.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates curl jq libgomp1 && \
    rm -rf /var/lib/apt/lists/* && \
    useradd -r -u 10001 -g root -d /home/bairav -m bairav

COPY --from=builder /build/build/bin/llama-server /usr/local/bin/llama-server
COPY entrypoint.sh /usr/local/bin/bairav-serve
# Explicit modes, not +x: the build context may carry 0600 files (umask,
# copied trees), and the non-root user must be able to read the script.
RUN chmod 0755 /usr/local/bin/bairav-serve /usr/local/bin/llama-server

# Configurable at runtime via env. Defaults match the operator's expectations.
ENV BAIRAV_PORT=8080 \
    BAIRAV_HOST=0.0.0.0 \
    BAIRAV_MODELS_DIR=/models

EXPOSE 8080
USER bairav
WORKDIR /home/bairav

ENTRYPOINT ["/usr/local/bin/bairav-serve"]
