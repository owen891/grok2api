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

runtime=${REGISTRATION_RUNTIME:-}
if [ -z "$runtime" ]; then
  runtime=$(sed -n 's/^REGISTRATION_RUNTIME=//p' .env | tail -n 1)
fi
runtime=${runtime:-protocol}

compose() {
  case "$runtime" in
    protocol)
      docker compose --env-file .env -f compose.production.yml -f compose.registration.yml "$@"
      ;;
    browser)
      docker compose --env-file .env -f compose.production.yml -f compose.browser-registration.yml "$@"
      ;;
    both)
      docker compose --env-file .env -f compose.production.yml -f compose.registration.yml -f compose.browser-registration.yml "$@"
      ;;
    none)
      docker compose --env-file .env -f compose.production.yml "$@"
      ;;
    *)
      echo "REGISTRATION_RUNTIME must be protocol, browser, both, or none" >&2
      exit 2
      ;;
  esac
}

echo "Registration runtime: $runtime"
compose config --quiet
compose pull
compose up -d

port=${GROK2API_PORT:-}
if [ -z "$port" ]; then
  port=$(sed -n 's/^GROK2API_PORT=//p' .env | tail -n 1)
fi
port=${port:-8000}
attempt=0
while [ "$attempt" -lt 30 ]; do
  if curl -fsS "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then
    echo "grok2api is healthy"
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 2
done

echo "grok2api did not become healthy; inspect with the same registration runtime: ./install.sh, then compose logs --tail=100 grok2api" >&2
exit 1
