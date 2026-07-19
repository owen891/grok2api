ARG NODE_VERSION=22
ARG GO_VERSION=1.26
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS frontend-builder

WORKDIR /src/frontend
RUN corepack enable

COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN --mount=type=cache,id=grok2api-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm fetch --frozen-lockfile

RUN --mount=type=cache,id=grok2api-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm install --offline --frozen-lockfile

COPY frontend/index.html frontend/vite.config.ts frontend/tsconfig.json frontend/tsconfig.app.json frontend/tsconfig.node.json ./
COPY VERSION /src/VERSION
COPY frontend/public ./public
COPY frontend/src ./src
RUN --mount=type=cache,id=grok2api-tsc,target=/src/frontend/.cache,sharing=locked \
    pnpm build


FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS backend-builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src/backend
RUN apk add --no-cache ca-certificates git

COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,id=grok2api-go-mod,target=/go/pkg/mod,sharing=locked \
    go mod download

COPY backend/cmd ./cmd
COPY backend/internal ./internal
COPY backend/docs/docs.go ./docs/docs.go
RUN --mount=type=cache,id=grok2api-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=grok2api-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/grok2api ./cmd/grok2api


FROM python:3.13-slim-bookworm AS registration-builder

ENV VIRTUAL_ENV=/opt/registration-venv \
    PATH="/opt/registration-venv/bin:$PATH"
ENV UV_HTTP_TIMEOUT=300

COPY --from=ghcr.io/astral-sh/uv:0.6 /uv /uvx /bin/
COPY registration/requirements.protocol.lock /tmp/registration-requirements.lock
RUN uv venv "$VIRTUAL_ENV" && \
    uv pip install --python "$VIRTUAL_ENV/bin/python" --require-hashes -r /tmp/registration-requirements.lock && \
    find "$VIRTUAL_ENV" -type d -name __pycache__ -prune -exec rm -rf {} + && \
    find "$VIRTUAL_ENV" -type f -name '*.pyc' -delete && \
    rm -rf /root/.cache /tmp/registration-requirements.lock


FROM python:3.13-slim-bookworm AS browser-registration-builder

ENV VIRTUAL_ENV=/opt/registration-venv \
    PATH="/opt/registration-venv/bin:$PATH" \
    UV_HTTP_TIMEOUT=300

COPY --from=ghcr.io/astral-sh/uv:0.6 /uv /uvx /bin/
COPY registration/requirements.lock /tmp/registration-requirements.lock
RUN uv venv "$VIRTUAL_ENV" && \
    uv pip install --python "$VIRTUAL_ENV/bin/python" --require-hashes -r /tmp/registration-requirements.lock && \
    find "$VIRTUAL_ENV" -type d -name __pycache__ -prune -exec rm -rf {} + && \
    find "$VIRTUAL_ENV" -type f -name '*.pyc' -delete && \
    rm -rf /root/.cache /tmp/registration-requirements.lock


FROM python:3.13-slim-bookworm AS runtime-base

RUN DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gosu \
        tzdata && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --gid 10001 grok2api && \
    useradd --uid 10001 --gid grok2api --no-create-home --shell /usr/sbin/nologin grok2api && \
    mkdir -p /app/data/registration/home /app/data/registration/spool/incoming /app/data/registration/spool/processing /app/data/registration/spool/processed /app/data/registration/spool/failed /run/grok2api && \
    chown -R grok2api:grok2api /app/data /run/grok2api

ENV TZ=Asia/Shanghai \
    GROK2API_CONFIG_SOURCE=/run/grok2api/config.yaml \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HOME=/app/data/registration/home \
    VIRTUAL_ENV=/opt/registration-venv \
    REGISTRATION_WORKDIR=/app/registration \
    REGISTRATION_DATA_DIR=/app/data/registration \
    REGISTRATION_CONFIG_FILE=/app/data/registration/config.json \
    REGISTRATION_CPA_EXPORT_DIR=/app/data/registration/cpa_auths \
    REGISTRATION_CPA_HOTLOAD_DIR=/app/data/registration/spool/incoming \
    REGISTRATION_DISABLE_REMOTE_IMPORT=1

ENV PATH="$VIRTUAL_ENV/bin:$PATH"

WORKDIR /app

COPY --from=backend-builder --chmod=0755 /out/grok2api /app/grok2api
COPY --from=frontend-builder /src/frontend/dist /app/frontend/dist
COPY --from=registration-builder /opt/registration-venv /opt/registration-venv
COPY registration/protocol_register_cli.py registration/protocol_spool.py registration/yyds_mail.py registration/local_turnstile.py registration/clearance_provider.py /app/registration/
COPY registration/config.protocol.example.json /app/registration/config.example.json
COPY registration/cpa_xai/__init__.py registration/cpa_xai/schema.py registration/cpa_xai/writer.py /app/registration/cpa_xai/
COPY registration/protocol_auth/*.py /app/registration/protocol_auth/
COPY registration/protocol_auth/xconsole_client /app/registration/protocol_auth/xconsole_client
COPY --chmod=0755 docker/entrypoint.sh /usr/local/bin/grok2api-entrypoint
COPY --chmod=0755 docker/registration-entrypoint.sh /usr/local/bin/grok2api-registration
RUN sed -i 's/\r$//' /usr/local/bin/grok2api-entrypoint /usr/local/bin/grok2api-registration

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8000/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/grok2api-entrypoint"]
CMD ["/app/grok2api", "--config", "/app/config.yaml", "--listen", "0.0.0.0:8000"]


FROM runtime-base AS browser-runtime

RUN DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -y --no-install-recommends \
        chromium \
        fonts-liberation \
        xauth \
        xvfb && \
    rm -rf /var/lib/apt/lists/*

COPY --from=browser-registration-builder /opt/registration-venv /opt/registration-venv
COPY registration/*.py /app/registration/
COPY registration/config.example.json /app/registration/config.example.json
COPY registration/cpa_xai/ /app/registration/cpa_xai/
COPY registration/turnstilePatch/ /app/registration/turnstilePatch/
COPY --chmod=0755 docker/browser-entrypoint.sh /usr/local/bin/grok2api-browser-entrypoint
RUN sed -i 's/\r$//' /usr/local/bin/grok2api-browser-entrypoint

ENV DISPLAY=:99 \
    REGISTRATION_BROWSER_MODE=xvfb \
    REGISTRATION_BROWSER_PATH=/usr/bin/chromium \
    REGISTRATION_BROWSER_WINDOW=1280,900 \
    REGISTRATION_XVFB_SCREEN=1280x900x24

ENTRYPOINT ["/usr/local/bin/grok2api-browser-entrypoint"]


FROM runtime-base AS runtime
