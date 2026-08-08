package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestVideoSubmitDisabledProviderStopsBeforeQuoteReserveScheduleOrUpstream(t *testing.T) {
	tests := []struct {
		name     string
		platform string
		provider string
	}{
		{name: "grok", platform: PlatformGrok, provider: PlatformGrok},
		{name: "seedance", platform: PlatformVideo, provider: VideoProviderSeedance},
		{name: "kling", platform: PlatformVideo, provider: VideoProviderKling},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := newVideoSubmitHarness(t)
			deps.service.videoConfig = config.VideoConfig{Enabled: true}
			command := deps.command()
			command.Platform = tt.platform
			command.Provider = tt.provider
			command.Group.Platform = tt.platform

			_, err := deps.service.Submit(context.Background(), command)

			require.ErrorIs(t, err, ErrVideoProviderDisabled)
			require.Zero(t, deps.pricing.calls)
			require.Zero(t, deps.submissions.reserveCalls)
			require.Zero(t, deps.scheduler.selectCalls)
			require.Zero(t, deps.provider.submitCalls)
			require.Empty(t, deps.order)
		})
	}
}

func TestVideoSubmitRejectsUnsupportedBeforeHold(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.capabilities.err = ErrVideoUnsupportedCapability

	_, err := deps.service.Submit(context.Background(), deps.command())

	require.ErrorIs(t, err, ErrVideoUnsupportedCapability)
	require.Zero(t, deps.submissions.reserveCalls)
	require.Zero(t, deps.scheduler.selectCalls)
	require.Zero(t, deps.provider.submitCalls)
	require.Equal(t, []string{"validate"}, deps.order)
}

func TestVideoSubmitSkipsAccountWithAdministratorDisabledCapability(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.account.Extra[VideoDisabledCapabilitiesExtraKey] = []any{"text"}

	_, err := deps.service.Submit(context.Background(), deps.command())

	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Equal(t, 1, deps.pricing.calls)
	require.Equal(t, 1, deps.submissions.reserveCalls)
	require.Equal(t, 1, deps.billing.releaseCalls)
	require.Zero(t, deps.provider.submitCalls)
}

func TestVideoSubmitRejectsVideoAccountWithoutExactModelMapping(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mapping any
	}{
		{name: "missing"},
		{name: "wildcard only", mapping: map[string]any{"seedance-*": "endpoint-id"}},
		{name: "different exact model", mapping: map[string]any{"seedance-1.0": "endpoint-id"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deps := newVideoSubmitHarness(t)
			delete(deps.account.Credentials, "model_mapping")
			if tt.mapping != nil {
				deps.account.Extra[VideoModelMappingExtraKey] = tt.mapping
			}

			_, err := deps.service.Submit(context.Background(), deps.command())

			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Equal(t, 1, deps.submissions.reserveCalls)
			require.Equal(t, 1, deps.billing.releaseCalls)
			require.Zero(t, deps.provider.submitCalls)
		})
	}
}

func TestVideoSubmitRejectsAccountWithCorruptPersistedDisabledCapabilities(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.account.Extra[VideoDisabledCapabilitiesExtraKey] = []any{"telepathy"}

	_, err := deps.service.Submit(context.Background(), deps.command())

	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Equal(t, 1, deps.submissions.reserveCalls)
	require.Equal(t, 1, deps.billing.releaseCalls)
	require.Zero(t, deps.provider.submitCalls)
}

func TestVideoSubmitAcceptsDurableGrokWhenEnabled(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.service.videoConfig.GrokEnabled = true
	command := deps.command()
	command.Group.Platform = PlatformGrok
	command.Platform = PlatformGrok
	command.Provider = PlatformGrok
	command.Request.Model = "grok-imagine-video"
	deps.pricing.quote.ExternalModel = command.Request.Model
	deps.account.Platform = PlatformGrok
	deps.account.Extra = nil
	deps.account.Credentials = map[string]any{
		"api_key":       "provider-secret-never-persisted",
		"model_mapping": map[string]any{"grok-imagine-video": "grok-imagine-video"},
	}
	deps.capabilities.catalog[VideoModelCapabilityKey(PlatformGrok, command.Request.Model)] = VideoProviderCapabilities{
		VideoOperationGeneration: {Text: true},
	}
	grokProvider := &videoSubmitProvider{harness: deps, name: PlatformGrok, submitResult: VideoSubmitResult{
		UpstreamTaskID: "grok-upstream-task", Status: VideoTaskQueued, UpstreamStatus: "queued",
	}}
	registry, err := NewVideoProviderRegistry(deps.provider, grokProvider)
	require.NoError(t, err)
	deps.service.providers = registry

	task, err := deps.service.Submit(context.Background(), command)

	require.NoError(t, err)
	require.Equal(t, PlatformGrok, task.Platform)
	require.Equal(t, PlatformGrok, task.Provider)
	require.Equal(t, "grok-upstream-task", *task.UpstreamTaskID)
	require.Equal(t, 1, deps.pricing.calls)
	require.Equal(t, 1, deps.submissions.reserveCalls)
	require.Equal(t, 1, deps.scheduler.selectCalls)
	require.Equal(t, 1, grokProvider.submitCalls)
	require.Zero(t, deps.provider.submitCalls)
}

