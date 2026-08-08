package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type adminVideoRepositoryStub struct {
	task           *VideoTask
	replaced       []VideoPricingRuleInput
	refundCalls    int
	completeCalls  int
	reconcileCalls int
	lastRefund     AdminVideoRefundMutation
	lastComplete   AdminVideoCompleteMutation
	lastReconcile  AdminVideoReconcileMutation
}

var adminVideoTestURLHashKey = AdminVideoURLHashKey("0123456789abcdef0123456789abcdef")

func (r *adminVideoRepositoryStub) ListPricingRules(context.Context, int64) ([]VideoPricingRule, error) {
	return nil, nil
}

func (r *adminVideoRepositoryStub) ReplacePricingRules(_ context.Context, _ int64, rules []VideoPricingRuleInput) ([]VideoPricingRule, error) {
	r.replaced = append([]VideoPricingRuleInput(nil), rules...)
	return nil, nil
}

func (r *adminVideoRepositoryStub) ListTasks(context.Context, VideoTaskListQuery) ([]VideoTask, int, error) {
	return nil, 0, nil
}

func (r *adminVideoRepositoryStub) GetTask(context.Context, string) (*AdminVideoTaskDetail, error) {
	return &AdminVideoTaskDetail{Task: *r.task}, nil
}

func (r *adminVideoRepositoryStub) ReplayAction(context.Context, string, string, AdminVideoActionMetadata) (*AdminVideoActionResult, bool, error) {
	return nil, false, nil
}

func (r *adminVideoRepositoryStub) Reconcile(_ context.Context, command AdminVideoReconcileMutation) (*AdminVideoActionResult, error) {
	r.reconcileCalls++
	r.lastReconcile = command
	return &AdminVideoActionResult{Task: *r.task}, nil
}

func (r *adminVideoRepositoryStub) Refund(_ context.Context, command AdminVideoRefundMutation) (*AdminVideoActionResult, error) {
	r.refundCalls++
	r.lastRefund = command
	return &AdminVideoActionResult{Task: *r.task}, nil
}

func (r *adminVideoRepositoryStub) Complete(_ context.Context, command AdminVideoCompleteMutation) (*AdminVideoActionResult, error) {
	r.completeCalls++
	r.lastComplete = command
	return &AdminVideoActionResult{Task: *r.task}, nil
}

func TestAdminVideoPricingRejectsOverlapMissingCoverageAndNonFinitePrices(t *testing.T) {
	repo := &adminVideoRepositoryStub{}
	catalog := VideoCapabilityCatalog{
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-2.0"): {
			VideoOperationGeneration: {Text: true, Audio: true, Resolutions: []string{"720p", "1080p"}},
		},
	}
	service := NewAdminVideoService(repo, catalog, nil, adminVideoTestURLHashKey)

	tests := []struct {
		name  string
		rules []VideoPricingRuleInput
		want  error
	}{
		{
			name: "duplicate dimensions overlap",
			rules: []VideoPricingRuleInput{
				{ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_request", UnitPrice: 1, Enabled: true},
				{ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_request", UnitPrice: 2, Enabled: true},
			},
			want: ErrVideoPricingRuleOverlap,
		},
		{
			name: "missing wildcard coverage",
			rules: []VideoPricingRuleInput{
				{ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "720p", AudioMode: "any", Unit: "per_request", UnitPrice: 1, Enabled: true},
			},
			want: ErrVideoPricingCoverage,
		},
		{
			name: "unknown dormant kling capability",
			rules: []VideoPricingRuleInput{
				{ExternalModel: "kling-3.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_request", UnitPrice: 1, Enabled: true},
			},
			want: ErrVideoPricingCoverage,
		},
		{
			name: "nan price",
			rules: []VideoPricingRuleInput{
				{ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any", Unit: "per_request", UnitPrice: math.NaN(), Enabled: true},
			},
			want: ErrVideoPricingRuleInvalid,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := service.ReplacePricingRules(context.Background(), 7, test.rules)
			require.ErrorIs(t, err, test.want)
			require.Empty(t, repo.replaced)
		})
	}
}

func TestAdminVideoPricingAcceptsAuthoritativeCoveredRule(t *testing.T) {
	repo := &adminVideoRepositoryStub{}
	catalog := VideoCapabilityCatalog{
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-2.0"): {
			VideoOperationGeneration: {Text: true, Audio: true},
		},
	}
	service := NewAdminVideoService(repo, catalog, nil, adminVideoTestURLHashKey)
	rules := []VideoPricingRuleInput{{
		ExternalModel: "seedance-2.0", Operation: "generation", Resolution: "*", AudioMode: "any",
		Unit: "per_output_second", UnitPrice: 0.25, Enabled: true,
	}}
	require.NoError(t, service.ReplacePricingRules(context.Background(), 7, rules))
	require.Equal(t, rules, repo.replaced)
}

