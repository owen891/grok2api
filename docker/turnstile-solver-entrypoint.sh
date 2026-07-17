#!/usr/bin/env bash
set -euo pipefail

host="${TURNSTILE_HOST:-0.0.0.0}"
port="${TURNSTILE_PORT:-5072}"
thread="${TURNSTILE_THREAD:-1}"
browser_type="${TURNSTILE_BROWSER_TYPE:-camoufox}"
proxy="${TURNSTILE_PROXY:-}"

args=(
  python /app/api_solver.py
  --browser_type "$browser_type"
  --thread "$thread"
  --host "$host"
  --port "$port"
)

if [[ "${TURNSTILE_DEBUG:-0}" == "1" || "${TURNSTILE_DEBUG:-false}" == "true" ]]; then
  args+=(--debug)
fi

if [[ -n "$proxy" ]]; then
  case "$proxy" in
    http://*|https://*|socks4://*|socks5://*) ;;
    *)
      echo "[turnstile-solver] TURNSTILE_PROXY must be a reachable proxy URL" >&2
      exit 64
      ;;
  esac
  printf '%s\n' "$proxy" > /app/proxies.txt
  chmod 0600 /app/proxies.txt
  args+=(--proxy)
  echo "[turnstile-solver] proxy enabled"
else
  rm -f /app/proxies.txt
  echo "[turnstile-solver] direct egress"
fi

exec "${args[@]}"