func TestVideoSubmitRejectsPermissionAndInputBeforeQuoteOrHold(t *testing.T) {
	t.Run("permission", func(t *testing.T) {
		deps := newVideoSubmitHarness(t)
		command := deps.command()
		command.Group.AllowVideoGeneration = false

		_, err := deps.service.Submit(context.Background(), command)

		require.ErrorIs(t, err, ErrVideoGenerationNotAllowed)
		require.Zero(t, deps.pricing.calls)
		require.Zero(t, deps.submissions.reserveCalls)
		require.Zero(t, deps.scheduler.selectCalls)
	})

	t.Run("canonical input", func(t *testing.T) {
		deps := newVideoSubmitHarness(t)
		command := deps.command()
		command.Request.Model = ""

		_, err := deps.service.Submit(context.Background(), command)

		require.ErrorIs(t, err, ErrVideoInvalidRequest)
		require.Zero(t, deps.pricing.calls)
		require.Zero(t, deps.submissions.reserveCalls)
		require.Zero(t, deps.scheduler.selectCalls)
	})
}

func TestVideoSubmitQuoteMustSucceedBeforeAtomicReserve(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.pricing.err = ErrVideoPricingUnavailable

	_, err := deps.service.Submit(context.Background(), deps.command())

	require.ErrorIs(t, err, ErrVideoPricingUnavailable)
	require.Equal(t, 1, deps.pricing.calls)
	require.Zero(t, deps.submissions.reserveCalls)
	require.Zero(t, deps.scheduler.selectCalls)
	require.Equal(t, []string{"validate", "quote"}, deps.order)
}

func TestVideoSubmitMaintainsSubscriptionWindowsBeforeAtomicReserve(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	command := deps.command()
	command.BillingMode = "subscription"
	command.Subscription = &UserSubscription{
		ID:        81,
		UserID:    command.UserID,
		GroupID:   command.Group.ID,
		StartsAt:  deps.now.Add(-72 * time.Hour),
		ExpiresAt: deps.now.Add(72 * time.Hour),
		Status:    SubscriptionStatusActive,
	}
	deps.subscriptions.refreshed = command.Subscription

	_, err := deps.service.Submit(context.Background(), command)

	require.NoError(t, err)
	require.Equal(t, 1, deps.subscriptions.calls)
	require.Equal(t, []string{"validate", "quote", "maintain", "reserve", "select", "validate", "mark_submitting", "submit", "mark_submitted"}, deps.order)
	require.Equal(t, int64(81), *deps.submissions.lastParams.SubscriptionID)
}

func TestVideoSubmitReplayReturnsSameTaskWithoutSecondChargeOrSideEffects(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	first, err := deps.service.Submit(context.Background(), deps.commandWithKey("same-key"))
	require.NoError(t, err)

	second, err := deps.service.Submit(context.Background(), deps.commandWithKey("same-key"))

	require.NoError(t, err)
	require.Equal(t, first.RequestID, second.RequestID)
	require.Equal(t, 2, deps.submissions.createCalls, "the atomic repository is also the replay lookup")
	require.Equal(t, 1, deps.submissions.reserveCalls, "the replay must not create a second hold")
	require.Equal(t, 1, deps.scheduler.selectCalls)
	require.Equal(t, 1, deps.provider.submitCalls)
	require.Equal(t, 1, deps.scheduler.releaseCalls)
}

func TestVideoSubmitReplayComparesCanonicalRequestHash(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	first := deps.commandWithKey("same-key")
	_, err := deps.service.Submit(context.Background(), first)
	require.NoError(t, err)

	second := deps.commandWithKey("same-key")
	second.Request.Prompt = "different canonical request"
	deps.submissions.skipReplayHashCheck = true
	_, err = deps.service.Submit(context.Background(), second)

	require.ErrorIs(t, err, ErrVideoIdempotencyConflict)
	require.Equal(t, 1, deps.submissions.reserveCalls)
	require.Equal(t, 1, deps.scheduler.selectCalls)
	require.Equal(t, 1, deps.provider.submitCalls)
}

func TestVideoSubmitIdempotencyKeyCannotCrossOperations(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	first := deps.commandWithKey("one-caller-key")
	_, err := deps.service.Submit(context.Background(), first)
	require.NoError(t, err)

	second := deps.commandWithKey("one-caller-key")
	second.Request.Operation = VideoOperationExtension
	second.Request.ReferenceVideos = []VideoAsset{{URL: "https://example.com/source.mp4"}}
	deps.capabilities.catalog[VideoModelCapabilityKey(VideoProviderSeedance, second.Request.Model)][VideoOperationExtension] = VideoCapability{Extension: true}
	deps.pricing.quote.Operation = string(VideoOperationExtension)
	_, err = deps.service.Submit(context.Background(), second)

	require.ErrorIs(t, err, ErrVideoIdempotencyConflict)
	require.Equal(t, 1, deps.submissions.reserveCalls)
	require.Equal(t, 1, deps.scheduler.selectCalls)
	require.Equal(t, 1, deps.provider.submitCalls)
}

func TestVideoSubmitCanonicalizesProviderOptionsForReplayHash(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	first := deps.commandWithKey("same-options")
	first.Request.ProviderOptions = map[string]json.RawMessage{
		VideoProviderSeedance: json.RawMessage(`{"watermark":true,"seed":42}`),
	}
	_, err := deps.service.Submit(context.Background(), first)
	require.NoError(t, err)

	second := deps.commandWithKey("same-options")
	second.Request.ProviderOptions = map[string]json.RawMessage{
		VideoProviderSeedance: json.RawMessage(`{ "seed" : 42, "watermark" : true }`),
	}
	task, err := deps.service.Submit(context.Background(), second)

	require.NoError(t, err)
	require.NotNil(t, task)
	require.Equal(t, 1, deps.submissions.reserveCalls)
	require.Equal(t, 1, deps.scheduler.selectCalls)
	require.Equal(t, 1, deps.provider.submitCalls)
}

