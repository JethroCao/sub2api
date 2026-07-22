# OpenAI JSON Schema Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an account-level mode that downgrades unsupported OpenAI `json_schema` requests to `json_object` while preserving the schema as a best-effort instruction.

**Architecture:** Store the mode in `accounts.extra`, normalize it through an `Account` method, and run one focused request-body transformer after account selection for both Chat Completions and Responses paths. Reuse the existing account form and scheduler-cache patterns so no database migration is required.

**Tech Stack:** Go 1.26, Gin, gjson/sjson, Vue 3, TypeScript, Vitest, PostgreSQL JSONB, Redis scheduler cache.

## Global Constraints

- Existing accounts default to `auto`; deployment alone must change no traffic.
- Apply only to OpenAI API-key accounts configured as `force_json_object`.
- Never hard-code account names, Volcano hostnames, or DeepSeek model IDs.
- Preserve streaming behavior; do not buffer output for schema validation.
- Do not log schemas or request content.
- No database migration or new dependency.

---

### Task 1: Account mode and scheduler metadata

**Files:**
- Modify: `backend/internal/service/account.go`
- Modify: `backend/internal/repository/scheduler_cache.go`
- Test: `backend/internal/service/account_json_schema_mode_test.go`
- Test: `backend/internal/repository/scheduler_cache_unit_test.go`

**Interfaces:**
- Produces: `OpenAIJSONSchemaMode` constants and `(*Account).GetOpenAIJSONSchemaMode() string`.
- Consumes: existing `Account.Extra` and `Account.IsOpenAI()`.

- [ ] **Step 1: Write failing account normalization tests**

Cover nil account, non-OpenAI account, missing/invalid metadata, `auto`, `passthrough`, and `force_json_object`.

```go
func TestAccountGetOpenAIJSONSchemaMode(t *testing.T) {
    tests := []struct {
        name string
        account *Account
        want string
    }{
        {"missing defaults auto", &Account{Platform: PlatformOpenAI}, OpenAIJSONSchemaModeAuto},
        {"force object", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_json_schema_mode": "force_json_object"}}, OpenAIJSONSchemaModeForceJSONObject},
        {"invalid defaults auto", &Account{Platform: PlatformOpenAI, Extra: map[string]any{"openai_json_schema_mode": "bad"}}, OpenAIJSONSchemaModeAuto},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            require.Equal(t, tt.want, tt.account.GetOpenAIJSONSchemaMode())
        })
    }
}
```

- [ ] **Step 2: Run the tests and verify failure**

Run: `cd backend && go test ./internal/service -run TestAccountGetOpenAIJSONSchemaMode -count=1`

Expected: compile failure because the constants and method do not exist.

- [ ] **Step 3: Implement normalized account mode**

Add constants and a method that returns `auto` unless the account is OpenAI API-key and the stored value is one of the supported values.

```go
const (
    OpenAIJSONSchemaModeAuto = "auto"
    OpenAIJSONSchemaModePassthrough = "passthrough"
    OpenAIJSONSchemaModeForceJSONObject = "force_json_object"
)

func (a *Account) GetOpenAIJSONSchemaMode() string {
    if a == nil || !a.IsOpenAI() || a.Type != AccountTypeAPIKey || a.Extra == nil {
        return OpenAIJSONSchemaModeAuto
    }
    mode, _ := a.Extra["openai_json_schema_mode"].(string)
    switch strings.ToLower(strings.TrimSpace(mode)) {
    case OpenAIJSONSchemaModePassthrough:
        return OpenAIJSONSchemaModePassthrough
    case OpenAIJSONSchemaModeForceJSONObject:
        return OpenAIJSONSchemaModeForceJSONObject
    default:
        return OpenAIJSONSchemaModeAuto
    }
}
```

- [ ] **Step 4: Retain the key in compact scheduler metadata**

Add `openai_json_schema_mode` to the explicit `extra` key list in `scheduler_cache.go`, then extend the existing scheduler cache unit test to assert it survives conversion.

