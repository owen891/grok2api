#!/usr/bin/env bash
set -euo pipefail

host="${TURNSTILE_HOST:-0.0.0.0}"
port="${TURNSTILE_PORT:-5072}"
thread="${TURNSTILE_THREAD:-1}"
browser_type="${TURNSTILE_BROWSER_TYPE:-camoufox}"
proxy="${TURNSTILE_PROXY:-}"
solver_variant="${TURNSTILE_SOLVER_VARIANT:-full}"

if [[ "$solver_variant" == "camoufox-linux" && "$browser_type" != "camoufox" ]]; then
  echo "[turnstile-solver] camoufox-linux image only supports TURNSTILE_BROWSER_TYPE=camoufox; use the full image for Chromium" >&2
  exit 64
fi

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
    http://*|https://*|socks4://*|socks5://*|socks5h://*) ;;
    *)
      echo "[turnstile-solver] TURNSTILE_PROXY must be a reachable proxy URL" >&2
      exit 64
      ;;
  esac
  printf '%s\n' "$proxy" > /app/proxies.txt
  chmod 0600 /app/proxies.txt
  args+=(--proxy)
fi

exec "${args[@]}"
