FROM python:3.12-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    HOME=/root \
    TURNSTILE_HOST=0.0.0.0 \
    TURNSTILE_PORT=5072 \
    TURNSTILE_THREAD=1 \
    TURNSTILE_BROWSER_TYPE=camoufox \
    DEBIAN_FRONTEND=noninteractive

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        fonts-liberation \
        fonts-noto-color-emoji \
        libasound2 \
        libatk-bridge2.0-0 \
        libatk1.0-0 \
        libcups2 \
        libdbus-1-3 \
        libdrm2 \
        libgbm1 \
        libgtk-3-0 \
        libnspr4 \
        libnss3 \
        libpango-1.0-0 \
        libx11-6 \
        libx11-xcb1 \
        libxcb1 \
        libxcomposite1 \
        libxdamage1 \
        libxext6 \
        libxfixes3 \
        libxkbcommon0 \
        libxrandr2 \
        libxshmfence1 \
        libxss1 \
        libxtst6 \
        xvfb \
    && rm -rf /var/lib/apt/lists/*

COPY docker/turnstile-solver.requirements.txt /tmp/requirements.txt
RUN python -m pip install --no-cache-dir -U pip setuptools wheel \
    && python -m pip install --no-cache-dir -r /tmp/requirements.txt \
    && rm -f /tmp/requirements.txt

COPY turnstile-solver/api_solver.py turnstile-solver/browser_configs.py turnstile-solver/db_results.py /app/
COPY docker/turnstile-solver-entrypoint.sh /app/entrypoint.sh
COPY docker/patch-turnstile-solver.py /tmp/patch-turnstile-solver.py

RUN python /tmp/patch-turnstile-solver.py /app/api_solver.py \
    && sed -i 's/\r$//' /app/entrypoint.sh \
    && chmod 0755 /app/entrypoint.sh \
    && rm -f /tmp/patch-turnstile-solver.py

# Camoufox downloads through the GitHub API. The token prevents anonymous API
# rate limits during CI and is mounted only for this build step.
RUN --mount=type=secret,id=github_token,required=true \
    GITHUB_TOKEN="$(cat /run/secrets/github_token)" python -m camoufox fetch

RUN mkdir -p /app/logs /app/keys

EXPOSE 5072

HEALTHCHECK --interval=15s --timeout=5s --start-period=45s --retries=8 \
    CMD curl -fsS "http://127.0.0.1:${TURNSTILE_PORT:-5072}/" >/dev/null || exit 1

ENTRYPOINT ["/app/entrypoint.sh"]
