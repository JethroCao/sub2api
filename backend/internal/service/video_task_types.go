package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const MaxVideoTaskPayloadBytes = 64 * 1024

type VideoTaskStatus string

const (
	VideoTaskCreated    VideoTaskStatus = "created"
	VideoTaskSubmitting VideoTaskStatus = "submitting"
	VideoTaskSubmitted  VideoTaskStatus = "submitted"
	VideoTaskQueued     VideoTaskStatus = "queued"
	VideoTaskRunning    VideoTaskStatus = "running"
	VideoTaskSucceeded  VideoTaskStatus = "succeeded"
	VideoTaskFailed     VideoTaskStatus = "failed"
	VideoTaskCancelled  VideoTaskStatus = "cancelled"
	VideoTaskUnknown    VideoTaskStatus = "unknown"
)

var (
	ErrVideoTaskNotFound            = infraerrors.New(http.StatusNotFound, "VIDEO_TASK_NOT_FOUND", "video task not found")
	ErrVideoIdempotencyConflict     = infraerrors.Conflict("VIDEO_IDEMPOTENCY_CONFLICT", "idempotency key reused with a different video request")
	ErrVideoTaskVersionConflict     = infraerrors.Conflict("VIDEO_TASK_VERSION_CONFLICT", "video task was changed by another worker")
	ErrVideoTaskLeaseConflict       = infraerrors.Conflict("VIDEO_TASK_LEASE_CONFLICT", "video task lease is not owned by this worker")
	ErrVideoTaskInvalidTransition   = infraerrors.Conflict("VIDEO_TASK_INVALID_TRANSITION", "invalid video task status transition")
	ErrVideoTaskInvalidRequest      = infraerrors.BadRequest("VIDEO_TASK_INVALID_REQUEST", "invalid video task request")
	ErrVideoTaskUnsafePayload       = infraerrors.BadRequest("VIDEO_TASK_UNSAFE_PAYLOAD", "video task payload contains credentials or binary media")
	ErrVideoTaskPayloadTooLarge     = infraerrors.BadRequest("VIDEO_TASK_PAYLOAD_TOO_LARGE", "video task payload exceeds the durable ledger limit")
	ErrVideoTaskAmountExceedsFrozen = infraerrors.Conflict("VIDEO_TASK_AMOUNT_EXCEEDS_FROZEN", "video task settled amount exceeds the frozen amount")
)

// MinimizedVideoPayload is the only payload value accepted by the durable task
// repository. Its bytes are private so callers cannot bypass size and secret checks.
type MinimizedVideoPayload struct {
	data []byte
}

func NewMinimizedVideoPayload(value any) (MinimizedVideoPayload, error) {
	if value == nil {
		return MinimizedVideoPayload{}, nil
	}
	if containsBinaryVideoValue(reflect.ValueOf(value), make(map[uintptr]struct{})) {
		return MinimizedVideoPayload{}, ErrVideoTaskUnsafePayload
	}
	b, err := json.Marshal(value)
	if err != nil {
		return MinimizedVideoPayload{}, ErrVideoTaskInvalidRequest.WithCause(err)
	}
	if len(b) > MaxVideoTaskPayloadBytes {
		return MinimizedVideoPayload{}, ErrVideoTaskPayloadTooLarge
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		return MinimizedVideoPayload{}, ErrVideoTaskInvalidRequest.WithCause(err)
	}
	if containsUnsafeVideoJSON(decoded) {
		return MinimizedVideoPayload{}, ErrVideoTaskUnsafePayload
	}
	return MinimizedVideoPayload{data: append([]byte(nil), b...)}, nil
}

func (p MinimizedVideoPayload) Bytes() []byte {
	return append([]byte(nil), p.data...)
}

func NewVideoRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "vid_" + hex.EncodeToString(value[:]), nil
}

type VideoTaskError struct {
	Code      string
	Message   string
	Retryable bool
}

type VideoTask struct {
	ID                      int64
	RequestID               string
	UserID                  int64
	APIKeyID                int64
	SubscriptionID          *int64
	GroupID                 int64
	AccountID               int64
	Platform                string
	Provider                string
	Operation               string
	ExternalModel           string
	UpstreamModel           string
	IdempotencyKeyHash      string
	RequestHash             string
	ProviderSubmissionToken *string
	RequestPayload          []byte
	Status                  VideoTaskStatus
	UpstreamTaskID          *string
	UpstreamStatus          *string
	ResultURL               *string
	ResultURLExpiresAt      *time.Time
	ResultContentType       *string
	ResultDurationSeconds   *float64
	ResultWidth             *int
	ResultHeight            *int
	PricingUnit             string
	UnitPrice               float64
	EstimatedUnits          float64
	EstimatedAmount         float64
	FrozenAmount            float64
	SettledAmount           *float64
	Currency                string
	BillingMode             string
	BillingStatus           string
	BillingReference        *string
	SubmissionAttempts      int
	PollAttempts            int
	SettlementAttempts      int
	NextPollAt              *time.Time
	LeaseOwner              *string
	LeaseExpiresAt          *time.Time
	LastErrorCode           *string
	LastErrorMessage        *string
	LastErrorRetryable      bool
	Version                 int64
	CreatedAt               time.Time
	UpdatedAt               time.Time
	SubmittedAt             *time.Time
	StartedAt               *time.Time
	FinishedAt              *time.Time
	SettledAt               *time.Time
}

