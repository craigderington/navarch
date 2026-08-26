#!/usr/bin/env bash
# The test runner with opinions. Two things `go test ./...` alone cannot
# guarantee on this repo:
#
# 1. The dev stack's control plane must not be running while tests run: its
#    scheduler, controller and reaper loops mutate the same database the
#    tests use, and the reaper DELETEs environments — a running control plane
#    corrupts fixtures mid-run and fails unrelated tests. This script stops
#    it (and the agents, which have nothing to do while it is down) and
#    restarts afterwards exactly what was running before, so someone who
#    started only Postgres does not come back to a full fleet.
# 2. A green run must mean green. The Postgres-backed packages skip — loudly,
#    by design — when the database is unreachable, and `go test` still
#    reports `ok` for them. Almost every store/rollout/api test can skip that
#    way while everything looks fine, and the suite proves nothing. This
#    script counts the skips and fails the run if there are any.
set -euo pipefail

cd "$(dirname "$0")/.."

# A GOROOT inherited from the environment silently mixes binaries across
# installs — the shell's exported GOROOT pointing at one Go install while
# PATH resolves `go` from another produces "compile: version ... does not
# match go tool version" on exactly the builds that recompile the stdlib
# (-race among them). Every modern Go locates its own root; the variable has
# no legitimate use here and every illegitimate one is this failure mode.
unset GOROOT

LOG=$(mktemp /tmp/navarch-test.XXXXXX)
trap 'rm -f "$LOG"' EXIT

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() { printf '\033[31m%s\033[0m\n' "$1" >&2; }

# The services whose loops must not run during the tests. Everything else in
# the dev stack — Postgres above all — stays exactly as it is.
LOOP_SERVICES="controlplane"
WAS_RUNNING=""

if docker compose ps >/dev/null 2>&1; then
  # Agents have nothing to do while the control plane is down, and their
  # connection-refused spam buries anything else in the logs.
  AGENT_SERVICES=$(docker compose config --services 2>/dev/null | grep '^agent-' || true)
  LOOP_SERVICES="$LOOP_SERVICES $AGENT_SERVICES"

  WAS_RUNNING=$(docker compose ps --status running --format '{{.Service}}' 2>/dev/null \
    | grep -E '^(controlplane|agent-)' || true)

  step "Ensuring Postgres is up"
  # `up -d --wait` blocks until the healthcheck passes, so a cold database
  # volume is ready before anything connects to it. Migrations run
  # synchronously after that: `run` waits for the one-shot to finish, which
  # `up -d` does not, and a test suite connecting mid-migration is flake.
  docker compose up -d --wait postgres
  docker compose run --rm migrate

  if [ -n "$WAS_RUNNING" ]; then
    step "Stopping the control plane (it corrupts test fixtures)"
    docker compose stop $LOOP_SERVICES
  fi
else
  note "docker compose unavailable — no dev stack to stop; tests must bring their own Postgres"
fi

step "Running the suite"
if go test ./... -race -count=1 -v >"$LOG" 2>&1; then
  note "tests passed"
else
  fail "tests FAILED — full output follows"
  cat "$LOG"
  exit 1
fi

step "Checking for skips"
# A skip here means a live dependency was missing: Postgres down, or no
# Docker daemon for the dockerd suite. Either way the run above was not the
# run it claimed to be, so it fails the build rather than passing quietly.
SKIPS=$(grep -c '^--- SKIP' "$LOG" || true)
if [ "$SKIPS" -gt 0 ]; then
  grep '^--- SKIP' "$LOG" | sed 's/^/  /'
  fail "$SKIPS test(s) skipped — a green run proves nothing. Start Postgres (make up) and the Docker daemon, then re-run."
  exit 1
fi
note "no skips — every test ran"

if [ -n "$WAS_RUNNING" ]; then
  step "Restoring the dev stack"
  docker compose start $WAS_RUNNING
fi
