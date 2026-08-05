package service

import (
	"context"
	"strings"
	"sync"
)

// VideoAccountScheduleRequest is the provider-neutral scheduling input used by
// durable video submission. Exclusions are account IDs already rejected by a
// post-selection provider/capability recheck.
type VideoAccountScheduleRequest struct {
	GroupID            int64
	Platform           string
	ExternalModel      string
	Operation          VideoOperation
	ExcludedAccountIDs map[int64]struct{}
}

// VideoAccountSelection owns one scheduler concurrency lease. Release is
// idempotent and must be called after the upstream submission attempt ends.
type VideoAccountSelection struct {
	Account      *Account
	GroupMatched bool
	Release      func()
}

type VideoAccountScheduler interface {
	Select(context.Context, VideoAccountScheduleRequest) (*VideoAccountSelection, error)
}

// OpenAIVideoAccountScheduler reuses the mature OpenAI/Grok account scheduler
// while forcing video requests onto either the Grok or dedicated Video pool.
type OpenAIVideoAccountScheduler struct {
	gateway *OpenAIGatewayService
}

func NewOpenAIVideoAccountScheduler(gateway *OpenAIGatewayService) *OpenAIVideoAccountScheduler {
	return &OpenAIVideoAccountScheduler{gateway: gateway}
}

func (s *OpenAIVideoAccountScheduler) Select(ctx context.Context, request VideoAccountScheduleRequest) (*VideoAccountSelection, error) {
	if s == nil || s.gateway == nil || request.GroupID <= 0 || strings.TrimSpace(request.ExternalModel) == "" ||
		(request.Platform != PlatformGrok && request.Platform != PlatformVideo) ||
		(request.Operation != VideoOperationGeneration && request.Operation != VideoOperationEdit && request.Operation != VideoOperationExtension) {
		return nil, ErrNoAvailableAccounts
	}

	requiredCapability := OpenAIEndpointCapability("")
	if request.Platform == PlatformGrok {
		requiredCapability = OpenAIEndpointCapabilityGrokMediaGeneration
	}
	groupID := request.GroupID
	selection, _, err := s.gateway.SelectAccountWithSchedulerForCapability(
		ctx,
		&groupID,
		"",
		"",
		strings.TrimSpace(request.ExternalModel),
		cloneExcludedAccountIDs(request.ExcludedAccountIDs),
		OpenAIUpstreamTransportAny,
		requiredCapability,
		false,
		false,
		false,
		request.Platform,
	)
	if err != nil {
		return nil, err
	}
	if selection == nil || selection.Account == nil || !selection.Acquired || selection.ReleaseFunc == nil {
		if selection != nil && selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return nil, ErrNoAvailableAccounts
	}

	var once sync.Once
	release := func() {
		once.Do(selection.ReleaseFunc)
	}
	return &VideoAccountSelection{
		Account:      selection.Account,
		GroupMatched: s.gateway.openAIAccountMatchesSchedulingGroup(selection.Account, &groupID),
		Release:      release,
	}, nil
}
