#!/usr/bin/env bash
set -euo pipefail
BIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
cd "${BIN_DIR}"

# resonate-worker only shells out to resonate once a task is submitted, so a
# missing binary would otherwise go unnoticed until the first upload fails.
# Check up front instead, the same fail-fast spirit as resonate's own CLI.
RESONATE_BIN="${RESONATE_BIN:-resonate}"
if ! command -v "${RESONATE_BIN}" >/dev/null 2>&1; then
  echo "error: resonate binary not found: \"${RESONATE_BIN}\"" >&2
  echo "  install it (see https://github.com/jecklgamis/resonate/blob/main/docs/install.md)" >&2
  echo "  or point RESONATE_BIN at it, e.g. RESONATE_BIN=/path/to/resonate $0" >&2
  exit 1
fi

if [ -z "${API_TOKEN:-}" ]; then
  echo "warning: API_TOKEN is not set - every endpoint will be open to anyone who can reach this address" >&2
fi

go build -o resonate-worker .
exec ./resonate-worker
