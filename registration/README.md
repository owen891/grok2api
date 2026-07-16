# 内置注册执行器

该目录包含 Grok2API 管理后台使用的浏览器注册执行器。日常操作入口为
`/admin/registration`，不要直接修改运行期账号文件。

运行期数据统一写入 `${DATA_DIR}/registration`：

- `config.json`：邮箱、代理和 CPA 回传配置
- `accounts_cli.txt`：注册结果账本
- `protocol_accounts.jsonl`：协议引擎追加式成功账本
- `jobs/*.json`：协议注册阶段 checkpoint；服务重启后优先续跑 SSO/OAuth/导入阶段
- `cpa_auths/`：CPA OAuth 导出文件
- `cookies/`、`screenshots/`：浏览器调试产物

源码目录只保留执行代码、`cpa_xai` 和 `turnstilePatch`。网站启动注册任务前会执行
依赖预检，并将 CPA 导入地址同步为当前 Grok2API Admin API。

协议引擎以“完成凭据导入和首次同步”的账号数作为任务进度。协议 OAuth 失败时会复用
checkpoint 中的同一账号进入 DrissionPage OAuth 回退；凭据已生成但导入失败时只重新发布
凭据，不重复注册账号。

