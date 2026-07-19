# Egress / Cloudflare 反复不可用分析

## 结论先说

这次截图对应的主因不是“Cloudflare 偶发抽风”，而是 `grok_web` 节点的代理连接失败。现有系统又把代理失败、浏览器 worker 故障、Cloudflare Cookie 失效都压成 `egress_unavailable`，所以用户只能看到同一句提示，无法知道该修代理、worker 还是 Cookie。

## 现网证据

本地运行日志在以下时间重复出现 Chromium `ERR_PROXY_CONNECTION_FAILED`：

- `v3-preview-system.out`：2026-07-17 19:51:56
- `v3-preview-system.out`：2026-07-17 20:06:57
- `v3-preview-system.out`：2026-07-17 20:21:57

这些错误发生在 `web_statsig_warmup_failed`，说明浏览器 worker 已启动，但它使用的代理无法建立连接。数据库当前节点摘要：

| 节点 | scope | enabled | health | failureCount | lastError | Cookie |
| --- | --- | ---: | ---: | ---: | --- | --- |
| `local-grok-web` | `grok_web` | 1 | 0.245 | 2 | `transport error` | 已配置 |
| `Local HTTP proxy` | `grok_build` | 1 | 0.343 | 3 | `transport error` | 不适用 |

因此当前 Web 节点不是缺 Cookie，而是代理出口不可达；Cookie 只解决 Cloudflare 身份，不能修复 `ERR_PROXY_CONNECTION_FAILED`。

## 失败链路

1. `web.Adapter.openLiteImageWithBrowser` 从 egress manager 取得 `grok_web` lease。
2. Go 把 `ProxyURL`、UA、Cookie、SSO token 发给 browser worker。
3. Chromium 通过该代理访问 `grok.com`。
4. 代理连接失败时 worker 返回 `proxy_unavailable`。
5. Go 做两次短重试；随后生成 `UnavailableError`。
6. 图片服务立即返回 `egress_unavailable`，不会继续尝试其他账号。
7. 前端把这个 code 显示成“代理出口或 Cloudflare 会话不可用”。

当前实现还有三个放大器：

- worker 不可用和代理不可用使用同一个错误类型。
- `browser_unavailable` 不会让节点进入冷却，下一次仍可能选中同一节点。
- worker 进程内的 Chromium session 指纹原来只包含 proxy 和 UA；管理员更新 Cookie 后，旧 `cf_clearance` 可能继续留在活跃浏览器里。

## 已实施修复

- `scripts/grok_web_browser_worker.py`
  - session fingerprint 现在包含规范化后的 Cloudflare Cookie 集合。
  - Cookie 更新会自动重启 Chromium 并重新注入 Cookie。
- `backend/internal/infra/egress/manager.go`
  - `UnavailableError` 增加 `Reason` 和 `NodeID`。
  - 支持区分 proxy unavailable、browser worker unavailable、cooling。
- `backend/internal/infra/provider/web/browser_worker.go`
  - `proxy_unavailable` 和 worker 进程故障返回不同原因。
- `backend/internal/application/gateway/service.go`
  - worker 故障使用 `browser_worker_unavailable`，不再伪装成出口节点故障。
- `frontend/src/features/chat/chat-error.ts`
  - 对 browser worker 故障显示单独的检查提示。

## 立即止血

1. 在 Settings -> 出口节点中禁用 `local-grok-web`，或改成可从运行 Go 服务的机器访问的代理地址。
2. 在同一个 `grok_web` 节点上重新录入与该代理出口 IP/UA 匹配的 `cf_clearance` 和 `__cf_bm`。
3. 保存后重启 browser worker；新版本会在 Cookie 变化时自动重建 session，但重启可以清掉旧的 Chromium 状态。
4. 从运行环境测试代理，而不是从桌面浏览器测试：

```bash
curl -x http://USER:PASS@HOST:PORT -I --max-time 10 https://grok.com/
```

5. 查看服务日志，确认不再出现 `ERR_PROXY_CONNECTION_FAILED`，再测试 `grok-imagine-image`。

## 推荐的完整方案

### 1. 节点保存时做 Preflight

新增“测试节点”接口和 UI 按钮，至少检查：

- TCP 代理连接
- 通过代理访问 `https://grok.com/`
- Cloudflare 响应是否为 challenge
- browser worker 是否能用该 proxy/UA/Cookie 完成 warm

保存配置不应等价于节点健康；只有 Preflight 成功才把节点置为可用。

### 2. 增加节点状态机

建议状态：`unknown`、`healthy`、`proxy_failed`、`cloudflare_expired`、`worker_failed`、`disabled`。

- `proxy_failed`：只冷却该节点，指数退避并显示最后一次探测时间。
- `cloudflare_expired`：不要每 30 秒重试，要求重新录入 Cookie 或人工确认。
- `worker_failed`：使用全局 worker 熔断器，不要污染所有 egress 节点健康度。
- 连续失败达到阈值后自动禁用，避免每隔几分钟重复打 Cloudflare。

### 3. 让请求真正具备节点级 failover

当前图片路径只在同一 lease 内做两次尝试。应改为：

- 代理失败后立即释放当前节点并排除该 NodeID。
- 在同一请求预算内选择下一个健康节点。
- 所有节点都失败时才返回 `egress_unavailable`。
- 错误响应附带 `reason`、`node_id`、`retry_after`，不暴露代理凭据。

### 4. 降低启动风暴

启动时 42 个账号并发 warm 会把一个坏代理放大成大量 Cloudflare/worker 日志。建议：

- warmup 按节点去重，而不是按账号重复建立浏览器会话。
- 单节点 warm 失败后暂停该节点，不继续对所有账号重试。
- startup catch-up 使用有界并发和指数退避。

### 5. 监控指标

至少记录并告警：

- `egress_probe_success/failure{node,reason}`
- `browser_worker_requests{code}`
- `cloudflare_challenge_total{node}`
- `proxy_connection_failed_total{node}`
- 节点最后成功时间、连续失败次数、冷却截止时间

这样下次不会只能从用户截图反推根因。

## 验收标准

- 坏代理只影响对应节点，健康节点仍能完成图片请求。
- worker 宕机显示 worker 专用错误，不把所有出口标红。
- 更新 Cookie 后无需手工清浏览器 profile，下一次请求使用新 Cookie。
- 同一坏节点不再每隔几分钟被重复选中。
- 所有节点失败时日志能明确指出是 `proxy_failed`、`cloudflare_expired` 还是 `worker_failed`。

