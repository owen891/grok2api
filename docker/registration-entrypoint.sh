#!/bin/sh
set -eu

if [ "${1:-}" = "--protocol-worker" ]; then
  shift
fi

exec /opt/registration-venv/bin/python -u /app/registration/protocol_register_cli.py "$@"
