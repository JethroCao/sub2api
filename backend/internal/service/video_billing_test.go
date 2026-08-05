package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVideoBillingLifecycleNeverCapturesAboveHold(t *testing.T) {
	repo := &fakeVideoBillingRepo{}
	svc := NewVideoBillingService(repo)
	task := VideoTask{RequestID: "vid_1", UserID: 9, APIKeyID: 4, FrozenAmount: 2.0, RequestHash: "hash"}
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

func TestVideoBillingFailedAndCancelledReleaseHold(t *testing.T) {
	for _, status := range []VideoTaskStatus{VideoTaskFailed, VideoTaskCancelled} {
		t.Run(string(status), func(t *testing.T) {
			repo := &fakeVideoBillingRepo{}
			svc := NewVideoBillingService(repo)
			task := VideoTask{
				RequestID:    "vid_terminal",
				UserID:       9,
				APIKeyID:     4,
				FrozenAmount: 2,
				RequestHash:  "hash",
				Status:       status,
			}
			require.NoError(t, svc.HandleTerminal(context.Background(), task))
			require.Equal(t, []string{"video_release:vid_terminal"}, repo.requestIDs)
		})
	}
}

func TestVideoBillingSubscriptionQuotaLifecycle(t *testing.T) {
	repo := &fakeVideoBillingRepo{}
	svc := NewVideoBillingService(repo)
	subscriptionID := int64(31)
	task := VideoTask{
		RequestID:      "vid_sub",
		UserID:         9,
		APIKeyID:       4,
		SubscriptionID: &subscriptionID,
		FrozenAmount:   2.0,
		RequestHash:    "hash",
		BillingMode:    "subscription",
	}
	require.NoError(t, svc.Reserve(context.Background(), task))
	require.NoError(t, svc.Capture(context.Background(), task, 1.25))
	require.Equal(t, []string{"video_hold:vid_sub", "video_capture:vid_sub"}, repo.requestIDs)
	require.Equal(t, []int64{31, 31}, repo.subscriptionIDs)
}

type fakeVideoBillingRepo struct {
	requestIDs      []string
	subscriptionIDs []int64
}

func (r *fakeVideoBillingRepo) Apply(context.Context, *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	return &UsageBillingApplyResult{}, nil
}

func (r *fakeVideoBillingRepo) ReserveBatchImageBalance(context.Context, *BatchImageBalanceHoldCommand) (*BatchImageBalanceHoldResult, error) {
	return &BatchImageBalanceHoldResult{}, nil
}

func (r *fakeVideoBillingRepo) CaptureBatchImageBalance(context.Context, *BatchImageBalanceHoldCommand) (*BatchImageBalanceHoldResult, error) {
	return &BatchImageBalanceHoldResult{}, nil
}

func (r *fakeVideoBillingRepo) ReleaseBatchImageBalance(context.Context, *BatchImageBalanceHoldCommand) (*BatchImageBalanceHoldResult, error) {
	return &BatchImageBalanceHoldResult{}, nil
}

func (r *fakeVideoBillingRepo) ReserveVideo(_ context.Context, cmd *VideoHoldCommand) (*VideoHoldResult, error) {
	r.record(cmd)
	return &VideoHoldResult{Applied: true}, nil
}

func (r *fakeVideoBillingRepo) CaptureVideo(_ context.Context, cmd *VideoHoldCommand) (*VideoHoldResult, error) {
	r.record(cmd)
	return &VideoHoldResult{Applied: true}, nil
}

func (r *fakeVideoBillingRepo) ReleaseVideo(_ context.Context, cmd *VideoHoldCommand) (*VideoHoldResult, error) {
	r.record(cmd)
	return &VideoHoldResult{Applied: true}, nil
}

func (r *fakeVideoBillingRepo) record(cmd *VideoHoldCommand) {
	r.requestIDs = append(r.requestIDs, cmd.RequestID)
	if cmd.SubscriptionID != nil {
		r.subscriptionIDs = append(r.subscriptionIDs, *cmd.SubscriptionID)
	}
}

var _ UsageBillingRepository = (*fakeVideoBillingRepo)(nil)
