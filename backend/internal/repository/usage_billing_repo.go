package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type usageBillingRepository struct {
	db *sql.DB
}

func NewUsageBillingRepository(_ *dbent.Client, sqlDB *sql.DB) service.UsageBillingRepository {
	return &usageBillingRepository{db: sqlDB}
}

func (r *usageBillingRepository) Apply(ctx context.Context, cmd *service.UsageBillingCommand) (_ *service.UsageBillingApplyResult, err error) {
	if cmd == nil {
		return &service.UsageBillingApplyResult{}, nil
	}
	if r == nil || r.db == nil {
		return nil, errors.New("usage billing repository db is nil")
	}

	cmd.Normalize()
	if cmd.RequestID == "" {
		return nil, service.ErrUsageBillingRequestIDRequired
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	applied, err := claimUsageBillingRequest(ctx, tx, cmd.RequestID, cmd.APIKeyID, cmd.RequestFingerprint)
	if err != nil {
		return nil, err
	}
	if !applied {
		return &service.UsageBillingApplyResult{Applied: false}, nil
	}

	result := &service.UsageBillingApplyResult{Applied: true}
	if err := r.applyUsageBillingEffects(ctx, tx, cmd, result); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	tx = nil
	return result, nil
}

func claimUsageBillingRequest(ctx context.Context, tx *sql.Tx, requestID string, apiKeyID int64, requestFingerprint string) (bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO usage_billing_dedup (request_id, api_key_id, request_fingerprint)
		VALUES ($1, $2, $3)
		ON CONFLICT (request_id, api_key_id) DO NOTHING
		RETURNING id
	`, requestID, apiKeyID, requestFingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var existingFingerprint string
		if err := tx.QueryRowContext(ctx, `
			SELECT request_fingerprint
			FROM usage_billing_dedup
			WHERE request_id = $1 AND api_key_id = $2
		`, requestID, apiKeyID).Scan(&existingFingerprint); err != nil {
			return false, err
		}
		if strings.TrimSpace(existingFingerprint) != strings.TrimSpace(requestFingerprint) {
			return false, service.ErrUsageBillingRequestConflict
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var archivedFingerprint string
	err = tx.QueryRowContext(ctx, `
		SELECT request_fingerprint
		FROM usage_billing_dedup_archive
		WHERE request_id = $1 AND api_key_id = $2
	`, requestID, apiKeyID).Scan(&archivedFingerprint)
	if err == nil {
		if strings.TrimSpace(archivedFingerprint) != strings.TrimSpace(requestFingerprint) {
			return false, service.ErrUsageBillingRequestConflict
		}
		if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM usage_billing_dedup WHERE id = $1`, id); deleteErr != nil {
			return false, deleteErr
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return true, nil
}

func (r *usageBillingRepository) ReserveBatchImageBalance(ctx context.Context, cmd *service.BatchImageBalanceHoldCommand) (*service.BatchImageBalanceHoldResult, error) {
	return r.applyBatchImageBalanceHold(ctx, cmd, reserveUsageBillingBatchImageBalance)
}

func (r *usageBillingRepository) CaptureBatchImageBalance(ctx context.Context, cmd *service.BatchImageBalanceHoldCommand) (*service.BatchImageBalanceHoldResult, error) {
	return r.applyBatchImageBalanceHold(ctx, cmd, captureUsageBillingBatchImageBalance)
}

func (r *usageBillingRepository) ReleaseBatchImageBalance(ctx context.Context, cmd *service.BatchImageBalanceHoldCommand) (*service.BatchImageBalanceHoldResult, error) {
	return r.applyBatchImageBalanceHold(ctx, cmd, releaseUsageBillingBatchImageBalance)
}

func (r *usageBillingRepository) applyBatchImageBalanceHold(
	ctx context.Context,
	cmd *service.BatchImageBalanceHoldCommand,
	apply func(context.Context, *sql.Tx, *service.BatchImageBalanceHoldCommand) (*service.BatchImageBalanceHoldResult, error),
) (_ *service.BatchImageBalanceHoldResult, err error) {
	if cmd == nil {
		return &service.BatchImageBalanceHoldResult{}, nil
	}
	if r == nil || r.db == nil {
		return nil, errors.New("usage billing repository db is nil")
	}
	cmd.Normalize()
	if cmd.RequestID == "" {
		return nil, service.ErrUsageBillingRequestIDRequired
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	applied, err := claimUsageBillingRequest(ctx, tx, cmd.RequestID, cmd.APIKeyID, cmd.RequestFingerprint)
	if err != nil {
		return nil, err
	}
	if !applied {
		return &service.BatchImageBalanceHoldResult{Applied: false}, nil
	}

	result, err := apply(ctx, tx, cmd)
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = &service.BatchImageBalanceHoldResult{}
	}
	result.Applied = true

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	tx = nil
	return result, nil
}

type videoHoldMutation int

const (
	reserveVideoHold videoHoldMutation = iota
	captureVideoHold
	releaseVideoHold
)

type storedVideoHold struct {
	RequestID        string
	UserID           int64
	APIKeyID         int64
	SubscriptionID   *int64
	GroupID          int64
	BillingMode      string
	ActualWithinHold bool
}

func (r *usageBillingRepository) ReserveVideo(ctx context.Context, cmd *service.VideoHoldCommand) (*service.VideoHoldResult, error) {
	return r.applyVideoHold(ctx, cmd, reserveVideoHold)
}

func (r *usageBillingRepository) CaptureVideo(ctx context.Context, cmd *service.VideoHoldCommand) (*service.VideoHoldResult, error) {
	return r.applyVideoHold(ctx, cmd, captureVideoHold)
}

func (r *usageBillingRepository) ReleaseVideo(ctx context.Context, cmd *service.VideoHoldCommand) (*service.VideoHoldResult, error) {
	return r.applyVideoHold(ctx, cmd, releaseVideoHold)
}

func (r *usageBillingRepository) applyVideoHold(
	ctx context.Context,
	cmd *service.VideoHoldCommand,
	mutation videoHoldMutation,
) (_ *service.VideoHoldResult, err error) {
	if cmd == nil {
		return &service.VideoHoldResult{}, nil
	}
	if r == nil || r.db == nil {
		return nil, errors.New("usage billing repository db is nil")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	result, err := applyVideoHoldInTx(ctx, tx, cmd, mutation)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	tx = nil
	return result, nil
}

func applyVideoHoldInTx(
	ctx context.Context,
	tx *sql.Tx,
	cmd *service.VideoHoldCommand,
	mutation videoHoldMutation,
) (*service.VideoHoldResult, error) {
	if err := validateVideoHoldCommand(cmd, mutation); err != nil {
		return nil, err
	}
	cmd.Normalize()
	hold, err := lockStoredVideoHold(ctx, tx, cmd)
	if err != nil {
		return nil, err
	}
	applied, err := claimUsageBillingRequest(ctx, tx, cmd.RequestID, cmd.APIKeyID, cmd.RequestFingerprint)
	if err != nil {
		return nil, err
	}
	if !applied {
		return &service.VideoHoldResult{Applied: false}, nil
	}
	if mutation != reserveVideoHold {
		held, err := usageBillingClaimExists(ctx, tx, service.VideoHoldRequestID(cmd.VideoRequestID), cmd.APIKeyID)
		if err != nil {
			return nil, err
		}
		if !held {
			return nil, service.ErrVideoTaskInvalidTransition
		}
		otherPrefix := service.VideoReleaseRequestID(cmd.VideoRequestID)
		if mutation == releaseVideoHold {
			otherPrefix = service.VideoCaptureRequestID(cmd.VideoRequestID)
		}
		finalized, err := usageBillingClaimExists(ctx, tx, otherPrefix, cmd.APIKeyID)
		if err != nil {
			return nil, err
		}
		if finalized {
			return nil, service.ErrVideoBillingAlreadyFinalized
		}
	}
	if mutation == captureVideoHold && !hold.ActualWithinHold {
		return nil, service.ErrVideoFinalCostExceedsHold
	}

	var result *service.VideoHoldResult
	switch hold.BillingMode {
	case "balance":
		result, err = mutateVideoBalanceHold(ctx, tx, hold, cmd.ActualAmount, mutation)
	case "subscription":
		result, err = mutateVideoSubscriptionHold(ctx, tx, hold, cmd.ActualAmount, mutation)
	default:
		return nil, service.ErrVideoTaskInvalidRequest
	}
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = &service.VideoHoldResult{}
	}
	result.Applied = true
	return result, nil
}

func validateVideoHoldCommand(cmd *service.VideoHoldCommand, mutation videoHoldMutation) error {
	if cmd == nil {
		return service.ErrVideoTaskInvalidRequest
	}
	cmd.Normalize()
	if cmd.RequestID == "" {
		return service.ErrUsageBillingRequestIDRequired
	}
	expectedRequestID := service.VideoHoldRequestID(cmd.VideoRequestID)
	switch mutation {
	case captureVideoHold:
		expectedRequestID = service.VideoCaptureRequestID(cmd.VideoRequestID)
	case releaseVideoHold:
		expectedRequestID = service.VideoReleaseRequestID(cmd.VideoRequestID)
	}
	if cmd.RequestID != expectedRequestID {
		return service.ErrVideoTaskInvalidRequest
	}
	if cmd.UserID <= 0 || cmd.APIKeyID <= 0 || !service.IsVideoRequestID(cmd.VideoRequestID) ||
		cmd.HoldAmount < 0 || math.IsNaN(cmd.HoldAmount) || math.IsInf(cmd.HoldAmount, 0) ||
		cmd.ActualAmount < 0 || math.IsNaN(cmd.ActualAmount) || math.IsInf(cmd.ActualAmount, 0) ||
		(cmd.BillingMode != "balance" && cmd.BillingMode != "subscription") ||
		(cmd.BillingMode == "subscription" && (cmd.SubscriptionID == nil || *cmd.SubscriptionID <= 0)) {
		return service.ErrVideoTaskInvalidRequest
	}
	return nil
}

func lockStoredVideoHold(ctx context.Context, tx *sql.Tx, cmd *service.VideoHoldCommand) (*storedVideoHold, error) {
	var hold storedVideoHold
	var subscriptionID sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT request_id, user_id, api_key_id, subscription_id, group_id, billing_mode,
		       ROUND($2::numeric, 8) <= frozen_amount
		FROM video_tasks
		WHERE request_id = $1
		FOR UPDATE
	`, cmd.VideoRequestID, cmd.ActualAmount).Scan(
		&hold.RequestID,
		&hold.UserID,
		&hold.APIKeyID,
		&subscriptionID,
		&hold.GroupID,
		&hold.BillingMode,
		&hold.ActualWithinHold,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrVideoTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if subscriptionID.Valid {
		hold.SubscriptionID = &subscriptionID.Int64
	}
	hold.BillingMode = strings.ToLower(strings.TrimSpace(hold.BillingMode))
	if hold.UserID != cmd.UserID || hold.APIKeyID != cmd.APIKeyID || hold.BillingMode != cmd.BillingMode ||
		valueOrZeroInt64(hold.SubscriptionID) != valueOrZeroInt64(cmd.SubscriptionID) {
		return nil, service.ErrVideoTaskInvalidRequest
	}
	return &hold, nil
}

func mutateVideoBalanceHold(
	ctx context.Context,
	tx *sql.Tx,
	hold *storedVideoHold,
	actualAmount float64,
	mutation videoHoldMutation,
) (*service.VideoHoldResult, error) {
	var balance, frozen float64
	var err error
	switch mutation {
	case reserveVideoHold:
		err = tx.QueryRowContext(ctx, `
			UPDATE users AS u
			SET balance = u.balance - vt.frozen_amount,
				frozen_balance = COALESCE(u.frozen_balance, 0) + vt.frozen_amount,
				updated_at = NOW()
			FROM video_tasks AS vt
			WHERE vt.request_id = $1 AND u.id = $2 AND u.id = vt.user_id
			  AND u.deleted_at IS NULL AND u.balance >= vt.frozen_amount
			RETURNING u.balance, u.frozen_balance
		`, hold.RequestID, hold.UserID).Scan(&balance, &frozen)
	case captureVideoHold:
		err = tx.QueryRowContext(ctx, `
			UPDATE users AS u
			SET balance = u.balance + vt.frozen_amount - ROUND($2::numeric, 8),
				frozen_balance = COALESCE(u.frozen_balance, 0) - vt.frozen_amount,
				updated_at = NOW()
			FROM video_tasks AS vt
			WHERE vt.request_id = $1 AND u.id = $3 AND u.id = vt.user_id
			  AND u.deleted_at IS NULL
			  AND COALESCE(u.frozen_balance, 0) >= vt.frozen_amount
			  AND ROUND($2::numeric, 8) <= vt.frozen_amount
			RETURNING u.balance, u.frozen_balance
		`, hold.RequestID, actualAmount, hold.UserID).Scan(&balance, &frozen)
	case releaseVideoHold:
		err = tx.QueryRowContext(ctx, `
			UPDATE users AS u
			SET balance = u.balance + vt.frozen_amount,
				frozen_balance = COALESCE(u.frozen_balance, 0) - vt.frozen_amount,
				updated_at = NOW()
			FROM video_tasks AS vt
			WHERE vt.request_id = $1 AND u.id = $2 AND u.id = vt.user_id
			  AND u.deleted_at IS NULL
			  AND COALESCE(u.frozen_balance, 0) >= vt.frozen_amount
			RETURNING u.balance, u.frozen_balance
		`, hold.RequestID, hold.UserID).Scan(&balance, &frozen)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if mutation == reserveVideoHold {
			var exists int
			existsErr := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL`, hold.UserID).Scan(&exists)
			if errors.Is(existsErr, sql.ErrNoRows) {
				return nil, service.ErrUserNotFound
			}
			if existsErr != nil {
				return nil, existsErr
			}
			return nil, service.ErrVideoInsufficientBalance
		}
		return nil, errors.New("video frozen balance is insufficient")
	}
	if err != nil {
		return nil, err
	}
	return &service.VideoHoldResult{NewBalance: &balance, FrozenBalance: &frozen}, nil
}

