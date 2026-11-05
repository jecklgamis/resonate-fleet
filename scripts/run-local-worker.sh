#!/usr/bin/env bash
# Build and run a resonate-worker on this machine, registered with a local
# controller (see run-local-controller.sh).
#
#   scripts/run-local-worker.sh                 # register with http://localhost:58090
#   CONTROLLER_URL= scripts/run-local-worker.sh # no controller: a standalone worker
#
# Needs the `resonate` binary on PATH, or RESONATE_BIN pointing at it.
#
# Settings (environment, all optional):
#   API_TOKEN         this worker's bearer token           (default: worker-secret)
#   ADDR              listen address                       (default: :58080)
#   WORKSPACE_DIR     where task files go                  (default: ./workspace)
#   CONTROLLER_URL    controller to register with; set it to the empty string
#                     for a standalone worker              (default: http://localhost:58090)
#   CONTROLLER_TOKEN  the controller's token               (default: ctl-secret)
#   WORKER_NAME       name to register under               (default: local)
#   WORKER_LABELS     key=value,key=value                  (default: none)
#   ADVERTISE_URL     address the controller uses to reach this worker
#                     (default: http://127.0.0.1:<port of ADDR>)
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v "${RESONATE_BIN:-resonate}" >/dev/null 2>&1; then
  echo "error: the resonate binary was not found; put it on PATH or set RESONATE_BIN" >&2
  echo "       (see https://jecklgamis.github.io/resonate/#/install)" >&2
  exit 1
fi

export API_TOKEN="${API_TOKEN-worker-secret}"
export ADDR="${ADDR:-:58080}"
export WORKSPACE_DIR="${WORKSPACE_DIR:-workspace}"
export CONTROLLER_URL="${CONTROLLER_URL-http://localhost:58090}"

if [ -n "$CONTROLLER_URL" ]; then
  export CONTROLLER_TOKEN="${CONTROLLER_TOKEN-ctl-secret}"
  export WORKER_NAME="${WORKER_NAME:-local}"
  # Bind to IPv4 explicitly: "localhost" often resolves to ::1 first, and the
  # worker would then advertise an IPv6 loopback address.
  export ADVERTISE_URL="${ADVERTISE_URL:-http://127.0.0.1:${ADDR##*:}}"
fi

go build -o resonate-worker ./cmd/resonate-worker

echo "worker on ${ADDR}${CONTROLLER_URL:+, registering with $CONTROLLER_URL as $WORKER_NAME}" >&2
exec ./resonate-worker
