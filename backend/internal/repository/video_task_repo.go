package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var videoTaskSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type videoTaskRepository struct {
	db *sql.DB
}

func NewVideoTaskRepository(db *sql.DB) service.VideoTaskRepository {
	return &videoTaskRepository{db: db}
}

func (r *videoTaskRepository) CreateOrGet(ctx context.Context, params service.CreateVideoTaskParams) (*service.VideoTask, bool, error) {
	params, err := normalizeCreateVideoTaskParams(params)
	if err != nil {
		return nil, false, err
	}
	requestID, err := service.NewVideoRequestID()
	if err != nil {
		return nil, false, err
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	task, created, err := createOrGetVideoTask(ctx, tx, params, requestID, false)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return task, created, nil
}

func (r *videoTaskRepository) CreateTaskAndReserve(ctx context.Context, params service.CreateVideoTaskParams) (*service.VideoTask, bool, error) {
	params, err := normalizeCreateVideoTaskParams(params)
	if err != nil {
		return nil, false, err
	}
	requestID, err := service.NewVideoRequestID()
	if err != nil {
		return nil, false, err
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	task, created, err := createOrGetVideoTask(ctx, tx, params, requestID, true)
	if err != nil {
		return nil, false, err
	}
	cmd := &service.VideoHoldCommand{
		RequestID:          service.VideoHoldRequestID(task.RequestID),
		RequestPayloadHash: task.RequestHash,
		UserID:             task.UserID,
		APIKeyID:           task.APIKeyID,
		SubscriptionID:     task.SubscriptionID,
		VideoRequestID:     task.RequestID,
		BillingMode:        task.BillingMode,
		HoldAmount:         task.FrozenAmount,
	}
	if _, err := applyVideoHoldInTx(ctx, tx, cmd, reserveVideoHold); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return task, created, nil
}

func createOrGetVideoTask(
	ctx context.Context,
	tx *sql.Tx,
	params service.CreateVideoTaskParams,
	requestID string,
	lockReplayForUpdate bool,
) (*service.VideoTask, bool, error) {
	task, err := scanVideoTask(tx.QueryRowContext(ctx, `
INSERT INTO video_tasks (
    request_id, user_id, api_key_id, subscription_id, group_id, account_id,
    platform, provider, operation, external_model, upstream_model,
    idempotency_key_hash, request_hash, request_payload,
    pricing_unit, unit_price, estimated_units, estimated_amount, frozen_amount,
    currency, billing_mode, billing_status, next_poll_at
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11,
    $12, $13, $14,
    $15, $16, $17, ROUND($18::numeric, 8), ROUND($19::numeric, 8),
    $20, $21, $22, $23
)
ON CONFLICT (user_id, api_key_id, idempotency_key_hash)
    WHERE idempotency_key_hash <> ''
DO NOTHING
RETURNING `+videoTaskColumns, requestID, params.UserID, params.APIKeyID, params.SubscriptionID,
		params.GroupID, params.AccountID, params.Platform, params.Provider, params.Operation,
		params.ExternalModel, params.UpstreamModel, params.IdempotencyKeyHash, params.RequestHash,
		videoPayloadSQLValue(params.RequestPayload), params.PricingUnit, params.UnitPrice,
		params.EstimatedUnits, params.EstimatedAmount, params.FrozenAmount, params.Currency,
		params.BillingMode, params.BillingStatus, params.NextPollAt))
	created := err == nil
	if created {
		if err := validateVideoTaskBillingOwnership(ctx, tx, params); err != nil {
			return nil, false, err
		}
	}
	if errors.Is(err, sql.ErrNoRows) && params.IdempotencyKeyHash != "" {
		lockClause := "FOR SHARE"
		if lockReplayForUpdate {
			lockClause = "FOR UPDATE"
		}
		task, err = scanVideoTask(tx.QueryRowContext(ctx, `SELECT `+videoTaskColumns+`
FROM video_tasks
WHERE user_id = $1 AND api_key_id = $2 AND idempotency_key_hash = $3
`+lockClause, params.UserID, params.APIKeyID, params.IdempotencyKeyHash))
		if err == nil && task.RequestHash != params.RequestHash {
			return nil, false, service.ErrVideoIdempotencyConflict
		}
	}
	if err != nil {
		return nil, false, err
	}
	return task, created, nil
}

func (r *videoTaskRepository) GetOwned(ctx context.Context, requestID string, userID, apiKeyID int64) (*service.VideoTask, error) {
	if err := validateVideoRequestID(requestID); err != nil {
		return nil, err
	}
	task, err := scanVideoTask(r.db.QueryRowContext(ctx, `SELECT `+videoTaskColumns+`
FROM video_tasks WHERE request_id = $1 AND user_id = $2 AND api_key_id = $3`, requestID, userID, apiKeyID))
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	return task, nil
}

func (r *videoTaskRepository) GetByRequestID(ctx context.Context, requestID string) (*service.VideoTask, error) {
	if err := validateVideoRequestID(requestID); err != nil {
		return nil, err
	}
	task, err := scanVideoTask(r.db.QueryRowContext(ctx, `SELECT `+videoTaskColumns+` FROM video_tasks WHERE request_id = $1`, requestID))
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	return task, nil
}

func (r *videoTaskRepository) AssignAndMarkSubmitting(ctx context.Context, params service.AssignVideoSubmissionParams) error {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return err
	}
	if params.ExpectedVersion < 0 || params.AccountID <= 0 || params.Platform != service.PlatformVideo ||
		(params.Provider != service.VideoProviderSeedance && params.Provider != service.VideoProviderKling) ||
		strings.TrimSpace(params.UpstreamModel) == "" || strings.TrimSpace(params.ProviderSubmissionToken) == "" ||
		len(params.ProviderSubmissionToken) > 128 || params.NextPollAt.IsZero() {
		return service.ErrVideoTaskInvalidRequest
	}
	if params.UpdatedAt.IsZero() {
		params.UpdatedAt = time.Now().UTC()
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE video_tasks
SET account_id = $3, platform = $4, provider = $5, upstream_model = $6,
    status = 'submitting', provider_submission_token = $7,
    submission_attempts = submission_attempts + 1, last_error_code = NULL,
    last_error_message = NULL, last_error_retryable = FALSE,
    next_poll_at = $8, version = version + 1, updated_at = $9
WHERE request_id = $1 AND version = $2 AND status = 'created'
  AND account_id = 0 AND upstream_model = '' AND platform = $4 AND provider = $5`,
		params.RequestID, params.ExpectedVersion, params.AccountID, params.Platform,
		params.Provider, params.UpstreamModel, params.ProviderSubmissionToken, params.NextPollAt, params.UpdatedAt)
	if err != nil {
		return err
	}
	return r.checkMutationResult(ctx, result, params.RequestID, params.ExpectedVersion, nil)
}

func (r *videoTaskRepository) MarkSubmitting(ctx context.Context, requestID string, expectedVersion int64, providerSubmissionToken string) error {
	if err := validateVideoRequestID(requestID); err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE video_tasks
SET status = 'submitting', provider_submission_token = NULLIF($3, ''),
    submission_attempts = submission_attempts + 1, last_error_code = NULL,
    last_error_message = NULL, last_error_retryable = FALSE,
    version = version + 1, updated_at = NOW()
WHERE request_id = $1 AND version = $2 AND status = 'created'`, requestID, expectedVersion, providerSubmissionToken)
	if err != nil {
		return err
	}
	return r.checkMutationResult(ctx, result, requestID, expectedVersion, nil)
}

func (r *videoTaskRepository) MarkSubmitted(ctx context.Context, params service.MarkVideoSubmittedParams) error {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return err
	}
	if strings.TrimSpace(params.UpstreamTaskID) == "" || params.UpstreamTaskID == params.RequestID {
		return service.ErrVideoTaskInvalidRequest
	}
	if params.SubmittedAt.IsZero() {
		params.SubmittedAt = time.Now().UTC()
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
UPDATE video_tasks
SET status = 'submitted', upstream_task_id = $3, upstream_status = NULLIF($4, ''),
    request_payload = $5, next_poll_at = $6, submitted_at = COALESCE(submitted_at, $7),
    last_error_code = NULL, last_error_message = NULL, last_error_retryable = FALSE,
    version = version + 1, updated_at = $7
WHERE request_id = $1 AND version = $2 AND status = 'submitting'`, params.RequestID, params.ExpectedVersion,
		params.UpstreamTaskID, params.UpstreamStatus, videoPayloadSQLValue(params.RequestPayload), params.NextPollAt, params.SubmittedAt)
	if err != nil {
		return err
	}
	if err := checkVideoMutationResult(ctx, tx, result, params.RequestID, params.ExpectedVersion, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *videoTaskRepository) MarkSubmissionUnknown(ctx context.Context, requestID string, expectedVersion int64, taskError service.VideoTaskError) error {
	now := time.Now().UTC()
	return r.MarkSubmissionUnknownAt(ctx, service.MarkVideoSubmissionUnknownParams{
		RequestID:       requestID,
		ExpectedVersion: expectedVersion,
		Error:           taskError,
		NextPollAt:      now,
		UpdatedAt:       now,
	})
}

func (r *videoTaskRepository) MarkSubmissionUnknownAt(ctx context.Context, params service.MarkVideoSubmissionUnknownParams) error {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return err
	}
	if params.ExpectedVersion < 0 || params.NextPollAt.IsZero() {
		return service.ErrVideoTaskInvalidRequest
	}
	if params.UpdatedAt.IsZero() {
		params.UpdatedAt = time.Now().UTC()
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE video_tasks
SET status = 'unknown', last_error_code = NULLIF($3, ''),
    last_error_message = NULLIF($4, ''), last_error_retryable = $5,
	    next_poll_at = $6, version = version + 1, updated_at = $7
WHERE request_id = $1 AND version = $2 AND status = 'submitting'`, params.RequestID, params.ExpectedVersion,
		params.Error.Code(), params.Error.Message(), params.Error.Retryable(), params.NextPollAt, params.UpdatedAt)
	if err != nil {
		return err
	}
	return r.checkMutationResult(ctx, result, params.RequestID, params.ExpectedVersion, nil)
}

