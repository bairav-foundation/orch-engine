#!/usr/bin/env bash
# scripts/ci.sh — local pre-flight that runs everything CI runs.
#
# Use it as a daily reflex:
#   make ci
# or directly:
#   ./scripts/ci.sh
#
# Exit code 0 → safe to push. Non-zero → fix before pushing.
#
# What it checks, in order:
#   1. gofmt        — code is formatted
#   2. go mod tidy  — go.mod / go.sum are in sync (no dangling deps)
#   3. go vet       — basic correctness (printf format, unreachable code, etc.)
#   4. golangci-lint — full linter pack (skipped with a warning if not installed)
#   5. go test      — unit tests with -race
#   6. go build     — every binary actually compiles
#
# Walks every module that has a go.mod, skipping placeholders that haven't
# been initialised yet. Mirrors the defensive pattern in the Makefile so the
# modules without a go.mod don't
# break CI before they're real.

set -euo pipefail

# ----------------------------------------------------------------------
# Setup
# ----------------------------------------------------------------------

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
cd "${ROOT_DIR}"

MODULES=(cli operator)

# Colors (only when stdout is a TTY).
if [[ -t 1 ]]; then
  RED=$'\033[0;31m'
  GREEN=$'\033[0;32m'
  YELLOW=$'\033[0;33m'
  BOLD=$'\033[1m'
  DIM=$'\033[2m'
  RESET=$'\033[0m'
else
  RED='' GREEN='' YELLOW='' BOLD='' DIM='' RESET=''
fi

step()  { echo; echo "${BOLD}==> $*${RESET}"; }
ok()    { echo "   ${GREEN}✓${RESET} $*"; }
skip()  { echo "   ${DIM}↷${RESET} ${DIM}$*${RESET}"; }
warn()  { echo "   ${YELLOW}!${RESET} $*"; }
fail()  { echo "   ${RED}✗${RESET} $*" >&2; FAILED=1; }

FAILED=0
START_TIME=$SECONDS

# Run a step inside a module dir; only invoked when go.mod exists.
in_module() {
  local mod="$1"
  shift
  ( cd "${mod}" && "$@" )
}

# ----------------------------------------------------------------------
# 1. gofmt
# ----------------------------------------------------------------------
step "gofmt — code formatting"
unformatted=""
for mod in "${MODULES[@]}"; do
  [[ -f "${mod}/go.mod" ]] || { skip "${mod} (no go.mod yet)"; continue; }
  diff=$(in_module "${mod}" gofmt -l . 2>&1 || true)
  if [[ -n "${diff}" ]]; then
    unformatted+="${mod}: ${diff}"$'\n'
  else
    ok "${mod}"
  fi
done
if [[ -n "${unformatted}" ]]; then
  fail "gofmt would change these files (run 'make fmt' to fix):"
  echo "${unformatted}" | sed 's/^/      /' >&2
fi

# ----------------------------------------------------------------------
# 2. go mod tidy
# ----------------------------------------------------------------------
step "go mod tidy — dependency drift"
for mod in "${MODULES[@]}"; do
  [[ -f "${mod}/go.mod" ]] || { skip "${mod} (no go.mod yet)"; continue; }
  before=$(sha256sum "${mod}/go.mod" "${mod}/go.sum" 2>/dev/null || true)
  in_module "${mod}" go mod tidy
  after=$(sha256sum "${mod}/go.mod" "${mod}/go.sum" 2>/dev/null || true)
  if [[ "${before}" != "${after}" ]]; then
    fail "${mod}: go.mod or go.sum changed after tidy — commit the changes"
  else
    ok "${mod}"
  fi
done

# ----------------------------------------------------------------------
# 3. go vet
# ----------------------------------------------------------------------
step "go vet — correctness"
for mod in "${MODULES[@]}"; do
  [[ -f "${mod}/go.mod" ]] || { skip "${mod} (no go.mod yet)"; continue; }
  if in_module "${mod}" go vet ./... 2>&1; then
    ok "${mod}"
  else
    fail "${mod}"
  fi
done

# ----------------------------------------------------------------------
# 4. golangci-lint (optional — warn if missing)
# ----------------------------------------------------------------------
step "golangci-lint — full linter pack"
if ! command -v golangci-lint >/dev/null 2>&1; then
  warn "golangci-lint not installed — skipping (run 'make tools' or 'brew install golangci-lint')"
else
  for mod in "${MODULES[@]}"; do
    [[ -f "${mod}/go.mod" ]] || { skip "${mod} (no go.mod yet)"; continue; }
    if in_module "${mod}" golangci-lint run ./... 2>&1; then
      ok "${mod}"
    else
      fail "${mod}"
    fi
  done
fi

# ----------------------------------------------------------------------
# 5. go test (with -race)
# ----------------------------------------------------------------------
step "go test -race — unit tests"
for mod in "${MODULES[@]}"; do
  [[ -f "${mod}/go.mod" ]] || { skip "${mod} (no go.mod yet)"; continue; }
  if in_module "${mod}" go test -race -count=1 ./... 2>&1; then
    ok "${mod}"
  else
    fail "${mod}"
  fi
done

# ----------------------------------------------------------------------
# 6. go build (binary compiles end-to-end)
# ----------------------------------------------------------------------
step "go build — binaries compile"
for mod in "${MODULES[@]}"; do
  [[ -f "${mod}/go.mod" ]] || { skip "${mod} (no go.mod yet)"; continue; }
  if in_module "${mod}" go build ./... 2>&1; then
    ok "${mod}"
  else
    fail "${mod}"
  fi
done

# ----------------------------------------------------------------------
# Summary
# ----------------------------------------------------------------------
echo
DURATION=$((SECONDS - START_TIME))
if [[ "${FAILED}" -eq 0 ]]; then
  echo "${GREEN}${BOLD}All checks passed${RESET} ${DIM}(${DURATION}s)${RESET}"
  echo "${DIM}Safe to push. Run 'make build-cli' to refresh ./bin/bairav.${RESET}"
  exit 0
else
  echo "${RED}${BOLD}One or more checks failed${RESET} ${DIM}(${DURATION}s)${RESET}"
  exit 1
fi
