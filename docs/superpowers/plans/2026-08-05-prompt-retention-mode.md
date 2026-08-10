# Prompt Retention Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Add a capture_only mode that permanently stores in-scope user prompts as explicitly unreviewed administrator events without Redis, Guard endpoints, or external API calls.

**Architecture:** Extend the prompt-audit configuration and event enums, add one transactional repository operation that creates a completed job and unreviewed event, and route capture_only through the existing non-blocking coordinator path. The frontend exposes one four-state mode selector and reuses existing event search, detail, filtering, and deletion.

**Tech Stack:** Go 1.24+, PostgreSQL, database/sql, Gin, Vue 3, TypeScript 5.6, Vitest, Tailwind CSS.

## Global Constraints

- Effective modes are exactly off, capture_only, async_audit, and blocking.
- Captures use decision=unreviewed, risk_level=unknown, and action=Record; never label them safe.
- Capture-only must not write prompt payloads to Redis or call a Guard endpoint.
- Capture failures never block or alter the business request.
- Existing group scope applies unchanged.
- Full prompts remain capped at 65,536 Unicode characters and remain until administrator deletion.
- Legacy configs without capture_only retain current behavior.
- Full prompt text must not enter job rows, logs, metrics, audit logs, or list responses.
- No automatic retention, historical backfill, assistant-output capture, export, or end-user access.

---

### Task 1: Configuration Domain and Compatibility

**Files:**
- Modify: backend/internal/securityaudit/prompt_types.go
- Modify: backend/internal/securityaudit/prompt_config.go
- Modify: backend/internal/securityaudit/prompt_config_store.go
- Modify: backend/internal/securityaudit/prompt_handler.go
- Test: backend/internal/securityaudit/prompt_config_test.go
- Test: backend/internal/securityaudit/prompt_handler_test.go

**Interfaces:**
- Produces: ModeCaptureOnly Mode and CaptureOnly bool on storage, active, public, and update configs.
- Consumed by: persistence, coordinator, service, and frontend contract tasks.

- [ ] **Step 1: Write failing config tests**

Add literal assertions:

~~~go
func TestPromptAuditCaptureOnlyConfig(t *testing.T) {
	legacy := DefaultStorageConfig()
	require.False(t, legacy.CaptureOnly)

	capture := ActiveConfig{RiskControlEnabled: true, Enabled: true, CaptureOnly: true}
	require.Equal(t, ModeCaptureOnly, capture.EffectiveMode())

	valid := UpdateConfigRequest{
		ExpectedConfigVersion: 1, Enabled: true, CaptureOnly: true,
		Strategy: "priority", WorkerCount: 4, QueueCapacity: 100,
		Scanners: []string{"pii"}, AllGroups: true,
	}
	require.NoError(t, validateUpdateConfigRequest(valid))

	invalid := valid
	invalid.BlockingEnabled = true
	err := validateUpdateConfigRequest(invalid)
	require.Equal(t, "prompt_audit_capture_only_conflict", infraerrors.Reason(err))
}
~~~

Extend handler tests so saved config and sanitized admin-audit fields contain capture_only=true without endpoint tokens.

- [ ] **Step 2: Run tests and verify RED**

~~~bash
cd backend
go test ./internal/securityaudit -run 'TestPromptAuditCaptureOnlyConfig|TestPromptAdminHandler' -count=1
~~~

Expected: compile/assertion failure because CaptureOnly and ModeCaptureOnly are absent.

- [ ] **Step 3: Implement config fields and precedence**

Add:

~~~go
const (
	ModeOff         Mode = "off"
	ModeCaptureOnly Mode = "capture_only"
	ModeAsync       Mode = "async_audit"
	ModeBlocking    Mode = "blocking"
)
~~~

Add CaptureOnly bool with JSON key capture_only to storageConfig, ActiveConfig, PublicConfig, and UpdateConfigRequest. Copy it through parse/default, public conversion, active conversion, save/build-next-storage, change summary, and configAuditFields.

