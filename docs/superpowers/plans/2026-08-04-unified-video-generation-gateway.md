# Unified Video Generation Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a provider-neutral asynchronous video gateway for existing Grok video, Volcengine Seedance, and the direct Kling Open Platform while preserving the current five Grok-compatible routes, making PostgreSQL the task and billing source of truth, and exposing safe administration and operations controls.

**Architecture:** A dedicated video application service normalizes requests, resolves capability and pricing rules, freezes balance, selects an eligible account, submits through a provider adapter, and stores a stable public task ID. A multi-instance reconciler leases due tasks from PostgreSQL with `FOR UPDATE SKIP LOCKED`, polls the original account, settles or releases the frozen amount, and records append-only events. Status and content reads use the durable task first; pre-deployment Grok task IDs fall back to the existing Redis binding. Redis remains an optional acceleration layer and is never authoritative for new video tasks.

**Tech Stack:** Go 1.x, Gin, Ent, PostgreSQL, Redis, Google Wire, Vue 3, TypeScript, Pinia, Tailwind, Vitest, Testify.

## Global Constraints

- Preserve the five existing public routes exactly: `POST /v1/videos/generations`, `POST /v1/videos/edits`, `POST /v1/videos/extensions`, `GET /v1/videos/{request_id}`, and `GET /v1/videos/{request_id}/content`.
- Do not change Grok text or image behavior. Only extract the video-specific upstream call needed by the new adapter.
- Do not expose cancellation in the first release.
- Do not add object storage. Return the upstream temporary URL and `expires_at`; use the authenticated content endpoint when upstream download authentication is required.
- Store no API keys, JWTs, OAuth tokens, raw authorization headers, binary uploads, or full upstream credential responses in tasks, events, logs, or metrics.
- `video_tasks` is authoritative for every new task. Redis loss must not lose task ownership, status, or billing state.
- A customer can never be charged more than the amount frozen at submission. Success captures the final amount and releases the difference; terminal failure releases the full amount; an ambiguous submission keeps the hold until reconciliation or manual resolution.
- Unsupported provider operations fail with `unsupported_capability` before account concurrency acquisition, upstream submission, or balance freeze.
- Provider failover is limited to accounts whose model mapping resolves the same external model and whose provider supports the same operation and media inputs. Never silently switch `seedance-*` to `kling-*` or Grok.
- Direct Kling implementation must be backed by redacted fixtures exported from the user's provisioned Kling Open Platform documentation/console. The public documentation page is JavaScript-rendered and is not sufficient evidence for guessing field names.
- All schema changes need both Ent schema definitions and numbered SQL migrations. Do not rely on Ent auto-migration in production.
- After each task, run the listed focused tests before committing. Keep `.superpowers/` untracked and never include it in a commit.

---

## Task 1: Introduce the Video Platform and Account Provider Contract

**Files:**

- Modify: `backend/internal/domain/constants.go`
- Modify: `backend/internal/service/domain_constants.go`
- Modify: `backend/ent/schema/user_platform_quota.go`
- Create: `backend/internal/service/account_video.go`
- Create: `backend/internal/service/account_video_test.go`
- Modify: `backend/internal/service/admin_account.go`
- Create: `backend/internal/service/admin_account_video_test.go`

**Interfaces:**

- Produces `PlatformVideo`, `VideoProviderSeedance`, `VideoProviderKling`, `Account.VideoProvider()`, and `ValidateVideoAccountConfig`.
- Stores provider identity in `account.extra["video_provider"]` and uses the existing encrypted `credentials` JSON for secrets.
- First-release Video accounts accept only `AccountTypeAPIKey`.
- Consumed by Tasks 4, 8, 9, 10, 13, and 15.

- [ ] **Step 1: Write failing platform and account validation tests**

```go
func TestValidateVideoAccountConfig(t *testing.T) {
	tests := []struct {
		name        string
		accountType string
		extra       map[string]any
		credentials map[string]any
		wantErr     bool
	}{
		{"seedance api key", AccountTypeAPIKey, map[string]any{"video_provider": "seedance"}, map[string]any{"api_key": "ark-key"}, false},
		{"kling key pair", AccountTypeAPIKey, map[string]any{"video_provider": "kling"}, map[string]any{"access_key": "ak", "secret_key": "sk"}, false},
		{"missing provider", AccountTypeAPIKey, map[string]any{}, map[string]any{"api_key": "x"}, true},
		{"oauth rejected", AccountTypeOAuth, map[string]any{"video_provider": "seedance"}, map[string]any{"api_key": "x"}, true},
		{"unknown provider", AccountTypeAPIKey, map[string]any{"video_provider": "other"}, map[string]any{"api_key": "x"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateVideoAccountConfig(PlatformVideo, tt.accountType, tt.extra, tt.credentials)
			require.Equal(t, tt.wantErr, err != nil)
		})
	}
}

func TestAllowedQuotaPlatformsIncludesVideo(t *testing.T) {
	require.True(t, IsAllowedQuotaPlatform(PlatformVideo))
}
```

- [ ] **Step 2: Run the focused tests and confirm they fail**

Run: `cd backend && go test ./internal/service -run 'TestValidateVideoAccountConfig|TestAllowedQuotaPlatformsIncludesVideo' -count=1`

Expected: compile failure because the Video constants and validation functions do not exist.

- [ ] **Step 3: Implement the platform constants and strict account validation**

Add these public constants and methods:

```go
const (
	PlatformVideo         = domain.PlatformVideo
	VideoProviderSeedance = "seedance"
	VideoProviderKling    = "kling"
)

const videoProviderExtraKey = "video_provider"

func (a *Account) VideoProvider() string {
	if a == nil || a.Platform != PlatformVideo {
		return ""
	}
	provider, _ := a.Extra[videoProviderExtraKey].(string)
	return strings.ToLower(strings.TrimSpace(provider))
}
```

`ValidateVideoAccountConfig` must enforce:

- non-Video platforms pass through unchanged;
- Video requires `apikey` type;
- Seedance requires `credentials.api_key`;
- Kling requires both `credentials.access_key` and `credentials.secret_key`;
- provider is exactly `seedance` or `kling`.

Call the validator from both `CreateAccount` and `UpdateAccount` after sensitive credentials are merged, so masked update payloads cannot erase existing secrets.

- [ ] **Step 4: Extend the quota platform schema validation**

Add `video` to `AllowedQuotaPlatforms` and the independent Ent validator in `backend/ent/schema/user_platform_quota.go`.

- [ ] **Step 5: Re-run tests**

Run: `cd backend && go test ./internal/service -run 'TestValidateVideoAccountConfig|TestAllowedQuotaPlatformsIncludesVideo|TestAdminService.*Account' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/domain/constants.go backend/internal/service/domain_constants.go backend/ent/schema/user_platform_quota.go backend/internal/service/account_video.go backend/internal/service/account_video_test.go backend/internal/service/admin_account.go backend/internal/service/admin_account_video_test.go
git commit -m "feat(video): add video platform account contract"
```

## Task 2: Add Group Permission and Durable Pricing Rules

**Files:**

- Modify: `backend/ent/schema/group.go`
- Create: `backend/ent/schema/video_pricing_rule.go`
- Create: `backend/migrations/194_unified_video_group_pricing.sql`
- Create: `backend/migrations/195_unified_video_auth_cache_invalidation.sql`
- Create: `backend/migrations/unified_video_gateway_migration_test.go`
- Modify: `backend/internal/handler/dto/types.go`
- Modify: `backend/internal/handler/dto/mappers.go`
- Modify: `backend/internal/handler/admin/group_handler.go`
- Modify: `backend/internal/service/group.go`
- Modify: `backend/internal/service/admin_group.go`
- Modify: `backend/internal/repository/group_repo.go`
- Modify: `backend/internal/repository/api_key_repo.go`
- Modify: `backend/internal/repository/auth_cache_invalidation_outbox_integration_test.go`

**Interfaces:**

- Produces `Group.AllowVideoGeneration` and `VideoPricingRule`.
- Pricing uniqueness is `(group_id, external_model, operation, resolution, audio_mode)`.
- A rule's `unit_price` is the final customer price; it is not multiplied by the token rate multiplier.
- `upstream_unit_cost` is optional and used only for margin/operations reporting.
- Consumed by Tasks 5, 7, 12, 16, and 17.

- [ ] **Step 1: Write migration regression tests first**

```go
func TestUnifiedVideoMigrationsDefinePermissionPricingAndInvalidation(t *testing.T) {
	foundation, err := FS.ReadFile("194_unified_video_group_pricing.sql")
	require.NoError(t, err)
	sql := string(foundation)
	require.Contains(t, sql, "allow_video_generation BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS video_pricing_rules")
	require.Contains(t, sql, "UNIQUE (group_id, external_model, operation, resolution, audio_mode)")

	cache, err := FS.ReadFile("195_unified_video_auth_cache_invalidation.sql")
	require.NoError(t, err)
	require.Contains(t, string(cache), "OLD.allow_video_generation IS NOT DISTINCT FROM NEW.allow_video_generation")
}
```

- [ ] **Step 2: Run the test and confirm failure**

Run: `cd backend && go test ./migrations -run TestUnifiedVideoMigrationsDefinePermissionPricingAndInvalidation -count=1`

Expected: FAIL because migrations 194 and 195 do not exist.

- [ ] **Step 3: Add Ent fields and pricing-rule schema**

Use these fields in `VideoPricingRule`:

```go
field.Int64("group_id"),
field.String("external_model").MaxLen(128),
field.Enum("operation").Values("generation", "edit", "extension"),
field.String("resolution").MaxLen(32).Default("*"),
field.Enum("audio_mode").Values("any", "with_audio", "without_audio").Default("any"),
field.Enum("unit").Values("per_request", "per_output_second"),
field.Float("unit_price").SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
field.Float("upstream_unit_cost").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
field.Bool("enabled").Default(true),
field.Time("created_at").Immutable().Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
```

