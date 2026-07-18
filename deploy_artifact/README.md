# Server deployment

> Deployment simplification analysis: [DEPLOYMENT-SIMPLIFICATION.md](DEPLOYMENT-SIMPLIFICATION.md)
>
> The procedure below is the current release-bundle fallback. It rebuilds `Dockerfile.runtime` on the target host, so it is intentionally slower than the planned prebuilt-image path. For normal upgrades, run the build once and then use `docker compose --env-file .env -f docker-compose.runtime.yml up -d` without `--build`.

## Recommended production layout

The normal server does **not** need a host Python installation, a host PostgreSQL
installation, or a host Redis installation. Docker owns those runtime boundaries:

| Service | Runtime | Persistence | Purpose |
| --- | --- | --- | --- |
| `grok2api` | released application image (Go + frontend + Python worker) | `./data` | API, admin UI, registration worker |
| `postgres` | PostgreSQL container | `postgres-data` | durable relational data |
| `redis` | Redis container | `redis-data` | shared locks, rate limits, sessions, settings events |
| `grok-web-browser` | browser container | none | Grok Web session automation |
| `grok-turnstile-solver` | optional browser container | none | Docker clearance only |

The host requirements are Docker Engine, Compose v2, and enough disk/RAM. Use the
standard stack for a real multi-worker or multi-instance deployment:

```sh
cp .env.production.example .env
cp config.production.example.yaml config.production.yaml
# Set the same random PostgreSQL password in .env and config.production.yaml.
mkdir -p data
docker compose --env-file .env -f compose.production.yml config
docker compose --env-file .env -f compose.production.yml pull
docker compose --env-file .env -f compose.production.yml up -d
curl -fsS http://127.0.0.1:8000/healthz
```

The same flow is wrapped by `sh ./install.sh` (Linux) or `./install.ps1`
(Windows PowerShell). Both scripts are idempotent and refuse to start while
secret placeholders remain.

To produce a clean online deployment bundle from the repository, run:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\package-deployment.ps1
```

The packager intentionally excludes historical release directories, build
artifacts, runtime data, and duplicate solver archives. Add `-Offline` only when
the target cannot pull images from GHCR/Docker Hub; that mode saves one tar per
required image and is substantially larger.

The generated bundle includes `grok_web_browser_worker.py` and rewrites the
Compose mount to use that local file. For an offline bundle, load every tar in
`images/`, rename `.env.offline` to `.env`, and then run `install.ps1` or
`install.sh`.

Enable Docker clearance only when registration requires it:

```sh
docker compose --env-file .env \
  -f compose.production.yml -f compose.registration.yml \
  up -d
```

The registration overlay expects the fixed solver image published by
`.github/workflows/solver-image.yml`. It starts Camoufox eagerly, so a missing
browser binary fails the container health check instead of failing on the first
registration task. Set `TURNSTILE_BROWSER_TYPE=chromium` only as an explicit
temporary fallback after verifying the image contains Patchright Chromium.

Do not install application Python dependencies during deployment. They are built
once by CI and shipped inside `GROK2API_IMAGE`; normal upgrades are `pull` plus
`up -d`, not `up -d --build`.

For a low-volume single-host installation, the root `docker-compose.yml` remains
the simple SQLite/memory option. It is intentionally not the recommended topology
for multiple API replicas or high registration concurrency.

## Legacy release-bundle fallback (deprecated)

Only use this path when an offline environment cannot consume the prebuilt
Compose images. Normal deployments should use `install.ps1`/`install.sh` above.

1. Extract `server-v3-config.tar.gz` into `${GROK2API_DEPLOY_DIR}`.
2. Extract `server-v3-release.tar.gz` into a dated directory and point `${GROK2API_RELEASE_DIR}` at its `app-release` directory, preferably through an atomic `grok2api-current` symlink.
3. Copy `.env.example` to `.env` and set the three host directories.
4. Copy `config.docker.example.yaml` to `config.docker.yaml` and replace its secret placeholders.
5. Keep persistent data in `${GROK2API_DATA_DIR}` instead of a dated release directory.
6. In an offline bundle generated with `package-deployment.ps1 -Offline`, load the image tar files from `images/`.
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

## Proxy groups and clearance

Configure egress groups from the admin console under Settings -> Media & network:

1. Create a group for the required scope (`grok_build`, `grok_web`, `grok_console`, or `grok_web_asset`).
2. Choose a strategy: `least_load`, `round_robin`, `weighted`, or `sticky`.
3. Open the group editor and bulk-import one proxy URL per line. `host:port` is normalized to HTTP; explicit HTTP, HTTPS, SOCKS4, SOCKS4A, SOCKS5, and SOCKS5H URLs are preserved.
4. Set an optional same-scope fallback group, then save. Fallback cycles and cross-scope references are rejected.
5. Select the group on the Registration page and run Preflight before starting a batch. The worker resolves enabled, healthy members into an encrypted proxy pool and keeps one proxy per checkpoint.

Registration clearance is independent from the browser worker:

- `Docker clearance` uses `REGISTRATION_CAPTCHA_ENDPOINT` (normally `http://grok-turnstile-solver:5072`).
- `YesCaptcha` is selected in the Registration settings and requires its API key.
- The browser worker remains required for Grok Web sessions; it is not required just to use Docker clearance.

For concurrent registration on a memory-constrained host, keep one browser per solver
container and scale the solver service horizontally. The Compose service must not use
`container_name` when scaling:

```sh
docker compose --env-file .env -f docker-compose.runtime.yml up -d --scale grok-turnstile-solver=2
```

Compose DNS load-balances `http://grok-turnstile-solver:5072`; the worker also accepts
`captcha_endpoints` / `REGISTRATION_CAPTCHA_ENDPOINT` as a comma-separated endpoint pool
and fails over to the next solver when one task times out.

Protocol registration writes OAuth and SSO records under the persistent data directory. The runtime Compose file mounts `registration/protocol_auth/oauth_output` and `registration/protocol_auth/sso_output` over the read-only registration source tree.

For an API smoke check after login, call `GET /api/admin/v1/egress-groups`, create a temporary group, use `/import` with `dryRun: true`, and delete the temporary group after validation.

If `docker compose build` fails while resolving Docker Hub base images, check the Docker Desktop proxy setting before retrying:

```powershell
docker info --format 'HTTP={{.HTTPProxy}} HTTPS={{.HTTPSProxy}}'
Test-NetConnection <proxy-host> -Port <proxy-port>
```

The proxy endpoint must be reachable from Docker Desktop. A stale `http.docker.internal:3128` or a stopped local proxy causes base-image resolution to fail before the application build starts.