- [ ] **Step 5: Run focused backend tests**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'TestAccountGetOpenAIJSONSchemaMode|Test.*Scheduler.*Cache' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/service/account.go backend/internal/service/account_json_schema_mode_test.go backend/internal/repository/scheduler_cache.go backend/internal/repository/scheduler_cache_unit_test.go
git commit -m "feat: add account JSON schema compatibility mode"
```

### Task 2: Request body downgrade helper

**Files:**
- Create: `backend/internal/service/openai_json_schema_compat.go`
- Create: `backend/internal/service/openai_json_schema_compat_test.go`

**Interfaces:**
- Consumes: `(*Account).GetOpenAIJSONSchemaMode()` from Task 1.
- Produces: `normalizeOpenAIJSONSchemaForAccount(account *Account, body []byte, protocol openAIJSONSchemaProtocol) (normalized []byte, changed bool, err error)`.

- [ ] **Step 1: Write failing transformer tests**

Test Chat shape, Responses shape, existing instructions, malformed/non-schema formats, and non-forced accounts. Assert the rewritten format is exactly `{"type":"json_object"}` and the schema hint contains the original schema without replacing existing instructions.

```go
func TestNormalizeOpenAIJSONSchemaForAccountResponses(t *testing.T) {
    account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_json_schema_mode": "force_json_object"}}
    body := []byte(`{"instructions":"keep","text":{"format":{"type":"json_schema","name":"person","strict":true,"schema":{"type":"object","required":["name"]}}}}`)
    got, changed, err := normalizeOpenAIJSONSchemaForAccount(account, body, openAIJSONSchemaProtocolResponses)
    require.NoError(t, err)
    require.True(t, changed)
    require.JSONEq(t, `{"type":"json_object"}`, gjson.GetBytes(got, "text.format").Raw)
    require.Contains(t, gjson.GetBytes(got, "instructions").String(), "required")
}
```

- [ ] **Step 2: Run the tests and verify failure**

Run: `cd backend && go test ./internal/service -run TestNormalizeOpenAIJSONSchemaForAccount -count=1`

Expected: compile failure because the transformer does not exist.

- [ ] **Step 3: Implement the transformer**

Use `gjson` to recognize the exact `json_schema` type and standard `encoding/json` plus `sjson` to serialize the schema hint and rewrite atomically. Chat adds one system message; Responses appends to the string `instructions`. Return the original body unchanged when the mode or format does not match.

```go
type openAIJSONSchemaProtocol string

const (
    openAIJSONSchemaProtocolChat openAIJSONSchemaProtocol = "chat_completions"
    openAIJSONSchemaProtocolResponses openAIJSONSchemaProtocol = "responses"
)

