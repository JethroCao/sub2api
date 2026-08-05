# Prompt Retention Mode Design

## Goal

Add a `capture_only` prompt-processing mode that permanently stores user prompt text for administrator review without calling Qwen3Guard or any other external audit API.

## Product behavior

Prompt handling has four mutually exclusive effective modes:

- `off`: do not capture or inspect prompts.
- `capture_only`: store prompts without classifying them.
- `async_audit`: asynchronously classify prompts and store configured audit events.
- `blocking`: classify before upstream execution and block according to the existing policy.

The configuration page presents these as one mode choice. Existing `enabled` and `blocking_enabled` configurations continue to load with their current meaning. Selecting `capture_only` disables Guard-specific requirements but preserves the configured endpoint pool and scanner policy so an administrator can switch back later without rebuilding the configuration.

`capture_only` uses the existing all-groups or selected-groups scope. It captures only new, in-scope requests containing extractable user text. It does not backfill historical traffic.

## Data model

Captured records use the existing `prompt_audit_jobs` and `prompt_audit_events` identity, filtering, detail, and deletion infrastructure.

The database enums are extended with explicit non-classification values:

- `execution_mode`: `capture_only`
- `decision`: `unreviewed`
- `risk_level`: `unknown`
- `action`: `Record`

An unreviewed event has empty categories, scanner matches, scores, evidence, scanner version, and Guard endpoint. Its backend identifier is `capture-only`, its policy identifier is `capture-only`, and its latency and chunk count are zero. The full prompt is stored in `prompt_audit_events.full_prompt`; the job row keeps only the existing redacted metadata.

The maximum stored prompt remains 65,536 Unicode characters. Records have no automatic retention deadline and remain until an administrator deletes them through the existing single-event, batch, or filtered deletion controls.

## Request flow

When the global risk-control switch is on and effective mode is `capture_only`:

1. The existing gateway coordinator submits the request to the prompt service asynchronously.
2. The enqueuer checks group scope and extracts the prompt snapshot.
3. The repository writes a completed job and its matching unreviewed event in one database transaction.
4. No Redis payload is created, no worker claims the job, and no Guard endpoint is contacted.
5. A write failure is logged and counted as a dropped/failed capture but never blocks, delays, bills, or changes the upstream business request.

The insertion must preserve the existing event-to-job foreign key and must not leave an orphaned job if event insertion fails.

## Configuration and compatibility

Persist a new `capture_only` boolean in `prompt_audit_config`. The effective mode is derived in this order:

1. global risk control off or prompt processing disabled: `off`
2. `capture_only=true`: `capture_only`
3. `blocking_enabled=true`: `blocking`
4. otherwise: `async_audit`

Validation rules are mode-aware:

- `capture_only` requires no enabled endpoint and no Guard credential.
- Audit modes continue to require at least one enabled endpoint.
- `capture_only` and `blocking_enabled` cannot both be true.
- Group-scope validation still applies.
- Existing configurations without `capture_only` deserialize as `false`.

The public config, update request, runtime snapshot, frontend types, draft conversion, dirty-state fingerprint, and administrative audit summary include `capture_only`.

## Administrator UI

Replace the current combination of enable/blocking toggles with a single clearly labeled processing-mode selector containing `关闭`, `仅留存`, `异步审计`, and `同步审计并阻止`.

When `仅留存` is selected:

- The page explains that prompts are stored without a risk judgment or external API call.
- Guard endpoint and scanner controls remain visible but are marked inactive for the current mode.
- `保存安全事件` is inactive because every captured prompt is stored.
- Saving is allowed with an empty endpoint pool.

The event list and detail dialog display `未审计`, `未知`, and `仅留存` rather than a safe/allow result. Risk summaries remain empty, and the detail view explains that no Guard result exists.

Decision and risk filters include the new values. Existing delete filters and confirmation behavior apply without special handling.

## Security and privacy

The feature intentionally stores unredacted user content. Only existing administrator-authorized prompt-audit routes can retrieve it. Request logs, operational logs, job rows, metrics, and errors must not include the full prompt.

The capture path does not send prompt content outside sub2api and PostgreSQL. Redis is not used in this mode. Existing response serialization and audit-log body omission rules remain unchanged.

## Failure handling and observability

Capture is best-effort. Add a `captured_total` runtime metric for successful transactional captures. Database admission or transaction failures increment the existing dropped/record-failed metrics and emit sanitized structured logs with request and identity metadata only. They do not affect the client response.

Runtime mode reports `capture_only`. Worker and queue statistics remain available but no queued work is expected from capture-only traffic. `captured_total` increases after a successful transactional capture; the dropped counter increases after a failed capture.

## Testing

Backend tests cover:

- legacy config compatibility and effective-mode precedence;
- mode-aware validation with no Guard endpoints;
- scope filtering and no-text skipping;
- transactional creation of a completed job and unreviewed event;
- preservation of the full prompt only on the event row;
- no Redis or scanner call in `capture_only`;
- best-effort error behavior;
- event listing, detail, filtering, and deletion with the new enum values;
- migration constraints and indexes.

Frontend tests cover:

- conversion between server config, draft, update payload, and fingerprint;
- the four-mode selector and backward-compatible initial selection;
- inactive Guard controls in capture-only mode;
- unreviewed event labels, filters, and detail presentation;
- save behavior with no endpoint configured.

Contract and route coverage tests continue to ensure that full prompt data is returned only from the administrator detail endpoint.

## Out of scope

- Automatic retention or scheduled deletion.
- Historical backfill.
- Capturing assistant outputs.
- End-user access to captured prompts.
- Exporting full prompts.
- Encryption of the full-prompt database column beyond the deployment's existing PostgreSQL storage protections.
