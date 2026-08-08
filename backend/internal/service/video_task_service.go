package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	videoSubmissionRecoveryDelay      = 10 * time.Second
	videoSubmissionCrashRecoveryDelay = 5 * time.Minute
	videoSubmissionRecoveryIOTimeout  = 5 * time.Second
)

var (
	ErrVideoGenerationNotAllowed = infraerrors.Forbidden("VIDEO_GENERATION_NOT_ALLOWED", "video generation is not allowed for this group")
	ErrVideoServiceUnavailable   = infraerrors.ServiceUnavailable("VIDEO_SERVICE_UNAVAILABLE", "video submission service is unavailable")
	ErrVideoProviderDisabled     = infraerrors.ServiceUnavailable("VIDEO_PROVIDER_DISABLED", "video provider is disabled")
)

type VideoCapabilityValidator interface {
	Validate(string, CanonicalVideoRequest) error
}

type VideoSubscriptionWindowMaintainer interface {
	EnsureWindowMaintenance(context.Context, *UserSubscription) (*UserSubscription, error)
}

type VideoSubmitCommand struct {
	UserID         int64
	APIKeyID       int64
	Group          *Group
	Subscription   *UserSubscription
	Platform       string
	Provider       string
	BillingMode    string
	IdempotencyKey string
	Request        CanonicalVideoRequest
}

type VideoTaskService struct {
	submissions        VideoSubmissionRepository
	tasks              VideoTaskRepository
	pricing            *VideoPricingService
	providers          *VideoProviderRegistry
	capabilities       VideoCapabilityValidator
	scheduler          VideoAccountScheduler
	subscriptions      VideoSubscriptionWindowMaintainer
	videoConfig        config.VideoConfig
	now                func() time.Time
	newSubmissionToken func() (string, error)
}

func NewVideoTaskService(
	submissions VideoSubmissionRepository,
	tasks VideoTaskRepository,
	pricing *VideoPricingService,
	_ *VideoBillingService,
	providers *VideoProviderRegistry,
	capabilities VideoCapabilityValidator,
	scheduler VideoAccountScheduler,
	subscriptions VideoSubscriptionWindowMaintainer,
	configs ...config.VideoConfig,
) *VideoTaskService {
	videoConfig := config.VideoConfig{}
	if len(configs) > 0 {
		videoConfig = configs[0]
	}
	return &VideoTaskService{
		submissions:        submissions,
		tasks:              tasks,
		pricing:            pricing,
		providers:          providers,
		capabilities:       capabilities,
		scheduler:          scheduler,
		subscriptions:      subscriptions,
		videoConfig:        videoConfig,
		now:                time.Now,
		newSubmissionToken: newVideoSubmissionToken,
	}
}

