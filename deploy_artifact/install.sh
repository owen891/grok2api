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
if [ "$runtime" = "protocol" ] || [ "$runtime" = "both" ]; then
  if ! compose config --services | grep -qx 'grok-turnstile-solver'; then
    echo "protocol registration selected, but grok-turnstile-solver is missing from the Compose project" >&2
    exit 2
  fi
fi
compose pull
compose up -d

check_protocol_runtime() {
  if [ "$runtime" != "protocol" ] && [ "$runtime" != "both" ]; then
    return 0
  fi
  if ! compose exec -T grok2api /opt/registration-venv/bin/python -c \
      "import urllib.request; response = urllib.request.urlopen('http://grok-turnstile-solver:5072/health', timeout=5); assert 200 <= response.status < 300" \
      >/dev/null 2>&1; then
    return 1
  fi
  return 0
}

wait_protocol_runtime() {
  if [ "$runtime" != "protocol" ] && [ "$runtime" != "both" ]; then
    return 0
  fi
  attempt=0
  while [ "$attempt" -lt 60 ]; do
    if check_protocol_runtime; then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 3
  done
  return 1
}

port=${GROK2API_PORT:-}
if [ -z "$port" ]; then
  port=$(sed -n 's/^GROK2API_PORT=//p' .env | tail -n 1)
fi
port=${port:-8000}
attempt=0
while [ "$attempt" -lt 30 ]; do
  if curl -fsS "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then
    if ! wait_protocol_runtime; then
      echo "grok2api is healthy, but grok-turnstile-solver is not reachable from its Compose network" >&2
      compose logs --tail=100 grok2api grok-turnstile-solver >&2 || true
      exit 1
    fi
    echo "grok2api is healthy"
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 2
done

echo "grok2api did not become healthy; inspect with the same registration runtime: ./install.sh, then compose logs --tail=100 grok2api" >&2
exit 1
