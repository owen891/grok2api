#!/bin/sh
set -eu

case "${REGISTRATION_BROWSER_MODE:-xvfb}" in
  xvfb|background)
    exec xvfb-run --auto-servernum \
      --server-args="-screen 0 ${REGISTRATION_XVFB_SCREEN:-1280x900x24} -nolisten tcp -ac" \
      /opt/registration-venv/bin/python /app/registration/register_cli.py "$@"
    ;;
  headless)
    exec /opt/registration-venv/bin/python /app/registration/register_cli.py "$@"
    ;;
  headed)
    if [ -z "${DISPLAY:-}" ]; then
      echo "REGISTRATION_BROWSER_MODE=headed requires DISPLAY" >&2
      exit 1
    fi
    exec /opt/registration-venv/bin/python /app/registration/register_cli.py "$@"
    ;;
  *)
    echo "invalid REGISTRATION_BROWSER_MODE: ${REGISTRATION_BROWSER_MODE}" >&2
    exit 1
    ;;
esac
