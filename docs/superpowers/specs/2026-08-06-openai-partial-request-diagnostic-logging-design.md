# OpenAI `partial` 请求诊断日志设计

## 目标

临时记录导致 OpenAI 兼容上游返回 `MissingParameter: partial` 的完整请求 JSON，以便比较客户端原始请求与 sub2api 实际发送的请求，定位 `partial` 字段在哪一层缺失或被删除。

## 范围

- 仅修改 OpenAI Responses HTTP 转发路径。
- 不按用户、API Key 或账号过滤；任何上游命中目标错误的请求都记录。
- 不记录请求头、Authorization、账号凭证或代理配置。
- 不改变请求转换、重试、路由和错误返回行为。
- 这是临时诊断代码，完成问题定位后删除。

## 触发条件

必须同时满足：

1. 上游 HTTP 状态码为 `400`；
2. 错误代码不区分大小写等于 `MissingParameter`；
3. `error.param` 不区分大小写等于 `partial`，或错误消息明确表示缺少 `partial` 参数。

普通请求和其他错误不打印请求体。

## 日志内容

命中时连续输出两条结构化 WARN 日志：

- `openai.partial_debug.client_body`：进入当前 OpenAI Responses 转发调用时的客户端请求 JSON；
- `openai.partial_debug.upstream_body`：本次失败时实际发送给上游的 JSON。

两条日志均携带 Request ID、账号 ID、账号名称、原始模型、上游模型、请求体字节数和 SHA-256。完整 JSON 放在单独字段中，便于按 Request ID 检索和对比。

客户端请求体仅保留一个只读引用直到本次转发结束，不额外复制大字节数组。日志只在目标错误发生时序列化输出。

## 安全与运维

- 日志会包含完整提示词、工具参数以及可能存在的 Base64 附件；仅用于公司内部故障诊断。
- 不打印任何 HTTP 请求头。
- 使用已有容器日志轮转，不额外永久建表或写业务数据库。
- 抓到可复现样本后，按 Request ID 导出日志，随后删除诊断代码并重新部署。

## 错误处理

诊断日志本身不得影响用户请求。即使日志字段构造失败，也继续执行原有错误处理；不得因此重试、修改状态码或覆盖上游错误。

## 测试

- `400 + MissingParameter + partial` 会触发诊断判定。
- 错误码大小写变化仍能触发。
- `error.param` 缺失但消息明确缺少 `partial` 时触发。
- 非 400、其他错误码、其他参数均不触发。
- 转换前后请求体不同的集成用例中，诊断载荷能同时保留两份不同 JSON。