func (s *VideoTaskService) Submit(ctx context.Context, command VideoSubmitCommand) (*VideoTask, error) {
	if s == nil || s.submissions == nil || s.tasks == nil || s.pricing == nil ||
		s.providers == nil || s.capabilities == nil || s.scheduler == nil {
		return nil, ErrVideoServiceUnavailable
	}
	if !s.submissionProviderEnabled(command.Provider) {
		return nil, ErrVideoProviderDisabled
	}

	command, provider, idempotencyHash, requestHash, recoveryPayload, err := s.validateSubmitCommand(command)
	if err != nil {
		return nil, err
	}

	quote, err := s.pricing.Quote(ctx, VideoPricingQuery{
		GroupID:         command.Group.ID,
		ExternalModel:   command.Request.Model,
		Operation:       string(command.Request.Operation),
		Resolution:      command.Request.Resolution,
		Audio:           command.Request.Audio != nil && *command.Request.Audio,
		DurationSeconds: float64(command.Request.DurationSeconds),
	})
	if err != nil {
		return nil, err
	}

	subscriptionID, err := s.maintainSubscriptionBeforeReserve(ctx, command)
	if err != nil {
		return nil, err
	}
	routeRecoveryAt := s.currentTime().Add(videoSubmissionCrashRecoveryDelay)
	task, created, err := s.submissions.CreateTaskAndReserve(ctx, CreateVideoTaskParams{
		UserID:             command.UserID,
		APIKeyID:           command.APIKeyID,
		SubscriptionID:     subscriptionID,
		GroupID:            command.Group.ID,
		AccountID:          0,
		Platform:           command.Platform,
		Provider:           command.Provider,
		Operation:          string(command.Request.Operation),
		ExternalModel:      command.Request.Model,
		UpstreamModel:      "",
		IdempotencyKeyHash: idempotencyHash,
		RequestHash:        requestHash,
		RequestPayload:     recoveryPayload,
		PricingUnit:        quote.Unit,
		UnitPrice:          quote.UnitPrice,
		EstimatedUnits:     quote.Units,
		EstimatedAmount:    quote.HoldAmount,
		FrozenAmount:       quote.HoldAmount,
		Currency:           "USD",
		BillingMode:        command.BillingMode,
		BillingStatus:      "held",
		NextPollAt:         &routeRecoveryAt,
	})
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrVideoServiceUnavailable
	}
	if task.RequestHash != requestHash {
		return nil, ErrVideoIdempotencyConflict
	}
	if !created {
		return task, nil
	}

	selection, upstreamModel, err := s.selectValidatedAccount(ctx, command)
	if err != nil {
		return nil, s.releaseRejectedSubmission(task, err)
	}
	defer selection.Release()

	submissionToken, err := s.newSubmissionToken()
	if err != nil {
		return nil, s.releaseRejectedSubmission(task, err)
	}
	updatedAt := s.currentTime()
	assignmentRecoveryAt := updatedAt.Add(videoSubmissionCrashRecoveryDelay)
	assignment := AssignVideoSubmissionParams{
		RequestID:               task.RequestID,
		ExpectedVersion:         task.Version,
		AccountID:               selection.Account.ID,
		Platform:                command.Platform,
		Provider:                command.Provider,
		UpstreamModel:           upstreamModel,
		ProviderSubmissionToken: submissionToken,
		NextPollAt:              assignmentRecoveryAt,
		UpdatedAt:               updatedAt,
	}
	err = s.tasks.AssignAndMarkSubmitting(ctx, assignment)
	if err != nil {
		stored, readErr := s.readTaskForRecovery(task.RequestID)
		if readErr != nil || stored == nil {
			return nil, errors.Join(err, readErr)
		}
		if videoTaskMatchesAssignedSubmission(stored, task, assignment) {
			task = stored
		} else if videoTaskMatchesPendingSubmission(stored, task) {
			return nil, s.releaseRejectedSubmission(stored, err)
		} else {
			return nil, errors.Join(err, ErrVideoTaskVersionConflict)
		}
	} else {
		task.AccountID = selection.Account.ID
		task.Platform = command.Platform
		task.Provider = command.Provider
		task.UpstreamModel = upstreamModel
		task.ProviderSubmissionToken = videoStringPointer(submissionToken)
		task.Status = VideoTaskSubmitting
		task.NextPollAt = &assignmentRecoveryAt
		task.Version++
	}
	if err := ctx.Err(); err != nil {
		return nil, s.releaseRejectedSubmission(task, err)
	}

	result, submitErr := provider.Submit(ctx, selection.Account, command.Request, submissionToken)
	if submitErr != nil {
		if videoSubmissionIsAmbiguous(submitErr) {
			return s.markSubmissionUnknown(ctx, task, videoTaskErrorForAmbiguousSubmission(submitErr))
		}
		return nil, s.releaseRejectedSubmission(task, submitErr)
	}
	if strings.TrimSpace(result.UpstreamTaskID) == "" || result.UpstreamTaskID == task.RequestID {
		contractErr := VideoProviderError{Code: "provider_contract_error", Retryable: true, Ambiguous: true}
		return s.markSubmissionUnknown(ctx, task, videoTaskErrorForSubmission(contractErr))
	}

	nextPollAt := result.NextPollAt
	if nextPollAt == nil {
		next := s.currentTime().Add(videoSubmissionRecoveryDelay)
		nextPollAt = &next
	}
	submittedAt := s.currentTime()
	pollPayload := minimizedVideoPollPayload(command.Provider, command.Request)
	err = s.tasks.MarkSubmitted(ctx, MarkVideoSubmittedParams{
		RequestID:       task.RequestID,
		ExpectedVersion: task.Version,
		UpstreamTaskID:  strings.TrimSpace(result.UpstreamTaskID),
		UpstreamStatus:  strings.TrimSpace(result.UpstreamStatus),
		RequestPayload:  pollPayload,
		NextPollAt:      nextPollAt,
		SubmittedAt:     submittedAt,
	})
	if err != nil {
		return s.resolveSubmittedPersistenceError(ctx, task, strings.TrimSpace(result.UpstreamTaskID), err)
	}
	task.Status = VideoTaskSubmitted
	task.UpstreamTaskID = videoStringPointer(strings.TrimSpace(result.UpstreamTaskID))
	if status := strings.TrimSpace(result.UpstreamStatus); status != "" {
		task.UpstreamStatus = videoStringPointer(status)
	}
	task.NextPollAt = nextPollAt
	task.SubmittedAt = &submittedAt
	task.RequestPayload = pollPayload.Bytes()
	task.Version++
	return s.refreshTaskOrFallback(ctx, task), nil
}

