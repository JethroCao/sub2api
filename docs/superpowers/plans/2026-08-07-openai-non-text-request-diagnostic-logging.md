# OpenAI 非文本请求错误诊断日志 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在火山上游明确拒绝非文本输入时，定向记录客户端原始 JSON 与实际发送给上游的完整 JSON，以便根据真实结构修复漏判路由。

**Architecture:** 新增一个只负责错误匹配与双请求体日志的独立 service 模块，并在 OpenAI HTTP 转发的上游 400 处理路径调用。诊断逻辑只观察、不修改请求、重试、调度或响应；部署使用 Git SHA 镜像标签逐台更新杭州节点。

**Tech Stack:** Go 1.26、Gin、gjson、zap/zaptest observer、Docker Buildx、Docker Compose

## Global Constraints

- 仅匹配 HTTP 400 且消息包含 `Model only support text input` 或 `Model do not support image input`，匹配忽略大小写。
- 每次命中只输出两条 Warn 日志：`body_kind=client` 与 `body_kind=upstream`。
- 日志事件名固定为 `openai.non_text_input_request_body`。
- 完整请求体只写容器日志，不写 PostgreSQL、不写临时文件，并设置 `logger.OpsSystemLogSkipField=true`。
- 正常请求及其他错误不得记录请求体。
- 本变更不修改图片检测规则、账号调度、自动重试或客户端错误响应。
- 部署目标仅为杭州 `120.55.76.195` 和 `121.41.9.209`。

## File Structure

- Create: `backend/internal/service/openai_non_text_request_diagnostic.go` — 目标错误识别、公共元数据和 client/upstream 双请求体日志。
- Create: `backend/internal/service/openai_non_text_request_diagnostic_test.go` — 纯函数匹配、日志字段和 Forward 接入回归测试。
- Modify: `backend/internal/service/openai_gateway_forward.go` — 在已读取上游错误正文后调用诊断器。
- Create: `docs/superpowers/plans/2026-08-07-openai-non-text-request-diagnostic-logging.md` — 本实施计划。

---

### Task 1: 目标错误匹配与双请求体日志模块

**Files:**
- Create: `backend/internal/service/openai_non_text_request_diagnostic.go`
- Create: `backend/internal/service/openai_non_text_request_diagnostic_test.go`

**Interfaces:**
- Produces: `isOpenAINonTextInputUnsupportedError(statusCode int, upstreamMsg string, upstreamBody []byte) bool`
- Produces: `logOpenAINonTextInputRequestBodies(ctx context.Context, c *gin.Context, account *Account, upstreamStatusCode int, upstreamMsg string, upstreamErrorBody, clientBody, failedUpstreamBody []byte, originalModel, upstreamModel string)`
- Consumes: `extractUpstreamErrorMessage`, `logger.FromContext`, `getAPIKeyIDFromContext`, `logger.OpsSystemLogSkipField`

- [ ] **Step 1: 写错误匹配的失败测试**

在 `backend/internal/service/openai_non_text_request_diagnostic_test.go` 添加表驱动测试：

```go
func TestIsOpenAINonTextInputUnsupportedError(t *testing.T) {
	tests := []struct {
		name string
		status int
		message string
		body string
		want bool
	}{
		{"text only", http.StatusBadRequest, "Model only support text input Request id: rid", `{"error":{"code":"InvalidParameter"}}`, true},
		{"image unsupported", http.StatusBadRequest, "", `{"error":{"message":"Model do not support image input.","param":"image_url"}}`, true},
		{"case insensitive", http.StatusBadRequest, "MODEL ONLY SUPPORT TEXT INPUT", `{}`, true},
		{"wrong status", http.StatusBadGateway, "Model only support text input", `{}`, false},
		{"unrelated invalid parameter", http.StatusBadRequest, "Unknown parameter: namespace", `{}`, false},
		{"ordinary text wording", http.StatusBadRequest, "text input is malformed", `{}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isOpenAINonTextInputUnsupportedError(tt.status, tt.message, []byte(tt.body)))
		})
	}
}
```

- [ ] **Step 2: 运行测试并确认因函数不存在而失败**

Run: `cd backend && go test ./internal/service -run '^TestIsOpenAINonTextInputUnsupportedError$' -count=1`

Expected: FAIL，提示 `undefined: isOpenAINonTextInputUnsupportedError`。

- [ ] **Step 3: 实现最小错误匹配函数**

在新文件中添加：

```go
const openAINonTextInputRequestBodyLogMessage = "openai.non_text_input_request_body"

