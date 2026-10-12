#!/usr/bin/env bash
# bairav-serve — container entrypoint for bairav/serving:laptop.
#
# Pulls the GGUF on first boot if missing, then exec's llama-server with the
# right flags. Designed to run inside a K8s Pod created by the BAIRAVS operator.
#
# Required env:
#   BAIRAV_REPO    — Hugging Face repo, e.g. "Qwen/Qwen2.5-0.5B-Instruct-GGUF"
#
# Optional env:
#   BAIRAV_FILE         — file path inside the repo (default: smallest .gguf)
#   BAIRAV_PORT         — bind port (default 8080)
#   BAIRAV_HOST         — bind address (default 0.0.0.0)
#   BAIRAV_CTX_SIZE     — context window in tokens (default: backend default)
#   BAIRAV_MODELS_DIR   — cache dir (default /models)
#   HF_TOKEN            — HF access token for gated/private repos

set -euo pipefail

: "${BAIRAV_REPO:?BAIRAV_REPO env var required}"
: "${BAIRAV_PORT:=8080}"
: "${BAIRAV_HOST:=0.0.0.0}"
: "${BAIRAV_MODELS_DIR:=/models}"

mkdir -p "${BAIRAV_MODELS_DIR}"

curl_args=(--silent --fail --location --retry 3 --retry-delay 2)
if [[ -n "${HF_TOKEN:-}" ]]; then
    curl_args+=(-H "Authorization: Bearer ${HF_TOKEN}")
fi

# If BAIRAV_FILE is empty, ask HF for the smallest .gguf in the repo.
if [[ -z "${BAIRAV_FILE:-}" ]]; then
    echo "[bairav-serve] selecting smallest .gguf from ${BAIRAV_REPO} ..."
    tree_url="https://huggingface.co/api/models/${BAIRAV_REPO}/tree/main?recursive=true"
    BAIRAV_FILE=$(curl "${curl_args[@]}" "${tree_url}" \
        | jq -r '[.[] | select(.type=="file" and (.path | endswith(".gguf")))]
                 | sort_by(.size) | first | .path // empty')
    if [[ -z "${BAIRAV_FILE}" ]]; then
        echo "[bairav-serve] ERROR: no .gguf files found in ${BAIRAV_REPO}" >&2
        exit 1
    fi
    echo "[bairav-serve] selected: ${BAIRAV_FILE}"
fi

local_path="${BAIRAV_MODELS_DIR}/${BAIRAV_FILE}"
mkdir -p "$(dirname "${local_path}")"

if [[ ! -f "${local_path}" ]]; then
    echo "[bairav-serve] pulling ${BAIRAV_REPO}/${BAIRAV_FILE} ..."
    blob_url="https://huggingface.co/${BAIRAV_REPO}/resolve/main/${BAIRAV_FILE}"
    curl "${curl_args[@]}" --output "${local_path}.partial" "${blob_url}"
    mv "${local_path}.partial" "${local_path}"
    echo "[bairav-serve] cached at ${local_path}"
fi

extra=()
if [[ -n "${BAIRAV_CTX_SIZE:-}" && "${BAIRAV_CTX_SIZE}" != "0" ]]; then
    extra+=(-c "${BAIRAV_CTX_SIZE}")
fi

echo "[bairav-serve] launching llama-server on ${BAIRAV_HOST}:${BAIRAV_PORT} ..."
exec llama-server \
    -m "${local_path}" \
    --host "${BAIRAV_HOST}" \
    --port "${BAIRAV_PORT}" \
    --alias "${BAIRAV_REPO}" \
    "${extra[@]}"