func (s *VideoTaskService) submissionProviderEnabled(provider string) bool {
	if s == nil || !s.videoConfig.Enabled {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case PlatformGrok:
		return s.videoConfig.GrokEnabled
	case VideoProviderSeedance:
		return s.videoConfig.SeedanceEnabled
	case VideoProviderKling:
		return s.videoConfig.KlingEnabled
	default:
		// Unknown providers still flow through canonical validation so callers get
		// the existing invalid-provider response instead of a misleading flag error.
		return true
	}
}

func (s *VideoTaskService) GetOwned(ctx context.Context, requestID string, userID, apiKeyID int64) (*VideoTask, error) {
	if s == nil || s.tasks == nil {
		return nil, ErrVideoServiceUnavailable
	}
	if !IsVideoRequestID(requestID) || userID <= 0 || apiKeyID <= 0 {
		return nil, ErrVideoTaskNotFound
	}
	return s.tasks.GetOwned(ctx, requestID, userID, apiKeyID)
}

func (s *VideoTaskService) validateSubmitCommand(command VideoSubmitCommand) (VideoSubmitCommand, VideoProvider, string, string, MinimizedVideoPayload, error) {
	if command.UserID <= 0 || command.APIKeyID <= 0 || command.Group == nil || command.Group.ID <= 0 ||
		!command.Group.Hydrated || !command.Group.IsActive() || !command.Group.AllowVideoGeneration {
		return command, nil, "", "", MinimizedVideoPayload{}, ErrVideoGenerationNotAllowed
	}
	command.Platform = strings.ToLower(strings.TrimSpace(command.Platform))
	command.Provider = strings.ToLower(strings.TrimSpace(command.Provider))
	if command.Platform != PlatformVideo && command.Platform != PlatformGrok {
		return command, nil, "", "", MinimizedVideoPayload{}, ErrVideoUnsupportedCapability
	}
	if command.Group.Platform != command.Platform && command.Group.Platform != PlatformComposite {
		return command, nil, "", "", MinimizedVideoPayload{}, ErrVideoGenerationNotAllowed
	}
	if (command.Platform == PlatformGrok && command.Provider != PlatformGrok) ||
		(command.Platform == PlatformVideo && command.Provider != VideoProviderSeedance && command.Provider != VideoProviderKling) {
		return command, nil, "", "", MinimizedVideoPayload{}, ErrVideoInvalidRequest
	}

	command.BillingMode = strings.ToLower(strings.TrimSpace(command.BillingMode))
	if command.BillingMode == "" {
		command.BillingMode = "balance"
	}
	switch command.BillingMode {
	case "balance":
		if command.Subscription != nil {
			return command, nil, "", "", MinimizedVideoPayload{}, ErrVideoTaskInvalidRequest
		}
	case "subscription":
		if command.Subscription == nil || command.Subscription.ID <= 0 ||
			command.Subscription.UserID != command.UserID || command.Subscription.GroupID != command.Group.ID {
			return command, nil, "", "", MinimizedVideoPayload{}, ErrVideoTaskInvalidRequest
		}
	default:
		return command, nil, "", "", MinimizedVideoPayload{}, ErrVideoTaskInvalidRequest
	}

	if command.IdempotencyKey != "" {
		normalizedKey, err := NormalizeIdempotencyKey(command.IdempotencyKey)
		if err != nil || normalizedKey == "" {
			if err != nil {
				return command, nil, "", "", MinimizedVideoPayload{}, err
			}
			return command, nil, "", "", MinimizedVideoPayload{}, ErrIdempotencyKeyInvalid
		}
	}
	command.Request = cloneCanonicalVideoRequest(command.Request)
	if err := validateCanonicalVideoRequest(&command.Request); err != nil {
		return command, nil, "", "", MinimizedVideoPayload{}, err
	}
	if err := validateCanonicalVideoProviderOptions(command.Provider, &command.Request); err != nil {
		return command, nil, "", "", MinimizedVideoPayload{}, err
	}
	provider, ok := s.providers.Get(command.Provider)
	if !ok || provider == nil {
		return command, nil, "", "", MinimizedVideoPayload{}, ErrVideoUnsupportedCapability
	}
	if err := s.capabilities.Validate(command.Provider, command.Request); err != nil {
		return command, nil, "", "", MinimizedVideoPayload{}, err
	}

	requestHash, err := hashCanonicalVideoRequest(command.Request)
	if err != nil {
		return command, nil, "", "", MinimizedVideoPayload{}, ErrVideoInvalidRequest.WithCause(err)
	}
	idempotencyHash := ""
	if command.IdempotencyKey != "" {
		idempotencyHash = HashIdempotencyKey(command.IdempotencyKey)
	}
	recoveryPayload := minimizedVideoRecoveryPayload(command.Provider, command.Request)
	return command, provider, idempotencyHash, requestHash, recoveryPayload, nil
}