func (r *videoTaskRepository) ReleaseAndMarkSubmissionFailed(ctx context.Context, params service.ReleaseAndFailVideoSubmissionParams) (*service.VideoTask, error) {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return nil, err
	}
	if params.ExpectedVersion < 0 || (params.ExpectedStatus != service.VideoTaskCreated && params.ExpectedStatus != service.VideoTaskSubmitting) ||
		(params.ExpectedStatus == service.VideoTaskCreated && strings.TrimSpace(params.ProviderSubmissionToken) != "") ||
		(params.ExpectedStatus == service.VideoTaskSubmitting && strings.TrimSpace(params.ProviderSubmissionToken) == "") {
		return nil, service.ErrVideoTaskInvalidRequest
	}
	if params.FailedAt.IsZero() {
		params.FailedAt = time.Now().UTC()
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	current, err := scanVideoTask(tx.QueryRowContext(ctx, `SELECT `+videoTaskColumns+` FROM video_tasks WHERE request_id = $1 FOR UPDATE`, params.RequestID))
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	if current.Status == service.VideoTaskFailed && current.BillingStatus == "released" && current.UpstreamTaskID == nil {
		return current, tx.Commit()
	}
	if current.Version != params.ExpectedVersion {
		return nil, service.ErrVideoTaskVersionConflict
	}
	currentToken := ""
	if current.ProviderSubmissionToken != nil {
		currentToken = strings.TrimSpace(*current.ProviderSubmissionToken)
	}
	if current.Status != params.ExpectedStatus || current.UpstreamTaskID != nil ||
		currentToken != strings.TrimSpace(params.ProviderSubmissionToken) {
		return nil, service.ErrVideoTaskInvalidTransition
	}
	if params.ExpectedStatus == service.VideoTaskCreated && (current.AccountID != 0 || strings.TrimSpace(current.UpstreamModel) != "") {
		return nil, service.ErrVideoTaskInvalidTransition
	}
	if params.ExpectedStatus == service.VideoTaskSubmitting && (current.AccountID <= 0 || strings.TrimSpace(current.UpstreamModel) == "") {
		return nil, service.ErrVideoTaskInvalidTransition
	}

	command := &service.VideoHoldCommand{
		RequestID:          service.VideoReleaseRequestID(current.RequestID),
		RequestPayloadHash: current.RequestHash,
		UserID:             current.UserID,
		APIKeyID:           current.APIKeyID,
		SubscriptionID:     current.SubscriptionID,
		VideoRequestID:     current.RequestID,
		BillingMode:        current.BillingMode,
		HoldAmount:         current.FrozenAmount,
	}
	if _, err := applyVideoHoldInTx(ctx, tx, command, releaseVideoHold); err != nil {
		return nil, err
	}
	updated, err := scanVideoTask(tx.QueryRowContext(ctx, `
UPDATE video_tasks
SET status = 'failed', provider_submission_token = NULL, request_payload = NULL,
    next_poll_at = NULL, last_error_code = NULLIF($3, ''),
    last_error_message = NULLIF($4, ''), last_error_retryable = $5,
    billing_status = 'released', settled_amount = 0, settled_at = $6,
    finished_at = COALESCE(finished_at, $6), lease_owner = NULL, lease_expires_at = NULL,
    version = version + 1, updated_at = $6
WHERE request_id = $1 AND version = $2 AND status = $7
  AND provider_submission_token IS NOT DISTINCT FROM NULLIF($8, '')
RETURNING `+videoTaskColumns,
		params.RequestID, params.ExpectedVersion, params.Error.Code(), params.Error.Message(),
		params.Error.Retryable(), params.FailedAt, params.ExpectedStatus, strings.TrimSpace(params.ProviderSubmissionToken)))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *videoTaskRepository) LeaseDue(ctx context.Context, owner string, limit int, lease time.Duration, now time.Time) ([]service.VideoTask, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, service.ErrVideoTaskInvalidRequest
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if lease <= 0 {
		lease = time.Minute
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
WITH due AS (
    SELECT id
    FROM video_tasks
    WHERE (
          (
              status IN ('succeeded', 'failed', 'cancelled')
              AND settled_at IS NULL
              AND COALESCE(next_poll_at, finished_at, updated_at, created_at) <= $3
          )
          OR (
              status IN ('created', 'submitting', 'submitted', 'queued', 'running', 'unknown')
              AND (
                  (upstream_task_id IS NOT NULL AND upstream_task_id <> '')
                  OR status = 'unknown'
                  OR (status = 'created' AND account_id = 0 AND upstream_model = ''
                      AND provider_submission_token IS NULL AND upstream_task_id IS NULL)
                  OR (status = 'submitting' AND account_id > 0 AND upstream_model <> ''
                      AND provider_submission_token IS NOT NULL AND upstream_task_id IS NULL)
              )
              AND next_poll_at IS NOT NULL AND next_poll_at <= $3
          )
      )
      AND (lease_expires_at IS NULL OR lease_expires_at <= $3)
    ORDER BY COALESCE(next_poll_at, finished_at, updated_at, created_at) ASC, id ASC
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
UPDATE video_tasks AS task
SET lease_owner = $1, lease_expires_at = $4, updated_at = $3, version = task.version + 1
FROM due
WHERE task.id = due.id
RETURNING `+prefixedVideoTaskColumns("task"), owner, limit, now, now.Add(lease))
	if err != nil {
		return nil, err
	}
	tasks, err := scanVideoTasks(rows, limit)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (r *videoTaskRepository) RenewLease(ctx context.Context, params service.RenewVideoTaskLeaseParams) error {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return err
	}
	if params.ExpectedVersion < 0 || strings.TrimSpace(params.LeaseOwner) == "" || params.LeaseDuration <= 0 {
		return service.ErrVideoTaskInvalidRequest
	}
	if params.UpdatedAt.IsZero() {
		params.UpdatedAt = time.Now().UTC()
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE video_tasks
SET lease_expires_at = clock_timestamp() + $4::interval, updated_at = $5
WHERE request_id = $1 AND version = $2 AND lease_owner = $3
  AND lease_expires_at > clock_timestamp()`, params.RequestID, params.ExpectedVersion,
		params.LeaseOwner, params.LeaseDuration.String(), params.UpdatedAt)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	var version int64
	var owner sql.NullString
	if err := r.db.QueryRowContext(ctx, `SELECT version, lease_owner FROM video_tasks WHERE request_id = $1`, params.RequestID).Scan(&version, &owner); err != nil {
		return translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	if version != params.ExpectedVersion {
		return service.ErrVideoTaskVersionConflict
	}
	return service.ErrVideoTaskLeaseConflict
}

func (r *videoTaskRepository) ApplyPollResult(ctx context.Context, params service.ApplyVideoPollResultParams) (*service.VideoTask, error) {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.LeaseOwner) == "" || !service.IsVideoTaskStatus(params.Status) ||
		(params.ResultDurationSeconds != nil && !isFiniteNonNegativeVideoAmount(*params.ResultDurationSeconds)) {
		return nil, service.ErrVideoTaskInvalidRequest
	}
	if params.UpdatedAt.IsZero() {
		params.UpdatedAt = time.Now().UTC()
	}
	if service.IsTerminalVideoTaskStatus(params.Status) && params.FinishedAt == nil {
		finished := params.UpdatedAt
		params.FinishedAt = &finished
	}
	if service.IsTerminalVideoTaskStatus(params.Status) && params.NextPollAt == nil {
		next := params.UpdatedAt
		params.NextPollAt = &next
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var current service.VideoTaskStatus
	var version int64
	var leaseOwner sql.NullString
	var leaseExpiresAt sql.NullTime
	var databaseNow time.Time
	if err := tx.QueryRowContext(ctx, `
		SELECT status, version, lease_owner, lease_expires_at, clock_timestamp()
		FROM video_tasks WHERE request_id = $1 FOR UPDATE`, params.RequestID).
		Scan(&current, &version, &leaseOwner, &leaseExpiresAt, &databaseNow); err != nil {
		return nil, translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	if version != params.ExpectedVersion {
		return nil, service.ErrVideoTaskVersionConflict
	}
	if !leaseOwner.Valid || leaseOwner.String != params.LeaseOwner || !leaseExpiresAt.Valid || !leaseExpiresAt.Time.After(databaseNow) {
		return nil, service.ErrVideoTaskLeaseConflict
	}
	if !service.CanApplyVideoPollStatus(current, params.Status) {
		return nil, service.ErrVideoTaskInvalidTransition
	}
	var errorCode, errorMessage any
	errorRetryable := false
	if params.Error != nil {
		errorCode = nullableString(params.Error.Code())
		errorMessage = nullableString(params.Error.Message())
		errorRetryable = params.Error.Retryable()
	}
	terminal := service.IsTerminalVideoTaskStatus(params.Status)
	updated, err := scanVideoTask(tx.QueryRowContext(ctx, `
UPDATE video_tasks
SET status = $3, upstream_status = NULLIF($4, ''), next_poll_at = $5,
    result_url = $6, result_url_expires_at = $7, result_content_type = $8,
    result_duration_seconds = $9, result_width = $10, result_height = $11,
    last_error_code = $12, last_error_message = $13, last_error_retryable = $14,
    started_at = COALESCE(started_at, $15), finished_at = COALESCE(finished_at, $16),
    poll_attempts = poll_attempts + 1,
    lease_owner = CASE WHEN $19 THEN lease_owner ELSE NULL END,
    lease_expires_at = CASE WHEN $19 THEN lease_expires_at ELSE NULL END,
    version = version + 1, updated_at = $17
WHERE request_id = $1 AND version = $2 AND lease_owner = $18 AND lease_expires_at > clock_timestamp()
RETURNING `+videoTaskColumns, params.RequestID, params.ExpectedVersion, params.Status,
		params.UpstreamStatus, params.NextPollAt, params.ResultURL, params.ResultURLExpiresAt,
		params.ResultContentType, params.ResultDurationSeconds, params.ResultWidth, params.ResultHeight,
		errorCode, errorMessage, errorRetryable, params.StartedAt, params.FinishedAt, params.UpdatedAt, params.LeaseOwner, terminal))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrVideoTaskLeaseConflict
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *videoTaskRepository) ApplyRecoveredSubmission(ctx context.Context, params service.RecoverVideoSubmissionParams) (*service.VideoTask, error) {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return nil, err
	}
	if params.ExpectedVersion < 0 || strings.TrimSpace(params.LeaseOwner) == "" ||
		strings.TrimSpace(params.UpstreamTaskID) == "" || params.UpstreamTaskID == params.RequestID {
		return nil, service.ErrVideoTaskInvalidRequest
	}
	if params.SubmittedAt.IsZero() {
		params.SubmittedAt = time.Now().UTC()
	}
	if params.UpdatedAt.IsZero() {
		params.UpdatedAt = params.SubmittedAt
	}
	if params.NextPollAt == nil {
		next := params.UpdatedAt
		params.NextPollAt = &next
	}
	updated, err := scanVideoTask(r.db.QueryRowContext(ctx, `
UPDATE video_tasks
SET status = 'submitted', upstream_task_id = $4, upstream_status = NULLIF($5, ''),
    provider_submission_token = NULL, request_payload = NULL, next_poll_at = $6,
    submitted_at = COALESCE(submitted_at, $7), last_error_code = NULL,
    last_error_message = NULL, last_error_retryable = FALSE,
    lease_owner = NULL, lease_expires_at = NULL,
    version = version + 1, updated_at = $8
WHERE request_id = $1 AND version = $2 AND lease_owner = $3
  AND lease_expires_at > clock_timestamp()
  AND status IN ('submitting', 'unknown')
  AND upstream_task_id IS NULL AND provider_submission_token IS NOT NULL
RETURNING `+videoTaskColumns, params.RequestID, params.ExpectedVersion, params.LeaseOwner,
		strings.TrimSpace(params.UpstreamTaskID), params.UpstreamStatus, params.NextPollAt,
		params.SubmittedAt, params.UpdatedAt))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, r.videoLeaseMutationConflict(ctx, params.RequestID, params.ExpectedVersion, params.LeaseOwner)
	}
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *videoTaskRepository) ScheduleRetry(ctx context.Context, params service.ScheduleVideoTaskRetryParams) error {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return err
	}
	if params.ExpectedVersion < 0 || strings.TrimSpace(params.LeaseOwner) == "" ||
		!service.IsVideoTaskStatus(params.Status) || params.NextPollAt.IsZero() {
		return service.ErrVideoTaskInvalidRequest
	}
	if params.UpdatedAt.IsZero() {
		params.UpdatedAt = time.Now().UTC()
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE video_tasks
SET status = $4, next_poll_at = $5,
    last_error_code = NULLIF($6, ''), last_error_message = NULLIF($7, ''),
    last_error_retryable = $8,
    poll_attempts = poll_attempts + CASE WHEN $9 THEN 1 ELSE 0 END,
    settlement_attempts = settlement_attempts + CASE WHEN $10 THEN 1 ELSE 0 END,
    lease_owner = NULL, lease_expires_at = NULL,
    version = version + 1, updated_at = $11
WHERE request_id = $1 AND version = $2 AND lease_owner = $3
  AND lease_expires_at > clock_timestamp()
  AND (
      status = $4
      OR (status = 'submitting' AND $4 = 'unknown')
  )`, params.RequestID, params.ExpectedVersion, params.LeaseOwner, params.Status,
		params.NextPollAt, params.Error.Code(), params.Error.Message(), params.Error.Retryable(),
		params.IncrementPollAttempts, params.IncrementSettlementAttempts, params.UpdatedAt)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	return r.videoLeaseMutationConflict(ctx, params.RequestID, params.ExpectedVersion, params.LeaseOwner)
}

func (r *videoTaskRepository) videoLeaseMutationConflict(ctx context.Context, requestID string, expectedVersion int64, owner string) error {
	var version int64
	var currentOwner sql.NullString
	if err := r.db.QueryRowContext(ctx, `SELECT version, lease_owner FROM video_tasks WHERE request_id = $1`, requestID).Scan(&version, &currentOwner); err != nil {
		return translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	if version != expectedVersion {
		return service.ErrVideoTaskVersionConflict
	}
	if !currentOwner.Valid || currentOwner.String != owner {
		return service.ErrVideoTaskLeaseConflict
	}
	return service.ErrVideoTaskInvalidTransition
}

func (r *videoTaskRepository) MarkSettled(ctx context.Context, params service.MarkVideoSettledParams) error {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return err
	}
	if !isFiniteNonNegativeVideoAmount(params.SettledAmount) || strings.TrimSpace(params.BillingStatus) == "" ||
		strings.TrimSpace(params.LeaseOwner) == "" {
		return service.ErrVideoTaskInvalidRequest
	}
	if params.SettledAt.IsZero() {
		params.SettledAt = time.Now().UTC()
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var status service.VideoTaskStatus
	var version int64
	var settledAt sql.NullTime
	var leaseOwner sql.NullString
	var leaseExpiresAt sql.NullTime
	var databaseNow time.Time
	var withinHold bool
	if err := tx.QueryRowContext(ctx, `
		SELECT status, version, settled_at, lease_owner, lease_expires_at,
		       clock_timestamp(), ROUND($2::numeric, 8) <= frozen_amount
		FROM video_tasks WHERE request_id = $1 FOR UPDATE
	`, params.RequestID, params.SettledAmount).Scan(
		&status, &version, &settledAt, &leaseOwner, &leaseExpiresAt, &databaseNow, &withinHold,
	); err != nil {
		return translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	if version != params.ExpectedVersion {
		return service.ErrVideoTaskVersionConflict
	}
	if !service.IsTerminalVideoTaskStatus(status) || settledAt.Valid {
		return service.ErrVideoTaskInvalidTransition
	}
	if !leaseOwner.Valid || leaseOwner.String != params.LeaseOwner || !leaseExpiresAt.Valid || !leaseExpiresAt.Time.After(databaseNow) {
		return service.ErrVideoTaskLeaseConflict
	}
	if !withinHold {
		return service.ErrVideoFinalCostExceedsHold
	}
	result, err := tx.ExecContext(ctx, `
UPDATE video_tasks
SET settled_amount = ROUND($3::numeric, 8), billing_status = $4, billing_reference = $5,
    settlement_attempts = settlement_attempts + 1, settled_at = COALESCE(settled_at, $6),
	last_error_code = NULLIF($8, ''), last_error_message = NULLIF($9, ''),
	last_error_retryable = $10, next_poll_at = NULL,
	lease_owner = NULL, lease_expires_at = NULL,
	version = version + 1, updated_at = $6
WHERE request_id = $1 AND version = $2 AND status IN ('succeeded', 'failed', 'cancelled')
	AND settled_amount IS NULL AND settled_at IS NULL AND lease_owner = $7
	AND lease_expires_at > clock_timestamp()`, params.RequestID, params.ExpectedVersion,
		params.SettledAmount, params.BillingStatus, params.BillingReference, params.SettledAt,
		params.LeaseOwner, params.Error.Code(), params.Error.Message(), params.Error.Retryable())
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return service.ErrVideoTaskLeaseConflict
	}
	return tx.Commit()
}

func (r *videoTaskRepository) ReleaseLease(ctx context.Context, requestID, owner string, now time.Time) error {
	if err := validateVideoRequestID(requestID); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE video_tasks
SET lease_owner = NULL, lease_expires_at = NULL, version = version + 1, updated_at = $3
WHERE request_id = $1 AND lease_owner = $2`, requestID, owner, now)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	var exists bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM video_tasks WHERE request_id = $1)`, requestID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return service.ErrVideoTaskNotFound
	}
	return service.ErrVideoTaskLeaseConflict
}

func (r *videoTaskRepository) ClearExpiredMetadata(ctx context.Context, params service.ClearVideoTaskMetadataParams) (int, error) {
	if params.Now.IsZero() || params.RetentionBefore.IsZero() || params.RetentionBefore.After(params.Now) {
		return 0, service.ErrVideoTaskInvalidRequest
	}
	if params.Limit <= 0 {
		params.Limit = 100
	}
	if params.Limit > 1000 {
		params.Limit = 1000
	}
	result, err := r.db.ExecContext(ctx, `
