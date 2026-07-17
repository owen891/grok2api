# 内置注册执行器

该目录包含 Grok2API 管理后台使用的纯协议注册执行器。日常操作入口为
`/admin/registration`，不要直接修改运行期账号文件。

运行期数据统一写入 `${DATA_DIR}/registration`：

- `config.json`：邮箱、代理和 CPA 回传配置
- `protocol_accounts.jsonl`：追加式成功账本
- `jobs/*.json`：协议注册阶段 checkpoint；服务重启后优先续跑 SSO/OAuth/导入阶段
- `cpa_auths/`：CPA OAuth 导出文件

容器只打包协议执行代码、`cpa_xai` 凭据 schema/writer 和 HTTP 依赖，不安装 Chromium、
DrissionPage 或图形运行环境。网站启动注册任务前会执行协议依赖预检，并将凭据导入地址
同步为当前 Grok2API 的受保护 spool。

执行器以“完成凭据导入和首次同步”的账号数作为任务进度。SSO/OAuth 失败时会从 checkpoint
续跑同一账号；凭据已生成但导入失败时只重新发布凭据，不重复注册账号。
