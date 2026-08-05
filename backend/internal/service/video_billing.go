package service

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	videoHoldRequestPrefix    = "video_hold:"
	videoCaptureRequestPrefix = "video_capture:"
	videoReleaseRequestPrefix = "video_release:"
)

var (
	ErrVideoFinalCostExceedsHold      = infraerrors.New(http.StatusConflict, "VIDEO_FINAL_COST_EXCEEDS_HOLD", "video final cost exceeds the held amount")
	ErrVideoInsufficientBalance       = infraerrors.New(http.StatusPaymentRequired, "VIDEO_INSUFFICIENT_BALANCE", "insufficient balance for video hold")
	ErrVideoSubscriptionQuotaExceeded = infraerrors.New(http.StatusPaymentRequired, "VIDEO_SUBSCRIPTION_QUOTA_EXCEEDED", "subscription quota is insufficient for video hold")
	ErrVideoBillingAlreadyFinalized   = infraerrors.New(http.StatusConflict, "VIDEO_BILLING_ALREADY_FINALIZED", "video billing hold was already captured or released")
	ErrVideoBillingUnavailable        = errors.New("video billing repository is not configured")
)

func VideoHoldRequestID(requestID string) string {
	return videoHoldRequestPrefix + strings.TrimSpace(requestID)
}

func VideoCaptureRequestID(requestID string) string {
	return videoCaptureRequestPrefix + strings.TrimSpace(requestID)
}

func VideoReleaseRequestID(requestID string) string {
	return videoReleaseRequestPrefix + strings.TrimSpace(requestID)
}

type VideoBillingService struct {
	repo UsageBillingRepository
}

func NewVideoBillingService(repo UsageBillingRepository) *VideoBillingService {
	return &VideoBillingService{repo: repo}
}

func (s *VideoBillingService) Reserve(ctx context.Context, task VideoTask) error {
	if s == nil || s.repo == nil {
		return ErrVideoBillingUnavailable
	}
	cmd, err := buildVideoHoldCommand(task, VideoHoldRequestID(task.RequestID), 0)
	if err != nil {
		return err
	}
	_, err = s.repo.ReserveVideo(ctx, cmd)
	return err
}

func (s *VideoBillingService) Capture(ctx context.Context, task VideoTask, actualAmount float64) error {
	if s == nil || s.repo == nil {
		return ErrVideoBillingUnavailable
	}
	if !validVideoBillingAmount(actualAmount) || actualAmount > task.FrozenAmount {
		return ErrVideoFinalCostExceedsHold
	}
	cmd, err := buildVideoHoldCommand(task, VideoCaptureRequestID(task.RequestID), actualAmount)
	if err != nil {
		return err
	}
	_, err = s.repo.CaptureVideo(ctx, cmd)
	return err
}

func (s *VideoBillingService) Release(ctx context.Context, task VideoTask) error {
	if s == nil || s.repo == nil {
		return ErrVideoBillingUnavailable
	}
	cmd, err := buildVideoHoldCommand(task, VideoReleaseRequestID(task.RequestID), 0)
	if err != nil {
		return err
	}
	_, err = s.repo.ReleaseVideo(ctx, cmd)
	return err
}

func (s *VideoBillingService) HandleTerminal(ctx context.Context, task VideoTask) error {
	switch task.Status {
	case VideoTaskUnknown:
		return nil
	case VideoTaskFailed, VideoTaskCancelled:
		return s.Release(ctx, task)
	case VideoTaskSucceeded:
		if task.SettledAmount == nil {
			return ErrVideoTaskInvalidRequest
		}
		return s.Capture(ctx, task, *task.SettledAmount)
	default:
		return ErrVideoTaskInvalidTransition
	}
}

func buildVideoHoldCommand(task VideoTask, requestID string, actualAmount float64) (*VideoHoldCommand, error) {
	if strings.TrimSpace(task.RequestID) == "" || task.UserID <= 0 || task.APIKeyID <= 0 ||
		!validVideoBillingAmount(task.FrozenAmount) || !validVideoBillingAmount(actualAmount) {
		return nil, ErrVideoTaskInvalidRequest
	}
	billingMode := strings.ToLower(strings.TrimSpace(task.BillingMode))
	if billingMode == "" {
		billingMode = "balance"
	}
	if billingMode != "balance" && billingMode != "subscription" {
		return nil, ErrVideoTaskInvalidRequest
	}
	if billingMode == "subscription" && (task.SubscriptionID == nil || *task.SubscriptionID <= 0) {
		return nil, ErrVideoTaskInvalidRequest
	}
	return &VideoHoldCommand{
		RequestID:          requestID,
		RequestPayloadHash: strings.TrimSpace(task.RequestHash),
		UserID:             task.UserID,
		APIKeyID:           task.APIKeyID,
		SubscriptionID:     task.SubscriptionID,
		VideoRequestID:     task.RequestID,
		BillingMode:        billingMode,
		HoldAmount:         task.FrozenAmount,
		ActualAmount:       actualAmount,
	}, nil
}

func validVideoBillingAmount(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
