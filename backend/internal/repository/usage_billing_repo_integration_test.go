//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestUsageBillingRepositoryBalanceVideoHoldLifecycleIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)
	task, apiKey, user := createVideoBillingTask(t, client, taskRepo, "balance-lifecycle", "balance", nil, 2, 10, nil, 0)

	reserve := videoHoldCommand(task, service.VideoHoldRequestID(task.RequestID), 0)
	result, err := billing.ReserveVideo(ctx, reserve)
	require.NoError(t, err)
	require.True(t, result.Applied)
	result, err = billing.ReserveVideo(ctx, reserve)
	require.NoError(t, err)
	require.False(t, result.Applied)

	capture := videoHoldCommand(task, service.VideoCaptureRequestID(task.RequestID), 1.25)
	result, err = billing.CaptureVideo(ctx, capture)
	require.NoError(t, err)
	require.True(t, result.Applied)
	result, err = billing.CaptureVideo(ctx, capture)
	require.NoError(t, err)
	require.False(t, result.Applied)

	var balance, frozen float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance, frozen_balance FROM users WHERE id = $1`, user.ID).Scan(&balance, &frozen))
	require.InDelta(t, 8.75, balance, 0.000000001)
	require.InDelta(t, 0, frozen, 0.000000001)

	var dedupCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_billing_dedup WHERE api_key_id = $1 AND request_id IN ($2, $3)`,
		apiKey.ID, reserve.RequestID, capture.RequestID).Scan(&dedupCount))
	require.Equal(t, 2, dedupCount)
}

func TestUsageBillingRepositoryBalanceVideoCaptureUsesStoredHold(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)
	task, _, user := createVideoBillingTask(t, client, taskRepo, "stored-balance-hold", "balance", nil, 2, 10, nil, 0)
	require.NoError(t, reserveVideoTask(t, billing, task))

	capture := videoHoldCommand(task, service.VideoCaptureRequestID(task.RequestID), 2.01)
	capture.HoldAmount = 99
	_, err := billing.CaptureVideo(ctx, capture)
	require.ErrorIs(t, err, service.ErrVideoFinalCostExceedsHold)

	var balance, frozen float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance, frozen_balance FROM users WHERE id = $1`, user.ID).Scan(&balance, &frozen))
	require.InDelta(t, 8, balance, 0.000000001)
	require.InDelta(t, 2, frozen, 0.000000001)
}

func TestUsageBillingRepositorySubscriptionVideoHoldLifecycleAndQuota(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)
	dailyLimit := 3.0
	task, _, _ := createVideoBillingTask(t, client, taskRepo, "subscription-lifecycle", "subscription", nil, 2, 0, &dailyLimit, 0.5)

	reserve := videoHoldCommand(task, service.VideoHoldRequestID(task.RequestID), 0)
	result, err := billing.ReserveVideo(ctx, reserve)
	require.NoError(t, err)
	require.True(t, result.Applied)
	result, err = billing.ReserveVideo(ctx, reserve)
	require.NoError(t, err)
	require.False(t, result.Applied)

	capture := videoHoldCommand(task, service.VideoCaptureRequestID(task.RequestID), 1.25)
	result, err = billing.CaptureVideo(ctx, capture)
	require.NoError(t, err)
	require.True(t, result.Applied)

	var frozen, daily, weekly, monthly float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT frozen_quota, daily_usage_usd, weekly_usage_usd, monthly_usage_usd
		FROM user_subscriptions WHERE id = $1`, *task.SubscriptionID).Scan(&frozen, &daily, &weekly, &monthly))
	require.InDelta(t, 0, frozen, 0.000000001)
	require.InDelta(t, 1.75, daily, 0.000000001)
	require.InDelta(t, 1.25, weekly, 0.000000001)
	require.InDelta(t, 1.25, monthly, 0.000000001)
}

