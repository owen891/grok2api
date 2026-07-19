#!/bin/sh
set -eu

if [ "${REGISTRATION_BROWSER_MODE:-}" = "xvfb" ]; then
  display="${DISPLAY:-:99}"
  screen="${REGISTRATION_XVFB_SCREEN:-1280x900x24}"
  mkdir -p /tmp/.X11-unix
  chmod 1777 /tmp/.X11-unix
  gosu grok2api:grok2api Xvfb "$display" \
    -screen 0 "$screen" \
    -nolisten tcp \
    -noreset &
  xvfb_pid=$!
  display_number=${display#:}
  display_number=${display_number%%.*}
  ready=0
  for _ in $(seq 1 50); do
    if [ -S "/tmp/.X11-unix/X${display_number}" ]; then
      ready=1
      break
    fi
    if ! kill -0 "$xvfb_pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  if [ "$ready" -ne 1 ]; then
    echo "Xvfb failed to initialize on ${display}" >&2
    exit 1
  fi
fi

exec /usr/local/bin/grok2api-entrypoint "$@"