func normalizeOpenAIJSONSchemaForAccount(account *Account, body []byte, protocol openAIJSONSchemaProtocol) ([]byte, bool, error) {
    if account.GetOpenAIJSONSchemaMode() != OpenAIJSONSchemaModeForceJSONObject {
        return body, false, nil
    }
    // Select response_format or text.format, extract the schema, build a safe
    // hint, then return a newly serialized body with json_object.
}
```

- [ ] **Step 4: Run transformer tests**

Run: `cd backend && go test ./internal/service -run TestNormalizeOpenAIJSONSchemaForAccount -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/openai_json_schema_compat.go backend/internal/service/openai_json_schema_compat_test.go
git commit -m "feat: downgrade unsupported JSON schema requests"
```

### Task 3: Integrate all OpenAI HTTP forwarding paths

**Files:**
- Modify: `backend/internal/service/openai_gateway_forward.go`
- Modify: `backend/internal/service/openai_gateway_chat_completions.go`
- Modify: `backend/internal/service/openai_gateway_chat_completions_raw.go`
- Modify: `backend/internal/service/openai_gateway_cc_pipeline.go`
- Test: `backend/internal/service/openai_json_schema_compat_integration_test.go`

**Interfaces:**
- Consumes: `normalizeOpenAIJSONSchemaForAccount` from Task 2.
- Produces: normalized upstream bodies for native Responses, raw Chat, and Responses-to-Chat fallback paths.

- [ ] **Step 1: Write failing forwarding tests**

Use `httptest.Server` upstreams and assert the received body for each route. Include streaming requests and an `auto` control account.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `cd backend && go test ./internal/service -run 'TestForward.*JSONSchemaCompatibility' -count=1`

Expected: tests fail because upstream still receives `json_schema`.

- [ ] **Step 3: Apply normalization before each upstream request**

For native Responses call the helper after model mapping and before request construction. For raw Chat call it after model replacement and before policy processing. For protocol bridges normalize the final upstream protocol shape, so the helper never guesses a body format.

Log only:

```go
logger.L().Debug("openai json_schema downgraded",
    zap.Int64("account_id", account.ID),
    zap.String("upstream_model", upstreamModel),
    zap.String("protocol", string(protocol)),
)
```

- [ ] **Step 4: Run forwarding and existing compatibility tests**

Run: `cd backend && go test ./internal/service -run 'JSONSchemaCompatibility|ChatCompletions|ResponsesChatFallback' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/openai_gateway_forward.go backend/internal/service/openai_gateway_chat_completions.go backend/internal/service/openai_gateway_chat_completions_raw.go backend/internal/service/openai_gateway_cc_pipeline.go backend/internal/service/openai_json_schema_compat_integration_test.go
git commit -m "feat: apply JSON schema compatibility during forwarding"
```

### Task 4: Account configuration UI

**Files:**
- Modify: `frontend/src/types/index.ts`
- Modify: `frontend/src/components/account/CreateAccountModal.vue`
- Modify: `frontend/src/components/account/EditAccountModal.vue`
- Modify: `frontend/src/components/account/BulkEditAccountModal.vue`
- Modify: `frontend/src/i18n/locales/zh/admin/accounts.ts`
- Modify: `frontend/src/i18n/locales/en/admin/accounts.ts`
- Test: `frontend/src/components/account/__tests__/CreateAccountModal.spec.ts`
- Test: `frontend/src/components/account/__tests__/EditAccountModal.spec.ts`
- Test: `frontend/src/components/account/__tests__/BulkEditAccountModal.spec.ts`

**Interfaces:**
- Consumes: `accounts.extra.openai_json_schema_mode`.
- Produces: `OpenAIJSONSchemaMode = 'auto' | 'passthrough' | 'force_json_object'` and persisted create/update payloads.

- [ ] **Step 1: Write failing modal tests**

Assert an OpenAI API-key account can select `force_json_object`, update persists the extra key, selecting `auto` deletes the key, and OAuth accounts do not render the control.

- [ ] **Step 2: Run modal tests and verify failure**

Run: `cd frontend && pnpm test:run src/components/account/__tests__/CreateAccountModal.spec.ts src/components/account/__tests__/EditAccountModal.spec.ts src/components/account/__tests__/BulkEditAccountModal.spec.ts`

Expected: FAIL because the field and type do not exist.

- [ ] **Step 3: Add type, dropdowns, persistence, and translations**

```ts
export type OpenAIJSONSchemaMode = 'auto' | 'passthrough' | 'force_json_object'
```

Render the dropdown beside the existing Responses/Compact controls only when `platform === 'openai' && type === 'apikey'`. Delete the extra key for `auto`; persist either explicit non-default value.

- [ ] **Step 4: Run frontend tests and typecheck**

Run: `cd frontend && pnpm test:run src/components/account/__tests__/CreateAccountModal.spec.ts src/components/account/__tests__/EditAccountModal.spec.ts src/components/account/__tests__/BulkEditAccountModal.spec.ts`

Run: `cd frontend && pnpm typecheck`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/types/index.ts frontend/src/components/account frontend/src/i18n/locales
git commit -m "feat: configure JSON schema compatibility per account"
```

### Task 5: Full verification and regression review

**Files:**
- Modify only files required by formatter or verified test failures.

**Interfaces:**
- Consumes: all prior tasks.
- Produces: a deployable commit set with backend and frontend verification evidence.

- [ ] **Step 1: Format changed code**

Run: `cd backend && gofmt -w internal/service/account.go internal/service/account_json_schema_mode_test.go internal/service/openai_json_schema_compat.go internal/service/openai_json_schema_compat_test.go internal/service/openai_json_schema_compat_integration_test.go`

- [ ] **Step 2: Run backend package tests**

Run: `cd backend && go test ./internal/service ./internal/repository -count=1`

Expected: PASS.

- [ ] **Step 3: Run frontend tests and build**

Run: `cd frontend && pnpm test:run src/components/account/__tests__`

Run: `cd frontend && pnpm build`

Expected: PASS.

- [ ] **Step 4: Inspect the final diff**

Run: `git diff --check && git status --short && git log --oneline -6`

Expected: no whitespace errors and only intended changes.

- [ ] **Step 5: Commit any verification-only fixes**

```bash
git add backend frontend
git commit -m "test: verify JSON schema compatibility mode"
```
