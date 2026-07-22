# OpenAI Account Custom Instructions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional per-account OpenAI instructions suffix that is preserved in scheduler snapshots and appended after client instructions for Responses, Chat Completions bridges, compact requests, and Responses WebSocket requests.

**Architecture:** Store `openai_custom_instructions` in `accounts.credentials`, expose it through an `Account` accessor, and use one idempotent raw-JSON merge helper at every final OpenAI Responses request boundary. Validate and save the field through the existing account create/update flow, and edit it through the existing OpenAI account forms.

**Tech Stack:** Go 1.24, PostgreSQL JSONB/Ent repository layer, Gin, gjson/sjson, Vue 3, TypeScript, Vitest, pnpm.

## Global Constraints

- Configuration key is exactly `openai_custom_instructions` in `accounts.credentials`; no database migration.
- Apply only to OpenAI OAuth, setup-token, and API Key accounts; never to non-OpenAI accounts.
- Preserve client instructions and append account instructions after exactly two newline characters.
- Apply to `/v1/responses`, `/v1/chat/completions` Responses bridges, `/responses/compact`, HTTP-to-upstream-WS, and client Responses WebSocket ingress.
- Merge must be idempotent across retries and failover; an account's suffix must not leak to another account.
- UTF-8 value is limited to 16 KiB (16,384 bytes).
- Never log or include the instructions text in error messages.
- Bulk edit remains out of scope.

---

### Task 1: Account configuration, validation, and scheduler snapshot

**Files:**
- Modify: `backend/internal/service/account.go`
- Create: `backend/internal/service/account_custom_instructions_test.go`
- Modify: `backend/internal/service/admin_account.go`
- Create: `backend/internal/service/admin_account_custom_instructions_test.go`
- Modify: `backend/internal/repository/scheduler_cache.go`
- Modify: `backend/internal/repository/scheduler_cache_unit_test.go`

**Interfaces:**
- Produces: `OpenAICustomInstructionsCredentialKey`, `OpenAICustomInstructionsMaxBytes`, `(*Account).GetOpenAICustomInstructions() string`, and `ValidateOpenAICustomInstructionsCredentials(platform string, credentials map[string]any) error`.
- Consumes: existing account create/update credential merge and scheduler metadata filtering.

- [ ] **Step 1: Write failing accessor and validation tests**

Add table tests asserting nil/non-OpenAI/wrong-type/blank values return no instructions, valid OpenAI OAuth/API Key values preserve internal newlines with outer whitespace trimmed, and values over 16,384 UTF-8 bytes return `OPENAI_CUSTOM_INSTRUCTIONS_TOO_LONG` without echoing the value.

```go
func TestAccountGetOpenAICustomInstructions(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		OpenAICustomInstructionsCredentialKey: "  first\nsecond  ",
	}}
	require.Equal(t, "first\nsecond", account.GetOpenAICustomInstructions())
}

func TestValidateOpenAICustomInstructionsCredentialsRejectsOversize(t *testing.T) {
	err := ValidateOpenAICustomInstructionsCredentials(PlatformOpenAI, map[string]any{
		OpenAICustomInstructionsCredentialKey: strings.Repeat("界", OpenAICustomInstructionsMaxBytes),
	})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "界界界")
}
```

- [ ] **Step 2: Run the focused tests and confirm failure**

Run: `cd backend && go test ./internal/service -run 'Test(AccountGetOpenAICustomInstructions|ValidateOpenAICustomInstructionsCredentials)' -count=1`

Expected: FAIL because the constants and functions do not exist.

- [ ] **Step 3: Implement the account accessor and validator**

Add constants and strict validation in `account.go`:

```go
const (
	OpenAICustomInstructionsCredentialKey = "openai_custom_instructions"
	OpenAICustomInstructionsMaxBytes      = 16 * 1024
)

func (a *Account) GetOpenAICustomInstructions() string {
	if a == nil || a.Platform != PlatformOpenAI {
		return ""
	}
	raw, ok := a.Credentials[OpenAICustomInstructionsCredentialKey].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(raw)
}
```

The validator accepts absent/nil/blank values, rejects non-string values and non-OpenAI usage, and checks `len([]byte(value)) <= OpenAICustomInstructionsMaxBytes`.

- [ ] **Step 4: Validate create and merged update credentials**