func TestUsageBillingRepositorySubscriptionVideoReserveRejectsInsufficientQuota(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)
	dailyLimit := 3.0
	task, apiKey, _ := createVideoBillingTask(t, client, taskRepo, "subscription-insufficient", "subscription", nil, 2.51, 0, &dailyLimit, 0.5)

	_, err := billing.ReserveVideo(ctx, videoHoldCommand(task, service.VideoHoldRequestID(task.RequestID), 0))
	require.ErrorIs(t, err, service.ErrVideoSubscriptionQuotaExceeded)

	var frozen float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT frozen_quota FROM user_subscriptions WHERE id = $1`, *task.SubscriptionID).Scan(&frozen))
	require.Zero(t, frozen)
	var dedupCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_billing_dedup WHERE api_key_id = $1 AND request_id = $2`,
		apiKey.ID, service.VideoHoldRequestID(task.RequestID)).Scan(&dedupCount))
	require.Zero(t, dedupCount)
}

func TestUsageBillingRepositorySubscriptionVideoReleaseDoesNotConsumeQuota(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)
	dailyLimit := 3.0
	task, _, _ := createVideoBillingTask(t, client, taskRepo, "subscription-release", "subscription", nil, 2, 0, &dailyLimit, 0.5)
	require.NoError(t, reserveVideoTask(t, billing, task))

	release := videoHoldCommand(task, service.VideoReleaseRequestID(task.RequestID), 0)
	result, err := billing.ReleaseVideo(ctx, release)
	require.NoError(t, err)
	require.True(t, result.Applied)
	result, err = billing.ReleaseVideo(ctx, release)
	require.NoError(t, err)
	require.False(t, result.Applied)

	var frozen, daily, weekly, monthly float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT frozen_quota, daily_usage_usd, weekly_usage_usd, monthly_usage_usd
		FROM user_subscriptions WHERE id = $1`, *task.SubscriptionID).Scan(&frozen, &daily, &weekly, &monthly))
	require.InDelta(t, 0, frozen, 0.000000001)
	require.InDelta(t, 0.5, daily, 0.000000001)
	require.InDelta(t, 0, weekly, 0.000000001)
	require.InDelta(t, 0, monthly, 0.000000001)
}

func TestUsageBillingRepositoryBalanceVideoCannotReleaseAfterCapture(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)
	task, _, user := createVideoBillingTask(t, client, taskRepo, "capture-then-release", "balance", nil, 2, 10, nil, 0)
	require.NoError(t, reserveVideoTask(t, billing, task))
	_, err := billing.CaptureVideo(ctx, videoHoldCommand(task, service.VideoCaptureRequestID(task.RequestID), 1.25))
	require.NoError(t, err)

	_, err = billing.ReleaseVideo(ctx, videoHoldCommand(task, service.VideoReleaseRequestID(task.RequestID), 0))
	require.ErrorIs(t, err, service.ErrVideoBillingAlreadyFinalized)

	var balance, frozen float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance, frozen_balance FROM users WHERE id = $1`, user.ID).Scan(&balance, &frozen))
	require.InDelta(t, 8.75, balance, 0.000000001)
	require.InDelta(t, 0, frozen, 0.000000001)
}

