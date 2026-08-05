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
    currency, billing_mode, billing_status
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11,
    $12, $13, $14,
    $15, $16, $17, $18, $19,
    $20, $21, $22
)
ON CONFLICT (user_id, api_key_id, operation, idempotency_key_hash)
    WHERE idempotency_key_hash <> ''
DO NOTHING
RETURNING `+videoTaskColumns, requestID, params.UserID, params.APIKeyID, params.SubscriptionID,
		params.GroupID, params.AccountID, params.Platform, params.Provider, params.Operation,
		params.ExternalModel, params.UpstreamModel, params.IdempotencyKeyHash, params.RequestHash,
		videoPayloadSQLValue(params.RequestPayload), params.PricingUnit, params.UnitPrice,
		params.EstimatedUnits, params.EstimatedAmount, params.FrozenAmount, params.Currency,
		params.BillingMode, params.BillingStatus))
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
WHERE user_id = $1 AND api_key_id = $2 AND operation = $3 AND idempotency_key_hash = $4
`+lockClause, params.UserID, params.APIKeyID, params.Operation, params.IdempotencyKeyHash))
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
    request_payload = NULL, next_poll_at = $5, submitted_at = COALESCE(submitted_at, $6),
    last_error_code = NULL, last_error_message = NULL, last_error_retryable = FALSE,
    version = version + 1, updated_at = $6
WHERE request_id = $1 AND version = $2 AND status = 'submitting'`, params.RequestID, params.ExpectedVersion,
		params.UpstreamTaskID, params.UpstreamStatus, params.NextPollAt, params.SubmittedAt)
	if err != nil {
		return err
	}
	if err := checkVideoMutationResult(ctx, tx, result, params.RequestID, params.ExpectedVersion, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *videoTaskRepository) MarkSubmissionUnknown(ctx context.Context, requestID string, expectedVersion int64, taskError service.VideoTaskError) error {
	if err := validateVideoRequestID(requestID); err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE video_tasks
SET status = 'unknown', last_error_code = NULLIF($3, ''),
    last_error_message = NULLIF($4, ''), last_error_retryable = $5,
    version = version + 1, updated_at = NOW()
WHERE request_id = $1 AND version = $2 AND status = 'submitting'`, requestID, expectedVersion,
		taskError.Code(), taskError.Message(), taskError.Retryable())
	if err != nil {
		return err
	}
	return r.checkMutationResult(ctx, result, requestID, expectedVersion, nil)
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
    WHERE status IN ('submitted', 'queued', 'running', 'unknown')
      AND upstream_task_id IS NOT NULL AND upstream_task_id <> ''
      AND next_poll_at IS NOT NULL AND next_poll_at <= $3
      AND (lease_expires_at IS NULL OR lease_expires_at <= $3)
    ORDER BY next_poll_at ASC, id ASC
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

