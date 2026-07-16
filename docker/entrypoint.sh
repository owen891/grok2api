#!/bin/sh
set -eu

umask 077

if [ ! -f "${GROK2API_CONFIG_SOURCE}" ]; then
  echo "missing config: ${GROK2API_CONFIG_SOURCE}" >&2
  echo "mount config.yaml to /run/grok2api/config.yaml" >&2
  exit 1
fi

cp "${GROK2API_CONFIG_SOURCE}" /app/config.yaml
mkdir -p \
  /app/data/registration/cpa_auths \
  /app/data/registration/home \
  /app/data/registration/spool/incoming \
  /app/data/registration/spool/processed \
  /app/data/registration/spool/failed
if [ ! -f "${REGISTRATION_CONFIG_FILE}" ]; then
  cp /app/registration/config.example.json "${REGISTRATION_CONFIG_FILE}"
fi
chown grok2api:grok2api /app/config.yaml
chown -R grok2api:grok2api /app/data/registration
chmod 0600 /app/config.yaml
chmod 0600 "${REGISTRATION_CONFIG_FILE}"

exec gosu grok2api:grok2api "$@"