Add `allow_video_generation` to Group with default `false`. Migration 194 must create the rule table, foreign key, unique constraint, and lookup index `(group_id, external_model, operation, enabled)`. Migration 195 must replace the latest invalidation trigger body from migration 193 and include the new permission field.

- [ ] **Step 4: Thread the permission through DTOs, repositories, and admin requests**

Add `AllowVideoGeneration bool` to read DTOs and `*bool` to update inputs. Extend create/update bindings to permit `video` groups and permit `video` as a composite target. Include the flag in API-key authorization projections and cached snapshots.

- [ ] **Step 5: Generate Ent code and run focused tests**

Run:

```bash
cd backend
go generate ./ent
go test ./migrations -run TestUnifiedVideoMigrationsDefinePermissionPricingAndInvalidation -count=1
go test ./internal/repository -run 'TestAuthCacheInvalidation.*Group|TestAPIKey.*Projection' -count=1
go test ./internal/handler/admin -run 'Test.*Group' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/ent backend/migrations/194_unified_video_group_pricing.sql backend/migrations/195_unified_video_auth_cache_invalidation.sql backend/migrations/unified_video_gateway_migration_test.go backend/internal/handler/dto/types.go backend/internal/handler/dto/mappers.go backend/internal/handler/admin/group_handler.go backend/internal/service/group.go backend/internal/service/admin_group.go backend/internal/repository/group_repo.go backend/internal/repository/api_key_repo.go backend/internal/repository/auth_cache_invalidation_outbox_integration_test.go
git commit -m "feat(video): add group permission and pricing rules"
```

## Task 3: Create the Durable Video Task Ledger and Lease Repository

**Files:**

- Create: `backend/ent/schema/video_task.go`
- Create: `backend/ent/schema/video_task_event.go`
- Create: `backend/migrations/196_unified_video_tasks.sql`
- Modify: `backend/migrations/unified_video_gateway_migration_test.go`
- Create: `backend/internal/service/video_task_types.go`
- Create: `backend/internal/service/video_task_repository.go`
- Create: `backend/internal/repository/video_task_repo.go`
- Create: `backend/internal/repository/video_task_repo_integration_test.go`
- Modify: `backend/internal/repository/wire.go`

**Interfaces:**

- Produces the durable `VideoTask`, append-only `VideoTaskEvent`, and `VideoTaskRepository` contract.
- Public IDs use `vid_` plus 32 lowercase hex characters and remain distinct from `upstream_task_id`.
- New tasks are leased from PostgreSQL; Redis is not part of this interface.
- Consumed by Tasks 6, 7, 8, 11, 12, and 14.

- [ ] **Step 1: Write failing repository integration tests**

The test must create two due tasks, lease them from two repository instances, and prove no task is leased twice:

```go
func TestVideoTaskRepositoryLeaseDueTasksSkipsLockedRows(t *testing.T) {
	db := setupIntegrationDB(t)
	repoA := repository.NewVideoTaskRepository(db)
	repoB := repository.NewVideoTaskRepository(db)
	ctx := context.Background()
	insertDueVideoTasks(t, db, "vid_a", "vid_b")

	a, err := repoA.LeaseDue(ctx, "worker-a", 1, time.Minute, time.Now())
	require.NoError(t, err)
	b, err := repoB.LeaseDue(ctx, "worker-b", 2, time.Minute, time.Now())
	require.NoError(t, err)
	require.Len(t, a, 1)
	require.Len(t, b, 1)
	require.NotEqual(t, a[0].RequestID, b[0].RequestID)
}

func TestVideoTaskRepositoryCreateIdempotencyConflict(t *testing.T) {
	// Same owner + operation + key + request hash returns the existing task.
	// Same key with a different request hash returns ErrVideoIdempotencyConflict.
}
```

- [ ] **Step 2: Run integration tests and confirm compile failure**

Run: `cd backend && go test -tags=integration ./internal/repository -run TestVideoTaskRepository -count=1`

Expected: compile failure because the repository and schema do not exist.

- [ ] **Step 3: Define the task state and schema**

Use these states exactly:

```go
const (
	VideoTaskCreated    VideoTaskStatus = "created"
	VideoTaskSubmitting VideoTaskStatus = "submitting"
	VideoTaskSubmitted  VideoTaskStatus = "submitted"
	VideoTaskQueued     VideoTaskStatus = "queued"
	VideoTaskRunning    VideoTaskStatus = "running"
	VideoTaskSucceeded  VideoTaskStatus = "succeeded"
	VideoTaskFailed     VideoTaskStatus = "failed"
	VideoTaskCancelled  VideoTaskStatus = "cancelled"
	VideoTaskUnknown    VideoTaskStatus = "unknown"
)
```

The task schema must contain ownership (`user_id`, `api_key_id`, nullable `subscription_id`, `group_id`), routing (`account_id`, `platform`, `provider`, operation, external/upstream model), idempotency (`idempotency_key_hash`, `request_hash`, `provider_submission_token`), minimized `request_payload`, upstream identity/status, result URL metadata, price snapshot and amounts, billing mode/status, retry counters, next poll time, lease fields, error fields, optimistic `version`, and timestamps. Clear `request_payload` in the same transaction that persists a non-empty upstream task ID.

Use a partial unique index on `(user_id, api_key_id, operation, idempotency_key_hash)` when the key hash is non-empty. Add indexes for `(status, next_poll_at)`, lease expiration, upstream identity, and owner lookup.

- [ ] **Step 4: Implement repository operations**

The interface must expose exact transactional operations rather than a generic `Save`:

```go
type VideoTaskRepository interface {
	CreateOrGet(context.Context, CreateVideoTaskParams) (*VideoTask, bool, error)
	GetOwned(context.Context, string, int64, int64) (*VideoTask, error)
	GetByRequestID(context.Context, string) (*VideoTask, error)
	MarkSubmitting(context.Context, string, int64, string) error
	MarkSubmitted(context.Context, MarkVideoSubmittedParams) error
	MarkSubmissionUnknown(context.Context, string, int64, VideoTaskError) error
	LeaseDue(context.Context, string, int, time.Duration, time.Time) ([]VideoTask, error)
	ApplyPollResult(context.Context, ApplyVideoPollResultParams) error
	MarkSettled(context.Context, MarkVideoSettledParams) error
	ReleaseLease(context.Context, string, string, time.Time) error
	AppendEvent(context.Context, VideoTaskEvent) error
	ListAdmin(context.Context, VideoTaskListQuery) ([]VideoTask, int, error)
}
```

`LeaseDue` must use one SQL transaction with `FOR UPDATE SKIP LOCKED`, set lease owner/expiry, and return leased rows.

- [ ] **Step 5: Generate and run tests**

Run:

```bash
cd backend
go generate ./ent
go test ./migrations -run TestUnifiedVideoMigrationsDefineTasks -count=1
go test -tags=integration ./internal/repository -run TestVideoTaskRepository -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/ent backend/migrations/196_unified_video_tasks.sql backend/migrations/unified_video_gateway_migration_test.go backend/internal/service/video_task_types.go backend/internal/service/video_task_repository.go backend/internal/repository/video_task_repo.go backend/internal/repository/video_task_repo_integration_test.go backend/internal/repository/wire.go
git commit -m "feat(video): add durable video task ledger"
```

## Task 4: Define Canonical Requests, Capabilities, and Provider Ports

**Files:**

- Create: `backend/internal/service/video_request.go`
- Create: `backend/internal/service/video_request_test.go`
- Create: `backend/internal/service/video_capability.go`
- Create: `backend/internal/service/video_capability_test.go`
- Create: `backend/internal/service/video_provider.go`

**Interfaces:**

- Produces the provider-neutral request, normalized response/error, capability catalog, and adapter interface.
- Consumed by all provider adapters, submission service, reconciler, and handlers.

- [ ] **Step 1: Write failing normalization and capability tests**

```go
func TestNormalizeVideoRequestPreservesGrokAliases(t *testing.T) {
	body := []byte(`{"model":"grok-imagine-video-1.5","prompt":"animate","image":{"image_url":"https://example.com/a.png"},"duration":10}`)
	req, err := NormalizeVideoRequest(VideoOperationGeneration, "application/json", body)
	require.NoError(t, err)
	require.Equal(t, "grok-imagine-video-1.5", req.Model)
	require.Equal(t, []VideoAsset{{URL: "https://example.com/a.png"}}, req.FirstFrame)
	require.Equal(t, 10, req.DurationSeconds)
}

func TestValidateVideoCapabilityRejectsBeforeBilling(t *testing.T) {
	catalog := VideoCapabilityCatalog{VideoProviderSeedance: {
		VideoOperationGeneration: {Text: true, FirstFrame: true},
	}}
	req := CanonicalVideoRequest{Operation: VideoOperationExtension, Model: "seedance-2.0"}
	err := catalog.Validate(VideoProviderSeedance, req)
	require.ErrorIs(t, err, ErrVideoUnsupportedCapability)
}
```

- [ ] **Step 2: Run the tests and confirm compile failure**

Run: `cd backend && go test ./internal/service -run 'TestNormalizeVideoRequest|TestValidateVideoCapability' -count=1`

Expected: compile failure because canonical types do not exist.

- [ ] **Step 3: Implement the canonical contract**

```go
type CanonicalVideoRequest struct {
	Operation       VideoOperation
	Model           string
	Prompt          string
	DurationSeconds int
	Resolution      string
	AspectRatio     string
	FirstFrame      []VideoAsset
	LastFrame       []VideoAsset
	ReferenceImages []VideoAsset
	ReferenceVideos []VideoAsset
	Audio           *bool
	ProviderOptions map[string]json.RawMessage
}

type VideoProvider interface {
	Name() string
	Capabilities() VideoProviderCapabilities
	Submit(context.Context, *Account, CanonicalVideoRequest, string) (VideoSubmitResult, error)
	RecoverSubmission(context.Context, *Account, CanonicalVideoRequest, string) (VideoSubmitResult, bool, error)
	Poll(context.Context, *Account, string) (VideoPollResult, error)
	OpenContent(context.Context, *Account, VideoTask) (io.ReadCloser, http.Header, int64, error)
}
```