func mutateVideoSubscriptionHold(
	ctx context.Context,
	tx *sql.Tx,
	hold *storedVideoHold,
	actualAmount float64,
	mutation videoHoldMutation,
) (*service.VideoHoldResult, error) {
	if hold.SubscriptionID == nil {
		return nil, service.ErrVideoTaskInvalidRequest
	}
	if mutation == reserveVideoHold {
		return reserveVideoSubscriptionHold(ctx, tx, hold)
	}
	var frozen float64
	var exists int
	err := tx.QueryRowContext(ctx, `
		SELECT 1
		FROM user_subscriptions
		WHERE id = $1 AND user_id = $2
		FOR UPDATE
	`, *hold.SubscriptionID, hold.UserID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, err
	}
	if mutation == captureVideoHold {
		err = tx.QueryRowContext(ctx, `
			UPDATE user_subscriptions AS us
			SET frozen_quota = us.frozen_quota - vt.frozen_amount,
				daily_usage_usd = daily_usage_usd + ROUND($2::numeric, 8),
				weekly_usage_usd = weekly_usage_usd + ROUND($2::numeric, 8),
				monthly_usage_usd = monthly_usage_usd + ROUND($2::numeric, 8),
				updated_at = NOW()
			FROM video_tasks AS vt
			WHERE vt.request_id = $1 AND us.id = $3 AND us.user_id = $4
			  AND us.frozen_quota >= vt.frozen_amount
			  AND ROUND($2::numeric, 8) <= vt.frozen_amount
			RETURNING us.frozen_quota
		`, hold.RequestID, actualAmount, *hold.SubscriptionID, hold.UserID).Scan(&frozen)
	} else {
		err = tx.QueryRowContext(ctx, `
			UPDATE user_subscriptions AS us
			SET frozen_quota = us.frozen_quota - vt.frozen_amount, updated_at = NOW()
			FROM video_tasks AS vt
			WHERE vt.request_id = $1 AND us.id = $2 AND us.user_id = $3
			  AND us.frozen_quota >= vt.frozen_amount
			RETURNING us.frozen_quota
		`, hold.RequestID, *hold.SubscriptionID, hold.UserID).Scan(&frozen)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("video subscription frozen quota is insufficient")
	}
	if err != nil {
		return nil, err
	}
	return &service.VideoHoldResult{FrozenQuota: &frozen}, nil
}