Call the validator in `CreateAccount` before `buildAccountForCreate`. In `UpdateAccount`, call it after `MergePreservingSensitiveCreds` so validation sees the actual final credential map. Return an `infraerrors.BadRequest` code without including the configured text.

- [ ] **Step 5: Preserve the field in scheduler metadata and test it**

Add `service.OpenAICustomInstructionsCredentialKey` to `filterSchedulerCredentials`. Extend `TestBuildSchedulerMetadataAccount` (or the nearest credential-filter test) with:

```go
service.OpenAICustomInstructionsCredentialKey: "account suffix",
```

Assert the filtered snapshot contains that value and still omits unrelated credentials.

- [ ] **Step 6: Run focused backend tests**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'CustomInstructions|SchedulerMetadata|SchedulerCredentials' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit Task 1**

```bash
git add backend/internal/service/account.go backend/internal/service/account_custom_instructions_test.go backend/internal/service/admin_account.go backend/internal/service/admin_account_custom_instructions_test.go backend/internal/repository/scheduler_cache.go backend/internal/repository/scheduler_cache_unit_test.go
git commit -m "feat: add OpenAI account instructions setting"
```

### Task 2: Idempotent instructions merge helper

**Files:**
- Create: `backend/internal/service/openai_account_custom_instructions.go`
- Create: `backend/internal/service/openai_account_custom_instructions_test.go`

**Interfaces:**
- Consumes: `(*Account).GetOpenAICustomInstructions()` from Task 1.
- Produces: `appendOpenAIAccountInstructions(account *Account, body []byte) ([]byte, bool, error)`.

- [ ] **Step 1: Write failing merge tests**

Cover missing, empty, and existing string instructions; exact two-newline append; Unicode preservation; no-op when the exact suffix is already present; non-string rejection; unconfigured and non-OpenAI no-op; and account A then account B producing only B when each attempt starts from the original client body.

```go
func TestAppendOpenAIAccountInstructionsPreservesClientInstructions(t *testing.T) {
	account := customInstructionsAccount("account suffix")
	got, changed, err := appendOpenAIAccountInstructions(account,
		[]byte(`{"model":"gpt-5.2","instructions":"client instructions","input":"hi"}`))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client instructions\n\naccount suffix", gjson.GetBytes(got, "instructions").String())
}
```

- [ ] **Step 2: Run tests and confirm failure**

Run: `cd backend && go test ./internal/service -run TestAppendOpenAIAccountInstructions -count=1`

Expected: FAIL because the helper does not exist.

- [ ] **Step 3: Implement the minimal raw-JSON helper**

Use `gjson` to inspect `instructions`, reject an existing non-string value, and `sjson.SetBytes` to set the combined string. Idempotency must compare the final trimmed value against either the exact suffix or `"\n\n"+suffix`; it must not use a substring match.

```go
func appendOpenAIAccountInstructions(account *Account, body []byte) ([]byte, bool, error) {
	suffix := account.GetOpenAICustomInstructions()
	if suffix == "" { return body, false, nil }
	current := gjson.GetBytes(body, "instructions")
	if current.Exists() && current.Type != gjson.String {
		return body, false, errors.New("OpenAI instructions must be a string")
	}
	existing := strings.TrimSpace(current.String())
	if existing == suffix || strings.HasSuffix(existing, "\n\n"+suffix) { return body, false, nil }
	combined := suffix
	if existing != "" { combined = existing + "\n\n" + suffix }
	next, err := sjson.SetBytes(body, "instructions", combined)
	return next, err == nil, err
}
```

- [ ] **Step 4: Run tests and commit**

Run: `cd backend && go test ./internal/service -run TestAppendOpenAIAccountInstructions -count=1`

Expected: PASS.

```bash
git add backend/internal/service/openai_account_custom_instructions.go backend/internal/service/openai_account_custom_instructions_test.go
git commit -m "feat: merge account instructions into OpenAI requests"
```

### Task 3: HTTP Responses, Chat Completions bridge, compact, and upstream WS integration

**Files:**
- Modify: `backend/internal/service/openai_gateway_forward.go`
- Modify: `backend/internal/service/openai_gateway_chat_completions.go`
- Create: `backend/internal/service/openai_account_custom_instructions_integration_test.go`

**Interfaces:**
- Consumes: `appendOpenAIAccountInstructions` from Task 2.
- Produces: identical final instructions behavior for native Responses and Chat Completions-to-Responses paths.

- [ ] **Step 1: Write failing native Responses tests**