`Submit` receives the stable provider submission token. Errors must carry `HTTPStatus`, stable public `Code`, `Retryable`, and `Ambiguous` flags. The provider registry must reject duplicate names at construction.

Normalize and validate:

- JSON and current multipart Grok aliases;
- positive duration and supported resolution/ratio syntax;
- HTTPS URLs or existing accepted `data:` image forms only; use `internal/util/urlvalidator` to reject loopback/private/link-local/reserved targets before provider submission;
- operation-specific required inputs;
- maximum request body and asset counts using explicit constants.

- [ ] **Step 4: Run tests**

Run: `cd backend && go test ./internal/service -run 'TestNormalizeVideoRequest|TestValidateVideoCapability|TestVideoProviderRegistry' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/video_request.go backend/internal/service/video_request_test.go backend/internal/service/video_capability.go backend/internal/service/video_capability_test.go backend/internal/service/video_provider.go
git commit -m "feat(video): define canonical provider contract"
```

## Task 5: Implement Deterministic Video Price Resolution

**Files:**

- Create: `backend/internal/service/video_pricing.go`
- Create: `backend/internal/service/video_pricing_test.go`
- Create: `backend/internal/repository/video_pricing_repo.go`
- Create: `backend/internal/repository/video_pricing_repo_integration_test.go`
- Modify: `backend/internal/repository/wire.go`

**Interfaces:**

- Produces `VideoPricingRepository`, `VideoPricingService`, and immutable `VideoPriceQuote` snapshots.
- Exact-resolution and exact-audio rules outrank wildcard rules; ties are rejected at write time by the unique constraint.
- Consumed by Tasks 6, 7, 13, 16, and 17.

- [ ] **Step 1: Write failing resolver tests**

```go
func TestVideoPricingResolveSpecificityAndQuote(t *testing.T) {
	repo := &fakeVideoPricingRepo{rules: []VideoPricingRule{
		{ID: 1, ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_output_second", UnitPrice: 0.10},
		{ID: 2, ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "1080p", AudioMode: "with_audio", Unit: "per_output_second", UnitPrice: 0.25},
	}}
	svc := NewVideoPricingService(repo)
	quote, err := svc.Quote(context.Background(), VideoPricingQuery{GroupID: 7, ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "1080p", Audio: true, DurationSeconds: 6})
	require.NoError(t, err)
	require.EqualValues(t, 2, quote.RuleID)
	require.InDelta(t, 1.50, quote.HoldAmount, 1e-9)
}

func TestVideoPricingMissingRuleFailsClosed(t *testing.T) {
	_, err := NewVideoPricingService(&fakeVideoPricingRepo{}).Quote(context.Background(), VideoPricingQuery{GroupID: 7, ExternalModel: "kling-3.0", Operation: "edit"})
	require.ErrorIs(t, err, ErrVideoPricingUnavailable)
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd backend && go test ./internal/service -run TestVideoPricing -count=1`

Expected: compile failure.

- [ ] **Step 3: Implement repository and resolver**

The quote must store rule ID, unit, unit price, optional upstream cost, units, hold amount, and all matching dimensions. For `per_request`, units are `1`; for `per_output_second`, units are the requested duration. Reject zero/negative duration for per-second rules and negative prices for all rules.

- [ ] **Step 4: Run unit and integration tests**

Run:

```bash
cd backend
go test ./internal/service -run TestVideoPricing -count=1
go test -tags=integration ./internal/repository -run TestVideoPricingRepository -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/video_pricing.go backend/internal/service/video_pricing_test.go backend/internal/repository/video_pricing_repo.go backend/internal/repository/video_pricing_repo_integration_test.go backend/internal/repository/wire.go
git commit -m "feat(video): resolve durable video price quotes"
```

## Task 6: Add Idempotent Balance and Subscription-Quota Holds

**Files:**

- Modify: `backend/internal/service/usage_billing.go`
- Create: `backend/internal/service/video_billing.go`
- Create: `backend/internal/service/video_billing_test.go`
- Modify: `backend/ent/schema/user_subscription.go`
- Create: `backend/migrations/197_video_subscription_frozen_quota.sql`
- Modify: `backend/migrations/unified_video_gateway_migration_test.go`
- Modify: `backend/internal/repository/usage_billing_repo.go`
- Modify: `backend/internal/repository/usage_billing_repo_integration_test.go`
- Modify: `backend/internal/repository/video_task_repo.go`
- Modify: `backend/internal/repository/video_task_repo_integration_test.go`

**Interfaces:**

- Produces balance and subscription-quota variants of `ReserveVideo`, `CaptureVideo`, and `ReleaseVideo` through `UsageBillingRepository`.
- Produces `VideoSubmissionRepository.CreateTaskAndReserve`, which inserts/replays the task and reserves the quoted amount in one PostgreSQL transaction.
- Request IDs are `video_hold:<request_id>`, `video_capture:<request_id>`, and `video_release:<request_id>`.
- Capture atomically deducts the actual amount from frozen balance, deducts no more than the hold, and returns the unused difference to available balance.
- Subscription mode uses `user_subscriptions.frozen_quota`: reserve increments it only when `quota - quota_used - frozen_quota` covers the quote; capture decreases it and increments `quota_used`; release only decreases it.
- Consumed by Tasks 7 and 11.

- [ ] **Step 1: Write failing service tests**

```go
func TestVideoBillingLifecycleNeverCapturesAboveHold(t *testing.T) {
	repo := &fakeVideoBillingRepo{}
	svc := NewVideoBillingService(repo)
	task := VideoTask{RequestID: "vid_1", UserID: 9, APIKeyID: 4, HoldAmount: 2.0, RequestHash: "hash"}
	require.NoError(t, svc.Reserve(context.Background(), task))
	require.ErrorIs(t, svc.Capture(context.Background(), task, 2.01), ErrVideoFinalCostExceedsHold)
	require.NoError(t, svc.Capture(context.Background(), task, 1.25))
	require.Equal(t, []string{"video_hold:vid_1", "video_capture:vid_1"}, repo.requestIDs)
}

func TestVideoBillingUnknownDoesNotReleaseHold(t *testing.T) {
	repo := &fakeVideoBillingRepo{}
	svc := NewVideoBillingService(repo)
	require.NoError(t, svc.HandleTerminal(context.Background(), VideoTask{Status: VideoTaskUnknown}))
	require.Empty(t, repo.requestIDs)
}

func TestVideoSubscriptionQuotaLifecycle(t *testing.T) {
	repo := &fakeVideoBillingRepo{}
	svc := NewVideoBillingService(repo)
	subscriptionID := int64(31)
	task := VideoTask{RequestID: "vid_sub", UserID: 9, APIKeyID: 4, SubscriptionID: &subscriptionID, HoldAmount: 2.0, RequestHash: "hash", BillingMode: "subscription"}
	require.NoError(t, svc.Reserve(context.Background(), task))
	require.NoError(t, svc.Capture(context.Background(), task, 1.25))
	require.Equal(t, []string{"video_hold:vid_sub", "video_capture:vid_sub"}, repo.requestIDs)
	require.Equal(t, []int64{31, 31}, repo.subscriptionIDs)
}

func TestCreateVideoTaskAndReserveIsAtomic(t *testing.T) {
	// Force the hold update to fail and assert that no video_tasks row remains.
	// Retry the same valid command and assert one task plus one hold mutation.
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd backend && go test ./internal/service -run TestVideoBilling -count=1`

Expected: compile failure.

- [ ] **Step 3: Add a video-specific command without refactoring batch image billing**

```go
type VideoHoldCommand struct {
	RequestID          string
	RequestFingerprint string
	RequestPayloadHash string
	UserID             int64
	APIKeyID           int64
	SubscriptionID     *int64
	VideoRequestID     string
	BillingMode        string
	HoldAmount         float64
	ActualAmount       float64
}
```

Keep the batch image command and SQL behavior unchanged. Migration 197 adds `frozen_quota DECIMAL(20,10) NOT NULL DEFAULT 0` with a non-negative check to `user_subscriptions`; add the matching Ent field. Extract the existing usage-billing idempotency claim helper so `videoTaskRepository.CreateTaskAndReserve` can insert/replay the task, claim `video_hold:<request_id>`, and mutate the hold in one SQL transaction. Balance mode locks the user row; subscription mode locks the subscription row. Return `ErrVideoInsufficientBalance` or `ErrVideoSubscriptionQuotaExceeded` before upstream submission when reserve cannot succeed. A transaction failure must leave neither a task nor a hold.

- [ ] **Step 4: Run service and repository tests**

Run:

```bash
cd backend
go generate ./ent
go test ./migrations -run TestUnifiedVideoSubscriptionHoldMigration -count=1
go test ./internal/service -run TestVideoBilling -count=1
go test -tags=integration ./internal/repository -run 'TestUsageBillingRepository.*Video|TestCreateVideoTaskAndReserve' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/usage_billing.go backend/internal/service/video_billing.go backend/internal/service/video_billing_test.go backend/ent backend/migrations/197_video_subscription_frozen_quota.sql backend/migrations/unified_video_gateway_migration_test.go backend/internal/repository/usage_billing_repo.go backend/internal/repository/usage_billing_repo_integration_test.go backend/internal/repository/video_task_repo.go backend/internal/repository/video_task_repo_integration_test.go
git commit -m "feat(video): add idempotent usage holds"
```

## Task 7: Implement Atomic Submission Orchestration and Scheduling

**Files:**