func TestVideoSubmitHashesRawIdempotencyKeyAndPersistsOnlyMinimizedRecoveryData(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	command := deps.commandWithKey("  key-with-significant-space  ")
	command.Request.FirstFrame = []VideoAsset{{URL: "data:image/png;base64,iVBORw0KGgo="}}
	command.Request.ProviderOptions = map[string]json.RawMessage{
		VideoProviderSeedance: json.RawMessage(`{"seed":42,"watermark":true}`),
	}

	_, err := deps.service.Submit(context.Background(), command)

	require.NoError(t, err)
	rawSum := sha256.Sum256([]byte(command.IdempotencyKey))
	require.Equal(t, hex.EncodeToString(rawSum[:]), deps.submissions.lastParams.IdempotencyKeyHash)
	payload := string(deps.submissions.lastParams.RequestPayload.Bytes())
	require.NotContains(t, payload, command.IdempotencyKey)
	require.NotContains(t, payload, "data:image")
	require.NotContains(t, payload, "iVBOR")
	require.JSONEq(t, `{"prompt":"animate waves","duration_seconds":6,"resolution":"720p","aspect_ratio":"16:9","seed":42,"watermark":true}`, payload)
}

func TestVideoSubmitAmbiguousKeepsHoldAndQueuesRecovery(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.provider.submitErr = VideoProviderError{Code: "upstream_timeout", Retryable: true, Ambiguous: true}

	task, err := deps.service.Submit(context.Background(), deps.command())

	require.NoError(t, err)
	require.Equal(t, VideoTaskUnknown, task.Status)
	require.Equal(t, 1, deps.submissions.reserveCalls)
	require.Zero(t, deps.billing.releaseCalls)
	require.NotNil(t, task.ProviderSubmissionToken)
	require.NotEmpty(t, *task.ProviderSubmissionToken)
	require.NotNil(t, task.NextPollAt)
	require.Equal(t, deps.now.Add(videoSubmissionRecoveryDelay), *task.NextPollAt)
	require.NotEmpty(t, task.RequestPayload, "unknown submissions retain only minimized recovery data")
	require.Equal(t, 1, deps.scheduler.releaseCalls)
	require.Equal(t, 1, deps.tasks.unknownCalls)
}

func TestVideoSubmitAcceptedButPersistenceAmbiguousQueuesRecovery(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.tasks.markSubmittedErr = errors.New("commit outcome unknown")

	task, err := deps.service.Submit(context.Background(), deps.command())

	require.NoError(t, err)
	require.Equal(t, VideoTaskUnknown, task.Status)
	require.Equal(t, 1, deps.provider.submitCalls)
	require.Zero(t, deps.billing.releaseCalls)
	require.Equal(t, 1, deps.tasks.unknownCalls)
	require.Equal(t, 1, deps.scheduler.releaseCalls)
}

func TestVideoSubmitAcceptedAndCommittedDespitePersistenceErrorReturnsStoredTask(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.tasks.markSubmittedErr = errors.New("commit acknowledgement lost")
	deps.tasks.markSubmittedCommits = true

	task, err := deps.service.Submit(context.Background(), deps.command())

	require.NoError(t, err)
	require.Equal(t, VideoTaskSubmitted, task.Status)
	require.Equal(t, "upstream-task-1", videoSubmitStringValue(task.UpstreamTaskID))
	require.Zero(t, deps.tasks.unknownCalls)
	require.Zero(t, deps.billing.releaseCalls)
}

func TestVideoSubmitAssignmentCommitAcknowledgementLossDoesNotResubmitOrRelease(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.tasks.assignCommits = true
	deps.tasks.assignErr = errors.New("assignment commit acknowledgement lost")

	task, err := deps.service.Submit(context.Background(), deps.command())

	require.NoError(t, err)
	require.Equal(t, VideoTaskSubmitted, task.Status)
	require.Equal(t, 1, deps.tasks.assignCalls)
	require.Equal(t, 1, deps.provider.submitCalls)
	require.Zero(t, deps.billing.releaseCalls)
}

func TestVideoSubmitAssignmentFailureTerminalizesPendingHoldAtomically(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.tasks.assignErr = errors.New("assignment rejected before commit")

	_, err := deps.service.Submit(context.Background(), deps.command())

	require.ErrorContains(t, err, "assignment rejected before commit")
	require.Equal(t, 1, deps.tasks.failedCalls)
	require.Equal(t, 1, deps.billing.releaseCalls)
	require.Zero(t, deps.provider.submitCalls)
	stored := deps.tasks.byRequestID("vid_00000000000000000000000000000002")
	require.NotNil(t, stored)
	require.Equal(t, VideoTaskFailed, stored.Status)
	require.Equal(t, "released", stored.BillingStatus)
}

func TestVideoSubmitUncertainAssignmentOutcomeRetainsScheduledHold(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.tasks.assignErr = errors.New("assignment outcome unknown")
	deps.tasks.getErr = errors.New("confirmation read unavailable")

	_, err := deps.service.Submit(context.Background(), deps.command())

	require.ErrorContains(t, err, "assignment outcome unknown")
	require.ErrorContains(t, err, "confirmation read unavailable")
	require.Zero(t, deps.billing.releaseCalls)
	require.Zero(t, deps.tasks.failedCalls)
	require.Zero(t, deps.provider.submitCalls)
	stored := deps.tasks.byRequestID("vid_00000000000000000000000000000002")
	require.Equal(t, VideoTaskCreated, stored.Status)
	require.NotNil(t, stored.NextPollAt)
}