Implement:

~~~go
func (cfg ActiveConfig) EffectiveMode() Mode {
	if !cfg.RiskControlEnabled || !cfg.Enabled {
		return ModeOff
	}
	if cfg.CaptureOnly {
		return ModeCaptureOnly
	}
	if cfg.BlockingEnabled {
		return ModeBlocking
	}
	return ModeAsync
}
~~~

Reject CaptureOnly with BlockingEnabled. Require enabled Guard endpoints only for Enabled && !CaptureOnly. Preserve group-scope validation.

- [ ] **Step 4: Run config tests and verify GREEN**

~~~bash
cd backend
go test ./internal/securityaudit -run 'TestPromptAuditCaptureOnlyConfig|TestDefaultConfigIsOff|TestPromptAdminHandler' -count=1
~~~

Expected: PASS.

- [ ] **Step 5: Commit**

~~~bash
git add backend/internal/securityaudit/prompt_types.go backend/internal/securityaudit/prompt_config.go backend/internal/securityaudit/prompt_config_store.go backend/internal/securityaudit/prompt_handler.go backend/internal/securityaudit/prompt_config_test.go backend/internal/securityaudit/prompt_handler_test.go
git commit -m "feat(prompt-audit): add capture-only configuration mode"
~~~

---

### Task 2: Transactional Persistence and Constraints

**Files:**
- Create: backend/migrations/194_prompt_capture_only.sql
- Modify: backend/internal/securityaudit/prompt_types.go
- Modify: backend/internal/securityaudit/prompt_repository.go
- Test: backend/internal/securityaudit/prompt_repository_integration_test.go
- Test: backend/internal/repository/migrations_schema_integration_test.go

**Interfaces:**
- Consumes: ModeCaptureOnly.
- Produces: RecordCapture(context.Context, PromptSnapshot, int64) (*Event, error) on JobRepository and PostgreSQLRepository.

- [ ] **Step 1: Write failing repository test**

~~~go
func TestPromptAuditRepositoryRecordsCaptureOnlyTransactionally(t *testing.T) {
	db := openPromptAuditIntegrationDB(t)
	resetPromptAuditIntegrationDB(t, db)
	repo := NewPostgreSQLRepository(db)
	snapshot := integrationSnapshot("capture-only")
	snapshot.FullPrompt = "private full prompt canary"

	event, err := repo.RecordCapture(context.Background(), snapshot, 3)
	require.NoError(t, err)
	require.Equal(t, EventUnreviewed, event.Decision)
	require.Equal(t, RiskUnknown, event.RiskLevel)
	require.Equal(t, ActionRecord, event.Action)
	require.Equal(t, "private full prompt canary", event.Snapshot.FullPrompt)

	var mode, status, preview string
	require.NoError(t, db.QueryRow(
		"SELECT execution_mode,status,redacted_preview FROM prompt_audit_jobs WHERE id=$1",
		event.JobID,
	).Scan(&mode, &status, &preview))
	require.Equal(t, "capture_only", mode)
	require.Equal(t, "done", status)
	require.NotContains(t, preview, "private full prompt canary")
}
~~~

Extend schema tests to accept each new value and reject unrelated values.
Update the prompt-audit integration harness to apply 194_prompt_capture_only.sql after migrations 181 and 182 so the test exercises the production constraints.

- [ ] **Step 2: Run test and verify RED**

~~~bash
cd backend
go test ./internal/securityaudit -run TestPromptAuditRepositoryRecordsCaptureOnlyTransactionally -count=1
~~~

Expected: compile failure because RecordCapture and enum values are absent.

- [ ] **Step 3: Add migration and repository transaction**

Define EventUnreviewed="unreviewed", RiskUnknown="unknown", and ActionRecord="Record".

Create migration 194_prompt_capture_only.sql that recreates constraints with:

~~~sql
CHECK (execution_mode IN ('capture_only','async_audit','blocking'))
CHECK (decision IN ('unreviewed','pass','flag','critical'))
CHECK (risk_level IN ('unknown','low','medium','high','critical'))
CHECK (action IN ('Record','Allow','Warn','Block'))
~~~

RecordCapture begins one transaction, inserts a done job using snapshot.Redacted() and ModeCaptureOnly, inserts an event using:

~~~go
result := &NormalizedResult{
	Decision: EventUnreviewed, RiskLevel: RiskUnknown, Action: ActionRecord,
	Categories: []string{}, MatchedScanners: []string{},
	ScannerScores: map[string]float64{}, ScannerEvidence: map[string]string{},
	ScannerBackend: "capture-only", PolicyID: "capture-only",
}
~~~

Pass the original snapshot to insertEvent so FullPrompt reaches only the event row. Commit only after both inserts succeed.

- [ ] **Step 4: Run integration and migration tests**

~~~bash
cd backend
go test ./internal/securityaudit -run 'TestPromptAuditRepositoryRecordsCaptureOnlyTransactionally|TestPromptAuditMigrationSchemaAndLeakageGate' -count=1
go test ./internal/repository -run Migration -count=1
~~~

Expected: PASS, or documented SKIP only when the integration database is unavailable.

- [ ] **Step 5: Commit**

~~~bash
git add backend/migrations/194_prompt_capture_only.sql backend/internal/securityaudit/prompt_types.go backend/internal/securityaudit/prompt_repository.go backend/internal/securityaudit/prompt_repository_integration_test.go backend/internal/repository/migrations_schema_integration_test.go
git commit -m "feat(prompt-audit): persist unreviewed prompt captures"
~~~

---

### Task 3: Best-Effort Capture Path and Metrics

**Files:**
- Modify: backend/internal/securityaudit/coordinator.go
- Modify: backend/internal/securityaudit/prompt_enqueue.go
- Modify: backend/internal/securityaudit/prompt_service.go
- Modify: backend/internal/securityaudit/prompt_metrics.go
- Modify: backend/internal/securityaudit/prompt_types.go
- Test: backend/internal/securityaudit/coordinator_test.go
- Test: backend/internal/securityaudit/prompt_worker_test.go
- Test: backend/internal/securityaudit/prompt_metrics_test.go

**Interfaces:**
- Consumes: RecordCapture from Task 2.
- Produces: Captured int64 in AuditMetricsSnapshot, CapturedTotal int64 in RuntimeSnapshot, and IncCaptured() on Metrics.

- [ ] **Step 1: Write failing routing and dependency-isolation tests**

~~~go
func TestEnqueuerCaptureOnlyRecordsWithoutRedis(t *testing.T) {
	cfg := asyncConfig()
	cfg.CaptureOnly = true
	cfg.Endpoints = nil
	repo := &fakeJobRepository{}
	metrics := NewAtomicMetrics()
	enqueuer := NewEnqueuer(&fakeConfigStore{cfg: cfg, active: true}, repo, nil, metrics)

	require.NoError(t, enqueuer.Enqueue(context.Background(), asyncRequest()))
	require.Equal(t, 1, repo.recordCaptureCalls)
	require.Contains(t, repo.recordCaptureSnapshot.FullPrompt, "payload canary text")
	require.Equal(t, int64(1), metrics.AuditSnapshot().Captured)
}
~~~

Add a coordinator test where Enqueue returns a database error and the final decision still has AllowNextStage=true.
Extend fakeJobRepository with a real interface implementation that records the supplied snapshot and returns recordCaptureErr when configured:

~~~go
func (r *fakeJobRepository) RecordCapture(_ context.Context, snapshot PromptSnapshot, _ int64) (*Event, error) {
	r.recordCaptureCalls++
	r.recordCaptureSnapshot = snapshot
	if r.recordCaptureErr != nil {
		return nil, r.recordCaptureErr
	}
	return &Event{ID: 100, Decision: EventUnreviewed, Snapshot: snapshot}, nil
}
~~~

