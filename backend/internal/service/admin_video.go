package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

var (
	ErrVideoPricingRuleInvalid      = infraerrors.BadRequest("VIDEO_PRICING_RULE_INVALID", "invalid video pricing rule")
	ErrVideoPricingRuleOverlap      = infraerrors.Conflict("VIDEO_PRICING_RULE_OVERLAP", "video pricing rules overlap")
	ErrVideoPricingCoverage         = infraerrors.BadRequest("VIDEO_PRICING_COVERAGE_INCOMPLETE", "video pricing rules do not cover an enabled capability")
	ErrVideoFinancialStateConflict  = infraerrors.Conflict("VIDEO_FINANCIAL_STATE_CONFLICT", "video task state does not permit this action")
	ErrVideoResultURLInvalid        = infraerrors.BadRequest("VIDEO_RESULT_URL_INVALID", "video result URL is invalid")
	ErrAdminVideoIdempotencyKey     = infraerrors.BadRequest("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required")
	ErrAdminVideoURLHashUnavailable = infraerrors.ServiceUnavailable("VIDEO_RESULT_URL_HASH_UNAVAILABLE", "video result URL hashing is unavailable")
)

const adminVideoReasonMaxBytes = 512

type VideoPricingRuleInput struct {
	ExternalModel    string   `json:"external_model"`
	Operation        string   `json:"operation"`
	Resolution       string   `json:"resolution"`
	AudioMode        string   `json:"audio_mode"`
	Unit             string   `json:"unit"`
	UnitPrice        float64  `json:"unit_price"`
	UpstreamUnitCost *float64 `json:"upstream_unit_cost,omitempty"`
	Enabled          bool     `json:"enabled"`
}

type AdminVideoTaskDetail struct {
	Task             VideoTask        `json:"task"`
	Events           []VideoTaskEvent `json:"events"`
	ResultURLSummary string           `json:"result_url_summary,omitempty"`
}

type AdminVideoActionResult struct {
	Task     VideoTask `json:"task"`
	Replayed bool      `json:"replayed"`
}

type AdminVideoActionMetadata struct {
	ActorUserID        int64
	AuditRequestID     string
	Reason             string
	IdempotencyKeyHash string
	RequestHash        string
}

type AdminVideoReconcileMutation struct {
	AdminVideoActionMetadata
	RequestID string
	Now       time.Time
}

type AdminVideoRefundMutation struct {
	AdminVideoActionMetadata
	RequestID string
}

type AdminVideoCompleteMutation struct {
	AdminVideoActionMetadata
	RequestID             string
	ProviderTaskID        string
	ResultURL             string
	ResultURLAuditSummary string
	DurationSeconds       float64
	Resolution            string
	FinalAmount           float64
	StoredUnitPrice       float64
}

type AdminVideoReconcileCommand struct {
	ActorUserID    int64     `json:"-"`
	AuditRequestID string    `json:"-"`
	Reason         string    `json:"reason"`
	IdempotencyKey string    `json:"-"`
	Now            time.Time `json:"-"`
}

type AdminVideoRefundCommand struct {
	ActorUserID    int64  `json:"-"`
	AuditRequestID string `json:"-"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"-"`
}

type AdminVideoCompleteCommand struct {
	ActorUserID     int64   `json:"-"`
	AuditRequestID  string  `json:"-"`
	Reason          string  `json:"reason"`
	IdempotencyKey  string  `json:"-"`
	ProviderTaskID  string  `json:"provider_task_id"`
	ResultURL       string  `json:"result_url"`
	DurationSeconds float64 `json:"duration_seconds"`
	Resolution      string  `json:"resolution"`
	FinalAmount     float64 `json:"final_amount"`
}

