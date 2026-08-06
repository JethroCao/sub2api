# OpenAI Partial Request Diagnostic Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Print both the client-entry JSON and failed upstream JSON only when an OpenAI Responses upstream returns HTTP 400 `MissingParameter` for `partial`.

**Architecture:** Keep a read-only alias to the body received by `OpenAIGatewayService.Forward`, then call a focused diagnostic helper after reading an upstream HTTP error body. The helper owns exact error matching, hashes and structured WARN emission; the forwarding path keeps all existing behavior unchanged.

**Tech Stack:** Go 1.26, Gin, gjson, zap structured logging, zap observer tests, testify.

## Global Constraints

- Only the OpenAI Responses HTTP forwarding path changes.
- Do not filter by user, API Key or account.
- Do not log request headers, Authorization, credentials or proxy configuration.
- Do not change request conversion, routing, retries, status codes or returned errors.
- Log full request JSON only for the target upstream error.
- The diagnostic code is temporary and must be removed after a useful reproduction is captured.

---

### Task 1: Conditional partial-error request-body diagnostics

**Files:**
- Create: `backend/internal/service/openai_partial_request_diagnostic.go`
- Create: `backend/internal/service/openai_partial_request_diagnostic_test.go`
- Modify: `backend/internal/service/openai_gateway_forward.go:18-25,900-940`

**Interfaces:**
- Consumes: request logger context, account, upstream status/message/error body, client-entry body, failed upstream body, original model and upstream model.
- Produces: `isOpenAIPartialMissingError(statusCode int, upstreamMsg string, upstreamBody []byte) bool` and `logOpenAIPartialMissingRequestBodies(...)`.

- [ ] **Step 1: Write failing predicate and structured-log tests**

Create table tests for HTTP 400 with exact and mixed-case `MissingParameter`, `error.param=partial`, message fallback, and non-matching status/code/parameter cases. Add a zap observer test that injects the logger with `logger.IntoContext`, invokes the logging helper, and asserts exactly two WARN entries named `openai.partial_debug.client_body` and `openai.partial_debug.upstream_body`. Assert both bodies, byte counts, SHA-256 values, account metadata and models are present, with no header or authorization fields.

```go
func TestIsOpenAIPartialMissingError(t *testing.T) {
    body := []byte(`{"error":{"code":"MissingParameter","param":"partial","message":"missing partial"}}`)
    require.True(t, isOpenAIPartialMissingError(http.StatusBadRequest, "", body))
    require.False(t, isOpenAIPartialMissingError(http.StatusUnauthorized, "", body))
}

func TestLogOpenAIPartialMissingRequestBodies(t *testing.T) {
    core, observed := observer.New(zap.WarnLevel)
    ctx := logger.IntoContext(context.Background(), zap.New(core))
    logOpenAIPartialMissingRequestBodies(ctx, nil, &Account{ID: 12, Name: "ark"},
        http.StatusBadRequest, "missing partial", errorBody,
        []byte(`{"input":"client"}`), []byte(`{"input":"upstream"}`), "gpt-5.6-sol", "ep-test")
    require.Len(t, observed.All(), 2)
}
```

- [ ] **Step 2: Run focused tests and verify failure**

Run: `cd backend && go test ./internal/service -run 'Test(IsOpenAIPartialMissingError|LogOpenAIPartialMissingRequestBodies)' -count=1`

Expected: compilation fails because the diagnostic helpers do not exist.

- [ ] **Step 3: Implement the minimal diagnostic helper**

Create these exact signatures:

```go
func isOpenAIPartialMissingError(statusCode int, upstreamMsg string, upstreamBody []byte) bool

func logOpenAIPartialMissingRequestBodies(
    ctx context.Context, c *gin.Context, account *Account,
    upstreamStatusCode int, upstreamMsg string, upstreamErrorBody []byte,
    clientBody []byte, failedUpstreamBody []byte,
    originalModel string, upstreamModel string,
)
```