- Create: `backend/internal/service/video_scheduler.go`
- Create: `backend/internal/service/video_task_service.go`
- Create: `backend/internal/service/video_task_service_test.go`
- Modify: `backend/internal/service/openai_gateway_scheduling.go`
- Modify: `backend/internal/service/openai_gateway_service_test.go`

**Interfaces:**

- Produces `VideoTaskService.Submit`, `GetOwned`, and scheduling abstraction `VideoAccountScheduler`.
- Submission order is validate permission/capability -> quote -> atomically create/replay task and reserve -> select/acquire -> mark submitting -> submit -> persist upstream ID or unknown state.
- Consumed by the public handler in Task 12.

- [ ] **Step 1: Write failing orchestration tests**

```go
func TestVideoSubmitRejectsUnsupportedBeforeHold(t *testing.T) {
	deps := newVideoSubmitHarness()
	deps.capabilities.err = ErrVideoUnsupportedCapability
	_, err := deps.service.Submit(context.Background(), deps.command())
	require.ErrorIs(t, err, ErrVideoUnsupportedCapability)
	require.Zero(t, deps.billing.reserveCalls)
	require.Zero(t, deps.provider.submitCalls)
}

func TestVideoSubmitAmbiguousKeepsHoldAndQueuesRecovery(t *testing.T) {
	deps := newVideoSubmitHarness()
	deps.provider.submitErr = VideoProviderError{Code: "upstream_timeout", Retryable: true, Ambiguous: true}
	task, err := deps.service.Submit(context.Background(), deps.command())
	require.NoError(t, err)
	require.Equal(t, VideoTaskUnknown, task.Status)
	require.Equal(t, 1, deps.billing.reserveCalls)
	require.Zero(t, deps.billing.releaseCalls)
	require.NotEmpty(t, task.ProviderSubmissionToken)
}

func TestVideoSubmitReplayReturnsSameTaskWithoutSecondCharge(t *testing.T) {
	deps := newVideoSubmitHarness()
	first, err := deps.service.Submit(context.Background(), deps.commandWithKey("same-key"))
	require.NoError(t, err)
	second, err := deps.service.Submit(context.Background(), deps.commandWithKey("same-key"))
	require.NoError(t, err)
	require.Equal(t, first.RequestID, second.RequestID)
	require.Equal(t, 1, deps.billing.reserveCalls)
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd backend && go test ./internal/service -run TestVideoSubmit -count=1`

Expected: compile failure.

- [ ] **Step 3: Implement the scheduler port**

Reuse `OpenAIGatewayService.SelectAccountWithSchedulerForCapability` through a narrow adapter that accepts forced platform `grok` or `video`, external model, excluded account IDs, and the required operation. The selected account must then pass `VideoProviderRegistry`, account-provider validation, capability validation, model mapping, schedulability, group membership, and concurrency acquisition.

On pre-acceptance errors that are explicitly non-ambiguous, release the hold. On ambiguous network failures, store `unknown`, preserve the provider submission token, set `next_poll_at`, and return the normalized accepted task response rather than issuing a duplicate submission.

- [ ] **Step 4: Add idempotency and request-data scrubbing**

Hash the raw `Idempotency-Key` with SHA-256 before persistence. Compare the canonical request hash on replay. Call `CreateTaskAndReserve` once; a replay returns the existing task without a second hold. Persist the minimized canonical payload only until `MarkSubmitted` stores the upstream ID; then set it to SQL `NULL` in the same transaction.

- [ ] **Step 5: Run tests**

Run: `cd backend && go test ./internal/service -run 'TestVideoSubmit|TestOpenAISelectAccount.*Video' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/service/video_scheduler.go backend/internal/service/video_task_service.go backend/internal/service/video_task_service_test.go backend/internal/service/openai_gateway_scheduling.go backend/internal/service/openai_gateway_service_test.go
git commit -m "feat(video): orchestrate durable video submissions"
```

## Task 8: Extract a Grok Video Adapter and Preserve Legacy Behavior

**Files:**

- Create: `backend/internal/service/video_provider_grok.go`
- Create: `backend/internal/service/video_provider_grok_test.go`
- Modify: `backend/internal/service/openai_gateway_grok.go`
- Modify: `backend/internal/service/openai_gateway_grok_test.go`
- Modify: `backend/internal/handler/grok_media.go`
- Modify: `backend/internal/handler/grok_media_test.go`

**Interfaces:**

- Produces `GrokVideoProvider` with no dependency on `gin.Context` for upstream submission, polling, or content.
- Leaves Grok image generation/edit routes in the existing handler.
- Keeps legacy Redis lookup functions intact for pre-deployment task IDs; Task 12 invokes the fallback.
- Consumed by Tasks 11 and 12.

- [ ] **Step 1: Add contract tests around the extracted adapter**

```go
func TestGrokVideoProviderSubmitPreservesCurrentWireContract(t *testing.T) {
	upstream := &recordingHTTPUpstream{response: response(200, `{"request_id":"up_123"}`)}
	provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))
	got, err := provider.Submit(context.Background(), grokAPIKeyAccount(), CanonicalVideoRequest{
		Operation: VideoOperationGeneration,
		Model: "grok-imagine-video-1.5",
		Prompt: "waves",
		Resolution: "720p",
		DurationSeconds: 10,
	}, "submit-token")
	require.NoError(t, err)
	require.Equal(t, "up_123", got.UpstreamTaskID)
	require.Equal(t, "/v1/videos/generations", upstream.Request.URL.Path)
	require.JSONEq(t, `{"model":"grok-imagine-video","prompt":"waves","resolution":"720p","duration":10}`, string(upstream.Body))
}
```

Add parity cases for image aliases, edit, extension, status mapping, API-key auth, OAuth auth, custom base URL, proxy, failover classification, Range content proxy, and temporary `vidgen.x.ai` URLs.

- [ ] **Step 2: Run current and new Grok tests before refactoring**

Run: `cd backend && go test ./internal/service ./internal/handler -run 'Test.*Grok.*Video|TestGrokMedia' -count=1`

Expected: new adapter test fails; all pre-existing tests pass.

- [ ] **Step 3: Extract request building and response parsing**

Move video-only upstream work from `ForwardGrokMedia` into methods that accept standard Go request/response values. Keep a compatibility wrapper so the existing handler's image paths and test fixtures remain unchanged until Task 12 switches video routing.

`RecoverSubmission` returns `(zero, false, nil)` because Grok has no verified client-token recovery endpoint; an ambiguous Grok submission therefore remains `unknown` for manual review instead of being resubmitted.

- [ ] **Step 4: Run the complete Grok media suite**

Run: `cd backend && go test ./internal/service ./internal/handler -run 'Test.*Grok|TestGrokMedia' -count=1`

Expected: PASS with byte-equivalent request bodies for existing cases.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/video_provider_grok.go backend/internal/service/video_provider_grok_test.go backend/internal/service/openai_gateway_grok.go backend/internal/service/openai_gateway_grok_test.go backend/internal/handler/grok_media.go backend/internal/handler/grok_media_test.go
git commit -m "refactor(video): extract Grok video provider"
```

## Task 9: Implement the Volcengine Seedance Adapter

**Files:**

- Create: `backend/internal/service/video_provider_seedance.go`
- Create: `backend/internal/service/video_provider_seedance_test.go`
- Create: `backend/internal/service/testdata/video/seedance/create_success.json`
- Create: `backend/internal/service/testdata/video/seedance/get_queued.json`
- Create: `backend/internal/service/testdata/video/seedance/get_succeeded.json`
- Create: `backend/internal/service/testdata/video/seedance/get_failed.json`

**Interfaces:**

- Implements `VideoProvider` for `video_provider=seedance` using the official Ark task API.
- Base URL defaults to `https://ark.cn-beijing.volces.com`; account credentials may override `base_url` only after URL validation.
- Submission is `POST /api/v3/contents/generations/tasks`; polling is `GET /api/v3/contents/generations/tasks/{id}`; auth is `Authorization: Bearer <api_key>`.
- Official status mapping: `queued`, `running`, `succeeded`, `failed`, and `cancelled`.
- First release enables generation for text, first frame, last frame, reference images/video, and audio only when the configured upstream model capability declares them. Edit and extension stay disabled unless an official contract fixture proves support for that configured model.

- [ ] **Step 1: Freeze redacted official response fixtures and write failing contract tests**

The create fixture contains `{"id":"cgt-2026-example"}`. The success fixture contains an official-shaped task response with `status`, `content.video_url`, `content.last_frame_url`, `resolution`, `ratio`, and `duration`; it contains no customer prompt or secret.

```go
func TestSeedanceProviderSubmitUsesArkTaskAPI(t *testing.T) {
	upstream := fixtureHTTPUpstream("testdata/video/seedance/create_success.json")
	p := NewSeedanceVideoProvider(upstream)
	got, err := p.Submit(context.Background(), seedanceAccount(), CanonicalVideoRequest{
		Operation: VideoOperationGeneration,
		Model: "seedance-2.0",
		Prompt: "camera pulls back",
		DurationSeconds: 5,
		Resolution: "720p",
		AspectRatio: "16:9",
		Audio: boolPtr(true),
	}, "submit-token")
	require.NoError(t, err)
	require.Equal(t, "cgt-2026-example", got.UpstreamTaskID)
	require.Equal(t, "/api/v3/contents/generations/tasks", upstream.Request.URL.Path)
	require.Equal(t, "Bearer ark-key", upstream.Request.Header.Get("Authorization"))
	require.JSONEq(t, `{"model":"ep-seedance","content":[{"type":"text","text":"camera pulls back"}],"duration":5,"resolution":"720p","ratio":"16:9","generate_audio":true}`, string(upstream.Body))
}

func TestSeedanceProviderPollMapsSucceeded(t *testing.T) {
	p := NewSeedanceVideoProvider(fixtureHTTPUpstream("testdata/video/seedance/get_succeeded.json"))
	got, err := p.Poll(context.Background(), seedanceAccount(), "cgt-2026-example")
	require.NoError(t, err)
	require.Equal(t, VideoTaskSucceeded, got.Status)
	require.Equal(t, "https://example.volces.com/result.mp4", got.ResultURL)
	require.Equal(t, 5, got.ActualDurationSeconds)
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd backend && go test ./internal/service -run TestSeedanceProvider -count=1`