- [ ] **Step 2: Run tests and verify RED**

~~~bash
cd backend
go test ./internal/securityaudit -run 'TestEnqueuerCaptureOnly|TestCoordinatorCaptureOnly|TestAtomicMetrics' -count=1
~~~

Expected: failures because capture-only is skipped and counters are absent.

- [ ] **Step 3: Implement routing and counters**

Route ModeCaptureOnly beside ModeAsync in Coordinator.Check. Permit both in PromptService.Enqueue.

In Enqueuer.Enqueue, require config and repository before mode selection. The capture branch checks group scope, extracts PromptSnapshot, calls RecordCapture, and never reads payload storage or endpoints. Success calls IncCaptured. Failure calls IncDropped and IncRecordFailed, logs sanitized metadata, then returns the error; PromptService and Coordinator keep it best-effort.

Add:

~~~go
type AuditMetricsSnapshot struct {
	Enqueued int64 `json:"enqueued"`
	Captured int64 `json:"captured"`
	Dropped  int64 `json:"dropped"`
}

func (m *AtomicMetrics) IncCaptured() {
	if m != nil {
		m.captured.Add(1)
	}
}
~~~

Expose captured_total from PromptService.Runtime. Do not add prompt content to log fields.

- [ ] **Step 4: Run the full security-audit package**

~~~bash
cd backend
go test ./internal/securityaudit -count=1
~~~

Expected: PASS with no prompt canary in output.

- [ ] **Step 5: Commit**

~~~bash
git add backend/internal/securityaudit/coordinator.go backend/internal/securityaudit/prompt_enqueue.go backend/internal/securityaudit/prompt_service.go backend/internal/securityaudit/prompt_metrics.go backend/internal/securityaudit/prompt_types.go backend/internal/securityaudit/coordinator_test.go backend/internal/securityaudit/prompt_worker_test.go backend/internal/securityaudit/prompt_metrics_test.go
git commit -m "feat(prompt-audit): capture prompts without guard calls"
~~~

---

### Task 4: Backend Event Contracts

**Files:**
- Modify: backend/internal/securityaudit/prompt_event_repository.go
- Modify: backend/internal/securityaudit/prompt_issue_summary.go
- Modify: backend/internal/securityaudit/prompt_handler.go
- Test: backend/internal/securityaudit/prompt_handler_test.go
- Test: backend/internal/securityaudit/prompt_repository_integration_test.go
- Test: backend/internal/server/api_contract_test.go

**Interfaces:**
- Consumes: stored unreviewed events.
- Produces: list, detail, filter, and delete behavior with empty risk summaries and detail-only full prompts.

- [ ] **Step 1: Write failing contract tests**

List a captured event with EventFilter{Decision:"unreviewed", RiskLevel:"unknown"}, fetch detail, delete it, and verify both event and orphan job disappear. Add a contract fixture with literal values:

~~~json
{
  "decision": "unreviewed",
  "risk_level": "unknown",
  "action": "Record",
  "scanner_backend": "capture-only",
  "issue_summaries": []
}
~~~

Assert list JSON omits snapshot.full_prompt and detail JSON contains it.

- [ ] **Step 2: Run tests and verify RED**

~~~bash
cd backend
go test ./internal/securityaudit -run 'CaptureOnly|Unreviewed' -count=1
go test ./internal/server -run APIContract -count=1
~~~

Expected: contract assertions fail until public semantics are updated.

- [ ] **Step 3: Implement public semantics**

Keep generic SQL equality filters for unreviewed and unknown. Make BuildIssueSummaries return an empty slice for EventUnreviewed. Keep full_prompt exclusively in eventDetailColumns.