func TestVideoSubmitAssignmentErrorNeverReleasesDifferentDurableAssignment(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.tasks.assignErr = errors.New("assignment outcome unknown")
	deps.tasks.assignAfter = func() {
		stored := deps.tasks.byRequestID("vid_00000000000000000000000000000002")
		stored.AccountID = 999
		stored.UpstreamModel = "different-route"
		stored.Status = VideoTaskSubmitting
		stored.ProviderSubmissionToken = videoSubmitStringPtr("different-token")
		stored.Version++
	}

	_, err := deps.service.Submit(context.Background(), deps.command())

	require.ErrorIs(t, err, ErrVideoTaskVersionConflict)
	require.Zero(t, deps.billing.releaseCalls)
	require.Zero(t, deps.tasks.failedCalls)
	require.Zero(t, deps.provider.submitCalls)
	require.NotNil(t, deps.tasks.byRequestID("vid_00000000000000000000000000000002").NextPollAt)
}

func TestVideoSubmitTerminalizationCommitAcknowledgementLossIsReconciled(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.provider.submitErr = VideoProviderError{Code: "invalid_request", Retryable: false, Ambiguous: false}
	deps.tasks.terminalCommits = true
	deps.tasks.terminalErr = errors.New("terminal commit acknowledgement lost")

	_, err := deps.service.Submit(context.Background(), deps.command())

	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.NotContains(t, err.Error(), "terminal commit acknowledgement lost")
	require.Equal(t, 1, deps.billing.releaseCalls)
	require.Equal(t, VideoTaskFailed, deps.tasks.byRequestID("vid_00000000000000000000000000000002").Status)
}

func TestVideoSubmitTerminalizationFailureRollsBackRelease(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.provider.submitErr = VideoProviderError{Code: "invalid_request", Retryable: false, Ambiguous: false}
	deps.tasks.terminalErr = errors.New("terminal mutation failed")

	_, err := deps.service.Submit(context.Background(), deps.command())

	require.ErrorContains(t, err, "terminal mutation failed")
	require.Zero(t, deps.billing.releaseCalls)
	require.Equal(t, VideoTaskSubmitting, deps.tasks.byRequestID("vid_00000000000000000000000000000002").Status)
}

func TestVideoSubmitCancellationAfterAssignmentTerminalizesWithoutUpstreamSubmit(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	deps.tasks.assignAfter = cancel

	_, err := deps.service.Submit(ctx, deps.command())

	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, deps.provider.submitCalls)
	require.Equal(t, 1, deps.billing.releaseCalls)
	require.Equal(t, VideoTaskFailed, deps.tasks.byRequestID("vid_00000000000000000000000000000002").Status)
}

func TestVideoSubmitCancellationBeforeReserveCreatesNoHold(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := deps.service.Submit(ctx, deps.command())

	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, deps.submissions.reserveCalls)
	require.Zero(t, deps.billing.releaseCalls)
	require.Zero(t, deps.provider.submitCalls)
}

func TestVideoSubmitCancellationAfterReserveTerminalizesPendingTask(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	deps.submissions.reserveAfter = cancel

	_, err := deps.service.Submit(ctx, deps.command())

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, deps.submissions.reserveCalls)
	require.Equal(t, 1, deps.billing.releaseCalls)
	require.Zero(t, deps.provider.submitCalls)
	require.Equal(t, VideoTaskFailed, deps.tasks.byRequestID("vid_00000000000000000000000000000002").Status)
}

func TestVideoSubmitCancellationDuringUpstreamSubmitQueuesRecovery(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.provider.submitErr = context.Canceled

	task, err := deps.service.Submit(context.Background(), deps.command())

	require.NoError(t, err)
	require.Equal(t, VideoTaskUnknown, task.Status)
	require.Zero(t, deps.billing.releaseCalls)
	require.Equal(t, 1, deps.provider.submitCalls)
	require.Equal(t, 1, deps.tasks.unknownCalls)
}

func TestVideoSubmitCancellationAfterUpstreamResponseQueuesRecovery(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	deps.provider.submitAfter = cancel

	task, err := deps.service.Submit(ctx, deps.command())

	require.NoError(t, err)
	require.Equal(t, VideoTaskUnknown, task.Status)
	require.Zero(t, deps.billing.releaseCalls)
	require.Equal(t, 1, deps.provider.submitCalls)
	require.Equal(t, 1, deps.tasks.unknownCalls)
}

func TestVideoSubmitUnclassifiedUpstreamFailureDefaultsToAmbiguous(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.provider.submitErr = errors.New("transport ended without an acceptance response")

	task, err := deps.service.Submit(context.Background(), deps.command())

	require.NoError(t, err)
	require.Equal(t, VideoTaskUnknown, task.Status)
	require.Equal(t, "SUBMISSION_STATE_UNKNOWN", videoSubmitStringValue(task.LastErrorCode))
	require.True(t, task.LastErrorRetryable)
	require.Zero(t, deps.billing.releaseCalls)
	require.Equal(t, 1, deps.tasks.unknownCalls)
}

func TestVideoSubmitNonAmbiguousFailureReleasesHoldAndLeaseExactlyOnce(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.provider.submitErr = VideoProviderError{Code: "invalid_request", Retryable: false, Ambiguous: false}

	_, err := deps.service.Submit(context.Background(), deps.command())

	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, 1, deps.submissions.reserveCalls)
	require.Equal(t, 1, deps.billing.releaseCalls)
	require.Equal(t, 1, deps.scheduler.releaseCalls)
	require.Equal(t, 1, deps.tasks.failedCalls)
}

