#!/usr/bin/env bash
# Build and run a resonate-controller on this machine.
#
#   scripts/run-local-controller.sh
#
# Settings (environment, all optional):
#   CONTROLLER_TOKEN  bearer token for the API         (default: ctl-secret)
#   ADDR              listen address                   (default: :58090)
#   WORKER_TTL, POLL_INTERVAL, RUN_TIMEOUT             (see docs/controller.md)
set -euo pipefail
cd "$(dirname "$0")/.."

export CONTROLLER_TOKEN="${CONTROLLER_TOKEN-ctl-secret}"
export ADDR="${ADDR:-:58090}"

go build -o resonate-controller ./cmd/resonate-controller

echo "controller on ${ADDR}; use: export RESONATE_CONTROLLER=http://localhost:${ADDR##*:} CONTROLLER_TOKEN=${CONTROLLER_TOKEN}" >&2
exec ./resonate-controller
