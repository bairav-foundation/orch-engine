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
#   BAIRAV_REVISION     — branch, tag or commit to pull from (default: main)
#   BAIRAV_SHA256       — expected SHA-256 of the file; a download or cached
#                         copy that doesn't match is deleted and the Pod stops
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
: "${BAIRAV_REVISION:=main}"

mkdir -p "${BAIRAV_MODELS_DIR}"

curl_args=(--silent --fail --location --retry 3 --retry-delay 2)
if [[ -n "${HF_TOKEN:-}" ]]; then
    curl_args+=(-H "Authorization: Bearer ${HF_TOKEN}")
fi

# If BAIRAV_FILE is empty, ask HF for the smallest .gguf in the repo.
if [[ -z "${BAIRAV_FILE:-}" ]]; then
    echo "[bairav-serve] selecting smallest .gguf from ${BAIRAV_REPO} ..."
    tree_url="https://huggingface.co/api/models/${BAIRAV_REPO}/tree/${BAIRAV_REVISION}?recursive=true"
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

# matches <path>: true when BAIRAV_SHA256 is unset or the file has that
# hash. A marker next to the file records a verified hash, so a multi-GB
# file isn't re-hashed on every restart; a changed file (different size or
# mtime) doesn't match its marker and is hashed again.
matches() {
    local f="$1" want="${BAIRAV_SHA256:-}"
    [[ -z "${want}" ]] && return 0
    local marker="${f}.sha256-ok" stamp got
    stamp="$(stat -c '%s-%Y' "${f}")"
    if [[ -f "${marker}" && "$(cat "${marker}")" == "${want} ${stamp}" ]]; then
        return 0
    fi
    echo "[bairav-serve] checking SHA-256 of ${BAIRAV_FILE} ..."
    got="$(sha256sum "${f}" | cut -d' ' -f1)"
    if [[ "${got}" != "${want}" ]]; then
        rm -f "${marker}"
        echo "[bairav-serve] SHA-256 is ${got}, the Model pins ${want}"
        return 1
    fi
    echo "${want} ${stamp}" > "${marker}"
    echo "[bairav-serve] SHA-256 ok"
}

if [[ -f "${local_path}" ]] && ! matches "${local_path}"; then
    echo "[bairav-serve] cached copy doesn't match the pin; downloading again"
    rm -f "${local_path}"
fi
if [[ ! -f "${local_path}" ]]; then
    echo "[bairav-serve] pulling ${BAIRAV_REPO}/${BAIRAV_FILE} @ ${BAIRAV_REVISION} ..."
    blob_url="https://huggingface.co/${BAIRAV_REPO}/resolve/${BAIRAV_REVISION}/${BAIRAV_FILE}"
    curl "${curl_args[@]}" --output "${local_path}.partial" "${blob_url}"
    mv "${local_path}.partial" "${local_path}"
    if ! matches "${local_path}"; then
        rm -f "${local_path}"
        echo "[bairav-serve] ERROR: the downloaded ${BAIRAV_REPO}/${BAIRAV_FILE} doesn't match the SHA-256 the Model pins." >&2
        echo "[bairav-serve] It was deleted and the model will not start. If the repo changed on purpose, redeploy to pin the new version (bairav model deploy ...)." >&2
        exit 1
    fi
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