Update config/runtime fixtures for capture_only and captured_total. Add capture_only to sanitized config audit fields without adding prompt text or credentials.

- [ ] **Step 4: Run backend packages**

~~~bash
cd backend
go test ./internal/securityaudit ./internal/server -count=1
~~~

Expected: PASS.

- [ ] **Step 5: Commit**

~~~bash
git add backend/internal/securityaudit/prompt_event_repository.go backend/internal/securityaudit/prompt_issue_summary.go backend/internal/securityaudit/prompt_handler.go backend/internal/securityaudit/prompt_handler_test.go backend/internal/securityaudit/prompt_repository_integration_test.go backend/internal/server/api_contract_test.go
git commit -m "feat(prompt-audit): expose unreviewed capture events"
~~~

---

### Task 5: Four-Mode Configuration UI

**Files:**
- Modify: frontend/src/features/prompt-audit/types.ts
- Modify: frontend/src/features/prompt-audit/viewModel.ts
- Modify: frontend/src/features/prompt-audit/PromptAuditView.vue
- Modify: frontend/src/features/prompt-audit/components/EndpointPool.vue
- Modify: frontend/src/features/prompt-audit/components/PolicyPanel.vue
- Modify: frontend/src/features/prompt-audit/components/RuntimeOverview.vue
- Modify: frontend/src/i18n/locales/zh/admin/promptAudit.ts
- Modify: frontend/src/i18n/locales/en/admin/promptAudit.ts
- Test: frontend/src/features/prompt-audit/__tests__/viewModel.spec.ts
- Test: frontend/src/features/prompt-audit/__tests__/PromptAuditView.spec.ts
- Test: frontend/src/features/prompt-audit/__tests__/components.spec.ts

**Interfaces:**
- Consumes: capture_only config and captured_total runtime.
- Produces: PromptProcessingMode, draftProcessingMode(), and applyProcessingMode().

- [ ] **Step 1: Write failing view-model/UI tests**

~~~ts
it('maps capture-only without Guard behavior', () => {
  const draft = configToDraft({
    ...config(), enabled: true, capture_only: true,
    blocking_enabled: false, endpoints: [],
  })
  expect(draftProcessingMode(draft)).toBe('capture_only')
  expect(buildUpdateRequest(draft)).toMatchObject({
    enabled: true, capture_only: true,
    blocking_enabled: false, endpoints: [],
  })
})
~~~

Add a legacy test where missing capture_only maps to async_audit. Mount the view, select capture_only, and assert the save payload, disabled Guard controls, editable group scope, and absence of blocking confirmation.

- [ ] **Step 2: Run focused tests and verify RED**

~~~bash
cd frontend
pnpm test:run src/features/prompt-audit/__tests__/viewModel.spec.ts src/features/prompt-audit/__tests__/PromptAuditView.spec.ts src/features/prompt-audit/__tests__/components.spec.ts
~~~

Expected: type/assertion failures because capture-only UI is absent.

- [ ] **Step 3: Implement mode mapping and controls**

Define PromptAuditMode and PromptProcessingMode as off | capture_only | async_audit | blocking. Add capture_only to config, draft, and update request.

~~~ts
export function draftProcessingMode(draft: PromptAuditDraft): PromptProcessingMode {
  if (!draft.enabled) return 'off'
  if (draft.capture_only) return 'capture_only'
  return draft.blocking_enabled ? 'blocking' : 'async_audit'
}

export function applyProcessingMode(
  draft: PromptAuditDraft,
  mode: PromptProcessingMode,
): PromptAuditDraft {
  return {
    ...cloneData(draft),
    enabled: mode !== 'off',
    capture_only: mode === 'capture_only',
    blocking_enabled: mode === 'blocking',
  }
}
~~~

Replace enable/block toggles with data-test="processing-mode". Preserve confirmation before blocking. Disable endpoint edit/probe and scanner/worker controls in capture-only, but keep group scope editable. Disable store_pass_events in capture-only. Add Chinese/English mode and privacy copy plus captured_total runtime display.

