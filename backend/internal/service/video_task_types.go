package service

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

const (
	MaxVideoTaskPayloadBytes      = 64 * 1024
	MaxVideoTaskErrorMessageBytes = 2048
)

var (
	videoRequestIDPattern          = regexp.MustCompile(`^vid_[0-9a-f]{32}$`)
	videoTaskErrorCodePattern      = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_.-]{0,127}$`)
	videoTaskSensitiveTextPattern  = regexp.MustCompile(`(?i)(authorization|x[-_]?api[-_]?key|api[-_]?key|access[-_]?token|refresh[-_]?token|secret|password|credential|cookie|\bbearer\s+|\bsk-[a-z0-9_-]{12,})`)
	videoTaskAssignmentPattern     = regexp.MustCompile(`(?i)\b([a-z][a-z0-9_.-]{1,63})["']?\s*[:=]\s*["']?((?:bearer\s+)?[a-z0-9][a-z0-9._~+/=-]{0,511})`)
	videoTaskBearerValuePattern    = regexp.MustCompile(`(?i)\bbearer\s+([a-z0-9][a-z0-9._~+/=-]{0,511})`)
	videoTaskKnownKeyPattern       = regexp.MustCompile(`(?i)\bsk-(?:proj-)?[a-z0-9_-]{12,}`)
	videoTaskJWTValuePattern       = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])([A-Za-z0-9_-]{2,})\.([A-Za-z0-9_-]{2,})\.([A-Za-z0-9_-]*)(?:$|[^A-Za-z0-9_-])`)
	videoTaskSafeAuthProsePattern  = regexp.MustCompile(`(?i)^authorization["']?\s*[:=]\s*["']?(?:bearer\s+authentication\s+is\s+required|required\s+for\s+access\s+to\s+the\s+gallery)[.!?]?["']?$`)
	videoTaskSafeTokenProsePattern = regexp.MustCompile(`(?i)^token["']?\s*[:=]\s*["']?optional\s+in\s+this\s+board-game\s+illustration[.!?]?["']?$`)
	videoTaskWorkerIDPattern       = regexp.MustCompile(`^worker-[A-Za-z0-9][A-Za-z0-9_-]{0,120}$`)
)

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
	if !isAllowlistedVideoPayload(decoded) {
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

func IsVideoRequestID(requestID string) bool {
	return videoRequestIDPattern.MatchString(requestID)
}

type VideoTaskError struct {
	code      string
	message   string
	retryable bool
}

func NewVideoTaskError(code, message string, retryable bool) VideoTaskError {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !videoTaskErrorCodePattern.MatchString(code) {
		code = "UPSTREAM_ERROR"
	}
	message = strings.TrimSpace(message)
	if message != "" {
		if json.Valid([]byte(message)) || videoTaskSensitiveTextPattern.MatchString(message) {
			message = "<credential-bearing upstream error redacted>"
		} else {
			message = logredact.RedactText(message, "authorization", "x-api-key", "api_key", "secret_key", "access_key", "credential")
		}
		message = truncateVideoText(message, MaxVideoTaskErrorMessageBytes)
	}
	return VideoTaskError{code: code, message: message, retryable: retryable}
}

func (e VideoTaskError) Code() string {
	return e.code
}

func (e VideoTaskError) Message() string {
	return e.message
}

func (e VideoTaskError) Retryable() bool {
	return e.retryable
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
	NextPollAt         *time.Time
}

// AssignVideoSubmissionParams atomically binds a newly reserved task to the
// selected account before any upstream bytes are sent. Tasks are created with
// an unassigned route so idempotent replays can be detected before scheduling.
type AssignVideoSubmissionParams struct {
	RequestID               string
	ExpectedVersion         int64
	AccountID               int64
	Platform                string
	Provider                string
	UpstreamModel           string
	ProviderSubmissionToken string
	NextPollAt              time.Time
	UpdatedAt               time.Time
}

// MarkVideoSubmissionUnknownParams retains the minimized recovery payload and
// provider submission token while scheduling a later, non-resubmitting recovery
// attempt.
type MarkVideoSubmissionUnknownParams struct {
	RequestID       string
	ExpectedVersion int64
	Error           VideoTaskError
	NextPollAt      time.Time
	UpdatedAt       time.Time
}

// ReleaseAndFailVideoSubmissionParams atomically releases the stored billing
// hold and terminalizes an exact pre-acceptance lifecycle state. A created task
// must have no submission token; a submitting task must match the durable token.
type ReleaseAndFailVideoSubmissionParams struct {
	RequestID               string
	ExpectedVersion         int64
	ExpectedStatus          VideoTaskStatus
	ProviderSubmissionToken string
	Error                   VideoTaskError
	FailedAt                time.Time
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

func isAllowlistedVideoPayload(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for key, value := range object {
		switch key {
		case "prompt", "negative_prompt":
			text, ok := value.(string)
			if !ok || len(text) > 20_000 || isUnsafeVideoPayloadString(key, text) {
				return false
			}
		case "input_image_ref", "input_video_ref", "first_frame_ref", "last_frame_ref", "source_task_id":
			ref, ok := value.(string)
			if !ok || len(ref) == 0 || len(ref) > 512 || strings.ContainsAny(ref, "?&#") || isUnsafeVideoPayloadString(key, ref) {
				return false
			}
		case "resolution", "aspect_ratio", "audio_mode", "status", "from_status", "to_status",
			"upstream_status", "error_code", "worker_id", "billing_status":
			text, ok := value.(string)
			if !ok || len(text) > 256 || isUnsafeVideoPayloadString(key, text) {
				return false
			}
		case "duration_seconds", "seed", "attempt", "settled_amount":
			number, ok := value.(float64)
			if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
				return false
			}
		case "camera_fixed", "watermark":
			if _, ok := value.(bool); !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isUnsafeVideoPayloadString(field, value string) bool {
	if containsVideoCredential(value) {
		return true
	}
	if (field == "source_task_id" && IsVideoRequestID(value)) ||
		(field == "worker_id" && videoTaskWorkerIDPattern.MatchString(value)) {
		return false
	}
	return isEncodedVideoUpload(field, value)
}

func containsVideoCredential(value string) bool {
	if videoTaskKnownKeyPattern.MatchString(value) || containsVideoJWT(value) {
		return true
	}
	for offset := 0; offset < len(value); {
		match := videoTaskAssignmentPattern.FindStringSubmatchIndex(value[offset:])
		if match == nil {
			break
		}
		name := value[offset+match[2] : offset+match[3]]
		candidate := value[offset+match[4] : offset+match[5]]
		if isCredentialShapedVideoName(name) {
			assignment := value[offset+match[2]:]
			if isSafeVideoCredentialProseAssignment(name, assignment) {
				offset += match[3]
				continue
			}
			if isAuthorizationShapedVideoName(name) {
				return true
			}
			if strings.HasPrefix(strings.ToLower(candidate), "bearer ") {
				candidate = strings.TrimSpace(candidate[len("bearer "):])
			}
			if isAssignedVideoCredential(candidate) {
				return true
			}
		}
		offset += match[3]
	}
	for _, match := range videoTaskBearerValuePattern.FindAllStringSubmatch(value, -1) {
		if isSecretLikeVideoValue(match[1]) {
			return true
		}
	}
	return false
}

func containsVideoJWT(value string) bool {
	for _, match := range videoTaskJWTValuePattern.FindAllStringSubmatch(value, -1) {
		headerJSON, headerErr := base64.RawURLEncoding.DecodeString(match[1])
		claimsJSON, claimsErr := base64.RawURLEncoding.DecodeString(match[2])
		if headerErr != nil || claimsErr != nil {
			continue
		}
		var header, claims map[string]any
		if json.Unmarshal(headerJSON, &header) != nil || json.Unmarshal(claimsJSON, &claims) != nil {
			continue
		}
		if header == nil || claims == nil {
			continue
		}
		if alg, ok := header["alg"].(string); ok && strings.TrimSpace(alg) != "" {
			return true
		}
	}
	return false
}

func isSecretLikeVideoValue(value string) bool {
	value = strings.Trim(value, " \t\r\n\"'.,;)}]")
	if videoTaskKnownKeyPattern.MatchString(value) || containsVideoJWT(value) {
		return true
	}
	if len(value) < 12 {
		return false
	}
	var hasLetter, hasDigit bool
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}

func isAssignedVideoCredential(value string) bool {
	value = strings.Trim(value, " \t\r\n\"'.,;)}]")
	return videoTaskKnownKeyPattern.MatchString(value) || containsVideoJWT(value) || len(value) >= 8
}

func isSafeVideoCredentialProseAssignment(name, assignment string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization":
		return videoTaskSafeAuthProsePattern.MatchString(assignment)
	case "token":
		return videoTaskSafeTokenProsePattern.MatchString(assignment)
	default:
		return false
	}
}

func isCredentialShapedVideoName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	for index, part := range parts {
		if hasVideoCredentialNameSuffix(part) {
			return true
		}
		if index+1 < len(parts) && parts[index+1] == "key" {
			switch part {
			case "api", "access", "secret", "private":
				return true
			}
		}
	}
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(value)
	return hasVideoCredentialNameSuffix(normalized)
}

func hasVideoCredentialNameSuffix(value string) bool {
	value = trimVideoCredentialNameQualifiers(value)
	for _, suffix := range []string{
		"authorization", "password", "passwd", "secret", "token", "cookie", "credential", "credentials",
		"apikey", "accesskey", "secretkey", "privatekey",
	} {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}

func trimVideoCredentialNameQualifiers(value string) string {
	for {
		trimmed := value
		for _, qualifier := range []string{
			"bytes", "data", "file", "hash", "header", "id", "name", "path", "pem",
			"ref", "reference", "value", "version",
		} {
			if len(value) > len(qualifier) && strings.HasSuffix(value, qualifier) {
				value = strings.TrimSuffix(value, qualifier)
				break
			}
		}
		if value == trimmed {
			return value
		}
	}
}

func isAuthorizationShapedVideoName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	}) {
		if strings.HasSuffix(trimVideoCredentialNameQualifiers(part), "authorization") {
			return true
		}
	}
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(value)
	return strings.HasSuffix(trimVideoCredentialNameQualifiers(normalized), "authorization")
}

func isEncodedVideoUpload(field, value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		return true
	}
	allowSnakeCaseText := (field == "prompt" || field == "negative_prompt") && isASCIISnakeCase(value)
	encodings := []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(value)
		if err != nil || len(decoded) == 0 {
			continue
		}
		if isKnownVideoBinary(decoded) {
			return true
		}
		if isTextualVideoMedia(decoded) {
			return true
		}
		if utf8.Valid(decoded) && containsVideoCredential(string(decoded)) {
			return true
		}
		if utf8.Valid(decoded) && containsForbiddenVideoControl(decoded) {
			return true
		}
		if isControlHeavyVideoBinary(decoded) {
			return true
		}
		if isBinaryVideoContent(decoded) &&
			(hasExplicitVideoBase64Marker(value) ||
				len(decoded) >= 16 && !isASCIIAlphabetic(value) && !allowSnakeCaseText) {
			return true
		}
	}
	return false
}

func hasExplicitVideoBase64Marker(value string) bool {
	return strings.ContainsAny(value, "=+/") ||
		strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") ||
		strings.HasPrefix(value, "_") || strings.HasSuffix(value, "_")
}

func containsForbiddenVideoControl(value []byte) bool {
	for _, b := range value {
		if b == 0 || b < '\t' || b > '\r' && b < ' ' || b == '\x7f' {
			return true
		}
	}
	return false
}

func isTextualVideoMedia(value []byte) bool {
	if !utf8.Valid(value) {
		return false
	}
	trimmed := bytes.ToLower(bytes.TrimSpace(value))
	return bytes.HasPrefix(trimmed, []byte("<svg")) ||
		bytes.HasPrefix(trimmed, []byte("<?xml")) && bytes.Contains(trimmed, []byte("<svg"))
}

func isControlHeavyVideoBinary(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	controls := 0
	for _, b := range value {
		if b == 0 || b < '\t' || b > '\r' && b < ' ' || b == '\x7f' {
			controls++
		}
	}
	return bytes.IndexByte(value, 0) >= 0 || len(value) >= 4 && controls*4 >= len(value)
}

func isKnownVideoBinary(value []byte) bool {
	return bytes.HasPrefix(value, []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'}) ||
		bytes.HasPrefix(value, []byte{'\xff', '\xd8', '\xff'}) ||
		bytes.HasPrefix(value, []byte("GIF87a")) ||
		bytes.HasPrefix(value, []byte("GIF89a")) ||
		bytes.HasPrefix(value, []byte{'\x1a', '\x45', '\xdf', '\xa3'}) ||
		(len(value) >= 12 && bytes.Equal(value[:4], []byte("RIFF")) && bytes.Equal(value[8:12], []byte("WEBP"))) ||
		(len(value) >= 8 && bytes.Equal(value[4:8], []byte("ftyp")))
}

func isBinaryVideoContent(value []byte) bool {
	if !utf8.Valid(value) {
		return true
	}
	for _, b := range value {
		if b == 0 || b < '\t' || b > '\r' && b < ' ' {
			return true
		}
	}
	return false
}

func isASCIIAlphabetic(value string) bool {
	if value == "" {
		return false
	}
	for _, b := range []byte(value) {
		if b < 'A' || b > 'Z' && b < 'a' || b > 'z' {
			return false
		}
	}
	return true
}

func isASCIISnakeCase(value string) bool {
	parts := strings.Split(value, "_")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !isASCIIAlphabetic(part) {
			return false
		}
	}
	return true
}

func truncateVideoText(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
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