Expected: compile failure.

- [ ] **Step 3: Implement request translation and status parsing**

Build `content` in stable order: prompt text, first frame with role `first_frame`, last frame with role `last_frame`, reference images with role `reference_image`, then reference videos with role `reference_video`. Map the account's model mapping before serialization. Pass only the explicitly allowlisted provider options `seed`, `watermark`, `return_last_frame`, and `service_tier`.

Treat transport timeout after the request body has been sent as ambiguous. Treat Ark validation and sensitive-content 400 responses as non-retryable. Treat 429 and 5xx as retryable only when the response proves no task ID was accepted.

`RecoverSubmission` calls the official list endpoint with the submission token only if the purchased Ark contract exposes a server-side tag field. With the current verified contract it returns `(zero, false, nil)`, so the reconciler never duplicates a possibly accepted task.

- [ ] **Step 4: Run contract tests**

Run: `cd backend && go test ./internal/service -run TestSeedanceProvider -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/video_provider_seedance.go backend/internal/service/video_provider_seedance_test.go backend/internal/service/testdata/video/seedance
git commit -m "feat(video): add Seedance task adapter"
```

## Task 10: Implement the Direct Kling Open Platform Adapter Behind a Contract Gate

**Files:**

- Create: `backend/internal/service/video_provider_kling.go`
- Create: `backend/internal/service/video_provider_kling_auth.go`
- Create: `backend/internal/service/video_provider_kling_test.go`
- Create: `backend/internal/service/testdata/video/kling/text_to_video_create.json`
- Create: `backend/internal/service/testdata/video/kling/text_to_video_succeeded.json`
- Create: `backend/internal/service/testdata/video/kling/image_to_video_create.json`
- Create: `backend/internal/service/testdata/video/kling/image_to_video_succeeded.json`
- Create: `backend/internal/service/testdata/video/kling/video_extend_create.json`
- Create: `backend/internal/service/testdata/video/kling/video_extend_succeeded.json`

**Interfaces:**

- Implements `VideoProvider` for the direct Kling Open Platform, not Alibaba-hosted Kling and not a third-party Kling reseller.
- Uses the purchased account's official API domain and access-key/secret-key JWT authentication.
- Contract fixtures are copied from the authenticated official Kling developer console and redacted before code is enabled.
- If any fixture path or field differs from the contract below, update the adapter and its tests to match the official fixture in the same commit; do not add permissive multi-shape parsing.

- [ ] **Step 1: Export and sanitize the six official contract fixtures**

From the user's authenticated Kling Open Platform documentation, save one create and one successful-query example for text-to-video, image-to-video, and video extension. Replace task IDs, request IDs, URLs, access keys, and user content with deterministic values while retaining every field name, nesting level, number/string type, and status value.

The expected contract gate is:

| Operation | Create path | Query path |
| --- | --- | --- |
| text generation | `POST /v1/videos/text2video` | `GET /v1/videos/text2video/{task_id}` |
| first/last-frame generation | `POST /v1/videos/image2video` | `GET /v1/videos/image2video/{task_id}` |
| extension | `POST /v1/videos/video-extend` | `GET /v1/videos/video-extend/{task_id}` |

JWT claims must be `iss=<access_key>`, `exp=now+1800`, and `nbf=now-5`, signed HS256 with the secret key. The adapter must inject a clock for deterministic tests.

- [ ] **Step 2: Write failing authentication and fixture tests**

```go
func TestKlingJWTClaims(t *testing.T) {
	clock := fixedClock(time.Unix(1_800_000_000, 0))
	token, err := SignKlingJWT("access", "secret", clock)
	require.NoError(t, err)
	claims := parseAndVerifyKlingJWT(t, token, "secret")
	require.Equal(t, "access", claims.Issuer)
	require.Equal(t, int64(1_800_001_800), claims.ExpiresAt.Unix())
	require.Equal(t, int64(1_799_999_995), claims.NotBefore.Unix())
}

func TestKlingProviderImageToVideoContract(t *testing.T) {
	upstream := fixtureHTTPUpstream("testdata/video/kling/image_to_video_create.json")
	p := NewKlingVideoProvider(upstream, fixedClock(time.Unix(1_800_000_000, 0)))
	got, err := p.Submit(context.Background(), klingAccount(), CanonicalVideoRequest{
		Operation: VideoOperationGeneration,
		Model: "kling-3.0",
		Prompt: "animate",
		FirstFrame: []VideoAsset{{URL: "https://example.com/first.png"}},
		LastFrame: []VideoAsset{{URL: "https://example.com/last.png"}},
		DurationSeconds: 5,
	}, "submit-token")
	require.NoError(t, err)
	require.Equal(t, "kling_task_example", got.UpstreamTaskID)
	require.Equal(t, "/v1/videos/image2video", upstream.Request.URL.Path)
}
```

- [ ] **Step 3: Run and confirm failure**

Run: `cd backend && go test ./internal/service -run 'TestKlingJWT|TestKlingProvider' -count=1`

Expected: compile failure.

- [ ] **Step 4: Implement strict operation routing and parsing**

Map the external model through the account before writing `model_name`. Select exactly one create/query pair from the table based on canonical operation and inputs. Reject edits unless the official fixture set contains a distinct edit contract and the capability catalog is updated in the same commit. Allowlist provider options rather than forwarding arbitrary JSON.

Normalize official `submitted`/`processing`/`succeed`/`failed` states, read the first returned video URL and duration, and keep the raw provider status in the task event. Never log JWTs or secret keys. Cache each JWT in memory only until five minutes before expiry.

`RecoverSubmission` uses an official client task identifier only if the fixture contract proves one exists. Otherwise it returns `(zero, false, nil)` and ambiguous submissions remain held for manual resolution.

- [ ] **Step 5: Run contract tests**

Run: `cd backend && go test ./internal/service -run 'TestKlingJWT|TestKlingProvider' -count=1`

Expected: PASS against all six official fixtures.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/service/video_provider_kling.go backend/internal/service/video_provider_kling_auth.go backend/internal/service/video_provider_kling_test.go backend/internal/service/testdata/video/kling
git commit -m "feat(video): add direct Kling task adapter"
```

## Task 11: Run Multi-Instance Reconciliation, Settlement, and Retention

**Files:**

- Modify: `backend/internal/config/config.go`
- Modify: `backend/internal/config/config_test.go`
- Modify: `deploy/config.example.yaml`
- Create: `backend/internal/service/video_reconciler.go`
- Create: `backend/internal/service/video_reconciler_test.go`
- Create: `backend/internal/service/video_runtime.go`
- Create: `backend/internal/service/video_runtime_test.go`
- Create: `backend/internal/service/video_retention.go`
- Create: `backend/internal/service/video_retention_test.go`
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/cmd/server/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`

**Interfaces:**

- Produces a start/stop runtime safe on every replica.
- Leases due tasks from PostgreSQL and polls only the stored account ID and provider.
- Handles submitted/queued/running/succeeded/failed/unknown without relying on caller polling.
- Clears expired result URLs and old minimized payloads; keeps billing/task audit rows.

- [ ] **Step 1: Write failing state-machine tests**

```go
func TestVideoReconcilerSuccessSettlesOnce(t *testing.T) {
	h := newVideoReconcilerHarness(VideoPollResult{Status: VideoTaskSucceeded, ResultURL: "https://cdn.example.com/v.mp4", ActualDurationSeconds: 6})
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 1, h.billing.captureCalls)
	require.Equal(t, 0, h.billing.releaseCalls)
	require.Equal(t, VideoTaskSucceeded, h.repo.applied.Status)
	require.NotEmpty(t, h.repo.settled.PricingSnapshot)
}

func TestVideoReconcilerFailureReleasesOnce(t *testing.T) {
	h := newVideoReconcilerHarness(VideoPollResult{Status: VideoTaskFailed, Error: VideoTaskError{Code: "content_rejected"}})
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Equal(t, 1, h.billing.releaseCalls)
	require.Equal(t, 0, h.billing.captureCalls)
}

func TestVideoReconcilerUnknownNeverResubmitsWithoutRecoveryProof(t *testing.T) {
	h := newUnknownVideoReconcilerHarness()
	require.NoError(t, h.reconciler.Process(context.Background(), h.task))
	require.Zero(t, h.provider.submitCalls)
	require.Equal(t, 1, h.provider.recoverCalls)
	require.Equal(t, VideoTaskUnknown, h.repo.applied.Status)
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd backend && go test ./internal/service -run 'TestVideoReconciler|TestVideoRuntime|TestVideoRetention' -count=1`

Expected: compile failure.

- [ ] **Step 3: Add explicit configuration with safe defaults**

```go
type VideoConfig struct {
	Enabled                  bool `mapstructure:"enabled"`
	GrokEnabled              bool `mapstructure:"grok_enabled"`
	SeedanceEnabled          bool `mapstructure:"seedance_enabled"`
	KlingEnabled             bool `mapstructure:"kling_enabled"`
	WorkerCount              int  `mapstructure:"worker_count"`
	LeaseSeconds             int  `mapstructure:"lease_seconds"`
	PollIntervalSeconds      int  `mapstructure:"poll_interval_seconds"`
	RetryBaseSeconds         int  `mapstructure:"retry_base_seconds"`
	RetryMaxSeconds          int  `mapstructure:"retry_max_seconds"`
	MaxPollAttempts          int  `mapstructure:"max_poll_attempts"`
	UnknownReviewAfterHours  int  `mapstructure:"unknown_review_after_hours"`
	ResultMetadataRetentionDays int `mapstructure:"result_metadata_retention_days"`
}
```