func TestVideoSubmitInvalidSelectedAccountNeverCrossesProviderAndReleasesEveryLease(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	wrong := *deps.account
	wrong.ID = 91
	wrong.Extra = map[string]any{"video_provider": VideoProviderKling}
	wrong.Credentials = map[string]any{
		"access_key":    "access",
		"secret_key":    "secret",
		"model_mapping": map[string]any{"seedance-2.0": "kling-v3"},
	}
	deps.scheduler.accounts = []*Account{&wrong, deps.account}

	task, err := deps.service.Submit(context.Background(), deps.command())

	require.NoError(t, err)
	require.Equal(t, deps.account.ID, task.AccountID)
	require.Equal(t, 2, deps.scheduler.selectCalls)
	require.Equal(t, map[int64]struct{}{wrong.ID: {}}, deps.scheduler.requests[1].ExcludedAccountIDs)
	require.Equal(t, 2, deps.scheduler.releaseCalls, "the rejected and accepted selections each release once")
	require.Equal(t, 1, deps.provider.submitCalls, "provider failover cannot cross provider identity")
}

func TestVideoSubmitPersistsSelectedRouteBeforeUpstreamAndClearsRecoveryOnSubmit(t *testing.T) {
	deps := newVideoSubmitHarness(t)

	task, err := deps.service.Submit(context.Background(), deps.command())

	require.NoError(t, err)
	require.Equal(t, VideoTaskSubmitted, task.Status)
	require.Equal(t, deps.account.ID, task.AccountID)
	require.Equal(t, "seedance-upstream-v2", task.UpstreamModel)
	require.Empty(t, task.RequestPayload)
	require.Zero(t, deps.submissions.lastParams.AccountID, "the atomic reservation must create a route-pending task")
	require.Empty(t, deps.submissions.lastParams.UpstreamModel, "the upstream route is unknown before scheduling")
	require.Equal(t, 1, deps.tasks.assignCalls, "only the atomic assignment transition binds the final route")
	require.Equal(t, deps.account.ID, deps.tasks.lastAssignParams.AccountID)
	require.Equal(t, "seedance-upstream-v2", deps.tasks.lastAssignParams.UpstreamModel)
	require.Equal(t, []string{"validate", "quote", "reserve", "select", "validate", "mark_submitting", "submit", "mark_submitted"}, deps.order)
}

func TestVideoSubmitSchedulerFailureReleasesHoldWithoutUpstream(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	deps.scheduler.err = ErrNoAvailableAccounts

	_, err := deps.service.Submit(context.Background(), deps.command())

	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Equal(t, 1, deps.billing.releaseCalls)
	require.Zero(t, deps.provider.submitCalls)
	require.Zero(t, deps.scheduler.releaseCalls)
}

func TestVideoGetOwnedUsesBothOwnerDimensions(t *testing.T) {
	deps := newVideoSubmitHarness(t)
	want := &VideoTask{RequestID: "vid_00000000000000000000000000000001", UserID: 7, APIKeyID: 8}
	deps.tasks.owned = want

	got, err := deps.service.GetOwned(context.Background(), want.RequestID, want.UserID, want.APIKeyID)

	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, want.RequestID, deps.tasks.ownedRequestID)
	require.Equal(t, want.UserID, deps.tasks.ownedUserID)
	require.Equal(t, want.APIKeyID, deps.tasks.ownedAPIKeyID)
}

func TestVideoAccountSchedulerForcesGrokMediaCapability(t *testing.T) {
	groupID := int64(912)
	repo := groupAwareStubOpenAIAccountRepo{stubOpenAIAccountRepo{accounts: []Account{
		{
			ID: 81, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1,
			Credentials:   map[string]any{"model_mapping": map[string]any{"grok-imagine-video": "wrong-platform"}},
			AccountGroups: []AccountGroup{{GroupID: groupID}},
		},
		{
			ID: 82, Platform: PlatformGrok, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1,
			Extra:         map[string]any{GrokMediaEligibleExtraKey: false},
			Credentials:   map[string]any{"api_key": "disabled", "model_mapping": map[string]any{"grok-imagine-video": "grok-imagine-video"}},
			AccountGroups: []AccountGroup{{GroupID: groupID}},
		},
		{
			ID: 83, Platform: PlatformGrok, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1,
			Credentials:   map[string]any{"api_key": "eligible", "model_mapping": map[string]any{"grok-imagine-video": "grok-imagine-video"}},
			AccountGroups: []AccountGroup{{GroupID: groupID}},
		},
	}}}
	gateway := &OpenAIGatewayService{
		accountRepo:        repo,
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}
	scheduler := NewOpenAIVideoAccountScheduler(gateway)

	selection, err := scheduler.Select(context.Background(), VideoAccountScheduleRequest{
		GroupID:       groupID,
		Platform:      PlatformGrok,
		ExternalModel: "grok-imagine-video",
		Operation:     VideoOperationGeneration,
	})

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(83), selection.Account.ID)
	require.Equal(t, PlatformGrok, selection.Account.Platform)
	require.True(t, selection.GroupMatched)
	selection.Release()
	selection.Release()
}

