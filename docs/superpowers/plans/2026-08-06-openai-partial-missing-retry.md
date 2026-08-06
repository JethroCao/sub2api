# OpenAI Responses Partial Missing Retry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Retry an OpenAI-compatible Responses request once when an enabled account explicitly rejects it because the last assistant message is missing `partial`.

**Architecture:** Keep proactive message normalization unchanged. Add a focused retry-body normalizer that activates only for the exact HTTP 400 missing-partial error, then call it from the existing HTTP upstream retry loop behind a per-request boolean guard.

**Tech Stack:** Go 1.26.5, Gin, `gjson`, `sjson`, Testify, Docker Buildx, Docker Compose.

## Global Constraints

- Retry only accounts with Responses message partial compatibility enabled.
- Match HTTP 400 and an explicit missing `partial` upstream error.
- Set `partial: true` only on the last assistant message.
- Retry the same account at most once.
- Do not change the first-attempt normalization behavior.
- Deploy only to `47.254.20.208`; do not touch the Hangzhou nodes.

---

### Task 1: Add the Conditional Retry Body Normalizer

**Files:**
- Create: `backend/internal/service/openai_responses_partial_missing_retry.go`
- Create: `backend/internal/service/openai_responses_partial_missing_retry_test.go`

**Interfaces:**
- Consumes: `isOpenAIPartialMissingError(int, string, []byte) bool`, `(*Account).ShouldEnableOpenAIResponsesMessagePartialCompat() bool`, and `isOpenAIResponsesMessage(gjson.Result) bool`.
- Produces: `normalizeOpenAIResponsesPartialMissingRetryBody(account *Account, statusCode int, upstreamMsg string, requestBody []byte, upstreamBody []byte) ([]byte, bool, error)`.

- [ ] **Step 1: Write the failing unit tests**

Add literal fixtures covering the captured shape and all no-retry gates:

```go
func TestNormalizeOpenAIResponsesPartialMissingRetryBodySetsLastAssistantBeforeToolSuffix(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"assistant","content":"draft"},
		{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"{}"},
		{"type":"custom_tool_call_output","call_id":"call_1","output":"ok"}
	]}`)
	errorBody := []byte(`{"error":{"code":"MissingParameter","message":"missing partial parameter","param":"partial"}}`)

	got, changed, err := normalizeOpenAIResponsesPartialMissingRetryBody(
		openAIResponsesMessagePartialCompatAccount(true), http.StatusBadRequest,
		"missing partial parameter", body, errorBody,
	)

	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, gjson.GetBytes(got, "input.0.partial").Bool())
	require.False(t, gjson.GetBytes(got, "input.1.partial").Exists())
	require.False(t, gjson.GetBytes(got, "input.2.partial").Exists())
}
```

Add table cases for disabled account, nonmatching error, no assistant message,
already-true last assistant, and invalid JSON.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
cd backend
go test ./internal/service -run TestNormalizeOpenAIResponsesPartialMissingRetryBody -count=1
```

Expected: build failure because `normalizeOpenAIResponsesPartialMissingRetryBody` does not exist.

- [ ] **Step 3: Implement the minimal normalizer**

Create the helper with this control flow:

```go
func normalizeOpenAIResponsesPartialMissingRetryBody(
	account *Account,
	statusCode int,
	upstreamMsg string,
	requestBody []byte,
	upstreamBody []byte,
) ([]byte, bool, error) {
	if !account.ShouldEnableOpenAIResponsesMessagePartialCompat() ||
		!isOpenAIPartialMissingError(statusCode, upstreamMsg, upstreamBody) {
		return requestBody, false, nil
	}
	if !gjson.ValidBytes(requestBody) {
		return requestBody, false, fmt.Errorf("normalize partial-missing retry: invalid request JSON")
	}
	input := gjson.GetBytes(requestBody, "input")
	if !input.IsArray() {
		return requestBody, false, nil
	}
	lastAssistantIndex := -1
	for index, item := range input.Array() {
		if isOpenAIResponsesMessage(item) &&
			strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "assistant") {
			lastAssistantIndex = index
		}
	}
	if lastAssistantIndex < 0 || gjson.GetBytes(requestBody, fmt.Sprintf("input.%d.partial", lastAssistantIndex)).Type == gjson.True {
		return requestBody, false, nil
	}
	retryBody, err := sjson.SetBytes(requestBody, fmt.Sprintf("input.%d.partial", lastAssistantIndex), true)
	if err != nil {
		return requestBody, false, fmt.Errorf("normalize partial-missing retry input.%d: %w", lastAssistantIndex, err)
	}
	return retryBody, true, nil
}
```

- [ ] **Step 4: Run the focused tests and verify GREEN**

Run the same focused command and expect `ok` with no failures.

- [ ] **Step 5: Commit the normalizer**

```bash
git add backend/internal/service/openai_responses_partial_missing_retry.go backend/internal/service/openai_responses_partial_missing_retry_test.go
git commit -m "fix(openai): prepare partial missing retry body"
```