Defaults: globally disabled, all provider flags disabled, 2 workers, 60-second lease, 10-second poll, 5-second retry base, 300-second retry max, 720 poll attempts, 24-hour unknown-review threshold, and 30-day result-metadata retention. Validation rejects negative values and rejects an enabled runtime with zero workers or lease shorter than the poll interval. A disabled provider rejects only new submissions; reconciliation for already-held tasks continues until they are terminal or manually resolved.

- [ ] **Step 4: Implement reconciliation transitions**

Use capped exponential backoff with jitter after retryable poll failures. Renew or finish within the lease. Settlement must be idempotent and use the stored price snapshot; never re-resolve current pricing. If actual duration produces a final amount above the hold, record `settlement_cost_exceeds_hold`, capture only the held amount, and surface the discrepancy for admin review.

For `unknown`, call `RecoverSubmission` with stored sanitized payload and submission token. If recovered, persist upstream ID and continue. If not recoverable, keep the hold and move the next review time; never call `Submit` again automatically.

- [ ] **Step 5: Wire lifecycle start and shutdown**

`ProvideVideoRuntime` starts the workers when enabled. Add it to `provideCleanup`, call `Stop`, and regenerate Wire. No goroutine may survive `Stop()` in `video_runtime_test.go` under `go test -race`.

- [ ] **Step 6: Run tests**

Run:

```bash
cd backend
go generate ./cmd/server
go test ./internal/config ./internal/service -run 'TestVideo|TestConfig.*Video' -count=1
go test -race ./internal/service -run TestVideoRuntime -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/config/config.go backend/internal/config/config_test.go deploy/config.example.yaml backend/internal/service/video_reconciler.go backend/internal/service/video_reconciler_test.go backend/internal/service/video_runtime.go backend/internal/service/video_runtime_test.go backend/internal/service/video_retention.go backend/internal/service/video_retention_test.go backend/internal/service/wire.go backend/cmd/server/wire.go backend/cmd/server/wire_gen.go
git commit -m "feat(video): reconcile and settle distributed tasks"
```

## Task 12: Switch the Five Public Routes to a Dedicated Video Handler

**Files:**

- Create: `backend/internal/handler/video_handler.go`
- Create: `backend/internal/handler/video_handler_test.go`
- Modify: `backend/internal/handler/handler.go`
- Modify: `backend/internal/handler/wire.go`
- Modify: `backend/internal/server/routes/gateway.go`
- Modify: `backend/internal/server/routes/gateway_test.go`
- Modify: `backend/internal/service/grok_media.go`
- Modify: `backend/internal/service/grok_media_content_test.go`
- Create: `backend/internal/service/video_content_fetch.go`
- Create: `backend/internal/service/video_content_fetch_test.go`
- Modify: `backend/cmd/server/wire_gen.go`

**Interfaces:**

- Produces the five stable public endpoints for `grok`, `video`, and composite groups.
- Submission responses return `request_id=vid_*`, `status`, `provider`, `model`, and timestamps while preserving accepted Grok compatibility fields.
- Status/content authorize by task owner (`user_id`, `api_key_id`); another key receives 404.
- Durable lookup precedes legacy Grok Redis fallback.

- [ ] **Step 1: Write failing route and ownership tests**

```go
func TestVideoRoutesDispatchGrokVideoToDedicatedHandler(t *testing.T) {
	router := newGatewayRouterWithVideoSpy(t, PlatformGrok)
	w := performJSON(router, http.MethodPost, "/v1/videos/generations", `{"model":"grok-imagine-video","prompt":"waves"}`)
	require.Equal(t, http.StatusAccepted, w.Code)
	require.Equal(t, 1, videoSpy.SubmitCalls)
	require.Equal(t, 0, grokMediaSpy.VideoCalls)
}

func TestVideoStatusDoesNotLeakAcrossAPIKeys(t *testing.T) {
	h := newVideoHandlerHarnessOwnedBy(10, 20)
	w := h.getStatusAs(10, 21, "vid_abc")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestVideoStatusFallsBackToLegacyGrokBinding(t *testing.T) {
	h := newVideoHandlerHarnessWithLegacyTask("old_grok_id")
	w := h.getStatusAs(10, 20, "old_grok_id")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, h.legacyLookupCalls)
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd backend && go test ./internal/handler ./internal/server/routes -run 'TestVideo|Test.*VideoRoutes' -count=1`

Expected: compile or assertion failure because routes still dispatch directly to Grok media.

- [ ] **Step 3: Implement submit/status handlers and error envelope**

Require authentication, group `allow_video_generation`, billing eligibility, body-size limit, and idempotency key processing. Return stable public errors: `invalid_request_error`, `unsupported_capability`, `video_pricing_unavailable`, `insufficient_balance`, `no_available_account`, `not_found_error`, `upstream_error`, and `rate_limit_error`.

Return HTTP 202 for accepted `created/submitting/submitted/queued/running/unknown` tasks, 200 for status reads, and existing compatible 4xx/5xx codes for rejected submissions.

- [ ] **Step 4: Implement safe content serving**

If the task has a public HTTPS result URL, use a dedicated fetcher that validates the initial URL, resolves every dial target, rejects loopback/private/link-local/reserved addresses, revalidates every redirect, caps redirects, response bytes, and duration, and streams with Range support. If the provider requires authentication, call `provider.OpenContent` with the task's stored account. Forward only `Content-Type`, `Content-Length`, `Content-Range`, `Accept-Ranges`, `ETag`, `Last-Modified`, and `Content-Disposition`; never forward upstream auth/cookie headers.

- [ ] **Step 5: Preserve old task IDs**

When no durable task exists and the group can route Grok, call the existing Redis account-binding lookup and old Grok status/content forwarding. Do not write a synthetic ledger entry or retroactively charge a legacy task.

- [ ] **Step 6: Wire and run compatibility tests**

Run:

```bash
cd backend
go generate ./cmd/server
go test ./internal/handler ./internal/server/routes -run 'TestVideo|TestGrokMedia' -count=1
go test ./internal/service -run 'Test.*Grok.*Video|TestGrokMediaContent' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/handler/video_handler.go backend/internal/handler/video_handler_test.go backend/internal/handler/handler.go backend/internal/handler/wire.go backend/internal/server/routes/gateway.go backend/internal/server/routes/gateway_test.go backend/internal/service/grok_media.go backend/internal/service/grok_media_content_test.go backend/internal/service/video_content_fetch.go backend/internal/service/video_content_fetch_test.go backend/cmd/server/wire_gen.go
git commit -m "feat(video): route unified public video API"
```

## Task 13: Add Video Account Administration and Non-Billable Connection Tests

**Files:**

- Modify: `backend/internal/handler/admin/account_handler.go`
- Create: `backend/internal/handler/admin/account_handler_video_test.go`
- Modify: `backend/internal/service/account_test_service.go`
- Create: `backend/internal/service/account_test_service_video_test.go`
- Create: `backend/internal/service/video_account_admin.go`
- Create: `backend/internal/service/video_account_admin_test.go`

**Interfaces:**

- Admin create/edit accepts provider, credentials, base URL, model mapping, disabled capabilities, concurrency, proxy, status, and group bindings.
- Read responses expose `video_provider` and derived capability tags but preserve existing secret masking.
- Connection test performs a non-billable authenticated probe only. A billable video smoke test is not part of the generic account test action.

- [ ] **Step 1: Write failing validation and probe tests**

```go
func TestVideoAccountAdminResponseNeverExposesSecrets(t *testing.T) {
	account := &Account{Platform: PlatformVideo, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_key": "access", "secret_key": "secret"},
		Extra: map[string]any{"video_provider": "kling"}}
	got := BuildVideoAccountAdminMetadata(account)
	require.Equal(t, "kling", got.Provider)
	require.NotContains(t, fmt.Sprint(got), "secret")
}

func TestAccountTestVideoDoesNotCreateGenerationTask(t *testing.T) {
	probe := &recordingVideoAccountProbe{}
	svc := newAccountTestServiceWithVideoProbe(probe)
	_, err := svc.TestAccount(context.Background(), videoAccountID)
	require.NoError(t, err)
	require.Equal(t, 1, probe.CredentialProbeCalls)
	require.Zero(t, probe.GenerationCalls)
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd backend && go test ./internal/service ./internal/handler/admin -run 'TestVideoAccount|TestAccountTestVideo' -count=1`

Expected: compile failure.

- [ ] **Step 3: Implement create/update DTO behavior**

Store these keys only:

```text
credentials.api_key                         Seedance
credentials.access_key                     Kling
credentials.secret_key                     Kling
credentials.base_url                       optional validated HTTPS override
extra.video_provider                       seedance | kling
extra.model_mapping                        existing mapping object
extra.video_disabled_capabilities          string array
```

Reject secret-clearing updates unless the request explicitly requests account disablement. Reuse existing credential masking and merging behavior.

- [ ] **Step 4: Implement safe probes**

For Seedance, use an official non-generation endpoint available to the account, or validate auth through a list request with page size one. For Kling, use the official account/health endpoint exported with the contract fixtures. If the purchased contract has no non-billable authenticated endpoint, return `probe_not_supported` with validated local configuration instead of submitting a video.

- [ ] **Step 5: Run tests**

Run: `cd backend && go test ./internal/service ./internal/handler/admin -run 'TestVideoAccount|TestAccountTestVideo' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/handler/admin/account_handler.go backend/internal/handler/admin/account_handler_video_test.go backend/internal/service/account_test_service.go backend/internal/service/account_test_service_video_test.go backend/internal/service/video_account_admin.go backend/internal/service/video_account_admin_test.go
git commit -m "feat(video): administer provider accounts safely"
```