type AdminVideoRepository interface {
	ListPricingRules(context.Context, int64) ([]VideoPricingRule, error)
	ReplacePricingRules(context.Context, int64, []VideoPricingRuleInput) ([]VideoPricingRule, error)
	ListTasks(context.Context, VideoTaskListQuery) ([]VideoTask, int, error)
	GetTask(context.Context, string) (*AdminVideoTaskDetail, error)
	ReplayAction(context.Context, string, string, AdminVideoActionMetadata) (*AdminVideoActionResult, bool, error)
	Reconcile(context.Context, AdminVideoReconcileMutation) (*AdminVideoActionResult, error)
	Refund(context.Context, AdminVideoRefundMutation) (*AdminVideoActionResult, error)
	Complete(context.Context, AdminVideoCompleteMutation) (*AdminVideoActionResult, error)
}

type AdminVideoResultURLValidator interface {
	Validate(context.Context, string) (normalized string, auditSummary string, err error)
}

type AdminVideoResultURLValidatorFunc func(context.Context, string) (string, string, error)

type AdminVideoURLHashKey []byte

func (f AdminVideoResultURLValidatorFunc) Validate(ctx context.Context, raw string) (string, string, error) {
	return f(ctx, raw)
}

type AdminVideoService struct {
	repo       AdminVideoRepository
	catalog    VideoCapabilityCatalog
	urlChecker AdminVideoResultURLValidator
	urlHashKey AdminVideoURLHashKey
}

type videoRefundFailureKey struct {
	Provider  string
	Model     string
	Operation string
	GroupID   int64
	Reason    string
}

var adminVideoRefundFailures = struct {
	sync.Mutex
	counts map[videoRefundFailureKey]int64
}{counts: make(map[videoRefundFailureKey]int64)}

func NewAdminVideoService(repo AdminVideoRepository, catalog VideoCapabilityCatalog, checker AdminVideoResultURLValidator, urlHashKey AdminVideoURLHashKey) *AdminVideoService {
	if checker == nil {
		checker = AdminVideoResultURLValidatorFunc(validateAdminVideoResultURL)
	}
	return &AdminVideoService{repo: repo, catalog: catalog, urlChecker: checker, urlHashKey: append(AdminVideoURLHashKey(nil), urlHashKey...)}
}

func (s *AdminVideoService) ListPricingRules(ctx context.Context, groupID int64) ([]VideoPricingRule, error) {
	if s == nil || s.repo == nil || groupID <= 0 {
		return nil, ErrVideoPricingRuleInvalid
	}
	return s.repo.ListPricingRules(ctx, groupID)
}