func (r *videoTaskRepository) ApplyPollResult(ctx context.Context, params service.ApplyVideoPollResultParams) error {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return err
	}
	if strings.TrimSpace(params.LeaseOwner) == "" || !service.IsVideoTaskStatus(params.Status) ||
		(params.ResultDurationSeconds != nil && !isFiniteNonNegativeVideoAmount(*params.ResultDurationSeconds)) {
		return service.ErrVideoTaskInvalidRequest
	}
	if params.UpdatedAt.IsZero() {
		params.UpdatedAt = time.Now().UTC()
	}
	if service.IsTerminalVideoTaskStatus(params.Status) && params.FinishedAt == nil {
		finished := params.UpdatedAt
		params.FinishedAt = &finished
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
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
		return translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	if version != params.ExpectedVersion {
		return service.ErrVideoTaskVersionConflict
	}
	if !leaseOwner.Valid || leaseOwner.String != params.LeaseOwner || !leaseExpiresAt.Valid || !leaseExpiresAt.Time.After(databaseNow) {
		return service.ErrVideoTaskLeaseConflict
	}
	if !service.CanApplyVideoPollStatus(current, params.Status) {
		return service.ErrVideoTaskInvalidTransition
	}
	var errorCode, errorMessage any
	errorRetryable := false
	if params.Error != nil {
		errorCode = nullableString(params.Error.Code())
		errorMessage = nullableString(params.Error.Message())
		errorRetryable = params.Error.Retryable()
	}
	result, err := tx.ExecContext(ctx, `
UPDATE video_tasks
SET status = $3, upstream_status = NULLIF($4, ''), next_poll_at = $5,
    result_url = $6, result_url_expires_at = $7, result_content_type = $8,
    result_duration_seconds = $9, result_width = $10, result_height = $11,
    last_error_code = $12, last_error_message = $13, last_error_retryable = $14,
    started_at = COALESCE(started_at, $15), finished_at = COALESCE(finished_at, $16),
    poll_attempts = poll_attempts + 1, lease_owner = NULL, lease_expires_at = NULL,
    version = version + 1, updated_at = $17
WHERE request_id = $1 AND version = $2 AND lease_owner = $18 AND lease_expires_at > clock_timestamp()`, params.RequestID, params.ExpectedVersion, params.Status,
		params.UpstreamStatus, params.NextPollAt, params.ResultURL, params.ResultURLExpiresAt,
		params.ResultContentType, params.ResultDurationSeconds, params.ResultWidth, params.ResultHeight,
		errorCode, errorMessage, errorRetryable, params.StartedAt, params.FinishedAt, params.UpdatedAt, params.LeaseOwner)
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

func (r *videoTaskRepository) MarkSettled(ctx context.Context, params service.MarkVideoSettledParams) error {
	if err := validateVideoRequestID(params.RequestID); err != nil {
		return err
	}
	if !isFiniteNonNegativeVideoAmount(params.SettledAmount) || strings.TrimSpace(params.BillingStatus) == "" {
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
	var frozenAmount float64
	var settledAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT status, version, frozen_amount, settled_at FROM video_tasks WHERE request_id = $1 FOR UPDATE`, params.RequestID).
		Scan(&status, &version, &frozenAmount, &settledAt); err != nil {
		return translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	if version != params.ExpectedVersion {
		return service.ErrVideoTaskVersionConflict
	}
	if !service.IsTerminalVideoTaskStatus(status) || settledAt.Valid {
		return service.ErrVideoTaskInvalidTransition
	}
	if params.SettledAmount > frozenAmount {
		return service.ErrVideoTaskAmountExceedsFrozen
	}
	_, err = tx.ExecContext(ctx, `
UPDATE video_tasks
SET settled_amount = $3, billing_status = $4, billing_reference = $5,
    settlement_attempts = settlement_attempts + 1, settled_at = COALESCE(settled_at, $6),
    version = version + 1, updated_at = $6
WHERE request_id = $1 AND version = $2 AND status IN ('succeeded', 'failed', 'cancelled')
  AND settled_amount IS NULL AND settled_at IS NULL`, params.RequestID, params.ExpectedVersion,
		params.SettledAmount, params.BillingStatus, params.BillingReference, params.SettledAt)
	if err != nil {
		return err
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
	if err := validateCreateVideoTaskParams(params); err != nil {
		return service.CreateVideoTaskParams{}, err
	}
	return params, nil
}

func validateCreateVideoTaskParams(params service.CreateVideoTaskParams) error {
	if params.UserID <= 0 || params.APIKeyID <= 0 || params.GroupID <= 0 || params.AccountID <= 0 ||
		params.Platform != service.PlatformVideo ||
		(params.Provider != service.VideoProviderSeedance && params.Provider != service.VideoProviderKling) ||
		(params.Operation != "generation" && params.Operation != "edit" && params.Operation != "extension") ||
		strings.TrimSpace(params.ExternalModel) == "" || strings.TrimSpace(params.UpstreamModel) == "" ||
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