func reserveVideoSubscriptionHold(ctx context.Context, tx *sql.Tx, hold *storedVideoHold) (*service.VideoHoldResult, error) {
	var frozen float64
	var sufficient bool
	err := tx.QueryRowContext(ctx, `
		SELECT us.frozen_quota,
		       (g.daily_limit_usd IS NULL OR g.daily_limit_usd <= 0
		        OR us.daily_usage_usd + us.frozen_quota + vt.frozen_amount <= g.daily_limit_usd)
		       AND (g.weekly_limit_usd IS NULL OR g.weekly_limit_usd <= 0
		        OR us.weekly_usage_usd + us.frozen_quota + vt.frozen_amount <= g.weekly_limit_usd)
		       AND (g.monthly_limit_usd IS NULL OR g.monthly_limit_usd <= 0
		        OR us.monthly_usage_usd + us.frozen_quota + vt.frozen_amount <= g.monthly_limit_usd)
		FROM user_subscriptions AS us
		JOIN groups AS g ON g.id = us.group_id
		JOIN video_tasks AS vt ON vt.request_id = $5 AND vt.subscription_id = us.id
		WHERE us.id = $1 AND us.user_id = $2 AND us.group_id = $3
		  AND us.deleted_at IS NULL AND us.status = $4 AND us.expires_at > NOW()
		  AND g.deleted_at IS NULL
		FOR UPDATE OF us, g
	`, *hold.SubscriptionID, hold.UserID, hold.GroupID, service.SubscriptionStatusActive, hold.RequestID).Scan(&frozen, &sufficient)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, err
	}
	if !sufficient {
		return nil, service.ErrVideoSubscriptionQuotaExceeded
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE user_subscriptions AS us
		SET frozen_quota = us.frozen_quota + vt.frozen_amount, updated_at = NOW()
		FROM video_tasks AS vt
		WHERE vt.request_id = $1 AND us.id = $2 AND us.user_id = $3
		RETURNING us.frozen_quota
	`, hold.RequestID, *hold.SubscriptionID, hold.UserID).Scan(&frozen); err != nil {
		return nil, err
	}
	return &service.VideoHoldResult{FrozenQuota: &frozen}, nil
}

func usageBillingClaimExists(ctx context.Context, tx *sql.Tx, requestID string, apiKeyID int64) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM usage_billing_dedup
		WHERE request_id = $1 AND api_key_id = $2
	`, requestID, apiKeyID).Scan(&exists)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM usage_billing_dedup_archive
		WHERE request_id = $1 AND api_key_id = $2
	`, requestID, apiKeyID).Scan(&exists)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func valueOrZeroInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (r *usageBillingRepository) applyUsageBillingEffects(ctx context.Context, tx *sql.Tx, cmd *service.UsageBillingCommand, result *service.UsageBillingApplyResult) error {
	if cmd.SubscriptionCost > 0 && cmd.SubscriptionID != nil {
		if err := incrementUsageBillingSubscription(ctx, tx, *cmd.SubscriptionID, cmd.SubscriptionCost); err != nil {
			return err
		}
	}

	if cmd.BalanceCost > 0 {
		newBalance, sufficient, err := deductUsageBillingBalance(ctx, tx, cmd.UserID, cmd.BalanceCost)
		if err != nil {
			return err
		}
		result.NewBalance = &newBalance
		result.BalanceOverdrafted = !sufficient
	}

	if cmd.APIKeyQuotaCost > 0 {
		exhausted, err := incrementUsageBillingAPIKeyQuota(ctx, tx, cmd.APIKeyID, cmd.APIKeyQuotaCost)
		if err != nil {
			return err
		}
		result.APIKeyQuotaExhausted = exhausted
	}

	if cmd.APIKeyRateLimitCost > 0 {
		if err := incrementUsageBillingAPIKeyRateLimit(ctx, tx, cmd.APIKeyID, cmd.APIKeyRateLimitCost); err != nil {
			return err
		}
	}

	if cmd.AccountQuotaCost > 0 && (strings.EqualFold(cmd.AccountType, service.AccountTypeAPIKey) || strings.EqualFold(cmd.AccountType, service.AccountTypeBedrock)) {
		quotaState, err := incrementUsageBillingAccountQuota(ctx, tx, cmd.AccountID, cmd.AccountQuotaCost)
		if err != nil {
			return err
		}
		result.QuotaState = quotaState
	}

	return nil
}

func incrementUsageBillingSubscription(ctx context.Context, tx *sql.Tx, subscriptionID int64, costUSD float64) error {
	const updateSQL = `
		UPDATE user_subscriptions us
		SET
			daily_usage_usd = us.daily_usage_usd + $1,
			weekly_usage_usd = us.weekly_usage_usd + $1,
			monthly_usage_usd = us.monthly_usage_usd + $1,
			updated_at = NOW()
		FROM groups g
		WHERE us.id = $2
			AND us.deleted_at IS NULL
			AND us.group_id = g.id
			AND g.deleted_at IS NULL
	`
	res, err := tx.ExecContext(ctx, updateSQL, costUSD, subscriptionID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		return nil
	}
	return service.ErrSubscriptionNotFound
}

func deductUsageBillingBalance(ctx context.Context, tx *sql.Tx, userID int64, amount float64) (float64, bool, error) {
	var newBalance float64
	err := tx.QueryRowContext(ctx, `
		UPDATE users
		SET balance = balance - $1,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL AND balance >= $1
		RETURNING balance
	`, amount, userID).Scan(&newBalance)
	if err == nil {
		return newBalance, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}

	err = tx.QueryRowContext(ctx, `
		UPDATE users
		SET balance = balance - $1,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
		RETURNING balance
	`, amount, userID).Scan(&newBalance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, service.ErrUserNotFound
	}
	if err != nil {
		return 0, false, err
	}
	return newBalance, false, nil
}

func reserveUsageBillingBatchImageBalance(ctx context.Context, tx *sql.Tx, cmd *service.BatchImageBalanceHoldCommand) (*service.BatchImageBalanceHoldResult, error) {
	if cmd.HoldAmount <= 0 {
		return &service.BatchImageBalanceHoldResult{}, nil
	}
	var balance, frozen float64
	err := tx.QueryRowContext(ctx, `
		UPDATE users
		SET balance = balance - $1,
			frozen_balance = COALESCE(frozen_balance, 0) + $1,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL AND balance >= $1
		RETURNING balance, frozen_balance
	`, cmd.HoldAmount, cmd.UserID).Scan(&balance, &frozen)
	if err == nil {
		return &service.BatchImageBalanceHoldResult{NewBalance: &balance, FrozenBalance: &frozen}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if exists, existsErr := userExistsForBilling(ctx, tx, cmd.UserID); existsErr != nil {
		return nil, existsErr
	} else if !exists {
		return nil, service.ErrUserNotFound
	}
	return nil, service.ErrBatchImageInsufficientBalance
}

func captureUsageBillingBatchImageBalance(ctx context.Context, tx *sql.Tx, cmd *service.BatchImageBalanceHoldCommand) (*service.BatchImageBalanceHoldResult, error) {
	if cmd.HoldAmount <= 0 && cmd.ActualAmount <= 0 {
		return &service.BatchImageBalanceHoldResult{}, nil
	}
	if cmd.ActualAmount-cmd.HoldAmount > 0.00000001 {
		return nil, service.ErrBatchImageSettlementCostExceedsHold
	}
	var balance, frozen float64
	err := tx.QueryRowContext(ctx, `
		UPDATE users
		SET balance = balance
				+ CASE WHEN $1 > $2 THEN $1 - $2 ELSE 0 END
				- CASE WHEN $2 > $1 THEN $2 - $1 ELSE 0 END,
			frozen_balance = COALESCE(frozen_balance, 0) - $1,
			updated_at = NOW()
		WHERE id = $3 AND deleted_at IS NULL AND COALESCE(frozen_balance, 0) >= $1
		RETURNING balance, frozen_balance
	`, cmd.HoldAmount, cmd.ActualAmount, cmd.UserID).Scan(&balance, &frozen)
	if err == nil {
		return &service.BatchImageBalanceHoldResult{NewBalance: &balance, FrozenBalance: &frozen}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if exists, existsErr := userExistsForBilling(ctx, tx, cmd.UserID); existsErr != nil {
		return nil, existsErr
	} else if !exists {
		return nil, service.ErrUserNotFound
	}
	return nil, errors.New("batch image frozen balance is insufficient")
}

func releaseUsageBillingBatchImageBalance(ctx context.Context, tx *sql.Tx, cmd *service.BatchImageBalanceHoldCommand) (*service.BatchImageBalanceHoldResult, error) {
	if cmd.HoldAmount <= 0 {
		return &service.BatchImageBalanceHoldResult{}, nil
	}
	// 释放前校验该 job 确实预留过 hold（hold request id 已被 claim），
	// 防止从未成功冻结的 job 触发"幻影释放"，从其他用户的冻结资金池中凭空生成余额。
	held, heldErr := batchImageHoldClaimExists(ctx, tx, service.BatchImageHoldRequestID(cmd.BatchID), cmd.APIKeyID)
	if heldErr != nil {
		return nil, heldErr
	}
	if !held {
		logger.LegacyPrintf("repository.usage_billing", "[BatchImage] release skipped, hold was never reserved: batch=%s", cmd.BatchID)
		return &service.BatchImageBalanceHoldResult{}, nil
	}
	var balance, frozen float64
	err := tx.QueryRowContext(ctx, `
		UPDATE users
		SET balance = balance + $1,
			frozen_balance = COALESCE(frozen_balance, 0) - $1,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL AND COALESCE(frozen_balance, 0) >= $1
		RETURNING balance, frozen_balance
	`, cmd.HoldAmount, cmd.UserID).Scan(&balance, &frozen)
	if err == nil {
		return &service.BatchImageBalanceHoldResult{NewBalance: &balance, FrozenBalance: &frozen}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if exists, existsErr := userExistsForBilling(ctx, tx, cmd.UserID); existsErr != nil {
		return nil, existsErr
	} else if !exists {
		return nil, service.ErrUserNotFound
	}
	return nil, errors.New("batch image frozen balance is insufficient")
}

// batchImageHoldClaimExists 检查 hold request id 是否已在 dedup（或归档）表中被 claim，
// 即该 batch 的冻结操作确实成功提交过。
func batchImageHoldClaimExists(ctx context.Context, tx *sql.Tx, holdRequestID string, apiKeyID int64) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, `
		SELECT 1
		FROM usage_billing_dedup
		WHERE request_id = $1 AND api_key_id = $2
	`, holdRequestID, apiKeyID).Scan(&exists)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	err = tx.QueryRowContext(ctx, `
		SELECT 1
		FROM usage_billing_dedup_archive
		WHERE request_id = $1 AND api_key_id = $2
	`, holdRequestID, apiKeyID).Scan(&exists)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func userExistsForBilling(ctx context.Context, tx *sql.Tx, userID int64) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, `
		SELECT 1
		FROM users
		WHERE id = $1 AND deleted_at IS NULL
	`, userID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func incrementUsageBillingAPIKeyQuota(ctx context.Context, tx *sql.Tx, apiKeyID int64, amount float64) (bool, error) {
	var exhausted bool
	err := tx.QueryRowContext(ctx, `
		UPDATE api_keys
		SET quota_used = quota_used + $1,
			status = CASE
				WHEN quota > 0
					AND status = $3
					AND quota_used < quota
					AND quota_used + $1 >= quota
				THEN $4
				ELSE status
			END,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
		RETURNING quota > 0 AND quota_used >= quota AND quota_used - $1 < quota
	`, amount, apiKeyID, service.StatusAPIKeyActive, service.StatusAPIKeyQuotaExhausted).Scan(&exhausted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, service.ErrAPIKeyNotFound
	}
	if err != nil {
		return false, err
	}
	return exhausted, nil
}

func incrementUsageBillingAPIKeyRateLimit(ctx context.Context, tx *sql.Tx, apiKeyID int64, cost float64) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE api_keys SET
			usage_5h = CASE WHEN window_5h_start IS NOT NULL AND window_5h_start + INTERVAL '5 hours' <= NOW() THEN $1 ELSE usage_5h + $1 END,
			usage_1d = CASE WHEN window_1d_start IS NOT NULL AND window_1d_start + INTERVAL '24 hours' <= NOW() THEN $1 ELSE usage_1d + $1 END,
			usage_7d = CASE WHEN window_7d_start IS NOT NULL AND window_7d_start + INTERVAL '7 days' <= NOW() THEN $1 ELSE usage_7d + $1 END,
			window_5h_start = CASE WHEN window_5h_start IS NULL OR window_5h_start + INTERVAL '5 hours' <= NOW() THEN NOW() ELSE window_5h_start END,
			window_1d_start = CASE WHEN window_1d_start IS NULL OR window_1d_start + INTERVAL '24 hours' <= NOW() THEN date_trunc('day', NOW()) ELSE window_1d_start END,
			window_7d_start = CASE WHEN window_7d_start IS NULL OR window_7d_start + INTERVAL '7 days' <= NOW() THEN date_trunc('day', NOW()) ELSE window_7d_start END,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
	`, cost, apiKeyID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAPIKeyNotFound
	}
	return nil
}