---

### Task 2: Retry Once in the HTTP Forward Loop

**Files:**
- Modify: `backend/internal/service/openai_gateway_forward.go:841-960`
- Modify: `backend/internal/service/openai_responses_message_partial_compat_integration_test.go`

**Interfaces:**
- Consumes: `normalizeOpenAIResponsesPartialMissingRetryBody(...)` from Task 1.
- Produces: observable forwarding behavior of exactly two upstream attempts for the targeted error and one attempt for all unaffected requests.

- [ ] **Step 1: Write the failing forward-path regression test**

Use an `httpUpstreamRecorder` with two responses: the exact
`MissingParameter/partial` 400 followed by a valid success. Enable the account
compatibility flag and send:

```json
{
  "model": "alias",
  "stream": false,
  "input": [
    {"type":"message","role":"assistant","content":"draft"},
    {"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"{}"},
    {"type":"custom_tool_call_output","call_id":"call_1","output":"ok"}
  ]
}
```

Assert the result succeeds, there are exactly two upstream bodies, the first
omits `input.0.partial`, and the second contains `input.0.partial=true` without
adding `partial` to the two tool items.

- [ ] **Step 2: Run the integration test and verify RED**

Run:

```bash
cd backend
go test ./internal/service -run TestForwardResponsesRetriesPartialMissingAfterToolSuffix -count=1
```

Expected: FAIL because the current forward loop returns the first upstream 400
without a second request.

- [ ] **Step 3: Add the single-attempt guard and retry branch**

Near the other HTTP retry guards, add:

```go
partialMissingRetryTried := false
```

After the diagnostic logging call and before generic rejected-field handling,
invoke the Task 1 normalizer. When it changes the body:

```go
partialMissingRetryTried = true
body = retryBody
requestView = newOpenAIRequestView(body)
reqBody = nil
rejectedFieldRetryState.remember(body)
logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying non-WSv2 request once after missing partial (account: %s)", account.Name)
continue
```

Return a wrapped normalization error if the helper fails. If the guard is
already true or the helper reports no change, continue through existing error
handling.

- [ ] **Step 4: Run targeted and service regression tests**

```bash
cd backend
go test ./internal/service -run 'Test(ForwardResponsesRetriesPartialMissingAfterToolSuffix|NormalizeOpenAIResponsesPartialMissingRetryBody|NormalizeOpenAIResponsesMessagePartialForAccount|OpenAIGatewayForwardLogsEntryAndFailedUpstreamBodiesForPartialMissing)' -count=1
go test ./internal/service -count=1
```

Expected: both commands exit 0.

- [ ] **Step 5: Commit the forwarding change**

```bash
git add backend/internal/service/openai_gateway_forward.go backend/internal/service/openai_responses_message_partial_compat_integration_test.go
git commit -m "fix(openai): retry missing partial once"
```

---

### Task 3: Verify, Publish, and Deploy the Fix

**Files:**
- Modify remotely: `/opt/sub2api/docker-compose.yml` on `47.254.20.208`
- Back up remotely: `/opt/sub2api/docker-compose.yml.bak-<commit>`

**Interfaces:**
- Consumes: the implementation commit from Tasks 1-2.
- Produces: immutable image `registry.cn-beijing.aliyuncs.com/aevumio/sub2api:<commit>` running on the US node.

- [ ] **Step 1: Run final repository verification**

```bash
cd backend
go test ./... -count=1
cd ..
git diff --check
git status --short
```

Expected: all Go packages pass; only the existing user-owned `.superpowers/`
path may remain untracked.

- [ ] **Step 2: Build and push the immutable image**

Use Docker Buildx for `linux/amd64`, the local proxy
`http://host.docker.internal:7890`, version `0.1.171`, and the implementation
commit for both `COMMIT` and the image tag.

- [ ] **Step 3: Verify the registry manifest**

Run `docker buildx imagetools inspect` for the immutable tag and require a
`linux/amd64` manifest and a stable digest.

- [ ] **Step 4: Deploy only the US node**

On `47.254.20.208`, back up Compose, replace the old immutable tag, manually
`docker pull` because the file uses `pull_policy: never`, then run:

```bash
docker compose up -d --no-deps sub2api
```

- [ ] **Step 5: Verify production health**

Require all of the following:

- Container status is `running` and health is `healthy`.
- Container image ID matches the pushed manifest digest.
- `wget -q -O - http://127.0.0.1:8080/health` inside the container returns
  `{"status":"ok"}`.
- `/app/sub2api --version` reports version `0.1.171` and the implementation
  commit.
- Startup logs contain no fatal error.

- [ ] **Step 6: Preserve the diagnostic logger temporarily**

Do not remove the conditional full-body logger in this release. It remains
available to confirm whether a later missing-partial failure reached the retry
branch; it logs only exact matching failures and skips the database log sink.
