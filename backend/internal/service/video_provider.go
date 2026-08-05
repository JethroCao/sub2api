package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"
)

var (
	ErrVideoProviderInvalid   = errors.New("invalid video provider")
	ErrVideoProviderDuplicate = errors.New("duplicate video provider")
)

type VideoSubmitResult struct {
	UpstreamTaskID string
	Status         VideoTaskStatus
	UpstreamStatus string
	NextPollAt     *time.Time
}

type VideoPollResult struct {
	Status                VideoTaskStatus
	UpstreamStatus        string
	ResultURL             string
	LastFrameURL          string
	ResultURLExpiresAt    *time.Time
	ResultContentType     string
	ActualDurationSeconds int
	Resolution            string
	AspectRatio           string
	Width                 int
	Height                int
	Error                 VideoTaskError
	NextPollAt            *time.Time
}

// VideoProviderError contains only public, provider-neutral classification.
// Raw response bodies and credential-bearing upstream errors are deliberately
// not retained by this type.
type VideoProviderError struct {
	HTTPStatus int
	Code       string
	Retryable  bool
	Ambiguous  bool
}

func NewVideoProviderError(httpStatus int, code string, retryable, ambiguous bool, _ error) VideoProviderError {
	if httpStatus < http.StatusBadRequest || httpStatus > 599 {
		httpStatus = http.StatusBadGateway
	}
	return VideoProviderError{
		HTTPStatus: httpStatus,
		Code:       stableVideoProviderErrorCode(code),
		Retryable:  retryable,
		Ambiguous:  ambiguous,
	}
}

func (e VideoProviderError) Error() string {
	return "video provider error: " + stableVideoProviderErrorCode(e.Code)
}

func stableVideoProviderErrorCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	switch code {
	case "invalid_request",
		"invalid_media",
		"unsupported_capability",
		"upstream_authentication",
		"upstream_rate_limit",
		"upstream_unavailable",
		"upstream_timeout",
		"upstream_task_failed",
		"content_rejected",
		"provider_contract_error",
		"upstream_error":
		return code
	default:
		return "upstream_error"
	}
}

type VideoProvider interface {
	Name() string
	Capabilities() VideoProviderCapabilities
	Submit(context.Context, *Account, CanonicalVideoRequest, string) (VideoSubmitResult, error)
	RecoverSubmission(context.Context, *Account, CanonicalVideoRequest, string) (VideoSubmitResult, bool, error)
	Poll(context.Context, *Account, string) (VideoPollResult, error)
	OpenContent(context.Context, *Account, VideoTask) (io.ReadCloser, http.Header, int64, error)
}

type VideoProviderRegistry struct {
	providers map[string]VideoProvider
}

func NewVideoProviderRegistry(providers ...VideoProvider) (*VideoProviderRegistry, error) {
	registry := &VideoProviderRegistry{providers: make(map[string]VideoProvider, len(providers))}
	for _, provider := range providers {
		if isNilVideoProvider(provider) {
			return nil, ErrVideoProviderInvalid
		}
		name := provider.Name()
		if !videoProviderNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%w: %q", ErrVideoProviderInvalid, name)
		}
		if _, exists := registry.providers[name]; exists {
			return nil, fmt.Errorf("%w: %s", ErrVideoProviderDuplicate, name)
		}
		registry.providers[name] = provider
	}
	return registry, nil
}

func isNilVideoProvider(provider VideoProvider) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (registry *VideoProviderRegistry) Get(name string) (VideoProvider, bool) {
	if registry == nil {
		return nil, false
	}
	provider, ok := registry.providers[name]
	return provider, ok
}