func incrementUsageBillingAccountQuota(ctx context.Context, tx *sql.Tx, accountID int64, amount float64) (*service.AccountQuotaState, error) {
	rows, err := tx.QueryContext(ctx,
		`UPDATE accounts SET extra = (
			COALESCE(extra, '{}'::jsonb)
			|| jsonb_build_object('quota_used', COALESCE((extra->>'quota_used')::numeric, 0) + $1)
			|| CASE WHEN COALESCE((extra->>'quota_daily_limit')::numeric, 0) > 0 THEN
				jsonb_build_object(
					'quota_daily_used',
					CASE WHEN `+dailyExpiredExpr+`
					THEN $1
					ELSE COALESCE((extra->>'quota_daily_used')::numeric, 0) + $1 END,
					'quota_daily_start',
					CASE WHEN `+dailyExpiredExpr+`
					THEN `+nowUTC+`
					ELSE COALESCE(extra->>'quota_daily_start', `+nowUTC+`) END
				)
				|| CASE WHEN `+dailyExpiredExpr+` AND `+nextDailyResetAtExpr+` IS NOT NULL
				   THEN jsonb_build_object('quota_daily_reset_at', `+nextDailyResetAtExpr+`)
				   ELSE '{}'::jsonb END
			ELSE '{}'::jsonb END
			|| CASE WHEN COALESCE((extra->>'quota_weekly_limit')::numeric, 0) > 0 THEN
				jsonb_build_object(
					'quota_weekly_used',
					CASE WHEN `+weeklyExpiredExpr+`
					THEN $1
					ELSE COALESCE((extra->>'quota_weekly_used')::numeric, 0) + $1 END,
					'quota_weekly_start',
					CASE WHEN `+weeklyExpiredExpr+`
					THEN `+nowUTC+`
					ELSE COALESCE(extra->>'quota_weekly_start', `+nowUTC+`) END
				)
				|| CASE WHEN `+weeklyExpiredExpr+` AND `+nextWeeklyResetAtExpr+` IS NOT NULL
				   THEN jsonb_build_object('quota_weekly_reset_at', `+nextWeeklyResetAtExpr+`)
				   ELSE '{}'::jsonb END
			ELSE '{}'::jsonb END
		), updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
		RETURNING
			COALESCE((extra->>'quota_used')::numeric, 0),
			COALESCE((extra->>'quota_limit')::numeric, 0),
			COALESCE((extra->>'quota_daily_used')::numeric, 0),
			COALESCE((extra->>'quota_daily_limit')::numeric, 0),
			COALESCE((extra->>'quota_weekly_used')::numeric, 0),
			COALESCE((extra->>'quota_weekly_limit')::numeric, 0)`,
		amount, accountID)
	if err != nil {
		return nil, err
	}

	var state service.AccountQuotaState
	if rows.Next() {
		if err := rows.Scan(
			&state.TotalUsed, &state.TotalLimit,
			&state.DailyUsed, &state.DailyLimit,
			&state.WeeklyUsed, &state.WeeklyLimit,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
	} else {
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
		return nil, service.ErrAccountNotFound
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	// 必须在执行下一条 SQL 前显式关闭 rows：pq 驱动在同一连接上
	// 不允许前一条查询的结果集未耗尽时启动新查询，否则会返回
	// "unexpected Parse response" 错误。
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// 任意维度额度在本次递增中从"未超"跨越到"已超"时，必须刷新调度快照，
	// 否则 Redis 中缓存的 Account 仍显示旧的 used 值，后续请求会继续选中本账号，
	// 最终观察到 daily_used / weekly_used 大幅超过配置的 limit。
	// 对于日/周额度，即使本次触发了周期重置（pre=0、post=amount），
	// 判定式 (post-amount) < limit 同样成立，逻辑与总额度保持一致。
	crossedTotal := state.TotalLimit > 0 && state.TotalUsed >= state.TotalLimit && (state.TotalUsed-amount) < state.TotalLimit
	crossedDaily := state.DailyLimit > 0 && state.DailyUsed >= state.DailyLimit && (state.DailyUsed-amount) < state.DailyLimit
	crossedWeekly := state.WeeklyLimit > 0 && state.WeeklyUsed >= state.WeeklyLimit && (state.WeeklyUsed-amount) < state.WeeklyLimit
	if crossedTotal || crossedDaily || crossedWeekly {
		if err := enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
			logger.LegacyPrintf("repository.usage_billing", "[SchedulerOutbox] enqueue quota exceeded failed: account=%d err=%v", accountID, err)
			return nil, err
		}
	}
	return &state, nil
}
