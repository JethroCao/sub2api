package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type adminVideoRepository struct {
	db *sql.DB
}

func NewAdminVideoRepository(db *sql.DB) service.AdminVideoRepository {
	return &adminVideoRepository{db: db}
}

func (r *adminVideoRepository) ListPricingRules(ctx context.Context, groupID int64) ([]service.VideoPricingRule, error) {
	if r == nil || r.db == nil || groupID <= 0 {
		return nil, service.ErrVideoPricingRuleInvalid
	}
	return listAdminVideoPricingRules(ctx, r.db, groupID)
}

func (r *adminVideoRepository) ReplacePricingRules(ctx context.Context, groupID int64, inputs []service.VideoPricingRuleInput) ([]service.VideoPricingRule, error) {
	if r == nil || r.db == nil || groupID <= 0 {
		return nil, service.ErrVideoPricingRuleInvalid
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var lockedID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, groupID).Scan(&lockedID); err != nil {
		return nil, translatePersistenceError(err, service.ErrGroupNotFound, nil)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM video_pricing_rules WHERE group_id = $1`, groupID); err != nil {
		return nil, err
	}
	for _, input := range inputs {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO video_pricing_rules (
    group_id, external_model, operation, resolution, audio_mode, unit,
    unit_price, upstream_unit_cost, enabled, created_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NOW(),NOW())`, groupID, input.ExternalModel, input.Operation,
			input.Resolution, input.AudioMode, input.Unit, input.UnitPrice, input.UpstreamUnitCost, input.Enabled); err != nil {
			return nil, err
		}
	}
	rules, err := listAdminVideoPricingRules(ctx, tx, groupID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rules, nil
}

type adminVideoQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listAdminVideoPricingRules(ctx context.Context, queryer adminVideoQueryer, groupID int64) ([]service.VideoPricingRule, error) {
	rows, err := queryer.QueryContext(ctx, `
SELECT id, group_id, external_model, operation, resolution, audio_mode,
       unit, unit_price, upstream_unit_cost, enabled
FROM video_pricing_rules WHERE group_id = $1
ORDER BY external_model, operation, resolution, audio_mode, id`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	rules := make([]service.VideoPricingRule, 0)
	for rows.Next() {
		var rule service.VideoPricingRule
		var upstream sql.NullFloat64
		if err := rows.Scan(&rule.ID, &rule.GroupID, &rule.ExternalModel, &rule.Operation, &rule.Resolution,
			&rule.AudioMode, &rule.Unit, &rule.UnitPrice, &upstream, &rule.Enabled); err != nil {
			return nil, err
		}
		if upstream.Valid {
			rule.UpstreamUnitCost = &upstream.Float64
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func (r *adminVideoRepository) ListTasks(ctx context.Context, query service.VideoTaskListQuery) ([]service.VideoTask, int, error) {
	if r == nil || r.db == nil {
		return nil, 0, errors.New("admin video repository db is nil")
	}
	return (&videoTaskRepository{db: r.db}).ListAdmin(ctx, query)
}

func (r *adminVideoRepository) GetTask(ctx context.Context, requestID string) (*service.AdminVideoTaskDetail, error) {
	if r == nil || r.db == nil || !service.IsVideoRequestID(requestID) {
		return nil, service.ErrVideoTaskInvalidRequest
	}
	task, err := scanVideoTask(r.db.QueryRowContext(ctx, `SELECT `+videoTaskColumns+` FROM video_tasks WHERE request_id=$1`, requestID))
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	events, err := listAdminVideoTaskEvents(ctx, r.db, requestID)
	if err != nil {
		return nil, err
	}
	return &service.AdminVideoTaskDetail{Task: *task, Events: events, ResultURLSummary: safeStoredResultURLSummary(task.ResultURL)}, nil
}

func (r *adminVideoRepository) ReplayAction(ctx context.Context, requestID, action string, metadata service.AdminVideoActionMetadata) (*service.AdminVideoActionResult, bool, error) {
	if r == nil || r.db == nil || !service.IsVideoRequestID(requestID) {
		return nil, false, service.ErrVideoTaskInvalidRequest
	}
	var storedHash string
	err := r.db.QueryRowContext(ctx, `SELECT request_hash FROM video_admin_actions WHERE request_id=$1 AND action=$2 AND idempotency_key_hash=$3`,
		requestID, action, metadata.IdempotencyKeyHash).Scan(&storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if storedHash != metadata.RequestHash {
		return nil, true, service.ErrIdempotencyKeyConflict
	}
	task, err := scanVideoTask(r.db.QueryRowContext(ctx, `SELECT `+videoTaskColumns+` FROM video_tasks WHERE request_id=$1`, requestID))
	if err != nil {
		return nil, true, err
	}
	return &service.AdminVideoActionResult{Task: *task, Replayed: true}, true, nil
}

type adminVideoRowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listAdminVideoTaskEvents(ctx context.Context, queryer adminVideoRowsQueryer, requestID string) ([]service.VideoTaskEvent, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT id, request_id, event_type, payload, created_at FROM video_task_events WHERE request_id=$1 ORDER BY created_at,id`, requestID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := make([]service.VideoTaskEvent, 0)
	for rows.Next() {
		var event service.VideoTaskEvent
		var payload []byte
		if err := rows.Scan(&event.ID, &event.RequestID, &event.EventType, &payload, &event.CreatedAt); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			var decoded map[string]any
			if err := json.Unmarshal(payload, &decoded); err != nil {
				return nil, err
			}
			event.Payload, err = service.NewMinimizedVideoPayload(decoded)
			if err != nil {
				return nil, err
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (r *adminVideoRepository) Reconcile(ctx context.Context, command service.AdminVideoReconcileMutation) (*service.AdminVideoActionResult, error) {
	return r.applyAction(ctx, command.RequestID, "reconcile", command.AdminVideoActionMetadata, func(tx *sql.Tx, current *service.VideoTask, databaseNow time.Time) (*service.VideoTask, error) {
		if service.IsTerminalVideoTaskStatus(current.Status) || (current.LeaseExpiresAt != nil && current.LeaseExpiresAt.After(databaseNow)) {
			return nil, service.ErrVideoFinancialStateConflict
		}
		updated, err := scanVideoTask(tx.QueryRowContext(ctx, `
UPDATE video_tasks SET lease_owner=NULL, lease_expires_at=NULL, next_poll_at=$2,
    version=version+1, updated_at=$2
WHERE request_id=$1 AND status NOT IN ('succeeded','failed','cancelled')
  AND (lease_expires_at IS NULL OR lease_expires_at <= $2)
RETURNING `+videoTaskColumns, command.RequestID, databaseNow))
		if err != nil {
			return nil, service.ErrVideoFinancialStateConflict
		}
		payload, err := service.NewMinimizedVideoPayload(map[string]any{"from_status": string(current.Status), "to_status": string(updated.Status)})
		if err != nil {
			return nil, err
		}
		if err := insertAdminVideoEvent(ctx, tx, command.RequestID, "admin_reconcile", payload, databaseNow); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

func (r *adminVideoRepository) Refund(ctx context.Context, command service.AdminVideoRefundMutation) (*service.AdminVideoActionResult, error) {
	return r.applyAction(ctx, command.RequestID, "refund", command.AdminVideoActionMetadata, func(tx *sql.Tx, current *service.VideoTask, databaseNow time.Time) (*service.VideoTask, error) {
		if current.BillingStatus != "held" || current.SettledAt != nil || (current.Status != service.VideoTaskUnknown && current.Status != service.VideoTaskFailed) {
			return nil, service.ErrVideoFinancialStateConflict
		}
		hold := adminVideoHoldCommand(current, service.VideoReleaseRequestID(current.RequestID), 0)
		if _, err := applyVideoHoldInTx(ctx, tx, hold, releaseVideoHold); err != nil {
			if errors.Is(err, service.ErrVideoBillingAlreadyFinalized) {
				return nil, service.ErrVideoFinancialStateConflict
			}
			return nil, err
		}
		updated, err := scanVideoTask(tx.QueryRowContext(ctx, `
UPDATE video_tasks SET status='failed', request_payload=NULL, billing_status='released',
    billing_reference=$2, settled_amount=0, settled_at=$3, finished_at=COALESCE(finished_at,$3),
    next_poll_at=NULL, lease_owner=NULL, lease_expires_at=NULL,
    last_error_code='ADMIN_REFUNDED', last_error_message=NULL, last_error_retryable=FALSE,
    version=version+1, updated_at=$3
WHERE request_id=$1 AND billing_status='held' AND settled_at IS NULL AND status IN ('unknown','failed')
RETURNING `+videoTaskColumns, command.RequestID, service.VideoReleaseRequestID(command.RequestID), databaseNow))
		if err != nil {
			return nil, service.ErrVideoFinancialStateConflict
		}
		payload, err := service.NewMinimizedVideoPayload(map[string]any{
			"from_status": string(current.Status), "to_status": string(updated.Status),
			"billing_status": "released", "settled_amount": float64(0),
		})
		if err != nil {
			return nil, err
		}
		if err := insertAdminVideoEvent(ctx, tx, command.RequestID, "admin_refund", payload, databaseNow); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

func (r *adminVideoRepository) Complete(ctx context.Context, command service.AdminVideoCompleteMutation) (*service.AdminVideoActionResult, error) {
	return r.applyAction(ctx, command.RequestID, "complete", command.AdminVideoActionMetadata, func(tx *sql.Tx, current *service.VideoTask, databaseNow time.Time) (*service.VideoTask, error) {
		if current.BillingStatus != "held" || current.SettledAt != nil || (current.Status != service.VideoTaskUnknown && current.Status != service.VideoTaskFailed) ||
			command.FinalAmount < 0 || command.FinalAmount > current.FrozenAmount || current.UnitPrice != command.StoredUnitPrice {
			return nil, service.ErrVideoFinancialStateConflict
		}
		hold := adminVideoHoldCommand(current, service.VideoCaptureRequestID(current.RequestID), command.FinalAmount)
		if _, err := applyVideoHoldInTx(ctx, tx, hold, captureVideoHold); err != nil {
			if errors.Is(err, service.ErrVideoBillingAlreadyFinalized) {
				return nil, service.ErrVideoFinancialStateConflict
			}
			return nil, err
		}
		width, height := adminVideoResolutionDimensions(command.Resolution)
		updated, err := scanVideoTask(tx.QueryRowContext(ctx, `
UPDATE video_tasks SET status='succeeded', upstream_task_id=$2, upstream_status='succeeded',
    request_payload=NULL, result_url=$3, result_duration_seconds=$4,
    result_width=$5, result_height=$6, billing_status='captured', billing_reference=$7,
    settled_amount=ROUND($8::numeric,8), settled_at=$9, finished_at=COALESCE(finished_at,$9),
    next_poll_at=NULL, lease_owner=NULL, lease_expires_at=NULL,
    last_error_code=NULL, last_error_message=NULL, last_error_retryable=FALSE,
    version=version+1, updated_at=$9
WHERE request_id=$1 AND billing_status='held' AND settled_at IS NULL AND status IN ('unknown','failed')
RETURNING `+videoTaskColumns, command.RequestID, command.ProviderTaskID, command.ResultURL,
			command.DurationSeconds, width, height, service.VideoCaptureRequestID(command.RequestID), command.FinalAmount, databaseNow))
		if err != nil {
			return nil, service.ErrVideoFinancialStateConflict
		}
		payload, err := service.NewMinimizedVideoPayload(map[string]any{
			"from_status": string(current.Status), "to_status": string(updated.Status), "resolution": command.Resolution,
			"duration_seconds": command.DurationSeconds, "billing_status": "captured", "settled_amount": command.FinalAmount,
		})
		if err != nil {
			return nil, err
		}
		if err := insertAdminVideoEvent(ctx, tx, command.RequestID, "admin_complete", payload, databaseNow); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

type adminVideoActionMutation func(*sql.Tx, *service.VideoTask, time.Time) (*service.VideoTask, error)

func (r *adminVideoRepository) applyAction(ctx context.Context, requestID, action string, metadata service.AdminVideoActionMetadata, mutate adminVideoActionMutation) (*service.AdminVideoActionResult, error) {
	if r == nil || r.db == nil || !service.IsVideoRequestID(requestID) {
		return nil, service.ErrVideoTaskInvalidRequest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanVideoTask(tx.QueryRowContext(ctx, `SELECT `+videoTaskColumns+` FROM video_tasks WHERE request_id=$1 FOR UPDATE`, requestID))
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrVideoTaskNotFound, nil)
	}
	var storedHash string
	err = tx.QueryRowContext(ctx, `SELECT request_hash FROM video_admin_actions WHERE request_id=$1 AND action=$2 AND idempotency_key_hash=$3`,
		requestID, action, metadata.IdempotencyKeyHash).Scan(&storedHash)
	if err == nil {
		if storedHash != metadata.RequestHash {
			return nil, service.ErrIdempotencyKeyConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &service.AdminVideoActionResult{Task: *current, Replayed: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var databaseNow time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return nil, err
	}
	updated, err := mutate(tx, current, databaseNow)
	if err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal(map[string]any{"status": updated.Status, "billing_status": updated.BillingStatus, "version": updated.Version})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO video_admin_actions (request_id,action,idempotency_key_hash,request_hash,actor_user_id,audit_request_id,reason,result_snapshot,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, requestID, action, metadata.IdempotencyKeyHash, metadata.RequestHash,
		metadata.ActorUserID, metadata.AuditRequestID, metadata.Reason, string(snapshot), databaseNow); err != nil {
		return nil, err
	}
	if err := insertAdminVideoAudit(ctx, tx, action, metadata, current, updated, databaseNow); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &service.AdminVideoActionResult{Task: *updated}, nil
}

func adminVideoHoldCommand(task *service.VideoTask, operationID string, actual float64) *service.VideoHoldCommand {
	return &service.VideoHoldCommand{
		RequestID: operationID, RequestPayloadHash: task.RequestHash, UserID: task.UserID, APIKeyID: task.APIKeyID,
		SubscriptionID: task.SubscriptionID, VideoRequestID: task.RequestID, BillingMode: task.BillingMode,
		HoldAmount: task.FrozenAmount, ActualAmount: actual,
	}
}

func insertAdminVideoEvent(ctx context.Context, tx *sql.Tx, requestID, eventType string, payload service.MinimizedVideoPayload, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO video_task_events (request_id,event_type,payload,created_at) VALUES ($1,$2,$3,$4)`,
		requestID, eventType, videoPayloadSQLValue(payload), at)
	return err
}

func insertAdminVideoAudit(ctx context.Context, tx *sql.Tx, action string, metadata service.AdminVideoActionMetadata, before, after *service.VideoTask, at time.Time) error {
	actor := metadata.ActorUserID
	log := &service.AuditLog{
		CreatedAt: at, ActorUserID: &actor, ActorRole: "admin", Action: "admin.video." + action,
		Method: "POST", Path: fmt.Sprintf("/api/v1/admin/video/tasks/%s/%s", before.RequestID, action),
		RequestID: metadata.AuditRequestID, StatusCode: 200,
		Extra: map[string]any{
			"target_request_id": before.RequestID, "reason": metadata.Reason,
			"before_status": before.Status, "after_status": after.Status,
			"before_billing_status": before.BillingStatus, "after_billing_status": after.BillingStatus,
			"idempotency_key_hash": metadata.IdempotencyKeyHash,
		},
	}
	if action == "complete" {
		log.Extra["result_url_summary"] = safeStoredResultURLSummary(after.ResultURL)
	}
	query := `INSERT INTO audit_logs (` + auditLogInsertColumns + `) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`
	_, err := tx.ExecContext(ctx, query, auditLogInsertValues(log)...)
	return err
}

func adminVideoResolutionDimensions(resolution string) (int, int) {
	switch resolution {
	case "480p":
		return 854, 480
	case "720p":
		return 1280, 720
	case "1080p":
		return 1920, 1080
	default:
		return 0, 0
	}
}

func safeStoredResultURLSummary(raw *string) string {
	if raw == nil {
		return ""
	}
	return service.SafeAdminVideoResultURLSummary(*raw)
}