func TestUsageBillingRepositorySubscriptionVideoConcurrentReservesLockQuota(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	billing := NewUsageBillingRepository(client, integrationDB)
	taskRepo := NewVideoTaskRepository(integrationDB)
	dailyLimit := 3.0
	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("video-lock-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name:                 "video-lock-group-" + uuid.NewString(),
		Platform:             service.PlatformVideo,
		SubscriptionType:     service.SubscriptionTypeSubscription,
		DailyLimitUSD:        &dailyLimit,
		AllowVideoGeneration: true,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID:  user.ID,
		GroupID: &group.ID,
		Key:     "sk-video-lock-" + uuid.NewString(),
		Name:    "video-lock",
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name:     "video-lock-account-" + uuid.NewString(),
		Platform: service.PlatformVideo,
	})
	subscription := mustCreateSubscription(t, client, &service.UserSubscription{
		UserID:  user.ID,
		GroupID: group.ID,
		Notes:   "video-hold-overlap",
	})
	tasks := make([]*service.VideoTask, 2)
	for i := range tasks {
		params := videoTaskCreateParams(t, fmt.Sprintf("concurrent-%d", i), "")
		params.UserID = user.ID
		params.APIKeyID = apiKey.ID
		params.SubscriptionID = &subscription.ID
		params.GroupID = group.ID
		params.AccountID = account.ID
		params.BillingMode = "subscription"
		params.FrozenAmount = 2
		params.EstimatedAmount = 2
		var err error
		tasks[i], _, err = taskRepo.CreateOrGet(ctx, params)
		require.NoError(t, err)
		cleanupVideoTask(t, tasks[i].RequestID)
	}
	installVideoSubscriptionReserveOverlapTrigger(t)

	errs := make([]error, len(tasks))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task *service.VideoTask) {
			defer wg.Done()
			<-start
			_, errs[i] = billing.ReserveVideo(ctx, videoHoldCommand(task, service.VideoHoldRequestID(task.RequestID), 0))
		}(i, task)
	}
	close(start)
	wg.Wait()

	var success, exceeded int
	for _, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, service.ErrVideoSubscriptionQuotaExceeded):
			exceeded++
		default:
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, exceeded)
	var frozen float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT frozen_quota FROM user_subscriptions WHERE id = $1`, subscription.ID).Scan(&frozen))
	require.InDelta(t, 2, frozen, 0.000000001)
	var holdClaims int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_billing_dedup WHERE api_key_id = $1 AND request_id LIKE 'video_hold:%'`, apiKey.ID).Scan(&holdClaims))
	require.Equal(t, 1, holdClaims)
}