func (s *AdminVideoService) ReplacePricingRules(ctx context.Context, groupID int64, inputs []VideoPricingRuleInput) error {
	if s == nil || s.repo == nil || groupID <= 0 || len(inputs) > 1000 {
		return ErrVideoPricingRuleInvalid
	}
	normalized := make([]VideoPricingRuleInput, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	coverage := make(map[string]bool)
	for i, input := range inputs {
		input.ExternalModel = strings.TrimSpace(input.ExternalModel)
		input.Operation = strings.ToLower(strings.TrimSpace(input.Operation))
		input.Resolution = strings.ToLower(strings.TrimSpace(input.Resolution))
		input.AudioMode = strings.ToLower(strings.TrimSpace(input.AudioMode))
		input.Unit = strings.ToLower(strings.TrimSpace(input.Unit))
		if input.Resolution == "" {
			input.Resolution = videoPricingResolutionAny
		}
		if input.AudioMode == "" {
			input.AudioMode = videoPricingAudioAny
		}
		if !validAdminVideoPricingRule(input) {
			return ErrVideoPricingRuleInvalid
		}
		dimension := strings.Join([]string{input.ExternalModel, input.Operation, input.Resolution, input.AudioMode}, "\x00")
		if _, exists := seen[dimension]; exists {
			return ErrVideoPricingRuleOverlap
		}
		seen[dimension] = struct{}{}
		if input.Enabled {
			provider := providerForAdminVideoModel(input.ExternalModel)
			capabilities, ok := s.catalog[VideoModelCapabilityKey(provider, input.ExternalModel)]
			operation := VideoOperation(input.Operation)
			capability, operationOK := capabilities[operation]
			if !ok || !operationOK || (operation == VideoOperationEdit && !capability.Edit) || (operation == VideoOperationExtension && !capability.Extension) {
				return ErrVideoPricingCoverage
			}
			if input.Resolution != "*" && len(capability.Resolutions) > 0 && !containsVideoCapabilityValue(capability.Resolutions, input.Resolution) {
				return ErrVideoPricingRuleInvalid
			}
			key := input.ExternalModel + "\x00" + input.Operation
			if input.Resolution == "*" && input.AudioMode == "any" {
				coverage[key] = true
			} else if _, exists := coverage[key]; !exists {
				coverage[key] = false
			}
		}
		normalized[i] = input
	}
	for _, covered := range coverage {
		if !covered {
			return ErrVideoPricingCoverage
		}
	}
	_, err := s.repo.ReplacePricingRules(ctx, groupID, normalized)
	return err
}

func validAdminVideoPricingRule(input VideoPricingRuleInput) bool {
	if input.ExternalModel == "" || len(input.ExternalModel) > 128 || strings.ContainsRune(input.ExternalModel, '\x00') {
		return false
	}
	if input.Operation != string(VideoOperationGeneration) && input.Operation != string(VideoOperationEdit) && input.Operation != string(VideoOperationExtension) {
		return false
	}
	if input.Resolution != "*" && input.Resolution != "480p" && input.Resolution != "720p" && input.Resolution != "1080p" {
		return false
	}
	if input.AudioMode != "any" && input.AudioMode != "with_audio" && input.AudioMode != "without_audio" {
		return false
	}
	if input.Unit != "per_request" && input.Unit != "per_output_second" {
		return false
	}
	if !finiteNonNegative(input.UnitPrice) {
		return false
	}
	return input.UpstreamUnitCost == nil || finiteNonNegative(*input.UpstreamUnitCost)
}

func providerForAdminVideoModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "seedance-"):
		return VideoProviderSeedance
	case strings.HasPrefix(model, "kling-"):
		return VideoProviderKling
	case strings.HasPrefix(model, "grok-"):
		return PlatformGrok
	default:
		return ""
	}
}

func (s *AdminVideoService) ListTasks(ctx context.Context, query VideoTaskListQuery) ([]VideoTask, int, error) {
	if s == nil || s.repo == nil || query.Limit < 0 || query.Limit > 200 || query.Offset < 0 || query.Offset > 1_000_000 {
		return nil, 0, ErrVideoTaskInvalidRequest
	}
	return s.repo.ListTasks(ctx, query)
}

func (s *AdminVideoService) GetTask(ctx context.Context, requestID string) (*AdminVideoTaskDetail, error) {
	if s == nil || s.repo == nil || !IsVideoRequestID(requestID) {
		return nil, ErrVideoTaskInvalidRequest
	}
	return s.repo.GetTask(ctx, requestID)
}

func (s *AdminVideoService) Reconcile(ctx context.Context, requestID string, command AdminVideoReconcileCommand) (*AdminVideoActionResult, error) {
	if s == nil || s.repo == nil || !IsVideoRequestID(requestID) {
		return nil, ErrVideoTaskInvalidRequest
	}
	metadata, err := buildAdminVideoActionMetadata(command.ActorUserID, command.AuditRequestID, command.Reason, command.IdempotencyKey, map[string]any{
		"action": "reconcile", "request_id": requestID, "reason": strings.TrimSpace(command.Reason),
	})
	if err != nil {
		return nil, err
	}
	return s.repo.Reconcile(ctx, AdminVideoReconcileMutation{AdminVideoActionMetadata: metadata, RequestID: requestID})
}