func (s *VideoTaskService) maintainSubscriptionBeforeReserve(ctx context.Context, command VideoSubmitCommand) (*int64, error) {
	if command.BillingMode != "subscription" {
		return nil, nil
	}
	if s.subscriptions == nil {
		return nil, ErrVideoServiceUnavailable
	}
	refreshed, err := s.subscriptions.EnsureWindowMaintenance(ctx, command.Subscription)
	if err != nil {
		return nil, err
	}
	if refreshed == nil || refreshed.ID != command.Subscription.ID || refreshed.UserID != command.UserID ||
		refreshed.GroupID != command.Group.ID {
		return nil, ErrVideoTaskInvalidRequest
	}
	subscriptionID := refreshed.ID
	return &subscriptionID, nil
}

func (s *VideoTaskService) selectValidatedAccount(ctx context.Context, command VideoSubmitCommand) (*VideoAccountSelection, string, error) {
	excluded := make(map[int64]struct{})
	for attempts := 0; attempts < 64; attempts++ {
		selection, err := s.scheduler.Select(ctx, VideoAccountScheduleRequest{
			GroupID:            command.Group.ID,
			Platform:           command.Platform,
			ExternalModel:      command.Request.Model,
			Operation:          command.Request.Operation,
			ExcludedAccountIDs: cloneExcludedAccountIDs(excluded),
		})
		if err != nil {
			return nil, "", err
		}
		if selection == nil || selection.Account == nil || selection.Release == nil {
			return nil, "", ErrNoAvailableAccounts
		}
		accountID := selection.Account.ID
		if accountID <= 0 {
			selection.Release()
			return nil, "", ErrNoAvailableAccounts
		}
		if _, duplicate := excluded[accountID]; duplicate {
			selection.Release()
			return nil, "", ErrNoAvailableAccounts
		}
		upstreamModel, valid := s.validateSelectedAccount(ctx, command, selection)
		if valid {
			return selection, upstreamModel, nil
		}
		selection.Release()
		excluded[accountID] = struct{}{}
	}
	return nil, "", ErrNoAvailableAccounts
}