func installVideoSubscriptionReserveOverlapTrigger(t *testing.T) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), `
		CREATE OR REPLACE FUNCTION video_subscription_test_hold_reserve()
		RETURNS TRIGGER AS $$
		BEGIN
			IF NEW.notes = 'video-hold-overlap' AND NEW.frozen_quota > OLD.frozen_quota THEN
				PERFORM pg_sleep(1);
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_video_subscription_test_hold_reserve
		BEFORE UPDATE ON user_subscriptions
		FOR EACH ROW EXECUTE FUNCTION video_subscription_test_hold_reserve();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS trg_video_subscription_test_hold_reserve ON user_subscriptions`)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS video_subscription_test_hold_reserve()`)
	})
}

func createVideoBillingTask(
	t *testing.T,
	client *dbent.Client,
	repo service.VideoTaskRepository,
	label, billingMode string,
	subscriptionID *int64,
	hold, balance float64,
	dailyLimit *float64,
	dailyUsage float64,
) (*service.VideoTask, *service.APIKey, *service.User) {
	t.Helper()
	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("video-billing-%s-%d@example.com", label, time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      balance,
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name:                 "video-billing-" + label + "-" + uuid.NewString(),
		Platform:             service.PlatformVideo,
		SubscriptionType:     service.SubscriptionTypeSubscription,
		DailyLimitUSD:        dailyLimit,
		AllowVideoGeneration: true,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID:  user.ID,
		GroupID: &group.ID,
		Key:     "sk-video-billing-" + uuid.NewString(),
		Name:    "video-billing-" + label,
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name:     "video-billing-account-" + uuid.NewString(),
		Platform: service.PlatformVideo,
	})
	if billingMode == "subscription" && subscriptionID == nil {
		subscription := mustCreateSubscription(t, client, &service.UserSubscription{
			UserID:        user.ID,
			GroupID:       group.ID,
			DailyUsageUSD: dailyUsage,
		})
		subscriptionID = &subscription.ID
	}
	params := videoTaskCreateParams(t, label, "")
	params.UserID = user.ID
	params.APIKeyID = apiKey.ID
	params.SubscriptionID = subscriptionID
	params.GroupID = group.ID
	params.AccountID = account.ID
	params.FrozenAmount = hold
	params.EstimatedAmount = hold
	params.BillingMode = billingMode
	task, _, err := repo.CreateOrGet(context.Background(), params)
	require.NoError(t, err)
	cleanupVideoTask(t, task.RequestID)
	return task, apiKey, user
}

func videoHoldCommand(task *service.VideoTask, requestID string, actual float64) *service.VideoHoldCommand {
	return &service.VideoHoldCommand{
		RequestID:          requestID,
		RequestPayloadHash: task.RequestHash,
		UserID:             task.UserID,
		APIKeyID:           task.APIKeyID,
		SubscriptionID:     task.SubscriptionID,
		VideoRequestID:     task.RequestID,
		BillingMode:        task.BillingMode,
		HoldAmount:         task.FrozenAmount,
		ActualAmount:       actual,
	}
}

func reserveVideoTask(t *testing.T, billing service.UsageBillingRepository, task *service.VideoTask) error {
	t.Helper()
	_, err := billing.ReserveVideo(context.Background(), videoHoldCommand(task, service.VideoHoldRequestID(task.RequestID), 0))
	return err
}

func TestUsageBillingRepositoryApply_DeduplicatesBalanceBilling(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)

	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("usage-billing-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      100,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID,
		Key:    "sk-usage-billing-" + uuid.NewString(),
		Name:   "billing",
		Quota:  1,
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name: "usage-billing-account-" + uuid.NewString(),
		Type: service.AccountTypeAPIKey,
	})

	requestID := uuid.NewString()
	cmd := &service.UsageBillingCommand{
		RequestID:           requestID,
		APIKeyID:            apiKey.ID,
		UserID:              user.ID,
		AccountID:           account.ID,
		AccountType:         service.AccountTypeAPIKey,
		BalanceCost:         1.25,
		APIKeyQuotaCost:     1.25,
		APIKeyRateLimitCost: 1.25,
	}

	result1, err := repo.Apply(ctx, cmd)
	require.NoError(t, err)
	require.NotNil(t, result1)
	require.True(t, result1.Applied)
	require.True(t, result1.APIKeyQuotaExhausted)

	result2, err := repo.Apply(ctx, cmd)
	require.NoError(t, err)
	require.NotNil(t, result2)
	require.False(t, result2.Applied)

	var balance float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance FROM users WHERE id = $1", user.ID).Scan(&balance))
	require.InDelta(t, 98.75, balance, 0.000001)

	var quotaUsed float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT quota_used FROM api_keys WHERE id = $1", apiKey.ID).Scan(&quotaUsed))
	require.InDelta(t, 1.25, quotaUsed, 0.000001)

	var usage5h float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT usage_5h FROM api_keys WHERE id = $1", apiKey.ID).Scan(&usage5h))
	require.InDelta(t, 1.25, usage5h, 0.000001)

	var status string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT status FROM api_keys WHERE id = $1", apiKey.ID).Scan(&status))
	require.Equal(t, service.StatusAPIKeyQuotaExhausted, status)

	var dedupCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id = $1 AND api_key_id = $2", requestID, apiKey.ID).Scan(&dedupCount))
	require.Equal(t, 1, dedupCount)
}

func TestUsageBillingRepositoryApply_DeduplicatesSubscriptionBilling(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)

	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("usage-billing-sub-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name:             "usage-billing-group-" + uuid.NewString(),
		Platform:         service.PlatformAnthropic,
		SubscriptionType: service.SubscriptionTypeSubscription,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID:  user.ID,
		GroupID: &group.ID,
		Key:     "sk-usage-billing-sub-" + uuid.NewString(),
		Name:    "billing-sub",
	})
	subscription := mustCreateSubscription(t, client, &service.UserSubscription{
		UserID:  user.ID,
		GroupID: group.ID,
	})

	requestID := uuid.NewString()
	cmd := &service.UsageBillingCommand{
		RequestID:        requestID,
		APIKeyID:         apiKey.ID,
		UserID:           user.ID,
		AccountID:        0,
		SubscriptionID:   &subscription.ID,
		SubscriptionCost: 2.5,
	}

	result1, err := repo.Apply(ctx, cmd)
	require.NoError(t, err)
	require.True(t, result1.Applied)

	result2, err := repo.Apply(ctx, cmd)
	require.NoError(t, err)
	require.False(t, result2.Applied)

	var dailyUsage float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT daily_usage_usd FROM user_subscriptions WHERE id = $1", subscription.ID).Scan(&dailyUsage))
	require.InDelta(t, 2.5, dailyUsage, 0.000001)
}

func TestUsageBillingRepositoryApply_RequestFingerprintConflict(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)

	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("usage-billing-conflict-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      100,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID,
		Key:    "sk-usage-billing-conflict-" + uuid.NewString(),
		Name:   "billing-conflict",
	})

	requestID := uuid.NewString()
	_, err := repo.Apply(ctx, &service.UsageBillingCommand{
		RequestID:   requestID,
		APIKeyID:    apiKey.ID,
		UserID:      user.ID,
		BalanceCost: 1.25,
	})
	require.NoError(t, err)

	_, err = repo.Apply(ctx, &service.UsageBillingCommand{
		RequestID:   requestID,
		APIKeyID:    apiKey.ID,
		UserID:      user.ID,
		BalanceCost: 2.50,
	})
	require.ErrorIs(t, err, service.ErrUsageBillingRequestConflict)
}

func TestUsageBillingRepositoryApply_UpdatesAccountQuota(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)

	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("usage-billing-account-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID,
		Key:    "sk-usage-billing-account-" + uuid.NewString(),
		Name:   "billing-account",
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name: "usage-billing-account-quota-" + uuid.NewString(),
		Type: service.AccountTypeAPIKey,
		Extra: map[string]any{
			"quota_limit": 100.0,
		},
	})

	_, err := repo.Apply(ctx, &service.UsageBillingCommand{
		RequestID:        uuid.NewString(),
		APIKeyID:         apiKey.ID,
		UserID:           user.ID,
		AccountID:        account.ID,
		AccountType:      service.AccountTypeAPIKey,
		AccountQuotaCost: 3.5,
	})
	require.NoError(t, err)

	var quotaUsed float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COALESCE((extra->>'quota_used')::numeric, 0) FROM accounts WHERE id = $1", account.ID).Scan(&quotaUsed))
	require.InDelta(t, 3.5, quotaUsed, 0.000001)
}

func TestUsageBillingRepositoryApply_EnqueuesSchedulerOutboxOnQuotaCrossing(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)

	newFixture := func(t *testing.T, extra map[string]any) (int64, int64) {
		t.Helper()
		user := mustCreateUser(t, client, &service.User{
			Email:        fmt.Sprintf("usage-billing-outbox-user-%d-%s@example.com", time.Now().UnixNano(), uuid.NewString()),
			PasswordHash: "hash",
		})
		apiKey := mustCreateApiKey(t, client, &service.APIKey{
			UserID: user.ID,
			Key:    "sk-usage-billing-outbox-" + uuid.NewString(),
			Name:   "billing-outbox",
		})
		account := mustCreateAccount(t, client, &service.Account{
			Name:  "usage-billing-outbox-" + uuid.NewString(),
			Type:  service.AccountTypeAPIKey,
			Extra: extra,
		})
		return apiKey.ID, account.ID
	}

	outboxCountFor := func(t *testing.T, accountID int64) int {
		t.Helper()
		var count int
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM scheduler_outbox WHERE event_type = $1 AND account_id = $2",
			service.SchedulerOutboxEventAccountChanged, accountID,
		).Scan(&count))
		return count
	}

	t.Run("daily_first_crossing_enqueues", func(t *testing.T) {
		apiKeyID, accountID := newFixture(t, map[string]any{
			"quota_daily_limit": 10.0,
		})
		// 第一次低于日限额：不应入队 outbox
		_, err := repo.Apply(ctx, &service.UsageBillingCommand{
			RequestID:        uuid.NewString(),
			APIKeyID:         apiKeyID,
			AccountID:        accountID,
			AccountType:      service.AccountTypeAPIKey,
			AccountQuotaCost: 4,
		})
		require.NoError(t, err)
		require.Equal(t, 0, outboxCountFor(t, accountID), "below limit should not enqueue")

		// 第二次跨越日限额：应入队一次 outbox
		_, err = repo.Apply(ctx, &service.UsageBillingCommand{
			RequestID:        uuid.NewString(),
			APIKeyID:         apiKeyID,
			AccountID:        accountID,
			AccountType:      service.AccountTypeAPIKey,
			AccountQuotaCost: 8,
		})
		require.NoError(t, err)
		require.Equal(t, 1, outboxCountFor(t, accountID), "crossing daily limit should enqueue once")

		// 再次递增（已超）：不应重复入队
		_, err = repo.Apply(ctx, &service.UsageBillingCommand{
			RequestID:        uuid.NewString(),
			APIKeyID:         apiKeyID,
			AccountID:        accountID,
			AccountType:      service.AccountTypeAPIKey,
			AccountQuotaCost: 2,
		})
		require.NoError(t, err)
		require.Equal(t, 1, outboxCountFor(t, accountID), "subsequent increments beyond limit should not re-enqueue")
	})

	t.Run("weekly_first_crossing_enqueues", func(t *testing.T) {
		apiKeyID, accountID := newFixture(t, map[string]any{
			"quota_weekly_limit": 10.0,
		})
		_, err := repo.Apply(ctx, &service.UsageBillingCommand{
			RequestID:        uuid.NewString(),
			APIKeyID:         apiKeyID,
			AccountID:        accountID,
			AccountType:      service.AccountTypeAPIKey,
			AccountQuotaCost: 15, // 单次即跨越
		})
		require.NoError(t, err)
		require.Equal(t, 1, outboxCountFor(t, accountID), "single-shot crossing weekly limit should enqueue once")
	})
}

func TestDashboardAggregationRepositoryCleanupUsageBillingDedup_BatchDeletesOldRows(t *testing.T) {
	ctx := context.Background()
	repo := newDashboardAggregationRepositoryWithSQL(integrationDB)

	oldRequestID := "dedup-old-" + uuid.NewString()
	newRequestID := "dedup-new-" + uuid.NewString()
	oldCreatedAt := time.Now().UTC().AddDate(0, 0, -400)
	newCreatedAt := time.Now().UTC().Add(-time.Hour)

	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO usage_billing_dedup (request_id, api_key_id, request_fingerprint, created_at)
		VALUES ($1, 1, $2, $3), ($4, 1, $5, $6)
	`,
		oldRequestID, strings.Repeat("a", 64), oldCreatedAt,
		newRequestID, strings.Repeat("b", 64), newCreatedAt,
	)
	require.NoError(t, err)

	require.NoError(t, repo.CleanupUsageBillingDedup(ctx, time.Now().UTC().AddDate(0, 0, -365)))

	var oldCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id = $1", oldRequestID).Scan(&oldCount))
	require.Equal(t, 0, oldCount)

	var newCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id = $1", newRequestID).Scan(&newCount))
	require.Equal(t, 1, newCount)

	var archivedCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_billing_dedup_archive WHERE request_id = $1", oldRequestID).Scan(&archivedCount))
	require.Equal(t, 1, archivedCount)
}