type CreateVideoTaskParams struct {
	UserID             int64
	APIKeyID           int64
	SubscriptionID     *int64
	GroupID            int64
	AccountID          int64
	Platform           string
	Provider           string
	Operation          string
	ExternalModel      string
	UpstreamModel      string
	IdempotencyKeyHash string
	RequestHash        string
	RequestPayload     MinimizedVideoPayload
	PricingUnit        string
	UnitPrice          float64
	EstimatedUnits     float64
	EstimatedAmount    float64
	FrozenAmount       float64
	Currency           string
	BillingMode        string
	BillingStatus      string
}

type MarkVideoSubmittedParams struct {
	RequestID       string
	ExpectedVersion int64
	UpstreamTaskID  string
	UpstreamStatus  string
	NextPollAt      *time.Time
	SubmittedAt     time.Time
}

type ApplyVideoPollResultParams struct {
	RequestID             string
	ExpectedVersion       int64
	LeaseOwner            string
	Status                VideoTaskStatus
	UpstreamStatus        string
	NextPollAt            *time.Time
	ResultURL             *string
	ResultURLExpiresAt    *time.Time
	ResultContentType     *string
	ResultDurationSeconds *float64
	ResultWidth           *int
	ResultHeight          *int
	Error                 *VideoTaskError
	StartedAt             *time.Time
	FinishedAt            *time.Time
	UpdatedAt             time.Time
}

type MarkVideoSettledParams struct {
	RequestID        string
	ExpectedVersion  int64
	SettledAmount    float64
	BillingStatus    string
	BillingReference *string
	SettledAt        time.Time
}

type VideoTaskEvent struct {
	ID        int64
	RequestID string
	EventType string
	Payload   MinimizedVideoPayload
	CreatedAt time.Time
}

type VideoTaskListQuery struct {
	RequestID     string
	UserID        *int64
	APIKeyID      *int64
	AccountID     *int64
	Status        VideoTaskStatus
	Provider      string
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	Limit         int
	Offset        int
}

func IsVideoTaskStatus(status VideoTaskStatus) bool {
	switch status {
	case VideoTaskCreated, VideoTaskSubmitting, VideoTaskSubmitted, VideoTaskQueued,
		VideoTaskRunning, VideoTaskSucceeded, VideoTaskFailed, VideoTaskCancelled, VideoTaskUnknown:
		return true
	default:
		return false
	}
}

func IsTerminalVideoTaskStatus(status VideoTaskStatus) bool {
	switch status {
	case VideoTaskSucceeded, VideoTaskFailed, VideoTaskCancelled:
		return true
	default:
		return false
	}
}

func CanApplyVideoPollStatus(from, to VideoTaskStatus) bool {
	if IsTerminalVideoTaskStatus(from) || !IsVideoTaskStatus(to) {
		return false
	}
	switch to {
	case VideoTaskQueued, VideoTaskRunning, VideoTaskSucceeded, VideoTaskFailed, VideoTaskCancelled, VideoTaskUnknown:
		return from == VideoTaskSubmitted || from == VideoTaskQueued || from == VideoTaskRunning || from == VideoTaskUnknown
	default:
		return false
	}
}

func containsUnsafeVideoJSON(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
			switch normalized {
			case "authorization", "proxyauthorization", "apikey", "token", "accesstoken", "refreshtoken",
				"password", "secret", "secretkey", "accesskey", "cookie", "setcookie", "credential", "credentials":
				return true
			}
			if containsUnsafeVideoJSON(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsUnsafeVideoJSON(child) {
				return true
			}
		}
	case string:
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(typed)), "data:")
	}
	return false
}

func containsBinaryVideoValue(value reflect.Value, seen map[uintptr]struct{}) bool {
	if !value.IsValid() {
		return false
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return false
		}
		if value.Kind() == reflect.Pointer {
			ptr := value.Pointer()
			if _, ok := seen[ptr]; ok {
				return false
			}
			seen[ptr] = struct{}{}
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return true
		}
		for i := 0; i < value.Len(); i++ {
			if containsBinaryVideoValue(value.Index(i), seen) {
				return true
			}
		}
	case reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return true
		}
		for i := 0; i < value.Len(); i++ {
			if containsBinaryVideoValue(value.Index(i), seen) {
				return true
			}
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			if containsBinaryVideoValue(iter.Value(), seen) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).PkgPath == "" && containsBinaryVideoValue(value.Field(i), seen) {
				return true
			}
		}
	}
	return false
}