- [ ] **Step 4: Run frontend tests and typecheck**

~~~bash
cd frontend
pnpm test:run src/features/prompt-audit
pnpm typecheck
~~~

Expected: PASS.

- [ ] **Step 5: Commit**

~~~bash
git add frontend/src/features/prompt-audit frontend/src/i18n/locales/zh/admin/promptAudit.ts frontend/src/i18n/locales/en/admin/promptAudit.ts
git commit -m "feat(prompt-audit): add capture-only configuration UI"
~~~

---

### Task 6: Unreviewed Event UI and Final Verification

**Files:**
- Modify: frontend/src/features/prompt-audit/components/EventWorkspace.vue
- Modify: frontend/src/features/prompt-audit/components/FilterDeleteDialog.vue
- Modify: frontend/src/features/prompt-audit/components/EventDetailDialog.vue
- Modify: frontend/src/features/prompt-audit/types.ts
- Modify: frontend/src/i18n/locales/zh/admin/promptAudit.ts
- Modify: frontend/src/i18n/locales/en/admin/promptAudit.ts
- Test: frontend/src/features/prompt-audit/__tests__/components.spec.ts

**Interfaces:**
- Consumes: unreviewed, unknown, Record, and capture-only.
- Produces: visible labels, filters, detail explanation, and deletion support.

- [ ] **Step 1: Write failing event UI tests**

Create a full event fixture with unreviewed/unknown/Record, empty Guard metadata, and full prompt "capture-only full prompt". Assert:

~~~ts
expect(workspace.text()).toContain('admin.promptAudit.decisions.unreviewed')
expect(detail.get('[data-test="summary-prompt-full"]').text()).toContain('capture-only full prompt')
expect(detail.get('[data-test="capture-only-notice"]').text()).toContain('admin.promptAudit.events.captureOnlyNotice')
expect(detail.find('[data-test="risk-guard-return"]').exists()).toBe(false)
~~~

Select unreviewed and unknown in list and deletion filters and assert emitted literal values.

- [ ] **Step 2: Run test and verify RED**

~~~bash
cd frontend
pnpm test:run src/features/prompt-audit/__tests__/components.spec.ts
~~~

Expected: missing labels, options, and capture-only notice.

- [ ] **Step 3: Implement presentation**

Extend TypeScript unions. Add list/delete filter options, neutral badge styling, and labels 未审计, 未知, 仅留存. For unreviewed detail, show the full prompt and identity metadata, replace Guard output with data-test="capture-only-notice", keep risk summaries empty, and show capture-only technical metadata.

- [ ] **Step 4: Run full verification**

~~~bash
cd backend
go test ./internal/securityaudit ./internal/server -count=1
go test ./internal/repository -run Migration -count=1

cd ../frontend
pnpm test:run src/features/prompt-audit
pnpm typecheck
pnpm lint:check
pnpm build
~~~

Expected: all commands exit 0 with no new warning and no prompt content in logs.

- [ ] **Step 5: Review privacy and diff**

~~~bash
git diff --check
git status --short
git diff --stat
~~~

Inspect every FullPrompt/full_prompt occurrence. Production writes target event detail storage only; list serialization and logs exclude it.

- [ ] **Step 6: Commit**

~~~bash
git add frontend/src/features/prompt-audit/components/EventWorkspace.vue frontend/src/features/prompt-audit/components/FilterDeleteDialog.vue frontend/src/features/prompt-audit/components/EventDetailDialog.vue frontend/src/features/prompt-audit/types.ts frontend/src/features/prompt-audit/__tests__/components.spec.ts frontend/src/i18n/locales/zh/admin/promptAudit.ts frontend/src/i18n/locales/en/admin/promptAudit.ts
git commit -m "feat(prompt-audit): present unreviewed prompt captures"
~~~