WITH expired AS (
    SELECT id
    FROM video_tasks
    WHERE status IN ('succeeded', 'failed', 'cancelled')
      AND settled_at IS NOT NULL
      AND (
          (result_url_expires_at IS NOT NULL AND result_url_expires_at <= $1)
          OR (
              COALESCE(finished_at, updated_at) <= $2
              AND (
                  result_url IS NOT NULL OR result_url_expires_at IS NOT NULL
                  OR result_content_type IS NOT NULL OR result_duration_seconds IS NOT NULL
                  OR result_width IS NOT NULL OR result_height IS NOT NULL
                  OR request_payload IS NOT NULL
              )
          )
      )
    ORDER BY COALESCE(finished_at, updated_at), id
    LIMIT $3
    FOR UPDATE SKIP LOCKED
)
UPDATE video_tasks AS task
SET result_url = CASE
        WHEN task.result_url_expires_at <= $1 OR COALESCE(task.finished_at, task.updated_at) <= $2 THEN NULL
        ELSE task.result_url END,
    result_url_expires_at = CASE
        WHEN task.result_url_expires_at <= $1 OR COALESCE(task.finished_at, task.updated_at) <= $2 THEN NULL
        ELSE task.result_url_expires_at END,
    result_content_type = CASE WHEN COALESCE(task.finished_at, task.updated_at) <= $2 THEN NULL ELSE task.result_content_type END,
    result_duration_seconds = CASE WHEN COALESCE(task.finished_at, task.updated_at) <= $2 THEN NULL ELSE task.result_duration_seconds END,
    result_width = CASE WHEN COALESCE(task.finished_at, task.updated_at) <= $2 THEN NULL ELSE task.result_width END,
    result_height = CASE WHEN COALESCE(task.finished_at, task.updated_at) <= $2 THEN NULL ELSE task.result_height END,
    request_payload = CASE WHEN COALESCE(task.finished_at, task.updated_at) <= $2 THEN NULL ELSE task.request_payload END,
    version = task.version + 1, updated_at = $1
