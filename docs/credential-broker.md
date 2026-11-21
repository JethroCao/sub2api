# OpenAI Access Token 节点接口（47 端）

此功能默认关闭，只给明确授权的节点提供 Access Token（AT）。不导出 Refresh Token、ID Token、账号完整配置或代理信息，不转发模型请求，不管理调用方的用户、密钥或用量统计。

## 启用前提

- 47 端必须仍拥有账号的原生 OAuth 刷新凭据，账号处于可调度状态；Home、影子、PAT、API Key 和 Agent 账号不可用。
- 两台 47 节点应使用相同数据库、Redis 和客户端配置。Redis 不可用时接口失败关闭，不降级为无锁刷新。
- 首次升级时先保持接口关闭，等所有共享账号的原生 Sub2 节点升级且旧版在途刷新结束后再启用；旧版本不具备带所有者校验和自动续期的刷新锁。
- 必须用 HTTPS。若由 Nginx 等终止 TLS，仅把实际直连代理的地址加入本功能自己的 `trusted_proxy_cidrs`，并确保代理**覆盖**客户端传入的 `X-Forwarded-Proto`。不要填客户端 IP 或宽泛网段；应用端口应只允许代理访问。
- 调用方拿到 AT 后直接请求 OpenAI；此接口不是 OpenAI API Base URL。107 端的自动取用/缓存功能仍需单独实现。

## 配置

在现有 `config.yaml` 中加入（以下均为占位内容）：

```yaml
credential_broker:
  enabled: false
  trusted_proxy_cidrs:
    - "127.0.0.1/32" # 仅当反向代理确实从此地址直连应用
  clients:
    - id: node-107
      key_sha256: "替换为专用节点密钥的64位SHA256摘要"
      account_ids: [4, 14] # 必须逐个授权，无通配符
```

使用密码管理器或 `openssl rand -hex 32` 生成独立随机密钥。原文仅存调用方的机密配置；47 端保存原文的 SHA-256 十六进制摘要（例如从安全的标准输入传给 `openssl dgst -sha256`，不要加入换行）。不要复用管理员密钥、用户 API Key 或上游凭据。不要将密钥放在 URL、截图、仓库或日志中。`enabled: true` 后重启应用才生效；配置无效会阻止启动。

每个客户端最多 60 次/分钟。客户端 ID 是 Redis 限流的稳定标识，切勿为同一节点不断创建新 ID 绕过限流。轮换密钥需更新所有 47 实例的摘要配置，并同步更新调用方；首版无动态管理页面。

## 调用协议

```http
POST /api/v1/credential-broker/openai/4/token
Authorization: Bearer <专用节点密钥原文>
Content-Type: application/json

{}
```

请求体不超过 2 KiB；只接受可选的 `rejected_revision` 字段。成功示例：

```json
{
  "account_id": 4,
  "access_token": "<AT>",
  "expires_at": "2026-11-02T12:00:00Z",
  "chatgpt_account_id": "<上游账号ID>",
  "revision": "<64位小写十六进制版本>"
}
```

所有响应均为 `Cache-Control: no-store`。调用方应仅在内存安全缓存 AT，到期前更新，不应每次推理都拉取。缓存应按 47 本地账号 ID 隔离。接口提供的是原账号 AT，不能缩小该 AT 在 OpenAI 上游的权限，所以仅供可信节点使用。

若 OpenAI 返回 401，携带这次被拒绝 AT 对应的版本：

```json
{"rejected_revision":"<之前返回的revision>"}
```

若版本已经更新，直接取得新 AT；只有被拒绝版本仍为当前版本时才尝试刷新。此类强制刷新全局每账号最多一次/30 秒。不要无限重试 401，也不要把其他账号的版本用于刷新。

## 错误与边界

错误体固定为 `{"error":{"type":"<code>","message":"<code>"}}`，不包含原始上游错误。

| HTTP | 含义 | 调用方处理 |
| --- | --- | --- |
| 400/413 | 参数非法或请求过大 | 修正请求，不自动重试 |
| 401/403 | 节点鉴权失败或不是可信 HTTPS | 修正密钥/传输配置 |
| 404 | 未启用、账号不存在或无权访问 | 不枚举账号；检查白名单 |
| 429 | 节点限流/刷新节流 | 遵守 `Retry-After` |
| 502 | 上游刷新失败 | 有界退避；必要时人工重新授权 |
| 503 | 依赖不可用、刷新进行中或账号不可用 | 遵守 `Retry-After`，不要热循环 |

此接口不共享推理并发名额。47 与调用方分别发起推理时，真实上游额度和并发仍叠加；后续若需全局账号并发限制，应另加租约机制。禁用接口/移除授权只阻止再次取 AT，不能收回已经发给节点的 AT；需要立即撤销时必须在上游撤销授权或等待 AT 过期。