func TestAdminVideoRefundRequiresUnknownOrFailedUnsettledHeldTask(t *testing.T) {
	tests := []VideoTask{
		{RequestID: "vid_00000000000000000000000000000001", Status: VideoTaskSucceeded, BillingStatus: "captured"},
		{RequestID: "vid_00000000000000000000000000000001", Status: VideoTaskFailed, BillingStatus: "released", SettledAt: timePtr(time.Now())},
		{RequestID: "vid_00000000000000000000000000000001", Status: VideoTaskRunning, BillingStatus: "held"},
	}
	for _, task := range tests {
		repo := &adminVideoRepositoryStub{task: &task}
		service := NewAdminVideoService(repo, nil, nil, adminVideoTestURLHashKey)
		_, err := service.Refund(context.Background(), task.RequestID, AdminVideoRefundCommand{
			ActorUserID: 1, Reason: "operator confirmed no upstream job", IdempotencyKey: "refund-1",
		})
		require.ErrorIs(t, err, ErrVideoFinancialStateConflict)
		require.Zero(t, repo.refundCalls)
	}
}

func TestAdminVideoRefundHashesIdempotencyAndNeverPersistsRawKey(t *testing.T) {
	task := VideoTask{RequestID: "vid_00000000000000000000000000000001", Status: VideoTaskUnknown, BillingStatus: "held"}
	repo := &adminVideoRepositoryStub{task: &task}
	service := NewAdminVideoService(repo, nil, nil, adminVideoTestURLHashKey)
	_, err := service.Refund(context.Background(), task.RequestID, AdminVideoRefundCommand{
		ActorUserID: 7, Reason: "provider confirmed missing", IdempotencyKey: "raw-secret-key",
	})
	require.NoError(t, err)
	require.Equal(t, 1, repo.refundCalls)
	require.Equal(t, HashIdempotencyKey("raw-secret-key"), repo.lastRefund.IdempotencyKeyHash)
	require.NotContains(t, repo.lastRefund.RequestHash, "raw-secret-key")
}

func TestAdminVideoRefundRejectsCredentialShapedAuditReason(t *testing.T) {
	task := VideoTask{RequestID: "vid_00000000000000000000000000000001", Status: VideoTaskUnknown, BillingStatus: "held"}
	repo := &adminVideoRepositoryStub{task: &task}
	admin := NewAdminVideoService(repo, nil, nil, adminVideoTestURLHashKey)
	_, err := admin.Refund(context.Background(), task.RequestID, AdminVideoRefundCommand{
		ActorUserID: 7, Reason: "api_key=sk-proj-this-is-a-secret-value", IdempotencyKey: "refund-1",
	})
	require.ErrorIs(t, err, ErrVideoTaskInvalidRequest)
	require.Zero(t, repo.refundCalls)
}