func (s *AdminVideoService) Refund(ctx context.Context, requestID string, command AdminVideoRefundCommand) (*AdminVideoActionResult, error) {
	detail, err := s.GetTask(ctx, requestID)
	if err != nil {
		return nil, err
	}
	metadata, err := buildAdminVideoActionMetadata(command.ActorUserID, command.AuditRequestID, command.Reason, command.IdempotencyKey, map[string]any{"action": "refund", "request_id": requestID, "reason": strings.TrimSpace(command.Reason)})
	if err != nil {
		return nil, err
	}
	if detail.Task.BillingStatus != "held" || detail.Task.SettledAt != nil || (detail.Task.Status != VideoTaskUnknown && detail.Task.Status != VideoTaskFailed) {
		if replay, found, replayErr := s.repo.ReplayAction(ctx, requestID, "refund", metadata); replayErr != nil || found {
			return replay, replayErr
		}
		s.recordRefundFailure(detail.Task, "state_conflict")
		return nil, ErrVideoFinancialStateConflict
	}
	result, err := s.repo.Refund(ctx, AdminVideoRefundMutation{AdminVideoActionMetadata: metadata, RequestID: requestID})
	if err != nil {
		reason := "repository_error"
		if infraerrors.Code(err) == http.StatusConflict {
			reason = "conflict"
		}
		s.recordRefundFailure(detail.Task, reason)
	}
	return result, err
}

func (s *AdminVideoService) recordRefundFailure(task VideoTask, reason string) {
	configuredModels := make(map[string]struct{})
	if s != nil {
		if _, ok := s.catalog[VideoModelCapabilityKey(task.Provider, task.ExternalModel)]; ok {
			configuredModels[task.ExternalModel] = struct{}{}
		}
	}
	dimensions := normalizeVideoMetricDimensions(task.Provider, task.ExternalModel, task.Operation, task.GroupID, configuredModels)
	key := videoRefundFailureKey{
		Provider: dimensions.Provider, Model: dimensions.Model, Operation: dimensions.Operation,
		GroupID: dimensions.GroupID, Reason: reason,
	}
	adminVideoRefundFailures.Lock()
	adminVideoRefundFailures.counts[key]++
	adminVideoRefundFailures.Unlock()
}

func snapshotAdminVideoRefundFailures() map[videoRefundFailureKey]int64 {
	adminVideoRefundFailures.Lock()
	defer adminVideoRefundFailures.Unlock()
	result := make(map[videoRefundFailureKey]int64, len(adminVideoRefundFailures.counts))
	for key, count := range adminVideoRefundFailures.counts {
		result[key] = count
	}
	return result
}

func acknowledgeAdminVideoRefundFailures(snapshot map[videoRefundFailureKey]int64) {
	adminVideoRefundFailures.Lock()
	defer adminVideoRefundFailures.Unlock()
	for key, count := range snapshot {
		remaining := adminVideoRefundFailures.counts[key] - count
		if remaining > 0 {
			adminVideoRefundFailures.counts[key] = remaining
		} else {
			delete(adminVideoRefundFailures.counts, key)
		}
	}
}

