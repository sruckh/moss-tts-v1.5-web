#!/usr/bin/env bash
# Operator convenience wrapper for the Timbre stack.
#
# WHY THIS EXISTS
#   The stack only gets its secrets (RUNPOD_API_KEY, the auth secrets, the S3
#   replication credentials) if the Infisical machine identity is in the SHELL
#   before `docker compose up`. scripts/env.sh loads it; docker-compose then
#   forwards INFISICAL_* into the containers, where docker/entrypoint.sh trades
#   it for a short-lived token and runs the app under `infisical run`.
#   `docker compose up` on its own starts the stack with NO secrets injected.
#   This script always sources env.sh first, so that ordering cannot be missed.
#
# USAGE
#   scripts/run.sh start     load the identity, build (cached), up -d, verify
#   scripts/run.sh stop      docker compose down (the db volume is kept)
#   scripts/run.sh restart   stop, then start
#   scripts/run.sh status    containers, /healthz, and the secret-injection check
#   scripts/run.sh logs      follow app + litestream logs
#
# First time only: `cp .env.example .env` and set RUNPOD_ENDPOINT in it. The
# Infisical identity is read from ~/.config/timbre/ by default (override with
# INFISICAL_CONFIG / INFISICAL_SECRET_GPG) — see scripts/env.sh.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$DIR/.." && pwd)"
cd "$ROOT"

NET=shared_net
COMPOSE=(docker compose)

say() { printf '\033[1m>> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m!! %s\033[0m\n' "$*" >&2; }
die() { printf '\033[1;31m!! %s\033[0m\n' "$*" >&2; exit 1; }

ensure_env_file() {
  if [ ! -f .env ]; then
    die ".env not found. First-time setup:
    cp .env.example .env  &&  \$EDITOR .env   # at least set RUNPOD_ENDPOINT"
  fi
}

ensure_network() {
  if ! docker network inspect "$NET" >/dev/null 2>&1; then
    say "Docker network '$NET' not found — creating it"
    docker network create "$NET"
  fi
}

# Source env.sh with this script's `set -e` suspended: env.sh is written to be
# sourced and uses `return`, and we want to capture its exit status ourselves.
load_identity() {
  say "Loading Infisical machine identity (scripts/env.sh)"
  set +e
  local rc=0
  # shellcheck disable=SC1091,SC2320  # source path is dynamic; capture its status
  . "$DIR/env.sh" || rc=$?
  set -e
  if [ "$rc" -ne 0 ]; then
    die "Could not load the Infisical machine identity.
  Expected at ~/.config/timbre/ (infisical.env + infisical_client_secret.gpg),
  or set INFISICAL_CONFIG / INFISICAL_SECRET_GPG to point elsewhere.
  See scripts/env.sh. Without it the stack starts but NO secrets are injected."
  fi
}

app_health() {
  "${COMPOSE[@]}" exec -T app wget -qO- http://127.0.0.1:8080/healthz 2>/dev/null || true
}

whisper_health() {
  "${COMPOSE[@]}" exec -T whisper-server wget -qO- http://127.0.0.1:8080/ 2>/dev/null || true
}

# Secrets land in the APP PROCESS's env, not the container's, so a fresh exec
# shell cannot see RUNPOD_API_KEY directly. Read it out of the running process's
# /proc/<pid>/environ instead — the documented way to confirm injection worked.
verify_secrets() {
  say "Verifying RUNPOD_API_KEY was injected into the app process"
  # shellcheck disable=SC2016  # $p expands in the INNER sh, not this shell
  if "${COMPOSE[@]}" exec -T app sh -c '
      for p in /proc/[0-9]*; do
        tr "\0" "\n" < "$p/environ" 2>/dev/null \
          | grep -q "^RUNPOD_API_KEY=." && { echo present; exit 0; }
      done; exit 1' 2>/dev/null | grep -q present; then
    printf '  RUNPOD_API_KEY: present\n'
  else
    warn "RUNPOD_API_KEY NOT found in the app process — secrets were not injected.
   Check the identity was loaded and RUNPOD_API_KEY exists in that Infisical project/env."
    return 1
  fi
}

cmd_start() {
  ensure_env_file
  ensure_network
  load_identity
  say "Building (cached) and starting whisper-server + app + litestream"
  "${COMPOSE[@]}" up -d --build whisper-server app litestream

  say "Waiting for the app to answer /healthz"
  local app_ready=""
  for _ in $(seq 1 30); do
    if [ -n "$(app_health)" ]; then app_ready=1; break; fi
    sleep 2
  done
  if [ -n "$app_ready" ]; then
    printf '  app /healthz: %s\n' "$(app_health)"
  else
    warn "app /healthz not answering yet — it may still be booting; check 'logs'."
  fi

  say "Waiting for whisper-server to load its model"
  local whisper_ready=""
  for _ in $(seq 1 60); do
    if [ -n "$(whisper_health)" ]; then whisper_ready=1; break; fi
    sleep 2
  done
  if [ -n "$whisper_ready" ]; then
    printf '  whisper-server: ready\n'
  else
    warn "whisper-server did not become ready; check 'scripts/run.sh logs'."
  fi

  verify_secrets || true
  say "Started app + whisper-server + litestream on '$NET' behind your reverse proxy (NPM → Cloudflare)."
}

cmd_stop() {
  say "Stopping the stack (the 'db' volume is kept)"
  "${COMPOSE[@]}" down
}

cmd_status() {
  "${COMPOSE[@]}" ps app whisper-server litestream
  echo
  if [ -n "$(app_health)" ]; then
    say "app /healthz: OK ($(app_health))"
  else
    warn "app /healthz: not answering (is the stack running?)"
  fi
  if [ -n "$(whisper_health)" ]; then
    say "whisper-server: ready"
  else
    warn "whisper-server: not answering (is the model still loading?)"
  fi
  verify_secrets || true
}

cmd_logs() {
  "${COMPOSE[@]}" logs -f --tail=200 app whisper-server litestream
}

usage() {
  cat <<'EOF'
Usage: scripts/run.sh {start|stop|restart|status|logs}

  start    Load the Infisical identity, build (cached), start app +
           whisper-server + litestream, wait for app/Whisper health, and verify
           secret injection. This is the correct way to start the full stack.
  stop     docker compose down  (the 'db' and Whisper model volumes are kept)
  restart  stop, then start
  status   show all three services, app/Whisper health, and secret injection
  logs     follow app + whisper-server + litestream logs
EOF
}

case "${1:-}" in
  start)   cmd_start ;;
  stop)    cmd_stop ;;
  restart) cmd_stop || true; cmd_start ;;
  status)  cmd_status ;;
  logs)    cmd_logs ;;
  *)       usage; exit 1 ;;
esac