The predicate requires HTTP 400, accepts case-insensitive `MissingParameter`, and requires either `error.param=partial` or an explicit missing/required `partial` message. The logger computes SHA-256 using `crypto/sha256` and `encoding/hex`, then emits two WARN entries through `logger.FromContext(ctx).With(...)`. Use `zap.ByteString("request_body", body)` and distinct `body_kind` values `client` and `upstream`; rely on the request logger already attached to context for Request ID correlation.

- [ ] **Step 4: Run focused helper tests and verify pass**

Run: `cd backend && go test ./internal/service -run 'Test(IsOpenAIPartialMissingError|LogOpenAIPartialMissingRequestBodies)' -count=1`

Expected: PASS.

- [ ] **Step 5: Write a failing Forward integration test**

Use the existing fake-upstream Forward pattern to return:

```json
{"error":{"code":"MissingParameter","param":"partial","message":"The request failed because it is missing `partial` parameter."}}
```

Use a client body changed by an existing account compatibility normalization before forwarding. Inject an observer logger and assert the client log contains the pre-normalization body while the upstream log contains the transformed body.

- [ ] **Step 6: Run the Forward test and verify failure**

Run: `cd backend && go test ./internal/service -run TestOpenAIGatewayServiceForwardLogsPartialMissingBodies -count=1`

Expected: FAIL because `Forward` has not retained the entry body or invoked the helper.

- [ ] **Step 7: Wire diagnostics into Forward**

At the first line of `Forward`, retain the original slice without copying:

```go
clientEntryBody := body
```

After reading the upstream error body and extracting the sanitized upstream message, invoke the helper with `clientEntryBody` and the current `body`. Do not return or mutate values based on logging.

- [ ] **Step 8: Run focused and package tests**

Run: `cd backend && go test ./internal/service -run 'Test(IsOpenAIPartialMissingError|LogOpenAIPartialMissingRequestBodies|OpenAIGatewayServiceForwardLogsPartialMissingBodies)' -count=1`

Run: `cd backend && go test ./internal/service -count=1`

Expected: PASS.

- [ ] **Step 9: Format and inspect the diff**

Run: `gofmt -w backend/internal/service/openai_partial_request_diagnostic.go backend/internal/service/openai_partial_request_diagnostic_test.go`

Run: `git diff --check`

Inspect the three changed implementation files. Expected: no header logging and no request-path behavior changes.

- [ ] **Step 10: Commit the implementation**

Stage only the two diagnostic files and `openai_gateway_forward.go`, then commit with `debug(openai): log bodies for missing partial errors`.

### Task 2: Build and deploy the temporary diagnostic image to the US server

**Files:**
- Modify: none after Task 1.
- Deploy: `/opt/sub2api/docker-compose.yml` on `47.254.20.208` using the existing image-tag workflow.

**Interfaces:**
- Consumes: committed implementation and existing Docker registry configuration.
- Produces: a healthy US `sub2api` container running the immutable diagnostic image.

- [ ] **Step 1: Run pre-deployment verification**

Run the service package tests, inspect `git status --short`, and confirm the implementation commit is HEAD. Expected: tests PASS; only the known unrelated `.superpowers/` path may remain untracked.

- [ ] **Step 2: Build and push an immutable image**

Use the established local Docker/proxy and registry workflow. Tag with the implementation commit's short SHA and push to `registry.cn-beijing.aliyuncs.com/aevumio/sub2api:<short-sha>`.

Expected: push succeeds and reports a content digest.

- [ ] **Step 3: Deploy only to the US server**

On `47.254.20.208`, back up `/opt/sub2api/docker-compose.yml`, replace only the sub2api image tag, pull and recreate the sub2api service. Leave database and Redis untouched.

Expected: the new container uses the pushed image digest and reaches healthy status.

- [ ] **Step 4: Verify runtime readiness**

Check container health, startup logs, image ID/digest and deployed version. Do not synthesize a production request containing company data; wait for the next natural matching error.

Expected: ordinary requests continue and no `openai.partial_debug.*` body logs appear until a matching error occurs.