FROM expired
WHERE task.id = expired.id`, params.Now, params.RetentionBefore, params.Limit)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

func (r *videoTaskRepository) AppendEvent(ctx context.Context, event service.VideoTaskEvent) error {
	if !service.IsVideoRequestID(event.RequestID) || strings.TrimSpace(event.EventType) == "" || len(event.EventType) > 64 {
		return service.ErrVideoTaskInvalidRequest
	}
	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO video_task_events (request_id, event_type, payload, created_at)
VALUES ($1, $2, $3, $4)`, event.RequestID, event.EventType, videoPayloadSQLValue(event.Payload), createdAt)
	return translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
}

func (r *videoTaskRepository) ListAdmin(ctx context.Context, query service.VideoTaskListQuery) ([]service.VideoTask, int, error) {
	if query.RequestID != "" && !service.IsVideoRequestID(query.RequestID) {
		return nil, 0, service.ErrVideoTaskInvalidRequest
	}
	where := make([]string, 0, 7)
	args := make([]any, 0, 9)
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if query.RequestID != "" {
		add("request_id = $%d", query.RequestID)
	}
	if query.UserID != nil {
		add("user_id = $%d", *query.UserID)
	}
	if query.APIKeyID != nil {
		add("api_key_id = $%d", *query.APIKeyID)
	}
	if query.AccountID != nil {
		add("account_id = $%d", *query.AccountID)
	}
	if query.Status != "" {
		add("status = $%d", query.Status)
	}
	if query.Provider != "" {
		add("provider = $%d", query.Provider)
	}
	if query.CreatedAfter != nil {
		add("created_at >= $%d", *query.CreatedAfter)
	}
	if query.CreatedBefore != nil {
		add("created_at < $%d", *query.CreatedBefore)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM video_tasks`+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := query.Offset
	if offset < 0 {
		offset = 0
	}
	listArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := r.db.QueryContext(ctx, `SELECT `+videoTaskColumns+` FROM video_tasks`+whereSQL+
		` ORDER BY created_at DESC, id DESC LIMIT $`+strconv.Itoa(len(args)+1)+` OFFSET $`+strconv.Itoa(len(args)+2), listArgs...)
	if err != nil {
		return nil, 0, err
	}
	tasks, err := scanVideoTasks(rows, limit)
	if err != nil {
		return nil, 0, err
	}
	return tasks, total, nil
}

func (r *videoTaskRepository) begin(ctx context.Context) (*sql.Tx, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil video task database")
	}
	return r.db.BeginTx(ctx, nil)
}

func (r *videoTaskRepository) checkMutationResult(ctx context.Context, result sql.Result, requestID string, expectedVersion int64, conflict error) error {
	return checkVideoMutationResult(ctx, r.db, result, requestID, expectedVersion, conflict)
}

func checkVideoMutationResult(ctx context.Context, sqlq interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, result sql.Result, requestID string, expectedVersion int64, conflict error) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	var version int64
	if err := sqlq.QueryRowContext(ctx, `SELECT version FROM video_tasks WHERE request_id = $1`, requestID).Scan(&version); err != nil {
		return translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	if version != expectedVersion {
		return service.ErrVideoTaskVersionConflict
	}
	if conflict != nil {
		return conflict
	}
	return service.ErrVideoTaskInvalidTransition
}

func normalizeCreateVideoTaskParams(params service.CreateVideoTaskParams) (service.CreateVideoTaskParams, error) {
	params.Currency = strings.TrimSpace(params.Currency)
	params.BillingMode = strings.ToLower(strings.TrimSpace(params.BillingMode))
	if params.Currency == "" {
		params.Currency = "USD"
	}
	if params.NextPollAt != nil {
		nextPollAt := params.NextPollAt.UTC()
		params.NextPollAt = &nextPollAt
	}
	if err := validateCreateVideoTaskParams(params); err != nil {
		return service.CreateVideoTaskParams{}, err
	}
	return params, nil
}

func validateCreateVideoTaskParams(params service.CreateVideoTaskParams) error {
	routeAssigned := params.AccountID > 0 && strings.TrimSpace(params.UpstreamModel) != ""
	routePending := params.AccountID == 0 && strings.TrimSpace(params.UpstreamModel) == ""
	if params.UserID <= 0 || params.APIKeyID <= 0 || params.GroupID <= 0 || (!routeAssigned && !routePending) ||
		(routePending && (params.NextPollAt == nil || params.NextPollAt.IsZero())) ||
		params.Platform != service.PlatformVideo ||
		(params.Provider != service.VideoProviderSeedance && params.Provider != service.VideoProviderKling) ||
		(params.Operation != "generation" && params.Operation != "edit" && params.Operation != "extension") ||
		strings.TrimSpace(params.ExternalModel) == "" ||
		!videoTaskSHA256Pattern.MatchString(params.RequestHash) ||
		(params.IdempotencyKeyHash != "" && !videoTaskSHA256Pattern.MatchString(params.IdempotencyKeyHash)) ||
		!isFiniteNonNegativeVideoAmount(params.UnitPrice) || !isFiniteNonNegativeVideoAmount(params.EstimatedUnits) ||
		!isFiniteNonNegativeVideoAmount(params.EstimatedAmount) || !isFiniteNonNegativeVideoAmount(params.FrozenAmount) ||
		strings.TrimSpace(params.PricingUnit) == "" || strings.TrimSpace(params.Currency) == "" ||
		(params.BillingMode != "balance" && params.BillingMode != "subscription") ||
		(params.BillingMode == "balance" && params.SubscriptionID != nil) ||
		(params.BillingMode == "subscription" && (params.SubscriptionID == nil || *params.SubscriptionID <= 0)) ||
		strings.TrimSpace(params.BillingStatus) == "" {
		return service.ErrVideoTaskInvalidRequest
	}
	return nil
}

func validateVideoTaskBillingOwnership(ctx context.Context, tx *sql.Tx, params service.CreateVideoTaskParams) error {
	if params.BillingMode != "subscription" {
		return nil
	}
	var exists int
	err := tx.QueryRowContext(ctx, `
		SELECT 1
		FROM user_subscriptions
		WHERE id = $1 AND user_id = $2 AND group_id = $3 AND deleted_at IS NULL
	`, *params.SubscriptionID, params.UserID, params.GroupID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrVideoTaskInvalidRequest
	}
	return err
}

func validateVideoRequestID(requestID string) error {
	if !service.IsVideoRequestID(requestID) {
		return service.ErrVideoTaskInvalidRequest
	}
	return nil
}

func isFiniteNonNegativeVideoAmount(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func videoPayloadSQLValue(payload service.MinimizedVideoPayload) any {
	b := payload.Bytes()
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

const videoTaskColumns = `
id, request_id, user_id, api_key_id, subscription_id, group_id, account_id,
platform, provider, operation, external_model, upstream_model,
idempotency_key_hash, request_hash, provider_submission_token, request_payload,
status, upstream_task_id, upstream_status,
result_url, result_url_expires_at, result_content_type, result_duration_seconds, result_width, result_height,
pricing_unit, unit_price, estimated_units, estimated_amount, frozen_amount, settled_amount,
currency, billing_mode, billing_status, billing_reference,
submission_attempts, poll_attempts, settlement_attempts, next_poll_at, lease_owner, lease_expires_at,
last_error_code, last_error_message, last_error_retryable, version,
created_at, updated_at, submitted_at, started_at, finished_at, settled_at`

func prefixedVideoTaskColumns(alias string) string {
	parts := strings.Split(videoTaskColumns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

type videoTaskScanner interface {
	Scan(...any) error
}

func scanVideoTask(scanner videoTaskScanner) (*service.VideoTask, error) {
	var task service.VideoTask
	var subscriptionID sql.NullInt64
	var providerToken, upstreamTaskID, upstreamStatus sql.NullString
	var requestPayload []byte
	var resultURL, resultContentType sql.NullString
	var resultURLExpiresAt sql.NullTime
	var resultDuration sql.NullFloat64
	var resultWidth, resultHeight sql.NullInt64
	var settledAmount sql.NullFloat64
	var billingReference sql.NullString
	var nextPollAt, leaseExpiresAt sql.NullTime
	var leaseOwner, lastErrorCode, lastErrorMessage sql.NullString
	var submittedAt, startedAt, finishedAt, settledAt sql.NullTime
	err := scanner.Scan(
		&task.ID, &task.RequestID, &task.UserID, &task.APIKeyID, &subscriptionID, &task.GroupID, &task.AccountID,
		&task.Platform, &task.Provider, &task.Operation, &task.ExternalModel, &task.UpstreamModel,
		&task.IdempotencyKeyHash, &task.RequestHash, &providerToken, &requestPayload,
		&task.Status, &upstreamTaskID, &upstreamStatus,
		&resultURL, &resultURLExpiresAt, &resultContentType, &resultDuration, &resultWidth, &resultHeight,
		&task.PricingUnit, &task.UnitPrice, &task.EstimatedUnits, &task.EstimatedAmount, &task.FrozenAmount, &settledAmount,
		&task.Currency, &task.BillingMode, &task.BillingStatus, &billingReference,
		&task.SubmissionAttempts, &task.PollAttempts, &task.SettlementAttempts, &nextPollAt, &leaseOwner, &leaseExpiresAt,
		&lastErrorCode, &lastErrorMessage, &task.LastErrorRetryable, &task.Version,
		&task.CreatedAt, &task.UpdatedAt, &submittedAt, &startedAt, &finishedAt, &settledAt,
	)
	if err != nil {
		return nil, err
	}
	task.RequestPayload = append([]byte(nil), requestPayload...)
	task.SubscriptionID = nullInt64Pointer(subscriptionID)
	task.ProviderSubmissionToken = nullStringPointer(providerToken)
	task.UpstreamTaskID = nullStringPointer(upstreamTaskID)
	task.UpstreamStatus = nullStringPointer(upstreamStatus)
	task.ResultURL = nullStringPointer(resultURL)
	task.ResultURLExpiresAt = nullTimePointer(resultURLExpiresAt)
	task.ResultContentType = nullStringPointer(resultContentType)
	task.ResultDurationSeconds = nullFloat64Pointer(resultDuration)
	task.ResultWidth = nullIntPointer(resultWidth)
	task.ResultHeight = nullIntPointer(resultHeight)
	task.SettledAmount = nullFloat64Pointer(settledAmount)
	task.BillingReference = nullStringPointer(billingReference)
	task.NextPollAt = nullTimePointer(nextPollAt)
	task.LeaseOwner = nullStringPointer(leaseOwner)
	task.LeaseExpiresAt = nullTimePointer(leaseExpiresAt)
	task.LastErrorCode = nullStringPointer(lastErrorCode)
	task.LastErrorMessage = nullStringPointer(lastErrorMessage)
	task.SubmittedAt = nullTimePointer(submittedAt)
	task.StartedAt = nullTimePointer(startedAt)
	task.FinishedAt = nullTimePointer(finishedAt)
	task.SettledAt = nullTimePointer(settledAt)
	return &task, nil
}

func scanVideoTasks(rows *sql.Rows, capacity int) ([]service.VideoTask, error) {
	defer func() { _ = rows.Close() }()
	tasks := make([]service.VideoTask, 0, capacity)
	for rows.Next() {
		task, err := scanVideoTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func nullInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func nullIntPointer(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	result := int(value.Int64)
	return &result
}

func nullFloat64Pointer(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