func (s *VideoTaskService) validateSelectedAccount(ctx context.Context, command VideoSubmitCommand, selection *VideoAccountSelection) (string, bool) {
	account := selection.Account
	if account == nil || account.ID <= 0 || account.Platform != command.Platform ||
		(!selection.GroupMatched && !openAIStickyAccountMatchesGroup(account, &command.Group.ID)) ||
		!account.IsSchedulableForModelWithContext(ctx, command.Request.Model) || !account.IsModelSupported(command.Request.Model) {
		return "", false
	}
	providerName := PlatformGrok
	if command.Platform == PlatformVideo {
		if err := ValidateVideoAccountConfig(account.Platform, account.Type, account.Extra, account.Credentials); err != nil {
			return "", false
		}
		providerName = account.VideoProvider()
	}
	if providerName != command.Provider {
		return "", false
	}
	if provider, ok := s.providers.Get(providerName); !ok || provider == nil {
		return "", false
	}
	if err := s.capabilities.Validate(providerName, command.Request); err != nil {
		return "", false
	}
	if err := ValidateVideoAccountCapabilityOverrides(account, command.Request); err != nil {
		return "", false
	}
	upstreamModel := strings.TrimSpace(account.GetMappedModel(command.Request.Model))
	if upstreamModel == "" {
		return "", false
	}
	return upstreamModel, true
}

func (s *VideoTaskService) markSubmissionUnknown(ctx context.Context, task *VideoTask, taskError VideoTaskError) (*VideoTask, error) {
	if ctx == nil || ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = s.recoveryContext()
		defer cancel()
	}
	now := s.currentTime()
	nextPollAt := now.Add(videoSubmissionRecoveryDelay)
	err := s.tasks.MarkSubmissionUnknownAt(ctx, MarkVideoSubmissionUnknownParams{
		RequestID:       task.RequestID,
		ExpectedVersion: task.Version,
		Error:           taskError,
		NextPollAt:      nextPollAt,
		UpdatedAt:       now,
	})
	if err != nil {
		return nil, err
	}
	task.Status = VideoTaskUnknown
	task.NextPollAt = &nextPollAt
	task.LastErrorCode = videoStringPointer(taskError.Code())
	if taskError.Message() != "" {
		task.LastErrorMessage = videoStringPointer(taskError.Message())
	}
	task.LastErrorRetryable = taskError.Retryable()
	task.Version++
	return s.refreshTaskOrFallback(ctx, task), nil
}

func (s *VideoTaskService) resolveSubmittedPersistenceError(ctx context.Context, task *VideoTask, upstreamTaskID string, persistenceErr error) (*VideoTask, error) {
	if ctx == nil || ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = s.recoveryContext()
		defer cancel()
	}
	stored, readErr := s.tasks.GetByRequestID(ctx, task.RequestID)
	if readErr == nil && stored != nil {
		if stored.UpstreamTaskID != nil && strings.TrimSpace(*stored.UpstreamTaskID) == upstreamTaskID {
			return stored, nil
		}
		if stored.Status == VideoTaskUnknown {
			return stored, nil
		}
		if stored.Status == VideoTaskSubmitting && stored.Version == task.Version {
			return s.markSubmissionUnknown(ctx, stored, NewVideoTaskError("SUBMISSION_STATE_UNKNOWN", "", true))
		}
		return nil, errors.Join(persistenceErr, ErrVideoTaskVersionConflict)
	}

	// If the confirmation read also failed, the optimistic transition can still
	// safely establish unknown only when MarkSubmitted did not commit. A committed
	// transaction has already advanced the version/status, so this update cannot
	// overwrite it.
	unknown, unknownErr := s.markSubmissionUnknown(ctx, task, NewVideoTaskError("SUBMISSION_STATE_UNKNOWN", "", true))
	if unknownErr == nil {
		return unknown, nil
	}
	return nil, errors.Join(persistenceErr, readErr, unknownErr)
}

