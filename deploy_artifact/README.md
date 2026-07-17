# Server deployment

1. Extract `server-v3-config.tar.gz` into `${GROK2API_DEPLOY_DIR}`.
2. Extract `server-v3-release.tar.gz` into a dated directory and point `${GROK2API_RELEASE_DIR}` at its `app-release` directory, preferably through an atomic `grok2api-current` symlink.
3. Copy `.env.example` to `.env` and set the three host directories.
4. Copy `config.docker.example.yaml` to `config.docker.yaml` and replace its secret placeholders.
5. Keep persistent data in `${GROK2API_DATA_DIR}` instead of a dated release directory.
6. On the first deployment, load the supplied solver image with `docker load -i grok-turnstile-solver-0d48ecf8b4ee.tar`.
7. Start with `docker compose --env-file .env -f docker-compose.runtime.yml up -d --build`.

The release directory owns the versioned binary, frontend, registration worker, and browser worker. The deployment directory owns Compose, image-build inputs, configuration, and secrets. Replacing the `grok2api-current` symlink therefore updates all versioned runtime code together.

`REGISTRATION_CAPTCHA_ENDPOINT` defaults to the Compose solver service. `REGISTRATION_PROXY` is intentionally empty by default so a copied workstation value such as `127.0.0.1:7890` is not used inside the server container.

Set one registration proxy mode in `.env` before deployment:

- Direct: leave `REGISTRATION_PROXY=` empty.
- Proxy on the Docker host: `REGISTRATION_PROXY=http://host.docker.internal:PORT`.
- Proxy service in the Compose network: use its service DNS name, for example `REGISTRATION_PROXY=socks5://warp:1080`.
- Registration-scoped Linux proxy environment: set `REGISTRATION_PROXY=system` and fill `REGISTRATION_HTTPS_PROXY`, `REGISTRATION_HTTP_PROXY`, or `REGISTRATION_ALL_PROXY`.

Container loopback points to the container itself. Do not use `127.0.0.1:PORT` for a proxy running on the server host.

The Turnstile solver is a separate browser container. When registration uses a proxy, also set `REGISTRATION_SOLVER_PROXY` to a concrete URL reachable from that container. For a proxy on the Docker host, a complete configuration is:

```env
REGISTRATION_PROXY=http://host.docker.internal:PORT
REGISTRATION_SOLVER_PROXY=http://host.docker.internal:PORT
```

Do not set `REGISTRATION_SOLVER_PROXY=system`; containers do not share the host operating system's proxy registry. Set `TURNSTILE_DEBUG=1` temporarily to include browser-level solver diagnostics in `docker compose logs grok-turnstile-solver`.

After startup, verify both the process and registration preflight:

```sh
docker compose --env-file .env -f docker-compose.runtime.yml ps
curl -fsS http://127.0.0.1:8001/healthz
docker compose --env-file .env -f docker-compose.runtime.yml logs --tail=100 grok2api
```