type videoSubmitHarness struct {
	t             *testing.T
	service       *VideoTaskService
	pricing       *videoSubmitPricingRepo
	submissions   *videoSubmitRepository
	tasks         *videoSubmitTaskRepository
	billing       *videoSubmitBillingRepo
	scheduler     *videoSubmitScheduler
	provider      *videoSubmitProvider
	capabilities  *videoSubmitCapabilities
	subscriptions *videoSubmitSubscriptionMaintainer
	account       *Account
	now           time.Time
	order         []string
}

func newVideoSubmitHarness(t *testing.T) *videoSubmitHarness {
	t.Helper()
	h := &videoSubmitHarness{t: t, now: time.Date(2026, time.August, 5, 8, 0, 0, 0, time.UTC)}
	groupID := int64(41)
	h.account = &Account{
		ID:          71,
		Platform:    PlatformVideo,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 2,
		Extra:       map[string]any{"video_provider": VideoProviderSeedance},
		Credentials: map[string]any{
			"api_key":       "provider-secret-never-persisted",
			"model_mapping": map[string]any{"seedance-2.0": "seedance-upstream-v2"},
		},
		AccountGroups: []AccountGroup{{AccountID: 71, GroupID: groupID}},
	}
	h.pricing = &videoSubmitPricingRepo{harness: h, quote: VideoPriceQuote{
		RuleID: 1, GroupID: groupID, ExternalModel: "seedance-2.0", Operation: "generation",
		Resolution: "720p", AudioMode: "without_audio", Unit: "per_output_second",
		UnitPrice: 0.2, Units: 6, HoldAmount: 1.2,
	}}
	h.submissions = &videoSubmitRepository{harness: h, tasks: make(map[string]*VideoTask)}
	h.tasks = &videoSubmitTaskRepository{harness: h, submissions: h.submissions}
	h.billing = &videoSubmitBillingRepo{}
	h.scheduler = &videoSubmitScheduler{harness: h, accounts: []*Account{h.account}}
	h.provider = &videoSubmitProvider{harness: h, name: VideoProviderSeedance, submitResult: VideoSubmitResult{
		UpstreamTaskID: "upstream-task-1", Status: VideoTaskQueued, UpstreamStatus: "queued",
	}}
	h.subscriptions = &videoSubmitSubscriptionMaintainer{harness: h}
	h.capabilities = &videoSubmitCapabilities{harness: h, catalog: VideoCapabilityCatalog{
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-2.0"): {
			VideoOperationGeneration: {
				Text: true, FirstFrame: true, MinDurationSeconds: 1, MaxDurationSeconds: 30,
				Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"},
			},
		},
	}}
	registry, err := NewVideoProviderRegistry(h.provider)
	require.NoError(t, err)
	h.service = NewVideoTaskService(
		h.submissions,
		h.tasks,
		NewVideoPricingService(h.pricing),
		NewVideoBillingService(h.billing),
		registry,
		h.capabilities,
		h.scheduler,
		h.subscriptions,
		config.VideoConfig{Enabled: true, SeedanceEnabled: true},
	)
	h.service.now = func() time.Time { return h.now }
	h.service.newSubmissionToken = func() (string, error) { return "submission-token-safe", nil }
	return h
}

func (h *videoSubmitHarness) command() VideoSubmitCommand {
	return VideoSubmitCommand{
		UserID:      11,
		APIKeyID:    12,
		Group:       &Group{ID: 41, Platform: PlatformVideo, Status: StatusActive, Hydrated: true, AllowVideoGeneration: true},
		Platform:    PlatformVideo,
		Provider:    VideoProviderSeedance,
		BillingMode: "balance",
		Request: CanonicalVideoRequest{
			Operation: VideoOperationGeneration,
			Model:     "seedance-2.0", Prompt: "animate waves", DurationSeconds: 6,
			Resolution: "720p", AspectRatio: "16:9",
		},
	}
}

func (h *videoSubmitHarness) commandWithKey(key string) VideoSubmitCommand {
	command := h.command()
	command.IdempotencyKey = key
	return command
}

type videoSubmitPricingRepo struct {
	harness *videoSubmitHarness
	quote   VideoPriceQuote
	err     error
	calls   int
}

func (r *videoSubmitPricingRepo) ListMatching(_ context.Context, _ VideoPricingQuery) ([]VideoPricingRule, error) {
	r.calls++
	r.harness.order = append(r.harness.order, "quote")
	if r.err != nil {
		return nil, r.err
	}
	return []VideoPricingRule{{
		ID: r.quote.RuleID, GroupID: r.quote.GroupID, ExternalModel: r.quote.ExternalModel,
		Operation: r.quote.Operation, Resolution: r.quote.Resolution, AudioMode: r.quote.AudioMode,
		Unit: r.quote.Unit, UnitPrice: r.quote.UnitPrice, Enabled: true,
	}}, nil
}

type videoSubmitRepository struct {
	harness             *videoSubmitHarness
	tasks               map[string]*VideoTask
	lastParams          CreateVideoTaskParams
	createCalls         int
	reserveCalls        int
	skipReplayHashCheck bool
	reserveAfter        func()
}