func (s *AdminVideoService) Complete(ctx context.Context, requestID string, command AdminVideoCompleteCommand) (*AdminVideoActionResult, error) {
	detail, err := s.GetTask(ctx, requestID)
	if err != nil {
		return nil, err
	}
	command.ProviderTaskID = strings.TrimSpace(command.ProviderTaskID)
	command.Resolution = strings.ToLower(strings.TrimSpace(command.Resolution))
	if command.ProviderTaskID == "" || len(command.ProviderTaskID) > 255 || command.ProviderTaskID == requestID || len(command.ResultURL) > 4096 || !finitePositive(command.DurationSeconds) ||
		(command.Resolution != "480p" && command.Resolution != "720p" && command.Resolution != "1080p") ||
		!finiteNonNegative(command.FinalAmount) || command.FinalAmount > detail.Task.FrozenAmount {
		if finiteNonNegative(command.FinalAmount) && command.FinalAmount > detail.Task.FrozenAmount {
			return nil, ErrVideoFinalCostExceedsHold
		}
		return nil, ErrVideoTaskInvalidRequest
	}
	normalizedURL, _, err := s.urlChecker.Validate(ctx, command.ResultURL)
	if err != nil {
		return nil, ErrVideoResultURLInvalid
	}
	summary := SafeAdminVideoResultURLSummary(normalizedURL)
	if summary == "" {
		return nil, ErrVideoResultURLInvalid
	}
	resultURLDigest, err := adminVideoResultURLDigest(s.urlHashKey, normalizedURL)
	if err != nil {
		return nil, err
	}
	metadata, err := buildAdminVideoActionMetadata(command.ActorUserID, command.AuditRequestID, command.Reason, command.IdempotencyKey, map[string]any{
		"action": "complete", "request_id": requestID, "provider_task_id": command.ProviderTaskID,
		"result_url_digest": resultURLDigest, "duration_seconds": command.DurationSeconds,
		"resolution": command.Resolution, "final_amount": command.FinalAmount,
	})
	if err != nil {
		return nil, err
	}
	if detail.Task.BillingStatus != "held" || detail.Task.SettledAt != nil || (detail.Task.Status != VideoTaskUnknown && detail.Task.Status != VideoTaskFailed) {
		if replay, found, replayErr := s.repo.ReplayAction(ctx, requestID, "complete", metadata); replayErr != nil || found {
			return replay, replayErr
		}
		return nil, ErrVideoFinancialStateConflict
	}
	return s.repo.Complete(ctx, AdminVideoCompleteMutation{
		AdminVideoActionMetadata: metadata, RequestID: requestID, ProviderTaskID: command.ProviderTaskID,
		ResultURL: normalizedURL, ResultURLAuditSummary: summary, DurationSeconds: command.DurationSeconds,
		Resolution: command.Resolution, FinalAmount: command.FinalAmount, StoredUnitPrice: detail.Task.UnitPrice,
	})
}

func buildAdminVideoActionMetadata(actor int64, auditRequestID, reason, rawKey string, payload any) (AdminVideoActionMetadata, error) {
	reason = strings.TrimSpace(reason)
	if actor <= 0 || reason == "" || len(reason) > adminVideoReasonMaxBytes || containsVideoCredential(reason) {
		return AdminVideoActionMetadata{}, ErrVideoTaskInvalidRequest
	}
	key, err := NormalizeIdempotencyKey(rawKey)
	if err != nil {
		return AdminVideoActionMetadata{}, err
	}
	if key == "" {
		return AdminVideoActionMetadata{}, ErrAdminVideoIdempotencyKey
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return AdminVideoActionMetadata{}, ErrVideoTaskInvalidRequest
	}
	sum := sha256.Sum256(encoded)
	return AdminVideoActionMetadata{
		ActorUserID: actor, AuditRequestID: strings.TrimSpace(auditRequestID), Reason: reason,
		IdempotencyKeyHash: HashIdempotencyKey(key), RequestHash: hex.EncodeToString(sum[:]),
	}, nil
}

func validateAdminVideoResultURL(_ context.Context, raw string) (string, string, error) {
	normalized, err := urlvalidator.ValidateHTTPSURL(strings.TrimSpace(raw), urlvalidator.ValidationOptions{AllowPrivate: false})
	if err != nil {
		return "", "", err
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.User != nil || strings.TrimSpace(parsed.Hostname()) == "" {
		return "", "", ErrVideoResultURLInvalid
	}
	if err := urlvalidator.ValidateResolvedIP(parsed.Hostname()); err != nil {
		return "", "", err
	}
	return normalized, SafeAdminVideoResultURLSummary(normalized), nil
}

func adminVideoResultURLDigest(key AdminVideoURLHashKey, normalized string) (string, error) {
	if len(key) < sha256.Size || strings.TrimSpace(normalized) == "" {
		return "", ErrAdminVideoURLHashUnavailable
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(normalized))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func SafeAdminVideoResultURLSummary(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil || parsed.Scheme == "" || strings.TrimSpace(parsed.Hostname()) == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + parsed.Host + "/video-result"
}

func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func finitePositive(value float64) bool { return value > 0 && finiteNonNegative(value) }