func TestUsageBillingRepositoryApply_DeduplicatesAgainstArchivedKey(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)
	aggRepo := newDashboardAggregationRepositoryWithSQL(integrationDB)

	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("usage-billing-archive-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      100,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID,
		Key:    "sk-usage-billing-archive-" + uuid.NewString(),
		Name:   "billing-archive",
	})

	requestID := uuid.NewString()
	cmd := &service.UsageBillingCommand{
		RequestID:   requestID,
		APIKeyID:    apiKey.ID,
		UserID:      user.ID,
		BalanceCost: 1.25,
	}

	result1, err := repo.Apply(ctx, cmd)
	require.NoError(t, err)
	require.True(t, result1.Applied)

	_, err = integrationDB.ExecContext(ctx, `
		UPDATE usage_billing_dedup
		SET created_at = $1
		WHERE request_id = $2 AND api_key_id = $3
	`, time.Now().UTC().AddDate(0, 0, -400), requestID, apiKey.ID)
	require.NoError(t, err)
	require.NoError(t, aggRepo.CleanupUsageBillingDedup(ctx, time.Now().UTC().AddDate(0, 0, -365)))

	result2, err := repo.Apply(ctx, cmd)
	require.NoError(t, err)
	require.False(t, result2.Applied)

	var balance float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance FROM users WHERE id = $1", user.ID).Scan(&balance))
	require.InDelta(t, 98.75, balance, 0.000001)
}
