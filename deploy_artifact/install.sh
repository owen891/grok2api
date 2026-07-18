#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$ROOT"

command -v docker >/dev/null 2>&1 || { echo "Docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }

[ -f .env ] || cp .env.production.example .env
[ -f config.production.yaml ] || cp config.production.example.yaml config.production.yaml
mkdir -p data

if grep -Eq 'replace-with|change-me' .env config.production.yaml; then
  echo "Edit .env and config.production.yaml first: replace all secret placeholders." >&2
  echo "POSTGRES_PASSWORD in .env must match the password in the PostgreSQL DSN." >&2
  exit 2
fi

docker compose --env-file .env -f compose.production.yml config --quiet
docker compose --env-file .env -f compose.production.yml pull
docker compose --env-file .env -f compose.production.yml up -d

attempt=0
while [ "$attempt" -lt 30 ]; do
  if curl -fsS http://127.0.0.1:"${GROK2API_PORT:-8000}"/healthz >/dev/null 2>&1; then
    echo "grok2api is healthy"
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 2
done

echo "grok2api did not become healthy; inspect: docker compose --env-file .env -f compose.production.yml logs --tail=100 grok2api" >&2
exit 1