func (s *VideoTaskService) releaseRejectedSubmission(task *VideoTask, cause error) error {
	if task == nil {
		return cause
	}
	ctx, cancel := s.recoveryContext()
	defer cancel()
	failedAt := s.currentTime()
	token := ""
	if task.ProviderSubmissionToken != nil {
		token = strings.TrimSpace(*task.ProviderSubmissionToken)
	}
	_, terminalErr := s.tasks.ReleaseAndMarkSubmissionFailed(ctx, ReleaseAndFailVideoSubmissionParams{
		RequestID:               task.RequestID,
		ExpectedVersion:         task.Version,
		ExpectedStatus:          task.Status,
		ProviderSubmissionToken: token,
		Error:                   videoTaskErrorForSubmission(cause),
		FailedAt:                failedAt,
	})
	if terminalErr == nil {
		return cause
	}
	stored, readErr := s.tasks.GetByRequestID(ctx, task.RequestID)
	if readErr == nil && stored != nil && stored.Status == VideoTaskFailed && stored.BillingStatus == "released" {
		return cause
	}
	return errors.Join(cause, terminalErr, readErr)
}

func (s *VideoTaskService) readTaskForRecovery(requestID string) (*VideoTask, error) {
	ctx, cancel := s.recoveryContext()
	defer cancel()
	return s.tasks.GetByRequestID(ctx, requestID)
}

func (s *VideoTaskService) recoveryContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), videoSubmissionRecoveryIOTimeout)
}

func videoTaskMatchesPendingSubmission(stored, original *VideoTask) bool {
	return stored != nil && original != nil && stored.RequestID == original.RequestID &&
		stored.RequestHash == original.RequestHash && stored.Version == original.Version &&
		stored.Status == VideoTaskCreated && stored.AccountID == 0 && strings.TrimSpace(stored.UpstreamModel) == "" &&
		stored.ProviderSubmissionToken == nil && stored.UpstreamTaskID == nil
}

func videoTaskMatchesAssignedSubmission(stored, original *VideoTask, assignment AssignVideoSubmissionParams) bool {
	return stored != nil && original != nil && stored.RequestID == original.RequestID &&
		stored.RequestHash == original.RequestHash && stored.Version == original.Version+1 &&
		stored.Status == VideoTaskSubmitting && stored.AccountID == assignment.AccountID &&
		stored.Platform == assignment.Platform && stored.Provider == assignment.Provider &&
		stored.UpstreamModel == assignment.UpstreamModel && stored.UpstreamTaskID == nil &&
		stored.ProviderSubmissionToken != nil && *stored.ProviderSubmissionToken == assignment.ProviderSubmissionToken
}

func (s *VideoTaskService) refreshTaskOrFallback(ctx context.Context, fallback *VideoTask) *VideoTask {
	if stored, err := s.tasks.GetByRequestID(ctx, fallback.RequestID); err == nil && stored != nil {
		return stored
	}
	return fallback
}

func (s *VideoTaskService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func newVideoSubmissionToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "vsub_" + hex.EncodeToString(value[:]), nil
}

func cloneCanonicalVideoRequest(request CanonicalVideoRequest) CanonicalVideoRequest {
	request.FirstFrame = append([]VideoAsset(nil), request.FirstFrame...)
	request.LastFrame = append([]VideoAsset(nil), request.LastFrame...)
	request.ReferenceImages = append([]VideoAsset(nil), request.ReferenceImages...)
	request.ReferenceVideos = append([]VideoAsset(nil), request.ReferenceVideos...)
	if len(request.ProviderOptions) == 0 {
		request.ProviderOptions = nil
	} else {
		options := make(map[string]json.RawMessage, len(request.ProviderOptions))
		for name, raw := range request.ProviderOptions {
			options[name] = append(json.RawMessage(nil), raw...)
		}
		request.ProviderOptions = options
	}
	if request.Audio != nil {
		audio := *request.Audio
		request.Audio = &audio
	}
	return request
}

func validateCanonicalVideoProviderOptions(provider string, request *CanonicalVideoRequest) error {
	if request == nil || len(request.ProviderOptions) == 0 {
		return nil
	}
	if len(request.ProviderOptions) != 1 {
		return ErrVideoInvalidRequest
	}
	raw, ok := request.ProviderOptions[provider]
	if !ok {
		return ErrVideoInvalidRequest
	}
	outer, err := json.Marshal(map[string]json.RawMessage{provider: raw})
	if err != nil {
		return ErrVideoInvalidRequest
	}
	validated := CanonicalVideoRequest{}
	if err := parseVideoProviderOptions(outer, &validated); err != nil {
		return err
	}
	canonical, err := canonicalVideoProviderOptions(provider, validated.ProviderOptions[provider])
	if err != nil {
		return err
	}
	request.ProviderOptions = map[string]json.RawMessage{provider: canonical}
	return nil
}