## Task 14: Add Pricing-Rule and Video-Task Operations APIs

**Files:**

- Create: `backend/internal/handler/admin/video_handler.go`
- Create: `backend/internal/handler/admin/video_handler_test.go`
- Create: `backend/internal/service/admin_video.go`
- Create: `backend/internal/service/admin_video_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Modify: `backend/internal/handler/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`
- Modify: `backend/internal/service/ops_metrics_collector.go`
- Create: `backend/internal/service/ops_metrics_collector_video_test.go`

**Interfaces:**

- Produces group-scoped pricing CRUD and admin task list/detail/actions.
- Endpoints:
  - `GET /api/v1/admin/groups/{id}/video-pricing-rules`
  - `PUT /api/v1/admin/groups/{id}/video-pricing-rules`
  - `GET /api/v1/admin/video/tasks`
  - `GET /api/v1/admin/video/tasks/{request_id}`
  - `POST /api/v1/admin/video/tasks/{request_id}/reconcile`
  - `POST /api/v1/admin/video/tasks/{request_id}/refund`
  - `POST /api/v1/admin/video/tasks/{request_id}/complete`
- Financial mutations use step-up protection when enabled, require `Idempotency-Key`, and write audit logs.
- Consumed by Tasks 16 and 17.

- [ ] **Step 1: Write failing admin policy tests**

```go
func TestAdminVideoRefundRequiresUnknownOrFailedUnsettledTask(t *testing.T) {
	svc := newAdminVideoHarness(VideoTask{Status: VideoTaskSucceeded, BillingStatus: "captured"})
	err := svc.service.Refund(context.Background(), "vid_1", AdminVideoRefundCommand{ActorUserID: 1, IdempotencyKey: "refund-1"})
	require.ErrorIs(t, err, ErrVideoFinancialStateConflict)
	require.Zero(t, svc.billing.releaseCalls)
}