func isOpenAINonTextInputUnsupportedError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	message := strings.TrimSpace(upstreamMsg)
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.message").String())
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "model only support text input") ||
		strings.Contains(lower, "model do not support image input")
}
```

- [ ] **Step 4: 运行匹配测试并确认通过**

Run: `cd backend && go test ./internal/service -run '^TestIsOpenAINonTextInputUnsupportedError$' -count=1`

Expected: PASS。

- [ ] **Step 5: 写双请求体日志的失败测试**

使用 `zaptest/observer` 构造 Warn logger，调用 `logOpenAINonTextInputRequestBodies`，断言：

```go
entries := observed.FilterMessage(openAINonTextInputRequestBodyLogMessage).All()
require.Len(t, entries, 2)
require.Equal(t, "client", entries[0].ContextMap()["body_kind"])
require.Equal(t, string(clientBody), entries[0].ContextMap()["request_body"])
require.EqualValues(t, len(clientBody), entries[0].ContextMap()["request_body_bytes"])
require.NotEmpty(t, entries[0].ContextMap()["request_body_sha256"])
require.EqualValues(t, 125, entries[0].ContextMap()["api_key_id"])
require.Equal(t, "upstream", entries[1].ContextMap()["body_kind"])
require.Equal(t, string(upstreamBody), entries[1].ContextMap()["request_body"])
require.Equal(t, "InvalidParameter", entries[1].ContextMap()["upstream_error_code"])
```

再调用一次无关 400 错误，断言 observer 没有新事件。

- [ ] **Step 6: 运行日志测试并确认因函数不存在而失败**

Run: `cd backend && go test ./internal/service -run '^TestLogOpenAINonTextInputRequestBodies' -count=1`

Expected: FAIL，提示 `undefined: logOpenAINonTextInputRequestBodies`。

- [ ] **Step 7: 实现双请求体日志函数**

实现与 `openai_partial_request_diagnostic.go` 相同的元数据结构，但使用独立事件名和 matcher。每条日志必须包含：

```go
zap.Bool(logger.OpsSystemLogSkipField, true)
zap.Int64("account_id", accountID)
zap.String("account_name", accountName)
zap.Int64("api_key_id", getAPIKeyIDFromContext(c))
zap.Int("upstream_status_code", upstreamStatusCode)
zap.String("upstream_error_code", gjson.GetBytes(upstreamErrorBody, "error.code").String())
zap.String("upstream_error_param", gjson.GetBytes(upstreamErrorBody, "error.param").String())
zap.String("upstream_error_message", errorMessage)
zap.String("original_model", strings.TrimSpace(originalModel))
zap.String("upstream_model", strings.TrimSpace(upstreamModel))
zap.String("body_kind", bodyKind)
zap.Int("request_body_bytes", len(body))
zap.String("request_body_sha256", fmt.Sprintf("%x", sha256.Sum256(body)))
zap.ByteString("request_body", body)
```

当 `ctx == nil` 时使用 `context.Background()`；当 `account == nil` 时记录零 ID 和空名称，不得 panic。

- [ ] **Step 8: 运行模块测试并确认通过**

Run: `cd backend && go test ./internal/service -run '^(TestIsOpenAINonTextInputUnsupportedError|TestLogOpenAINonTextInputRequestBodies)' -count=1`

Expected: PASS。

- [ ] **Step 9: 提交诊断模块**

```bash
git add backend/internal/service/openai_non_text_request_diagnostic.go backend/internal/service/openai_non_text_request_diagnostic_test.go
git commit -m "feat(openai): log non-text input error request bodies"
```

---

### Task 2: 接入 OpenAI Forward 上游错误路径

**Files:**
- Modify: `backend/internal/service/openai_gateway_forward.go:920-950`
- Modify: `backend/internal/service/openai_non_text_request_diagnostic_test.go`

**Interfaces:**
- Consumes: Task 1 的 `logOpenAINonTextInputRequestBodies(...)`
- Produces: 上游目标 400 错误命中时，Forward 恰好输出 client/upstream 两条诊断日志。

- [ ] **Step 1: 写 Forward 接入的失败测试**

复用 `newOpenAIRejectedFieldTestService`、`newOpenAIRejectedFieldTestAccount` 和 `newOpenAIRejectedFieldTestContext`：

```go
func TestOpenAIGatewayForwardLogsClientAndUpstreamBodiesForNonTextInputError(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	clientBody := []byte(`{"model":"gpt-5.5","stream":false,"input":[{"type":"computer_call_output","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,abc"}}]}`)
	errorBody := `{"error":{"code":"InvalidParameter","message":"Model only support text input Request id: rid","param":"","type":"BadRequest"}}`
	upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, errorBody)}
	account := newOpenAIRejectedFieldTestAccount()
	account.ID = 12
	account.Name = "火山引擎deepseek"
	account.Credentials["model_mapping"] = map[string]any{"gpt-5.5": "ep-deepseek"}
	c := newOpenAIRejectedFieldTestContext(clientBody)
	c.Set("api_key", &APIKey{ID: 125})

	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(ctx, c, account, clientBody)
	require.Error(t, err)
	require.Nil(t, result)
	require.Len(t, upstream.bodies, 1)
	require.Equal(t, "ep-deepseek", gjson.GetBytes(upstream.bodies[0], "model").String())

	entries := observed.FilterMessage(openAINonTextInputRequestBodyLogMessage).All()
	require.Len(t, entries, 2)
	require.Equal(t, string(clientBody), entries[0].ContextMap()["request_body"])
	require.Equal(t, string(upstream.bodies[0]), entries[1].ContextMap()["request_body"])
}
```

- [ ] **Step 2: 运行接入测试并确认没有日志而失败**

Run: `cd backend && go test ./internal/service -run '^TestOpenAIGatewayForwardLogsClientAndUpstreamBodiesForNonTextInputError$' -count=1`

Expected: FAIL，`entries` 长度为 0 而不是 2。

- [ ] **Step 3: 在 Forward 错误路径调用诊断器**

在 `openai_gateway_forward.go` 已计算 `upstreamMsg` 后、现有 `logOpenAIPartialMissingRequestBodies(...)` 相邻位置调用：

```go
logOpenAINonTextInputRequestBodies(
	ctx,
	c,
	account,
	resp.StatusCode,
	upstreamMsg,
	respBody,
	clientEntryBody,
	body,
	originalModel,
	upstreamModel,
)
```

不得把调用放到模型映射或兼容转换之前；`body` 必须是本次失败请求实际发送的正文，`clientEntryBody` 必须保持入口原文。

- [ ] **Step 4: 运行接入测试并确认通过**

Run: `cd backend && go test ./internal/service -run '^TestOpenAIGatewayForwardLogsClientAndUpstreamBodiesForNonTextInputError$' -count=1`

Expected: PASS。

- [ ] **Step 5: 运行相关 OpenAI service 测试**

Run: `cd backend && go test ./internal/service -run 'OpenAI.*(NonText|Partial|ImageInput|RejectedField)' -count=1`

Expected: PASS；现有 partial、图片检测和 rejected-field 行为不变。

- [ ] **Step 6: 运行完整 service 包测试和格式检查**

Run: `cd backend && gofmt -w internal/service/openai_non_text_request_diagnostic.go internal/service/openai_non_text_request_diagnostic_test.go`

Run: `cd backend && go test ./internal/service -count=1`

Run: `git diff --check`

Expected: 全部 PASS，且 `git diff --check` 无输出。

- [ ] **Step 7: 提交 Forward 接入**

```bash
git add backend/internal/service/openai_gateway_forward.go backend/internal/service/openai_non_text_request_diagnostic_test.go
git commit -m "feat(openai): capture non-text upstream failures"
```

---

### Task 3: 构建镜像并部署杭州双节点

**Files:**
- Modify remotely: `/opt/sub2api/docker-compose.yml` on `120.55.76.195`
- Modify remotely: `/opt/sub2api/docker-compose.yml` on `121.41.9.209`

**Interfaces:**
- Consumes: Task 2 已测试通过的 Git HEAD。
- Produces: `registry.cn-beijing.aliyuncs.com/aevumio/sub2api:<git-sha>` linux/amd64 镜像，以及两个运行相同镜像的健康杭州节点。

- [ ] **Step 1: 最终本地验证并生成镜像标签**

Run: `cd backend && go test ./internal/service -count=1`

Run: `git diff --check`

Run: `git status --short --branch`

Run: `git rev-parse --short=9 HEAD`

Expected: 测试 PASS；除已知 `.superpowers/` 未跟踪临时文件外无未提交业务改动；得到 9 位 SHA 标签。

- [ ] **Step 2: 用本机 Docker 构建并推送 linux/amd64 镜像**

使用本机已登录的阿里云仓库和本机 Docker 代理 `127.0.0.1:7890`：

```bash
IMAGE="registry.cn-beijing.aliyuncs.com/aevumio/sub2api:$(git rev-parse --short=9 HEAD)"
docker buildx build --platform linux/amd64 -t "$IMAGE" --push .
```

Expected: buildx 完成并显示 pushed manifest digest；不得使用 `latest` 覆盖旧镜像。

- [ ] **Step 3: 先部署 `120.55.76.195` 并验证**

在 `/opt/sub2api/docker-compose.yml` 中仅把 sub2api image 标签替换为新 SHA，然后执行：

```bash
cd /opt/sub2api
docker compose pull sub2api
docker compose up -d --no-deps sub2api
docker inspect sub2api --format '{{.Config.Image}}|{{.State.Status}}|{{.State.Health.Status}}'
docker logs --since 2m sub2api
```

Expected: 镜像标签为新 SHA，容器 `running|healthy`，启动日志没有 migration、panic 或数据库连接错误。

- [ ] **Step 4: 部署 `121.41.9.209` 并验证**

重复 Step 3，仅目标主机改为 `121.41.9.209`。

Expected: 镜像标签为新 SHA，容器 `running|healthy`，启动日志正常。

- [ ] **Step 5: 验证定向日志未污染正常请求**

分别检查两个节点最近五分钟日志：

```bash
docker logs --since 5m sub2api 2>&1 | grep 'openai.non_text_input_request_body'
```

Expected: 没有目标上游错误时无输出；若刚好命中错误，则同一 request ID 恰好出现 `body_kind=client` 和 `body_kind=upstream` 两条。

- [ ] **Step 6: 记录上线结果**

交付信息必须包含：Git 提交、镜像标签、两个节点的容器健康状态，以及后续按 request ID 提取日志的命令：

```bash
docker logs --since 24h sub2api 2>&1 | grep -F -C 1 'aabde109-f23e-401f-be43-d89b596bc988'
```

不在本任务中根据假设修改图片检测；等待下一次真实错误正文后再建立针对性回归测试和修复。