func canonicalVideoProviderOptions(provider string, raw json.RawMessage) (json.RawMessage, error) {
	schema, ok := videoProviderOptionSchema(provider)
	if !ok {
		return nil, ErrVideoInvalidRequest
	}
	options, unique := decodeUniqueVideoJSONObject(raw)
	if !unique {
		return nil, ErrVideoInvalidRequest
	}
	canonical := make(map[string]any, len(options))
	for name, value := range options {
		switch schema[name] {
		case videoProviderOptionInteger:
			var decoded int64
			if err := json.Unmarshal(value, &decoded); err != nil {
				return nil, ErrVideoInvalidRequest
			}
			canonical[name] = decoded
		case videoProviderOptionBoolean:
			var decoded bool
			if err := json.Unmarshal(value, &decoded); err != nil {
				return nil, ErrVideoInvalidRequest
			}
			canonical[name] = decoded
		case videoProviderOptionString:
			var decoded string
			if err := json.Unmarshal(value, &decoded); err != nil {
				return nil, ErrVideoInvalidRequest
			}
			canonical[name] = decoded
		case videoProviderOptionNumber:
			var decoded float64
			if err := json.Unmarshal(value, &decoded); err != nil || math.IsNaN(decoded) || math.IsInf(decoded, 0) {
				return nil, ErrVideoInvalidRequest
			}
			canonical[name] = decoded
		default:
			return nil, ErrVideoInvalidRequest
		}
	}
	normalized, err := json.Marshal(canonical)
	if err != nil {
		return nil, ErrVideoInvalidRequest.WithCause(err)
	}
	return normalized, nil
}