Add OAuth and API Key cases using the existing upstream recorder. Assert the upstream body contains `client\n\naccount suffix`, while the unconfigured control remains byte-equivalent. Include stream=false and stream=true, a passthrough-enabled request, and a compact-path request.

- [ ] **Step 2: Write failing Chat Completions bridge test**

Send `/v1/chat/completions` through an account that uses the Responses bridge and assert the converted upstream body ends with the account suffix after its converted system instructions.

- [ ] **Step 3: Run integration tests and confirm failure**

Run: `cd backend && go test ./internal/service -run 'TestForward.*AccountCustomInstructions|TestChatCompletions.*AccountCustomInstructions' -count=1`

Expected: FAIL because gateway paths do not invoke the helper.

- [ ] **Step 4: Integrate at the final HTTP request boundaries**

In `openai_gateway_forward.go`, invoke the helper in both final request branches:

- Passthrough: after JSON Schema compatibility normalization and before `forwardOpenAIPassthrough`.
- Normal Responses: after JSON Schema compatibility normalization and before token acquisition/transport choice.

The normal position covers HTTP forwarding, `/responses/compact`, and the HTTP request that selects upstream Responses WebSocket transport. Both calls always receive the attempt's original account-neutral body, so failover cannot inherit a prior account suffix.

In `openai_gateway_chat_completions.go`, invoke the helper after Chat Completions conversion and JSON Schema compatibility normalization, before fast-policy evaluation and upstream request creation.

On change, log only:

```go
logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Appended account custom instructions: account_id=%d bytes=%d", account.ID, len(account.GetOpenAICustomInstructions()))
```

- [ ] **Step 5: Run integration and regression tests**

Run: `cd backend && go test ./internal/service -run 'AccountCustomInstructions|JSONSchemaCompatibility|CompactModelMapping|ChatCompletions' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit Task 3**

```bash
git add backend/internal/service/openai_gateway_forward.go backend/internal/service/openai_gateway_chat_completions.go backend/internal/service/openai_account_custom_instructions_integration_test.go
git commit -m "feat: apply account instructions to OpenAI HTTP requests"
```

### Task 4: Client Responses WebSocket ingress

**Files:**
- Modify: `backend/internal/service/openai_ws_forwarder_ingress.go`
- Modify: `backend/internal/service/openai_ws_forwarder_ingress_test.go`

**Interfaces:**
- Consumes: `appendOpenAIAccountInstructions` from Task 2.
- Produces: every normalized `response.create` frame sent upstream includes the selected account suffix once.

- [ ] **Step 1: Write failing WebSocket payload tests**

Extend ingress tests with first and follow-up `response.create` frames. Assert both preserve their own client instructions, append the suffix once, and do not duplicate it when replay normalization sees an already-appended frame.

- [ ] **Step 2: Run the focused test and confirm failure**

Run: `cd backend && go test ./internal/service -run 'TestOpenAIWS.*AccountCustomInstructions' -count=1`

Expected: FAIL because ingress normalization does not invoke the helper.

- [ ] **Step 3: Apply the helper before WebSocket fast policy**

In `parseClientPayload`, call `appendOpenAIAccountInstructions(account, normalized)` after model/tool/instructions transforms and before `applyOpenAIFastPolicyToWSResponseCreate`. Convert helper errors to `NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid websocket instructions", err)` without exposing the configured text.

- [ ] **Step 4: Run ingress regression tests and commit**

Run: `cd backend && go test ./internal/service -run 'TestOpenAIWS' -count=1`

Expected: PASS.

```bash
git add backend/internal/service/openai_ws_forwarder_ingress.go backend/internal/service/openai_ws_forwarder_ingress_test.go
git commit -m "feat: apply account instructions to OpenAI websocket requests"
```

### Task 5: OpenAI account create/edit UI

**Files:**
- Modify: `frontend/src/components/account/CreateAccountModal.vue`
- Modify: `frontend/src/components/account/EditAccountModal.vue`
- Modify: `frontend/src/components/account/__tests__/CreateAccountModal.spec.ts`
- Modify: `frontend/src/components/account/__tests__/EditAccountModal.spec.ts`
- Modify: `frontend/src/i18n/locales/zh/admin/accounts.ts`
- Modify: `frontend/src/i18n/locales/en/admin/accounts.ts`

**Interfaces:**
- Consumes/produces: `credentials.openai_custom_instructions` string; no API shape change because credentials is already `Record<string, unknown>`.

