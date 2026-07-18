# 部署简化分析

## 结论

当前部署把三件不同的事情绑在了一起：发布物解压、运行镜像构建、运行时配置初始化。最耗时的是第二项：`docker-compose.runtime.yml` 要求执行 `up -d --build`，而 `Dockerfile.runtime` 每次在目标机创建 Python venv 并按 260 行的 hash lock 安装注册 worker 依赖。发布包虽然已经带有 Go binary、前端静态文件和 Python 源码，但没有带可直接运行的应用镜像，因此目标机仍要重复构建。

建议把生产部署收敛为：

1. CI 构建并发布应用镜像（包含 Go binary、前端和 Python venv）。
2. 服务器只拉取固定 tag/digest，挂载一个数据目录和一个配置文件。
3. browser worker 和 Turnstile solver 继续作为独立服务；它们是运行时依赖，不应和应用镜像构建耦合。
4. 保留源码构建 Compose 作为开发/离线 fallback，不作为生产默认路径。

## 已确认的复杂度来源

| 位置 | 现状 | 影响 |
| --- | --- | --- |
| `deploy_artifact/README.md` | 需要解压 config、release、solver 三类包，并维护 `GROK2API_DEPLOY_DIR`、`GROK2API_RELEASE_DIR`、`GROK2API_DATA_DIR` | 首次部署步骤多，路径错误概率高 |
| `deploy_artifact/docker-compose.runtime.yml` | 主服务通过 5 个 bind mount 读取 release/config/data，另外还有 browser、solver 两个服务 | 发布目录和部署目录强耦合，升级要切 symlink 或改环境变量 |
| `deploy_artifact/docker-compose.runtime.yml` | 文档要求 `up -d --build` | 每台机器重复构建基础运行镜像 |
| `deploy_artifact/Dockerfile.runtime` | 在构建阶段安装完整 `requirements.protocol.lock` | 首次部署受 PyPI/网络/CPU 影响，失败后通常需要重试整个 compose |
| `config.docker.example.yaml` + `.env.example` | 应用设置、容器路径、代理、solver endpoint 分散在 YAML 和 env | 同一个部署参数需要在多个文件中理解和修改 |
| `docker/entrypoint.sh` | 启动时复制配置、创建目录、初始化 registration JSON、chown/chmod | 配置文件路径和容器内路径再增加一层间接关系 |
| 根目录 `docker-compose.yml` | 另有一套源码构建/官方镜像 Compose | 用户需要先判断应该使用哪套 Compose |

## 推荐的目标形态

### 生产快路径

发布 CI 产出两个镜像：

- `ghcr.io/chenyme/grok2api:<version>`：应用、前端、registration worker、Python venv。
- `ghcr.io/owen891/grok2api:solver-<pin>`：现有 solver 镜像，继续固定 digest/tag。

生产 Compose 只保留镜像引用，不再使用 `build:` 和 release bind mounts。应用只挂载：

- `./config.yaml:/run/grok2api/config.yaml:ro`
- `./data:/app/data`

版本升级变成：修改一个 `GROK2API_IMAGE`，执行 `docker compose pull && docker compose up -d`。不再需要 release 目录、current symlink、`server-v3-release.tar.gz` 或目标机上的 Python 依赖安装。

### 配置快路径

应用已有 `defaultConfig()`，绝大多数 YAML 项都有代码默认值。生产模板可以缩减为只保留：

仓库已提供可复制的起点：[`config.minimal.example.yaml`](config.minimal.example.yaml)。

```yaml
server:
  listen: "0.0.0.0:8000"
frontend:
  publicApiBaseURL: "https://api.example.com"
secrets:
  jwtSecret: "..."
  credentialEncryptionKey: "..."
bootstrapAdmin:
  username: "admin"
  password: "..."
registration:
  enabled: true
provider:
  web:
    browserWorkerURL: "http://grok-web-browser:8192"
```

SQLite、memory store、local media、超时、并发、provider 默认值由后端填充。只有切换 PostgreSQL/Redis、代理模式或调整容量时才增加配置项。`config.example.yaml` 仍可保留作为完整 reference。

### 一条命令的安装入口

在部署包中增加 `install.sh`（或 Windows 管理机对应的 `install.ps1`），负责：

1. 检查 Docker Compose 版本。
2. 创建 `data/` 和最小 `config.yaml`。
3. 首次生成随机 JWT/key，并提示管理员密码。
4. 拉取应用、browser、solver 镜像。
5. 执行 `docker compose up -d`。
6. 等待 `/healthz`，输出登录地址和日志命令。

脚本必须幂等：已有 secret、config、data 时不覆盖；solver 镜像拉取失败时给出明确的离线 `docker load` fallback。

## 分阶段落地

### P0：马上能做，不改业务代码

- 生产文档默认改成 `docker compose pull && docker compose up -d`，明确 `--build` 只用于源码开发或镜像缺失时。
- 固定应用镜像 digest/tag，避免 `latest` 漂移。
- 把三份压缩包合并为一个 deployment bundle，至少把 `.env.example`、最小 config、Compose 和 install script 放在同一目录。
- 将运行数据统一放在 `./data`，不再要求用户理解 release/config/data 三个宿主机目录。

### P1：一次性改发布流水线

- 在 GHCR workflow 中发布 runtime image；复用现有 `Dockerfile` 的三阶段构建，直接把 Python venv 放进最终镜像。
- 为 amd64/arm64 生成 manifest，保留现有多架构策略。
- 生产 Compose 删除 `build:`、release bind mounts 和 `GROK2API_RELEASE_DIR`。
- 应用升级只拉镜像；配置和 data 不随版本包替换。

### P2：进一步降低认知负担

- 增加最小配置模板和 `install.sh`。
- 将 `REGISTRATION_*` 的容器内部固定路径移入镜像/Compose 默认值，只把代理和 endpoint 留给 `.env`。
- 用 Compose profiles 让 solver 成为 `registration` profile；不使用注册功能的实例只启动应用和 browser。

## 不建议的方向

- 不建议把 browser worker 或 solver 塞进同一个应用容器：浏览器生命周期、共享内存和故障隔离会变差。
- 不建议直接删除 PostgreSQL/Redis 配置能力；它们对多实例部署有价值，只应从默认模板隐藏。
- 不建议让所有配置都改成环境变量；当前 YAML 结构更适合审计和持久化，环境变量只保留部署差异和 secrets。

## 验收标准

- 新机器已安装 Docker 后，生产首次启动只需一个目录、一个配置文件和一条启动命令。
- 正常版本升级不执行 `docker build`，不触碰 data/config。
- 应用镜像已经包含 Python venv，启动日志中不再出现依赖安装。
- `docker compose ps` 中服务健康后，`curl http://127.0.0.1:<port>/healthz` 成功。
- 关闭 registration profile 后，普通 API/Web 会话仍可用。

## Runtime boundary correction

The prebuilt application image should contain the Go binary, frontend assets,
registration source, and its locked Python virtualenv. This removes Python from
the host and removes dependency installation from deployment.

PostgreSQL and Redis are different: they are stateful runtime services, not
build dependencies. In a standard Compose deployment they run as their own
containers with their own volumes. The application connects to `postgres:5432`
and `redis:6379` over the Compose network. A managed PostgreSQL/Redis service is
also valid; in that case remove those two Compose services and put the managed
DSNs in the production config.

The repository now provides `compose.production.yml` for this standard topology
and `compose.registration.yml` as an explicit Docker-clearance overlay. The root
`docker-compose.yml` remains the SQLite/memory single-host shortcut.