func (r *videoSubmitRepository) CreateTaskAndReserve(ctx context.Context, params CreateVideoTaskParams) (*VideoTask, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	r.createCalls++
	r.lastParams = params
	r.harness.order = append(r.harness.order, "reserve")
	key := params.IdempotencyKeyHash
	if key == "" {
		key = params.Operation + "|" + params.RequestHash
	}
	if params.IdempotencyKeyHash != "" {
		if existing := r.tasks[key]; existing != nil {
			if !r.skipReplayHashCheck && existing.RequestHash != params.RequestHash {
				return nil, false, ErrVideoIdempotencyConflict
			}
			copy := *existing
			copy.RequestPayload = append([]byte(nil), existing.RequestPayload...)
			return &copy, false, nil
		}
	}
	r.reserveCalls++
	requestID := "vid_00000000000000000000000000000001"
	if params.IdempotencyKeyHash == "" {
		requestID = "vid_00000000000000000000000000000002"
	}
	task := &VideoTask{
		RequestID: requestID, UserID: params.UserID, APIKeyID: params.APIKeyID,
		SubscriptionID: params.SubscriptionID, GroupID: params.GroupID, AccountID: params.AccountID,
		Platform: params.Platform, Provider: params.Provider, Operation: params.Operation,
		ExternalModel: params.ExternalModel, UpstreamModel: params.UpstreamModel,
		IdempotencyKeyHash: params.IdempotencyKeyHash, RequestHash: params.RequestHash,
		RequestPayload: params.RequestPayload.Bytes(), Status: VideoTaskCreated,
		PricingUnit: params.PricingUnit, UnitPrice: params.UnitPrice,
		EstimatedUnits: params.EstimatedUnits, EstimatedAmount: params.EstimatedAmount,
		FrozenAmount: params.FrozenAmount, Currency: params.Currency,
		BillingMode: params.BillingMode, BillingStatus: params.BillingStatus,
		NextPollAt: params.NextPollAt,
	}
	r.tasks[key] = task
	if r.reserveAfter != nil {
		r.reserveAfter()
	}
	copy := *task
	copy.RequestPayload = append([]byte(nil), task.RequestPayload...)
	return &copy, true, nil
}

type videoSubmitTaskRepository struct {
	VideoTaskRepository
	harness              *videoSubmitHarness
	submissions          *videoSubmitRepository
	unknownCalls         int
	failedCalls          int
	markSubmittedErr     error
	markSubmittedCommits bool
	assignErr            error
	assignCommits        bool
	assignAfter          func()
	terminalErr          error
	terminalCommits      bool
	getErr               error
	assignCalls          int
	lastAssignParams     AssignVideoSubmissionParams
	owned                *VideoTask
	ownedRequestID       string
	ownedUserID          int64
	ownedAPIKeyID        int64
}

func (r *videoSubmitTaskRepository) AssignAndMarkSubmitting(ctx context.Context, params AssignVideoSubmissionParams) error {
	r.harness.order = append(r.harness.order, "mark_submitting")
	r.assignCalls++
	r.lastAssignParams = params
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.assignErr != nil && !r.assignCommits {
		if r.assignAfter != nil {
			r.assignAfter()
		}
		return r.assignErr
	}
	task := r.byRequestID(params.RequestID)
	if task == nil {
		return ErrVideoTaskNotFound
	}
	task.AccountID = params.AccountID
	task.Platform = params.Platform
	task.Provider = params.Provider
	task.UpstreamModel = params.UpstreamModel
	task.Status = VideoTaskSubmitting
	task.ProviderSubmissionToken = videoSubmitStringPtr(params.ProviderSubmissionToken)
	task.NextPollAt = videoSubmitTimePtr(params.NextPollAt)
	task.Version++
	if r.assignAfter != nil {
		r.assignAfter()
	}
	return r.assignErr
}

func (r *videoSubmitTaskRepository) MarkSubmitted(ctx context.Context, params MarkVideoSubmittedParams) error {
	r.harness.order = append(r.harness.order, "mark_submitted")
	if err := ctx.Err(); err != nil && !r.markSubmittedCommits {
		return err
	}
	if r.markSubmittedErr != nil && !r.markSubmittedCommits {
		return r.markSubmittedErr
	}
	task := r.byRequestID(params.RequestID)
	task.Status = VideoTaskSubmitted
	task.UpstreamTaskID = videoSubmitStringPtr(params.UpstreamTaskID)
	task.UpstreamStatus = videoSubmitStringPtr(params.UpstreamStatus)
	task.NextPollAt = params.NextPollAt
	task.SubmittedAt = &params.SubmittedAt
	task.RequestPayload = params.RequestPayload.Bytes()
	task.Version++
	return r.markSubmittedErr
}

func (r *videoSubmitTaskRepository) MarkSubmissionUnknownAt(_ context.Context, params MarkVideoSubmissionUnknownParams) error {
	r.unknownCalls++
	task := r.byRequestID(params.RequestID)
	task.Status = VideoTaskUnknown
	task.NextPollAt = &params.NextPollAt
	task.LastErrorCode = videoSubmitStringPtr(params.Error.Code())
	task.LastErrorMessage = videoSubmitStringPtr(params.Error.Message())
	task.LastErrorRetryable = params.Error.Retryable()
	task.Version++
	return nil
}

func (r *videoSubmitTaskRepository) ReleaseAndMarkSubmissionFailed(_ context.Context, params ReleaseAndFailVideoSubmissionParams) (*VideoTask, error) {
	r.failedCalls++
	if r.terminalErr != nil && !r.terminalCommits {
		return nil, r.terminalErr
	}
	task := r.byRequestID(params.RequestID)
	if task == nil {
		return nil, ErrVideoTaskNotFound
	}
	if task.Status != params.ExpectedStatus || task.Version != params.ExpectedVersion {
		return nil, ErrVideoTaskVersionConflict
	}
	wantToken := ""
	if task.ProviderSubmissionToken != nil {
		wantToken = *task.ProviderSubmissionToken
	}
	if wantToken != params.ProviderSubmissionToken {
		return nil, ErrVideoTaskInvalidTransition
	}
	r.harness.billing.releaseCalls++
	task.Status = VideoTaskFailed
	task.BillingStatus = "released"
	zero := float64(0)
	task.SettledAmount = &zero
	task.SettledAt = videoSubmitTimePtr(params.FailedAt)
	task.LastErrorCode = videoSubmitStringPtr(params.Error.Code())
	task.LastErrorMessage = videoSubmitStringPtr(params.Error.Message())
	task.LastErrorRetryable = params.Error.Retryable()
	task.RequestPayload = nil
	task.ProviderSubmissionToken = nil
	task.NextPollAt = nil
	task.Version++
	copy := *task
	if r.terminalErr != nil {
		return nil, r.terminalErr
	}
	return &copy, nil
}

