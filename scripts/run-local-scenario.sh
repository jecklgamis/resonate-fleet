#!/usr/bin/env bash
# Run a scenario on the workers registered with the local controller, using
# resonatectl. Start the controller and a worker first:
#
#   scripts/run-local-controller.sh     # terminal 1
#   scripts/run-local-worker.sh         # terminal 2
#   scripts/run-local-scenario.sh       # terminal 3
#
# With no scenario argument it runs examples/local-scenario.yaml (flags such as
# --out still apply). Anything you pass goes straight to `resonatectl run`, so:
#
#   scripts/run-local-scenario.sh examples/scenario.yaml
#   scripts/run-local-scenario.sh --label region=sgp1 --data users.csv my.yaml
#   scripts/run-local-scenario.sh --out /tmp/reports my.yaml
#
# Settings (environment, all optional):
#   RESONATE_CONTROLLER  controller URL      (default: http://localhost:58090)
#   CONTROLLER_TOKEN     its bearer token    (default: ctl-secret)
set -euo pipefail
# Not cd-ing to the repo root: relative scenario and --data paths you pass must
# resolve from the directory you ran this in.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

export RESONATE_CONTROLLER="${RESONATE_CONTROLLER:-http://localhost:58090}"
export CONTROLLER_TOKEN="${CONTROLLER_TOKEN-ctl-secret}"

if command -v curl >/dev/null 2>&1 && ! curl -fsS -o /dev/null --max-time 3 "$RESONATE_CONTROLLER/healthz" 2>/dev/null; then
  echo "error: no controller answering at $RESONATE_CONTROLLER" >&2
  echo "       start one with scripts/run-local-controller.sh (and a worker with scripts/run-local-worker.sh)" >&2
  exit 1
fi

(cd "$ROOT" && go build -o resonatectl ./cmd/resonatectl)

# Add the default scenario unless one was given: an argument ending in .yaml/.yml
# or starting with http(s)://. So `--out dir` alone still runs the default.
has_scenario=false
for arg in "$@"; do
  case "$arg" in
    *.yaml|*.yml|http://*|https://*) has_scenario=true ;;
  esac
done
if [ "$has_scenario" = false ]; then
  set -- "$@" "$ROOT/examples/local-scenario.yaml"
fi

exec "$ROOT/resonatectl" run "$@"
