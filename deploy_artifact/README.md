# Server deployment

1. Copy `.env.example` to `.env` and set the three host directories.
2. Copy `config.docker.example.yaml` to `${GROK2API_DEPLOY_DIR}/config.docker.yaml` and replace its secret placeholders.
3. Keep persistent data in `${GROK2API_DATA_DIR}` instead of a dated release directory.
4. Point `${GROK2API_RELEASE_DIR}` at the current release, preferably through an atomic `grok2api-current` symlink.
5. Start with `docker compose --env-file .env -f docker-compose.runtime.yml up -d --build`.

`REGISTRATION_CAPTCHA_ENDPOINT` defaults to the Compose solver service. `REGISTRATION_PROXY` is intentionally empty by default so a copied workstation value such as `127.0.0.1:7890` is not used inside the server container.