func TestAdminVideoCompleteRequiresSafeResultAndStoredHoldCap(t *testing.T) {
	task := VideoTask{
		RequestID: "vid_00000000000000000000000000000001", Status: VideoTaskUnknown,
		BillingStatus: "held", FrozenAmount: 2, PricingUnit: "per_output_second", UnitPrice: 0.25,
	}
	repo := &adminVideoRepositoryStub{task: &task}
	validator := AdminVideoResultURLValidatorFunc(func(_ context.Context, raw string) (string, string, error) {
		if raw != "https://cdn.example.com/video.mp4?token=secret" {
			return "", "", ErrVideoResultURLInvalid
		}
		return raw, "https://cdn.example.com/video-result", nil
	})
	service := NewAdminVideoService(repo, nil, validator, adminVideoTestURLHashKey)

	_, err := service.Complete(context.Background(), task.RequestID, AdminVideoCompleteCommand{
		ActorUserID: 7, Reason: "verified in provider console", IdempotencyKey: "complete-1",
		ProviderTaskID: "provider-task", ResultURL: "https://cdn.example.com/video.mp4?token=secret",
		DurationSeconds: 6, Resolution: "720p", FinalAmount: 2.01,
	})
	require.ErrorIs(t, err, ErrVideoFinalCostExceedsHold)
	require.Zero(t, repo.completeCalls)

	_, err = service.Complete(context.Background(), task.RequestID, AdminVideoCompleteCommand{
		ActorUserID: 7, Reason: "verified in provider console", IdempotencyKey: "complete-1",
		ProviderTaskID: "provider-task", ResultURL: "https://cdn.example.com/video.mp4?token=secret",
		DurationSeconds: 6, Resolution: "720p", FinalAmount: 1.5,
	})
	require.NoError(t, err)
	require.Equal(t, 1, repo.completeCalls)
	require.Equal(t, "https://cdn.example.com/video-result", repo.lastComplete.ResultURLAuditSummary)
	require.NotContains(t, repo.lastComplete.ResultURLAuditSummary, "token")
	require.Equal(t, task.UnitPrice, repo.lastComplete.StoredUnitPrice)
}

func TestAdminVideoReconcileDelegatesClockAndReplayPolicyToRepository(t *testing.T) {
	now := time.Now().UTC()
	for _, task := range []VideoTask{
		{RequestID: "vid_00000000000000000000000000000001", Status: VideoTaskSucceeded},
		{RequestID: "vid_00000000000000000000000000000001", Status: VideoTaskRunning, LeaseExpiresAt: timePtr(now.Add(time.Minute))},
	} {
		repo := &adminVideoRepositoryStub{task: &task}
		service := NewAdminVideoService(repo, nil, nil, adminVideoTestURLHashKey)
		_, err := service.Reconcile(context.Background(), task.RequestID, AdminVideoReconcileCommand{
			ActorUserID: 7, Reason: "stuck task", IdempotencyKey: "reconcile-1", Now: now,
		})
		require.NoError(t, err)
		require.Equal(t, 1, repo.reconcileCalls)
	}
}

func TestAdminVideoCompleteHashBindsFullNormalizedURLWithoutPersistingIt(t *testing.T) {
	task := VideoTask{RequestID: "vid_00000000000000000000000000000001", Status: VideoTaskUnknown, BillingStatus: "held", FrozenAmount: 2, UnitPrice: 0.25}
	repo := &adminVideoRepositoryStub{task: &task}
	validator := AdminVideoResultURLValidatorFunc(func(_ context.Context, raw string) (string, string, error) {
		return raw, "https://cdn.example.com/private/video.mp4?token=unsafe-summary", nil
	})
	admin := NewAdminVideoService(repo, nil, validator, adminVideoTestURLHashKey)
	command := AdminVideoCompleteCommand{
		ActorUserID: 7, Reason: "verified", IdempotencyKey: "complete-1", ProviderTaskID: "provider-task",
		ResultURL: "https://cdn.example.com/private/video.mp4?token=first-secret", DurationSeconds: 6, Resolution: "720p", FinalAmount: 1.5,
	}
	_, err := admin.Complete(context.Background(), task.RequestID, command)
	require.NoError(t, err)
	require.Equal(t, "https://cdn.example.com/video-result", repo.lastComplete.ResultURLAuditSummary)
	firstHash := repo.lastComplete.RequestHash
	command.ResultURL = "https://cdn.example.com/private/video.mp4?token=second-secret"
	_, err = admin.Complete(context.Background(), task.RequestID, command)
	require.NoError(t, err)
	require.NotEqual(t, firstHash, repo.lastComplete.RequestHash)
	require.NotContains(t, firstHash, "first-secret")
}

func TestSafeAdminVideoResultURLSummaryOmitsUserinfoPathQueryAndFragment(t *testing.T) {
	require.Empty(t, SafeAdminVideoResultURLSummary("https://user:password@cdn.example.com/video.mp4"))
	require.Equal(t, "https://cdn.example.com/video-result",
		SafeAdminVideoResultURLSummary("https://cdn.example.com/private/customer/token/video.mp4?sig=secret#fragment"))
}

func timePtr(value time.Time) *time.Time { return &value }