func (r *videoSubmitTaskRepository) GetByRequestID(_ context.Context, requestID string) (*VideoTask, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	task := r.byRequestID(requestID)
	if task == nil {
		return nil, ErrVideoTaskNotFound
	}
	copy := *task
	copy.RequestPayload = append([]byte(nil), task.RequestPayload...)
	return &copy, nil
}

func (r *videoSubmitTaskRepository) GetOwned(_ context.Context, requestID string, userID, apiKeyID int64) (*VideoTask, error) {
	r.ownedRequestID, r.ownedUserID, r.ownedAPIKeyID = requestID, userID, apiKeyID
	if r.owned == nil {
		return nil, ErrVideoTaskNotFound
	}
	return r.owned, nil
}

func (r *videoSubmitTaskRepository) byRequestID(requestID string) *VideoTask {
	for _, task := range r.submissions.tasks {
		if task.RequestID == requestID {
			return task
		}
	}
	return nil
}

type videoSubmitBillingRepo struct {
	UsageBillingRepository
	releaseCalls int
}

func (r *videoSubmitBillingRepo) ReleaseVideo(_ context.Context, _ *VideoHoldCommand) (*VideoHoldResult, error) {
	r.releaseCalls++
	return &VideoHoldResult{Applied: true}, nil
}

type videoSubmitScheduler struct {
	harness      *videoSubmitHarness
	accounts     []*Account
	err          error
	selectCalls  int
	releaseCalls int
	requests     []VideoAccountScheduleRequest
}

func (s *videoSubmitScheduler) Select(ctx context.Context, request VideoAccountScheduleRequest) (*VideoAccountSelection, error) {
	s.selectCalls++
	s.harness.order = append(s.harness.order, "select")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request.ExcludedAccountIDs = cloneExcludedAccountIDs(request.ExcludedAccountIDs)
	s.requests = append(s.requests, request)
	if s.err != nil {
		return nil, s.err
	}
	for _, account := range s.accounts {
		if _, excluded := request.ExcludedAccountIDs[account.ID]; excluded {
			continue
		}
		released := false
		return &VideoAccountSelection{Account: account, Release: func() {
			if released {
				s.harness.t.Fatalf("scheduler lease for account %d released more than once", account.ID)
			}
			released = true
			s.releaseCalls++
		}}, nil
	}
	return nil, ErrNoAvailableAccounts
}

type videoSubmitProvider struct {
	harness      *videoSubmitHarness
	name         string
	submitResult VideoSubmitResult
	submitErr    error
	submitCalls  int
	submitAfter  func()
}

type videoSubmitCapabilities struct {
	harness *videoSubmitHarness
	catalog VideoCapabilityCatalog
	err     error
}

func (v *videoSubmitCapabilities) Validate(provider string, request CanonicalVideoRequest) error {
	v.harness.order = append(v.harness.order, "validate")
	if v.err != nil {
		return v.err
	}
	return v.catalog.Validate(provider, request)
}

func (p *videoSubmitProvider) Name() string { return p.name }
func (p *videoSubmitProvider) Capabilities() VideoProviderCapabilities {
	return VideoProviderCapabilities{VideoOperationGeneration: {Text: true, FirstFrame: true}}
}
func (p *videoSubmitProvider) Submit(_ context.Context, _ *Account, _ CanonicalVideoRequest, _ string) (VideoSubmitResult, error) {
	p.submitCalls++
	p.harness.order = append(p.harness.order, "submit")
	if p.submitAfter != nil {
		p.submitAfter()
	}
	return p.submitResult, p.submitErr
}
func (p *videoSubmitProvider) RecoverSubmission(context.Context, *Account, VideoTask, string) (VideoSubmitResult, bool, error) {
	panic("unexpected recovery")
}
func (p *videoSubmitProvider) Poll(context.Context, *Account, VideoTask) (VideoPollResult, error) {
	panic("unexpected poll")
}
func (p *videoSubmitProvider) OpenContent(context.Context, *Account, VideoTask) (io.ReadCloser, http.Header, int64, error) {
	panic("unexpected content")
}

type videoSubmitSubscriptionMaintainer struct {
	harness   *videoSubmitHarness
	refreshed *UserSubscription
	err       error
	calls     int
}

func (m *videoSubmitSubscriptionMaintainer) EnsureWindowMaintenance(_ context.Context, sub *UserSubscription) (*UserSubscription, error) {
	m.calls++
	m.harness.order = append(m.harness.order, "maintain")
	if m.err != nil {
		return nil, m.err
	}
	if m.refreshed != nil {
		return m.refreshed, nil
	}
	return sub, nil
}

func videoSubmitStringPtr(value string) *string     { return &value }
func videoSubmitTimePtr(value time.Time) *time.Time { return &value }

func videoSubmitStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