func hashCanonicalVideoRequest(request CanonicalVideoRequest) (string, error) {
	canonical := struct {
		Operation       VideoOperation             `json:"operation"`
		Model           string                     `json:"model"`
		Prompt          string                     `json:"prompt,omitempty"`
		DurationSeconds int                        `json:"duration_seconds,omitempty"`
		Resolution      string                     `json:"resolution,omitempty"`
		AspectRatio     string                     `json:"aspect_ratio,omitempty"`
		FirstFrame      []VideoAsset               `json:"first_frame,omitempty"`
		LastFrame       []VideoAsset               `json:"last_frame,omitempty"`
		ReferenceImages []VideoAsset               `json:"reference_images,omitempty"`
		ReferenceVideos []VideoAsset               `json:"reference_videos,omitempty"`
		Audio           *bool                      `json:"audio,omitempty"`
		ProviderOptions map[string]json.RawMessage `json:"provider_options,omitempty"`
	}{
		Operation: request.Operation, Model: request.Model, Prompt: request.Prompt,
		DurationSeconds: request.DurationSeconds, Resolution: request.Resolution, AspectRatio: request.AspectRatio,
		FirstFrame: request.FirstFrame, LastFrame: request.LastFrame,
		ReferenceImages: request.ReferenceImages, ReferenceVideos: request.ReferenceVideos,
		Audio: request.Audio, ProviderOptions: request.ProviderOptions,
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func minimizedVideoRecoveryPayload(provider string, request CanonicalVideoRequest) MinimizedVideoPayload {
	payload := make(map[string]any)
	if provider == VideoProviderKling {
		kind, err := klingProviderTaskKind(request)
		if err != nil {
			return MinimizedVideoPayload{}
		}
		addSafeVideoRecoveryField(payload, "provider_task_kind", kind)
	}
	addSafeVideoRecoveryField(payload, "prompt", request.Prompt)
	if request.DurationSeconds > 0 {
		addSafeVideoRecoveryField(payload, "duration_seconds", request.DurationSeconds)
	}
	addSafeVideoRecoveryField(payload, "resolution", request.Resolution)
	addSafeVideoRecoveryField(payload, "aspect_ratio", request.AspectRatio)
	if request.Audio != nil {
		mode := "without_audio"
		if *request.Audio {
			mode = "with_audio"
		}
		addSafeVideoRecoveryField(payload, "audio_mode", mode)
	}
	addSafeVideoRecoveryAsset(payload, "first_frame_ref", request.FirstFrame)
	addSafeVideoRecoveryAsset(payload, "last_frame_ref", request.LastFrame)
	addSafeVideoRecoveryAsset(payload, "input_image_ref", request.ReferenceImages)
	addSafeVideoRecoveryAsset(payload, "input_video_ref", request.ReferenceVideos)
	if raw := request.ProviderOptions[provider]; len(raw) > 0 {
		var options map[string]any
		if json.Unmarshal(raw, &options) == nil {
			if provider == VideoProviderKling {
				if value, ok := options["video_id"]; ok {
					addSafeVideoRecoveryField(payload, "video_id", value)
				}
			}
			for _, name := range []string{"seed", "watermark", "camera_fixed"} {
				if value, ok := options[name]; ok {
					addSafeVideoRecoveryField(payload, name, value)
				}
			}
		}
	}
	minimized, err := NewMinimizedVideoPayload(payload)
	if err != nil {
		return MinimizedVideoPayload{}
	}
	return minimized
}

// minimizedVideoPollPayload retains only the routing discriminator needed to
// query an accepted asynchronous task. Submission recovery data, including
// prompts, assets, and provider video IDs, must not outlive acceptance.
func minimizedVideoPollPayload(provider string, request CanonicalVideoRequest) MinimizedVideoPayload {
	if provider != VideoProviderKling {
		return MinimizedVideoPayload{}
	}
	kind, err := klingProviderTaskKind(request)
	if err != nil {
		return MinimizedVideoPayload{}
	}
	payload, err := NewMinimizedVideoPayload(map[string]any{"provider_task_kind": kind})
	if err != nil {
		return MinimizedVideoPayload{}
	}
	return payload
}

func addSafeVideoRecoveryAsset(payload map[string]any, field string, assets []VideoAsset) {
	if len(assets) == 0 {
		return
	}
	value := strings.TrimSpace(assets[0].URL)
	if value == "" || strings.HasPrefix(strings.ToLower(value), "data:") || strings.ContainsAny(value, "?&#") {
		return
	}
	addSafeVideoRecoveryField(payload, field, value)
}

func addSafeVideoRecoveryField(payload map[string]any, field string, value any) {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return
		}
		value = strings.TrimSpace(typed)
	}
	if _, err := NewMinimizedVideoPayload(map[string]any{field: value}); err == nil {
		payload[field] = value
	}
}

func videoSubmissionIsAmbiguous(err error) bool {
	var value VideoProviderError
	if errors.As(err, &value) {
		return value.Ambiguous
	}
	var pointer *VideoProviderError
	if errors.As(err, &pointer) && pointer != nil {
		return pointer.Ambiguous
	}
	// An unclassified transport error does not prove that the provider rejected
	// the request. Default to the duplicate-safe ambiguous path.
	return true
}

func videoTaskErrorForSubmission(err error) VideoTaskError {
	var value VideoProviderError
	if errors.As(err, &value) {
		return NewVideoTaskError(value.Code, "", value.Retryable)
	}
	var pointer *VideoProviderError
	if errors.As(err, &pointer) && pointer != nil {
		return NewVideoTaskError(pointer.Code, "", pointer.Retryable)
	}
	return NewVideoTaskError("SUBMISSION_FAILED", "", false)
}

func videoTaskErrorForAmbiguousSubmission(err error) VideoTaskError {
	var value VideoProviderError
	if errors.As(err, &value) {
		return NewVideoTaskError(value.Code, "", value.Retryable)
	}
	var pointer *VideoProviderError
	if errors.As(err, &pointer) && pointer != nil {
		return NewVideoTaskError(pointer.Code, "", pointer.Retryable)
	}
	return NewVideoTaskError("SUBMISSION_STATE_UNKNOWN", "", true)
}

func videoStringPointer(value string) *string {
	return &value
}
