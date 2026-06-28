#!/usr/bin/env bash
#
# opencode-as-harness smoke test: drive the router with the real opencode agent.
#
# Prereqs:
#   - router running:  ./bin/router --config config.local.yaml
#   - project opencode.jsonc present (defines the `router` provider)
#   - run from the repo root:  ./evals/opencode_smoke.sh
#
# Sends a pure-generation prompt through each router alias and asserts opencode
# got a non-empty answer back (exit 0). This exercises the full path a real
# coding agent uses: opencode -> router -> fleet.

set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

ROUTER_URL="${ROUTER_URL:-http://localhost:8080}"
MODELS=("${@:-router/north router/gemma router/smart}")
# shellcheck disable=SC2206
MODELS=(${MODELS[*]})

if ! curl -fsS "${ROUTER_URL}/healthz" >/dev/null 2>&1; then
  echo "router not reachable at ${ROUTER_URL} — start it: ./bin/router --config config.local.yaml"
  exit 1
fi

PROMPT="In one short sentence, explain what a reverse proxy is. Do not use any tools or edit files."
fails=0

for model in "${MODELS[@]}"; do
  printf '\n=== opencode run -m %s ===\n' "$model"
  out="$(opencode run --dangerously-skip-permissions -m "$model" "$PROMPT" 2>/tmp/opencode_smoke.err)"
  rc=$?
  if [[ $rc -ne 0 ]]; then
    echo "FAIL ($model): opencode exited $rc"
    sed -n '1,8p' /tmp/opencode_smoke.err
    fails=$((fails + 1))
    continue
  fi
  # Strip ANSI and whitespace to judge non-emptiness.
  clean="$(printf '%s' "$out" | sed $'s/\033\[[0-9;]*m//g' | tr -d '[:space:]')"
  if [[ -z "$clean" ]]; then
    echo "FAIL ($model): empty response"
    fails=$((fails + 1))
  else
    echo "PASS ($model)"
    printf '%s\n' "$out" | tail -n 6
  fi
done

echo
if [[ $fails -eq 0 ]]; then
  echo "opencode smoke: all models OK"
else
  echo "opencode smoke: $fails model(s) failed"
fi
exit "$fails"
