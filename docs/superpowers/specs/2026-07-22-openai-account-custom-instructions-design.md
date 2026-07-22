# OpenAI 账号级自定义 Instructions 设计

## 背景

Sub2API 当前会为 OpenAI Responses 请求保留或合成顶层 `instructions`，并支持一套全局 Codex instructions 模板，但没有账号级配置。管理员需要为某个 OpenAI 账号配置一段固定 instructions，同时保留客户端原始指令，并让 `/v1/responses` 与 `/v1/chat/completions` 两个入口行为一致。

本功能是通用的账号级请求定制能力，不绑定具体账号名称、分组或模型映射。

## 目标

- 在 OpenAI 账号编辑界面提供可选的“自定义 Instructions”多行文本配置。
- 将配置存入账号 `credentials` JSON，字段名为 `openai_custom_instructions`，不增加数据库列。
- 对 OpenAI OAuth 与 OpenAI API Key 账号生效。
- 同时覆盖原生 `/v1/responses` 和由 `/v1/chat/completions` 转换得到的 Responses 请求。
- 保留客户端最终 instructions，并将账号 instructions 追加到末尾。
- 保证重试、故障转移和同一请求的多阶段转换不会重复追加。
- 复用现有账号更新与 scheduler outbox 机制，使多节点在配置更新后获得新配置。

## 非目标

- 不提供分组级或全局级配置。
- 不改变模型映射、账号分组、计费模型或调度资格。
- 不修改用户输入、工具定义或响应正文。
- 不把该配置视为秘密；管理员不得在其中填写 Token、API Key 等敏感信息。
- 不保证模型一定服从身份或风格指令；它仍受上游模型与其他高优先级约束影响。

## 数据模型与管理界面

账号配置保存在现有 `accounts.credentials` JSON 中：

```json
{
  "openai_custom_instructions": "账号级固定指令"
}
```

前端仅在 OpenAI 账号的创建和编辑表单显示多行文本框：

- 标签：`自定义 Instructions`
- 空值表示关闭该能力，保存时删除该字段。
- 帮助文本明确说明：内容会追加到客户端 instructions 末尾，可能在上游或 Responses 返回结构中可见，不得填写敏感信息。
- 保留换行，仅使用首尾空白裁剪后的值判断是否启用。
- UTF-8 内容最大为 16 KiB；创建和更新接口执行同一校验，前端同步显示限制。

批量编辑不纳入本期范围，避免误把同一身份指令覆盖到多个不同账号。

## 请求处理

### 统一合并规则

新增一个独立、无副作用的合并函数，输入请求体和账号配置，输出修改后的请求体与是否发生修改：

1. 账号字段为空或仅包含空白：不修改。
2. 客户端最终 instructions 为空：写入账号 instructions。
3. 客户端最终 instructions 非空：写入 `客户端 instructions + "\n\n" + 账号 instructions`。
4. 如果最终 instructions 已经以完全相同的账号 instructions 结尾：不再次追加。
5. 如果请求中的 instructions 不是字符串，沿用当前规范化/校验行为，不用账号配置静默掩盖非法输入。

### 执行时机

账号 instructions 必须在该账号已经被选中，并且现有 Codex/OAuth、Chat Completions 桥接、JSON Schema 兼容等 instructions 转换完成之后执行；它是发送上游之前最后一项 instructions 合并操作。

这样可以保证：

- 客户端和现有兼容逻辑的指令不丢失。
- 账号指令稳定处于末尾。
- 模型映射只影响 `model`，不会影响是否注入。
- 故障转移到另一个账号时，使用新账号自己的配置重新构造请求，不继承前一账号的定制内容。

### 入口覆盖

- `/v1/responses`：在 OpenAI 上游请求最终定型前合并。
- `/v1/chat/completions`：先完成现有 Chat Completions → Responses 转换，再执行同一个合并函数。
- Responses WebSocket：必须应用同一合并函数；若当前使用独立构造路径，则在该路径发送上游前显式调用，保证 HTTP 与 WebSocket 行为一致。
- `/responses/compact`：默认同样应用，因为它仍属于该账号的 Responses 请求；不得覆盖 compact 专属模型映射或其他字段。

## 缓存与多节点

`openai_custom_instructions` 随账号 credentials 进入现有调度快照。管理端更新账号时继续使用现有 `account_changed` scheduler outbox 事件，无需新增 Redis 键或数据库迁移。

instructions 本身会参与当前会话种子和 prompt cache key 计算。相同账号配置产生稳定后缀；配置变化后自然形成不同缓存身份，避免复用旧指令缓存。

## 错误处理与可观测性

- 合并失败时拒绝当前请求，不发送半修改请求到上游。
- 日志只记录账号 ID、是否注入和文本长度，不记录 instructions 正文。
- 不在错误信息中回显账号 instructions。
- 超过 16 KiB 的值由创建和更新接口返回明确的参数校验错误；不新增数据库约束。

## 测试

后端单元测试覆盖：

- 空配置不修改请求。
- 原 instructions 为空时直接写入。
- 原 instructions 非空时以双换行追加。
- 已追加时保持幂等。
- 非字符串 instructions 不被静默覆盖。
- 账号 A 的配置不会泄漏到故障转移后的账号 B。

入口集成测试覆盖：

- 原生 `/v1/responses` OAuth 请求。
- 原生 `/v1/responses` API Key 请求。
- `/v1/chat/completions` 转 Responses 请求。
- streaming 与 non-streaming 请求。
- `/responses/compact`。
- Responses WebSocket 请求。

前端测试覆盖：

- OpenAI 创建/编辑账号时字段显示、回显和保存。
- 清空字段后从 credentials 删除。
- 非 OpenAI 账号不显示该字段。
- 帮助文本提示内容非秘密。

## 验收标准

- 配置该字段的 OpenAI 账号收到的上游请求，其最终 instructions 保留客户端内容并以账号内容结尾。
- 两个用户入口及流式/非流式行为一致。
- 未配置账号的请求字节级行为保持不变。
- 更新账号后所有部署节点无需重启即可读取新配置。
- 全量相关后端与前端测试通过。