func TestAdminVideoPricingRejectsOverlapAndMissingCoverage(t *testing.T) {
	svc := newAdminVideoHarnessForPricing()
	err := svc.service.ReplacePricingRules(context.Background(), 7, []VideoPricingRuleInput{
		{ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_request", UnitPrice: 1},
		{ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_request", UnitPrice: 2},
	})
	require.ErrorIs(t, err, ErrVideoPricingRuleOverlap)
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd backend && go test ./internal/service ./internal/handler/admin -run TestAdminVideo -count=1`

Expected: compile failure.

- [ ] **Step 3: Implement pricing replacement in one transaction**

Validate model name, operation, resolution, audio mode, unit, non-negative prices, duplicate dimensions, and capability coverage. Replace a group's rules atomically so readers never observe a partial set. Existing Grok `video_price_480p/720p/1080p` fields remain a compatibility fallback until the group has at least one explicit rule.

- [ ] **Step 4: Implement audited task actions**

- Reconcile: clear an expired lease and set `next_poll_at=now` only for non-terminal tasks.
- Refund: release a held amount only for `unknown` or failed-unsettled tasks, mark billing `released`, append an event, and never change a captured row.
- Complete: require provider task ID, verified result URL, actual duration/resolution, and a final amount no greater than the hold; capture and mark success atomically/idempotently.

Every action records actor user ID, request ID, reason, before/after state, and idempotency key in the existing audit system without storing secrets or prompt text.

- [ ] **Step 5: Add metrics**

Record counters/histograms for submission latency, provider queue time, completion time, status, rate limits, unknown count/age, expired leases, pending settlement, failed refund, revenue, upstream cost, and margin. Keep provider/model/operation/group dimensions bounded by configured values.

- [ ] **Step 6: Wire and run tests**

Run:

```bash
cd backend
go generate ./cmd/server
go test ./internal/service ./internal/handler/admin -run 'TestAdminVideo|TestVideo.*Metric' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/handler/admin/video_handler.go backend/internal/handler/admin/video_handler_test.go backend/internal/service/admin_video.go backend/internal/service/admin_video_test.go backend/internal/server/routes/admin.go backend/internal/handler/wire.go backend/cmd/server/wire_gen.go backend/internal/service/ops_metrics_collector.go backend/internal/service/ops_metrics_collector_video_test.go
git commit -m "feat(video): expose audited video operations"
```

## Task 15: Add the Video Account Create/Edit Experience

**Files:**

- Modify: `frontend/src/types/index.ts`
- Modify: `frontend/src/utils/platformColors.ts`
- Modify: `frontend/src/components/common/PlatformIcon.vue`
- Modify: `frontend/src/components/account/credentialsBuilder.ts`
- Modify: `frontend/src/components/account/CreateAccountModal.vue`
- Modify: `frontend/src/components/account/EditAccountModal.vue`
- Create: `frontend/src/components/account/VideoProviderFields.vue`
- Create: `frontend/src/components/account/__tests__/VideoProviderFields.spec.ts`
- Modify: `frontend/src/components/account/__tests__/credentialsBuilder.spec.ts`
- Modify: `frontend/src/components/account/__tests__/CreateAccountModal.spec.ts`
- Modify: `frontend/src/components/account/__tests__/EditAccountModal.spec.ts`
- Modify: `frontend/src/i18n/locales/zh/admin/accounts.ts`
- Modify: `frontend/src/i18n/locales/en/admin/accounts.ts`

**Interfaces:**

- Adds `video` to account/group platform unions and renders it with an existing icon-library Video icon.
- Provides provider-specific credentials, model mappings, base URL, derived capability tags, and capability-disable controls.
- Never renders a secret value received from the API.

- [ ] **Step 1: Write failing component tests**

```ts
it('builds Seedance credentials without Kling fields', () => {
  expect(buildCredentials({ platform: 'video', videoProvider: 'seedance', apiKey: 'ark', accessKey: '', secretKey: '' }))
    .toEqual({ api_key: 'ark' })
})

it('requires both Kling keys and emits provider metadata', async () => {
  const wrapper = mount(VideoProviderFields, { props: { provider: 'kling', credentials: {} } })
  await wrapper.get('[data-testid="video-access-key"]').setValue('ak')
  await wrapper.get('[data-testid="video-secret-key"]').setValue('sk')
  expect(wrapper.emitted('update:credentials')?.at(-1)?.[0]).toMatchObject({ access_key: 'ak', secret_key: 'sk' })
})
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd frontend && pnpm test:run -- src/components/account/__tests__/VideoProviderFields.spec.ts src/components/account/__tests__/credentialsBuilder.spec.ts`

Expected: compile failure because Video platform fields do not exist.

- [ ] **Step 3: Implement with existing modal patterns**

Add one Video platform option beside the existing platform choices. When selected, force API-key account type and show:

- provider select: Seedance / Kling;
- Seedance API key or Kling access/secret pair;
- optional validated base URL;
- model mapping editor already used by other API-key accounts;
- read-only derived capability badges;
- checkboxes to disable a provider capability for that account.

Use existing input, select, badge, disclosure, validation, and secret-mask components. Do not add custom SVG artwork or a new visual system.

- [ ] **Step 4: Run account tests and typecheck**

Run:

```bash
cd frontend
pnpm test:run -- src/components/account/__tests__/VideoProviderFields.spec.ts src/components/account/__tests__/credentialsBuilder.spec.ts src/components/account/__tests__/CreateAccountModal.spec.ts src/components/account/__tests__/EditAccountModal.spec.ts
pnpm typecheck
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/types/index.ts frontend/src/utils/platformColors.ts frontend/src/components/common/PlatformIcon.vue frontend/src/components/account/credentialsBuilder.ts frontend/src/components/account/CreateAccountModal.vue frontend/src/components/account/EditAccountModal.vue frontend/src/components/account/VideoProviderFields.vue frontend/src/components/account/__tests__/VideoProviderFields.spec.ts frontend/src/components/account/__tests__/credentialsBuilder.spec.ts frontend/src/components/account/__tests__/CreateAccountModal.spec.ts frontend/src/components/account/__tests__/EditAccountModal.spec.ts frontend/src/i18n/locales/zh/admin/accounts.ts frontend/src/i18n/locales/en/admin/accounts.ts
git commit -m "feat(video): add provider account forms"
```

## Task 16: Add Video Permission and Pricing-Rule Editing to Groups

**Files:**

- Modify: `frontend/src/types/index.ts`
- Modify: `frontend/src/api/admin/groups.ts`
- Create: `frontend/src/components/admin/group/VideoPricingRulesEditor.vue`
- Create: `frontend/src/components/admin/group/__tests__/VideoPricingRulesEditor.spec.ts`
- Modify: `frontend/src/views/admin/GroupsView.vue`
- Create: `frontend/src/views/admin/groupsVideoPricing.ts`
- Create: `frontend/src/views/admin/__tests__/groupsVideoPricing.spec.ts`
- Modify: `frontend/src/views/admin/__tests__/GroupsView.duplicate.spec.ts`
- Modify: `frontend/src/i18n/locales/zh/admin/overview.ts`
- Modify: `frontend/src/i18n/locales/en/admin/overview.ts`

**Interfaces:**

- Adds a separate `allow_video_generation` control; it does not reuse image-generation permission.
- Edits model/operation/resolution/audio/unit rules and previews the final customer quote.
- Warns when an enabled external video model has no covering rule.

- [ ] **Step 1: Write failing pure-helper and component tests**

```ts
it('selects the most specific video pricing rule', () => {
  const rules = [
    rule({ id: 1, resolution: '*', audio_mode: 'any', unit_price: 0.1 }),
    rule({ id: 2, resolution: '1080p', audio_mode: 'with_audio', unit_price: 0.25 }),
  ]
  expect(resolveVideoPricingRule(rules, { model: 'seedance-2.0', operation: 'generation', resolution: '1080p', audio: true })?.id).toBe(2)
})

it('emits an explicit missing-coverage warning', () => {
  const wrapper = mount(VideoPricingRulesEditor, { props: { rules: [], enabledModels: ['kling-3.0'] } })
  expect(wrapper.get('[data-testid="video-pricing-missing"]').text()).toContain('kling-3.0')
})
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd frontend && pnpm test:run -- src/components/admin/group/__tests__/VideoPricingRulesEditor.spec.ts src/views/admin/__tests__/groupsVideoPricing.spec.ts`

Expected: compile failure.

- [ ] **Step 3: Implement the rule editor using current group-form components**

Each row edits external model, operation, resolution (`*` allowed), audio mode, unit, customer unit price, optional upstream unit cost, and enabled status. Client validation mirrors backend rules, but the backend remains authoritative. Show a 5-second and 10-second price preview for per-second rules and a margin preview when upstream cost exists.

On group duplication, copy pricing rules only after the duplicate group ID is returned; use one replacement API request and surface partial failure without deleting the new group.

- [ ] **Step 4: Run tests and typecheck**

Run:

```bash
cd frontend
pnpm test:run -- src/components/admin/group/__tests__/VideoPricingRulesEditor.spec.ts src/views/admin/__tests__/groupsVideoPricing.spec.ts src/views/admin/__tests__/GroupsView.duplicate.spec.ts
pnpm typecheck
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/types/index.ts frontend/src/api/admin/groups.ts frontend/src/components/admin/group/VideoPricingRulesEditor.vue frontend/src/components/admin/group/__tests__/VideoPricingRulesEditor.spec.ts frontend/src/views/admin/GroupsView.vue frontend/src/views/admin/groupsVideoPricing.ts frontend/src/views/admin/__tests__/groupsVideoPricing.spec.ts frontend/src/views/admin/__tests__/GroupsView.duplicate.spec.ts frontend/src/i18n/locales/zh/admin/overview.ts frontend/src/i18n/locales/en/admin/overview.ts
git commit -m "feat(video): configure group video pricing"
```

## Task 17: Add a Video Tasks View to Existing Usage Operations

**Files:**

- Modify: `frontend/src/api/admin/usage.ts`
- Create: `frontend/src/components/usage/VideoTasksTable.vue`
- Create: `frontend/src/components/usage/VideoTaskDetailModal.vue`
- Create: `frontend/src/components/usage/__tests__/VideoTasksTable.spec.ts`
- Create: `frontend/src/components/usage/__tests__/VideoTaskDetailModal.spec.ts`
- Modify: `frontend/src/views/admin/UsageView.vue`
- Modify: `frontend/src/views/admin/__tests__/UsageView.spec.ts`
- Modify: `frontend/src/i18n/locales/zh/misc.ts`
- Modify: `frontend/src/i18n/locales/en/misc.ts`

**Interfaces:**

- Adds a `video` detail tab beside usage/errors/ranking, not a new top-level admin page.
- Displays request ID, user, provider, model, operation, status/progress, elapsed time, frozen/final/upstream cost, and timestamps.
- Detail modal shows state events and sanitized provider errors; reconcile/refund/complete actions use confirmation and idempotency keys.

- [ ] **Step 1: Write failing view tests**

```ts
it('loads video tasks only after the video tab is opened', async () => {
  const wrapper = mountUsageView()
  expect(adminUsageAPI.listVideoTasks).not.toHaveBeenCalled()
  await wrapper.get('[data-testid="usage-detail-tab-video"]').trigger('click')
  expect(adminUsageAPI.listVideoTasks).toHaveBeenCalledTimes(1)
})

it('disables refund for captured tasks', () => {
  const wrapper = mount(VideoTaskDetailModal, { props: { task: videoTask({ billing_status: 'captured', status: 'succeeded' }) } })
  expect(wrapper.get('[data-testid="video-task-refund"]').attributes('disabled')).toBeDefined()
})
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd frontend && pnpm test:run -- src/components/usage/__tests__/VideoTasksTable.spec.ts src/components/usage/__tests__/VideoTaskDetailModal.spec.ts src/views/admin/__tests__/UsageView.spec.ts`

Expected: compile failure.

- [ ] **Step 3: Implement lazy data loading and audited actions**

Reuse the existing usage card, tab, filters, pagination, table, badge, modal, and toast patterns. Filters cover provider, model, operation, status, group, user, and date range. Create a UUID idempotency key when the confirmation dialog opens and reuse it for retries of the same action.

Do not display raw request payload, prompt, authorization headers, credentials, or full signed result query strings. Render the result host and expiry separately.

- [ ] **Step 4: Run tests, typecheck, and build**

Run:

```bash
cd frontend
pnpm test:run -- src/components/usage/__tests__/VideoTasksTable.spec.ts src/components/usage/__tests__/VideoTaskDetailModal.spec.ts src/views/admin/__tests__/UsageView.spec.ts
pnpm typecheck
pnpm build
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/api/admin/usage.ts frontend/src/components/usage/VideoTasksTable.vue frontend/src/components/usage/VideoTaskDetailModal.vue frontend/src/components/usage/__tests__/VideoTasksTable.spec.ts frontend/src/components/usage/__tests__/VideoTaskDetailModal.spec.ts frontend/src/views/admin/UsageView.vue frontend/src/views/admin/__tests__/UsageView.spec.ts frontend/src/i18n/locales/zh/misc.ts frontend/src/i18n/locales/en/misc.ts
git commit -m "feat(video): add video task operations view"
```

## Task 18: Complete End-to-End Verification and Controlled Rollout

**Files:**

- Create: `backend/internal/integration/video_gateway_e2e_test.go`
- Create: `backend/internal/integration/video_gateway_legacy_grok_e2e_test.go`
- Create: `docs/video-gateway-runbook.md`
- Modify: `deploy/config.example.yaml`
- Modify: `README.md`

**Interfaces:**

- Verifies compatibility, idempotency, multi-instance leasing, settlement, security, and operator recovery as one release gate.
- Documents internal smoke tests without committing real credentials or billable provider outputs.

- [ ] **Step 1: Add end-to-end tests with fake provider servers**

Cover these cases:

1. Grok, Seedance, and Kling generation each return one stable `vid_*` ID.
2. Repeated `Idempotency-Key` with identical canonical body returns the same task and one hold.
3. Same key with different body returns HTTP 409.
4. Two app instances reconcile one due task exactly once.
5. Successful task captures below/equal hold and releases the difference.
6. Failed task releases the hold.
7. Ambiguous submission remains unknown and is never automatically resubmitted.
8. Unsupported edit/extension fails before billing.
9. Another API key cannot read status/content.
10. Content proxy rejects private, loopback, link-local, DNS-rebound, and unsafe-redirect targets.
11. Pre-deployment Grok ID resolves through Redis fallback without a new charge.
12. Redis restart does not affect a new durable task.

- [ ] **Step 2: Run focused end-to-end tests**

Run: `cd backend && go test -tags=e2e -v -timeout=300s ./internal/integration -run 'TestVideoGateway|TestLegacyGrokVideo' -count=1`

Expected: PASS.

- [ ] **Step 3: Run the full verification suite**

Run:

```bash
cd backend
go generate ./ent
go generate ./cmd/server
go test ./...
go test -race ./internal/service -run 'TestVideo|TestGrok' -count=1

cd ../frontend
pnpm test:run
pnpm typecheck
pnpm build
```

Expected: every command exits 0, and `git diff --exit-code` after generation confirms generated files are committed.

- [ ] **Step 4: Write the operator runbook**

Document:

- migration order 194 through 197;
- enabling `video.enabled` with one worker in one internal environment first;
- adding a Video group, permission, provider account, model mapping, and complete pricing coverage;
- non-billable account probe;
- one explicitly confirmed low-cost Seedance smoke task and one Kling smoke task;
- checking task event, hold, final charge, upstream cost, URL expiry, and content proxy;
- unknown/manual-review and failed-refund procedures;
- legacy Grok fallback verification;
- provider-specific feature flags and rollback, which disables new submissions while reconciliation remains on until all held tasks are terminal or manually resolved.

- [ ] **Step 5: Perform the rollout gates**

1. Deploy migrations with all provider adapters disabled.
2. Enable durable Grok submissions only; compare request bodies, status, content, and billing against the current release.
3. Enable Seedance for one internal group and run the confirmed low-cost smoke test.
4. Enable Kling only after all six direct official contract fixtures pass and run its confirmed low-cost smoke test.
5. Enable the mixed Video group, then the optional composite Grok + Video group.
6. Watch unknown-task age, reconciliation lag, settlement/refund failures, provider error rate, and margin for at least one full billing cycle before wider access.

- [ ] **Step 6: Commit the release gate**

```bash
git add backend/internal/integration/video_gateway_e2e_test.go backend/internal/integration/video_gateway_legacy_grok_e2e_test.go docs/video-gateway-runbook.md deploy/config.example.yaml README.md
git commit -m "test(video): add gateway release gates"
```

## Final Verification Checklist

- [ ] `git status --short` shows no unintended files and `.superpowers/` remains untracked.
- [ ] Every new migration is embedded by `backend/migrations/migrations.go` and passes regression tests.
- [ ] Generated Ent and Wire output is committed and reproducible.
- [ ] Existing Grok image, Grok video, OpenAI, Gemini, Anthropic, Antigravity, Feishu-login, and distributed-runtime tests remain green.
- [ ] All provider secrets and signed result URLs are redacted from logs, events, admin payloads, and test fixtures.
- [ ] New video tasks remain queryable and billable after Redis is flushed.
- [ ] No unknown task is automatically resubmitted without provider recovery proof.
- [ ] No success captures above its frozen quote.
- [ ] Video permission is independent from image permission.
- [ ] Missing pricing and unsupported capabilities fail before any billable upstream request.
- [ ] Direct Kling is not enabled until authenticated official fixtures pass.
- [ ] Deployment remains a separate explicit user action; this implementation plan does not authorize pushing images or changing production.