- [ ] **Step 1: Write failing create/edit component tests**

Assert the textarea appears only for `platform=openai` with OAuth/setup-token/API Key types, saves trimmed non-empty content, reloads existing content, deletes the key when cleared, shows a 16 KiB limit message, and is absent for Anthropic/Grok accounts. Do not add bulk-edit coverage.

- [ ] **Step 2: Run tests and confirm failure**

Run: `cd frontend && pnpm test:run src/components/account/__tests__/CreateAccountModal.spec.ts src/components/account/__tests__/EditAccountModal.spec.ts`

Expected: FAIL because the control and credential serialization do not exist.

- [ ] **Step 3: Implement the form control and serialization**

Add a local `ref('')`, a textarea with byte-aware validation, and a shared local application pattern in each component:

```ts
const applyOpenAICustomInstructions = (credentials: Record<string, unknown>) => {
  const value = openAICustomInstructions.value.trim()
  if (value) credentials.openai_custom_instructions = value
  else delete credentials.openai_custom_instructions
}
```

Call it for OpenAI OAuth/setup-token/API Key credential builders and update payloads. On edit initialization, load only string values from existing credentials. Reject submission when `new TextEncoder().encode(value).length > 16 * 1024`.

- [ ] **Step 4: Add localized copy**

Add Chinese and English labels, descriptions, non-secret warnings, and maximum-length validation messages under the existing OpenAI account settings namespace.

- [ ] **Step 5: Run frontend tests, typecheck, and commit**

Run: `cd frontend && pnpm test:run src/components/account/__tests__/CreateAccountModal.spec.ts src/components/account/__tests__/EditAccountModal.spec.ts`

Run: `cd frontend && pnpm typecheck`

Expected: PASS.

```bash
git add frontend/src/components/account/CreateAccountModal.vue frontend/src/components/account/EditAccountModal.vue frontend/src/components/account/__tests__/CreateAccountModal.spec.ts frontend/src/components/account/__tests__/EditAccountModal.spec.ts frontend/src/i18n/locales/zh/admin/accounts.ts frontend/src/i18n/locales/en/admin/accounts.ts
git commit -m "feat: configure OpenAI account instructions"
```

### Task 6: Full verification and operator documentation

**Files:**
- Modify only if needed: `docs/superpowers/specs/2026-07-22-openai-account-custom-instructions-design.md`

**Interfaces:**
- Verifies all interfaces and constraints from Tasks 1–5.

- [ ] **Step 1: Run backend formatting and complete tests**

Run: `gofmt -w backend/internal/service/account.go backend/internal/service/admin_account.go backend/internal/service/openai_account_custom_instructions.go backend/internal/service/openai_gateway_forward.go backend/internal/service/openai_gateway_chat_completions.go backend/internal/service/openai_ws_forwarder_ingress.go backend/internal/repository/scheduler_cache.go`

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin -count=1`

Expected: PASS.

- [ ] **Step 2: Run frontend verification**

Run: `cd frontend && pnpm lint:check`

Run: `cd frontend && pnpm typecheck`

Run: `cd frontend && pnpm test:run src/components/account/__tests__/CreateAccountModal.spec.ts src/components/account/__tests__/EditAccountModal.spec.ts`

Run: `cd frontend && pnpm build`

Expected: PASS.

- [ ] **Step 3: Review the final diff for secrets and scope**

Run: `git diff --check && git status --short && git diff --stat HEAD~5..HEAD`

Expected: no whitespace errors, no credentials/instruction text in tests or logs, no BulkEdit changes, and only planned files changed.

- [ ] **Step 4: Record a manual smoke-test request without real credentials**

Use a placeholder key and verify the upstream recorder or a local test account returns the requested model while its upstream request contains the suffix once:

```bash
curl -sS https://example.invalid/v1/responses \
  -H 'Authorization: Bearer sk-REDACTED' \
  -H 'Content-Type: application/json' \
  -d '{"model":"test-model","instructions":"client","input":"identify yourself"}'
```

- [ ] **Step 5: Commit any verification-only documentation correction**

If the design document required a factual correction discovered during implementation, commit only that correction:

```bash
git add docs/superpowers/specs/2026-07-22-openai-account-custom-instructions-design.md
git commit -m "docs: align account instructions design with implementation"
```

If no correction was required, do not create an empty commit.
