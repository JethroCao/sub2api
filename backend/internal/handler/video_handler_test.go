package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVideoSubmitReturnsStableDurableResponse(t *testing.T) {
	now := time.Date(2026, time.August, 8, 1, 2, 3, 0, time.UTC)
	tasks := &videoHandlerTaskStub{submitTask: &service.VideoTask{
		RequestID: "vid_0123456789abcdef0123456789abcdef",
		Status:    service.VideoTaskQueued, Provider: service.PlatformGrok,
		ExternalModel: "grok-imagine-video", CreatedAt: now, UpdatedAt: now,
	}}
	h := newVideoHandlerForTest(tasks, nil, nil, nil, nil)

	w := performVideoHandlerRequest(t, h.Generate, http.MethodPost, "/v1/videos/generations",
		`{"model":"grok-imagine-video","prompt":"waves"}`, ownedVideoAPIKey(10, 20, service.PlatformGrok))

	require.Equal(t, http.StatusAccepted, w.Code)
	require.Equal(t, "vid_0123456789abcdef0123456789abcdef", gjson.GetBytes(w.Body.Bytes(), "request_id").String())
	require.Equal(t, "queued", gjson.GetBytes(w.Body.Bytes(), "status").String())
	require.Equal(t, "grok", gjson.GetBytes(w.Body.Bytes(), "provider").String())
	require.Equal(t, "grok-imagine-video", gjson.GetBytes(w.Body.Bytes(), "model").String())
	require.Equal(t, now.Unix(), gjson.GetBytes(w.Body.Bytes(), "created_at").Int())
	require.Equal(t, service.PlatformGrok, tasks.submitCommand.Platform)
	require.Equal(t, service.PlatformGrok, tasks.submitCommand.Provider)
	require.Equal(t, "grok-imagine-video", tasks.submitCommand.Request.Model)
}

func TestVideoSubmitRejectsDisabledGroupBeforeService(t *testing.T) {
	tasks := &videoHandlerTaskStub{submitTask: &service.VideoTask{}}
	apiKey := ownedVideoAPIKey(10, 20, service.PlatformVideo)
	apiKey.Group.AllowVideoGeneration = false
	h := newVideoHandlerForTest(tasks, nil, nil, nil, nil)

	w := performVideoHandlerRequest(t, h.Generate, http.MethodPost, "/v1/videos/generations",
		`{"model":"seedance-2.0","prompt":"waves"}`, apiKey)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, "unsupported_capability", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
	require.Zero(t, tasks.submitCalls)
}

func TestVideoSubmitMapsStableErrorEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{name: "invalid request", err: service.ErrVideoInvalidRequest, wantStatus: http.StatusBadRequest, wantType: "invalid_request_error"},
		{name: "invalid idempotency key", err: service.ErrIdempotencyKeyInvalid, wantStatus: http.StatusBadRequest, wantType: "invalid_request_error"},
		{name: "idempotency conflict", err: service.ErrVideoIdempotencyConflict, wantStatus: http.StatusConflict, wantType: "idempotency_conflict"},
		{name: "unsupported", err: service.ErrVideoUnsupportedCapability, wantStatus: http.StatusBadRequest, wantType: "unsupported_capability"},
		{name: "pricing", err: service.ErrVideoPricingUnavailable, wantStatus: http.StatusServiceUnavailable, wantType: "video_pricing_unavailable"},
		{name: "balance", err: service.ErrVideoInsufficientBalance, wantStatus: http.StatusForbidden, wantType: "insufficient_balance"},
		{name: "account", err: service.ErrNoAvailableAccounts, wantStatus: http.StatusServiceUnavailable, wantType: "no_available_account"},
		{name: "provider invalid request", err: service.NewVideoProviderError(http.StatusBadRequest, "invalid_request", false, false, errors.New("secret")), wantStatus: http.StatusBadRequest, wantType: "invalid_request_error"},
		{name: "rate limit", err: service.NewVideoProviderError(http.StatusTooManyRequests, "upstream_rate_limit", true, false, errors.New("secret")), wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error"},
		{name: "upstream", err: service.NewVideoProviderError(http.StatusBadGateway, "upstream_error", true, false, errors.New("secret")), wantStatus: http.StatusBadGateway, wantType: "upstream_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasks := &videoHandlerTaskStub{submitErr: tt.err}
			h := newVideoHandlerForTest(tasks, nil, nil, nil, nil)
			w := performVideoHandlerRequest(t, h.Generate, http.MethodPost, "/v1/videos/generations",
				`{"model":"seedance-2.0","prompt":"waves"}`, ownedVideoAPIKey(10, 20, service.PlatformVideo))
			require.Equal(t, tt.wantStatus, w.Code)
			require.Equal(t, tt.wantType, gjson.GetBytes(w.Body.Bytes(), "error.type").String())
			require.NotEmpty(t, gjson.GetBytes(w.Body.Bytes(), "error.message").String())
			require.NotContains(t, w.Body.String(), "secret")
		})
	}
}

func TestVideoSubmitMapsBillingEligibilityErrorsWithoutTaskSideEffects(t *testing.T) {
	quotaErr := service.ErrUserPlatformDailyQuotaExhausted.WithMetadata(map[string]string{
		"window_resets_at": time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339),
	})
	tests := []struct {
		name           string
		err            error
		wantStatus     int
		wantType       string
		wantRetryAfter bool
	}{
		{name: "balance", err: service.ErrInsufficientBalance, wantStatus: http.StatusForbidden, wantType: "insufficient_balance"},
		{name: "billing service", err: service.ErrBillingServiceUnavailable, wantStatus: http.StatusServiceUnavailable, wantType: "upstream_error"},
		{name: "group rpm", err: service.ErrGroupRPMExceeded, wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error", wantRetryAfter: true},
		{name: "platform quota", err: quotaErr, wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error", wantRetryAfter: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasks := &videoHandlerTaskStub{}
			billing := &videoBillingEligibilityStub{err: tt.err}
			h := newVideoHandlerForTest(tasks, billing, nil, nil, nil)
			w := performVideoHandlerRequest(t, h.Generate, http.MethodPost, "/v1/videos/generations",
				`{"model":"grok-imagine-video","prompt":"waves"}`, ownedVideoAPIKey(10, 20, service.PlatformGrok))

			require.Equal(t, tt.wantStatus, w.Code)
			require.Equal(t, tt.wantType, gjson.GetBytes(w.Body.Bytes(), "error.type").String())
			if tt.wantRetryAfter {
				require.NotEmpty(t, w.Header().Get("Retry-After"))
			} else {
				require.Empty(t, w.Header().Get("Retry-After"))
			}
			require.Equal(t, 1, billing.calls)
			require.Zero(t, tasks.submitCalls)
		})
	}
}

func TestVideoSubmitRejectsKlingWhileProviderIsDormant(t *testing.T) {
	tasks := &videoHandlerTaskStub{}
	h := newVideoHandlerForTest(tasks, nil, nil, nil, nil)
	w := performVideoHandlerRequest(t, h.Generate, http.MethodPost, "/v1/videos/generations",
		`{"model":"kling-3.0","prompt":"waves"}`, ownedVideoAPIKey(10, 20, service.PlatformVideo))
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, "unsupported_capability", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
	require.Zero(t, tasks.submitCalls)
}

func TestVideoSubmitSecurityAuditBlocksBeforeBillingOrTaskSideEffects(t *testing.T) {
	engine := blockingHandlerPromptEngine()
	openAI := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine)}
	tasks := &videoHandlerTaskStub{submitTask: &service.VideoTask{RequestID: "vid_0123456789abcdef0123456789abcdef"}}
	billing := &videoBillingEligibilityStub{}
	h := NewVideoHandler(nil, nil, nil, nil, nil, openAI, nil)
	h.tasks = tasks
	h.billing = billing

	w := performVideoHandlerRequest(t, h.Generate, http.MethodPost, "/v1/videos/generations",
		`{"model":"grok-imagine-video","prompt":"blocked video prompt","image":{"url":"https://example.com/reference.png"}}`,
		ownedVideoAPIKey(10, 20, service.PlatformGrok))

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, securityaudit.ErrorCodeBlocked, gjson.GetBytes(w.Body.Bytes(), "error.code").String())
	require.Zero(t, billing.calls, "audit rejection must precede billing eligibility")
	require.Zero(t, tasks.submitCalls, "audit rejection must precede pricing, hold, task creation, scheduling, and provider submission")
	evaluated, _, requests := engine.snapshot()
	require.Equal(t, 1, evaluated)
	require.Len(t, requests, 1)
	require.Contains(t, string(requests[0].Body), "blocked video prompt")
	require.Contains(t, string(requests[0].Body), "https://example.com/reference.png")
}

func TestVideoStatusDoesNotLeakAcrossAPIKeys(t *testing.T) {
	tasks := &videoHandlerTaskStub{getOwned: func(_ context.Context, _ string, userID, apiKeyID int64) (*service.VideoTask, error) {
		if userID != 10 || apiKeyID != 20 {
			return nil, service.ErrVideoTaskNotFound
		}
		return &service.VideoTask{RequestID: "vid_0123456789abcdef0123456789abcdef", UserID: 10, APIKeyID: 20}, nil
	}}
	legacy := &legacyVideoHandlerStub{}
	h := newVideoHandlerForTest(tasks, nil, nil, legacy, nil)

	w := performVideoHandlerRequest(t, h.Status, http.MethodGet,
		"/v1/videos/vid_0123456789abcdef0123456789abcdef", "", ownedVideoAPIKey(10, 21, service.PlatformGrok))

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Equal(t, "not_found_error", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
	require.Zero(t, legacy.statusCalls, "durable IDs must never fall through to legacy Redis")
}

func TestVideoStatusFallsBackToLegacyGrokBindingOnlyAfterDurableNotFound(t *testing.T) {
	tasks := &videoHandlerTaskStub{getOwned: func(context.Context, string, int64, int64) (*service.VideoTask, error) {
		return nil, service.ErrVideoTaskNotFound
	}}
	legacy := &legacyVideoHandlerStub{statusCode: http.StatusOK, statusBody: `{"request_id":"old_grok_id","status":"done"}`}
	h := newVideoHandlerForTest(tasks, nil, nil, legacy, staticLegacyRouteGate(true))

	w := performVideoHandlerRequest(t, h.Status, http.MethodGet, "/v1/videos/old_grok_id", "", ownedVideoAPIKey(10, 20, service.PlatformGrok))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "old_grok_id", gjson.GetBytes(w.Body.Bytes(), "request_id").String())
	require.Equal(t, 1, legacy.statusCalls)
}

func TestVideoStatusDoesNotFallbackOnPersistenceFailure(t *testing.T) {
	tasks := &videoHandlerTaskStub{getOwned: func(context.Context, string, int64, int64) (*service.VideoTask, error) {
		return nil, errors.New("database unavailable")
	}}
	legacy := &legacyVideoHandlerStub{statusCode: http.StatusOK}
	h := newVideoHandlerForTest(tasks, nil, nil, legacy, staticLegacyRouteGate(true))

	w := performVideoHandlerRequest(t, h.Status, http.MethodGet, "/v1/videos/old_grok_id", "", ownedVideoAPIKey(10, 20, service.PlatformGrok))

	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
	require.Zero(t, legacy.statusCalls)
}

func TestVideoStatusDoesNotFallbackWhenCompositeCannotRouteGrok(t *testing.T) {
	tasks := &videoHandlerTaskStub{getOwned: func(context.Context, string, int64, int64) (*service.VideoTask, error) {
		return nil, service.ErrVideoTaskNotFound
	}}
	legacy := &legacyVideoHandlerStub{statusCode: http.StatusOK}
	h := newVideoHandlerForTest(tasks, nil, nil, legacy, staticLegacyRouteGate(false))

	w := performVideoHandlerRequest(t, h.Status, http.MethodGet, "/v1/videos/old_grok_id", "", ownedVideoAPIKey(10, 20, service.PlatformComposite))

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Zero(t, legacy.statusCalls)
}

func TestVideoContentUsesOwnedPublicURLAndForwardsOnlyAllowlistedHeaders(t *testing.T) {
	resultURL := "https://media.example/video.mp4"
	tasks := &videoHandlerTaskStub{getOwned: func(context.Context, string, int64, int64) (*service.VideoTask, error) {
		return &service.VideoTask{
			RequestID: "vid_0123456789abcdef0123456789abcdef", UserID: 10, APIKeyID: 20,
			Status: service.VideoTaskSucceeded, ResultURL: &resultURL,
		}, nil
	}}
	upstreamHeaders := make(http.Header)
	upstreamHeaders.Set("Content-Type", "video/mp4")
	upstreamHeaders.Set("Content-Range", "bytes 2-4/6")
	upstreamHeaders.Set("Accept-Ranges", "bytes")
	upstreamHeaders.Set("ETag", `"safe"`)
	upstreamHeaders.Set("Set-Cookie", "private=secret")
	upstreamHeaders.Set("Authorization", "Bearer secret")
	upstreamHeaders.Set("X-Upstream-Internal", "secret")
	fetcher := &videoContentFetcherStub{response: &http.Response{
		StatusCode: http.StatusPartialContent,
		Header:     upstreamHeaders,
		Body:       io.NopCloser(strings.NewReader("cde")), ContentLength: 3,
	}}
	h := newVideoHandler(tasks, nil, nil, nil, fetcher, nil, nil)

	w := performVideoHandlerRequest(t, h.Content, http.MethodGet,
		"/v1/videos/vid_0123456789abcdef0123456789abcdef/content", "", ownedVideoAPIKey(10, 20, service.PlatformVideo))

	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, "cde", w.Body.String())
	require.Equal(t, "video/mp4", w.Header().Get("Content-Type"))
	require.Equal(t, "bytes 2-4/6", w.Header().Get("Content-Range"))
	require.Equal(t, `"safe"`, w.Header().Get("ETag"))
	require.Empty(t, w.Header().Get("Set-Cookie"))
	require.Empty(t, w.Header().Get("Authorization"))
	require.Empty(t, w.Header().Get("X-Upstream-Internal"))
	require.Equal(t, resultURL, fetcher.rawURL)
}

func TestVideoContentOwnershipFailureNeverFetchesOrFallsBack(t *testing.T) {
	tasks := &videoHandlerTaskStub{getOwned: func(context.Context, string, int64, int64) (*service.VideoTask, error) {
		return nil, service.ErrVideoTaskNotFound
	}}
	legacy := &legacyVideoHandlerStub{}
	fetcher := &videoContentFetcherStub{}
	h := newVideoHandler(tasks, nil, nil, nil, fetcher, legacy, staticLegacyRouteGate(true))

	w := performVideoHandlerRequest(t, h.Content, http.MethodGet,
		"/v1/videos/vid_0123456789abcdef0123456789abcdef/content", "", ownedVideoAPIKey(10, 21, service.PlatformGrok))

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Zero(t, fetcher.calls)
	require.Zero(t, legacy.contentCalls)
}

func TestVideoContentWithoutPublicURLUsesStoredAccountProvider(t *testing.T) {
	task := &service.VideoTask{
		RequestID: "vid_0123456789abcdef0123456789abcdef", UserID: 10, APIKeyID: 20,
		AccountID: 77, Provider: service.PlatformGrok, Status: service.VideoTaskSucceeded,
	}
	tasks := &videoHandlerTaskStub{getOwned: func(context.Context, string, int64, int64) (*service.VideoTask, error) {
		return task, nil
	}}
	account := &service.Account{ID: 77, Platform: service.PlatformGrok}
	accounts := videoAccountReaderStub{account: account}
	provider := &videoContentProviderStub{name: service.PlatformGrok, body: "provider-bytes", headers: http.Header{
		"Content-Type":  {"video/mp4"},
		"Content-Range": {"bytes 0-13/14"},
	}, length: 14}
	registry, err := service.NewVideoProviderRegistry(provider)
	require.NoError(t, err)
	h := newVideoHandler(tasks, nil, accounts, registry, nil, nil, nil)

	w := performVideoHandlerRequest(t, h.Content, http.MethodGet,
		"/v1/videos/vid_0123456789abcdef0123456789abcdef/content", "", ownedVideoAPIKey(10, 20, service.PlatformGrok))

	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, "provider-bytes", w.Body.String())
	require.Same(t, account, provider.account)
	require.Equal(t, task.RequestID, provider.task.RequestID)
}

func TestVideoContentWithoutPublicURLPreservesProviderRangeStatus(t *testing.T) {
	task := &service.VideoTask{
		RequestID: "vid_0123456789abcdef0123456789abcdef", UserID: 10, APIKeyID: 20,
		AccountID: 77, Provider: service.PlatformGrok, Status: service.VideoTaskSucceeded,
	}
	tasks := &videoHandlerTaskStub{getOwned: func(context.Context, string, int64, int64) (*service.VideoTask, error) {
		return task, nil
	}}
	provider := &videoContentProviderStub{name: service.PlatformGrok, status: http.StatusRequestedRangeNotSatisfiable, headers: http.Header{
		"Content-Range": {"bytes */14"},
	}}
	registry, err := service.NewVideoProviderRegistry(provider)
	require.NoError(t, err)
	h := newVideoHandler(tasks, nil, videoAccountReaderStub{account: &service.Account{ID: 77}}, registry, nil, nil, nil)

	w := performVideoHandlerRequest(t, h.Content, http.MethodGet,
		"/v1/videos/vid_0123456789abcdef0123456789abcdef/content", "", ownedVideoAPIKey(10, 20, service.PlatformGrok))

	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code)
	require.Equal(t, "bytes */14", w.Header().Get("Content-Range"))
}

func TestVideoContentEndToEndClassifiesGrokRelayAndPublicURLs(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	for _, tt := range []struct {
		name           string
		account        *service.Account
		statusURL      string
		wantAuth       string
		wantProxyFetch bool
	}{
		{
			name: "api key protected relay",
			account: &service.Account{ID: 77, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "api-key", "base_url": "https://xai.example/v1"}},
			statusURL: "https://xai.example/v1/videos/upstream-task/content", wantAuth: "Bearer api-key",
		},
		{
			name: "oauth protected relay",
			account: &service.Account{ID: 77, Platform: service.PlatformGrok, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"base_url": "https://relay.example/v1"}},
			statusURL: "https://relay.example/v1/videos/upstream-task/content", wantAuth: "Bearer oauth-token",
		},
		{
			name: "public vidgen",
			account: &service.Account{ID: 77, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "api-key", "base_url": "https://xai.example/v1"}},
			statusURL: "https://vidgen.x.ai/signed/video.mp4?token=redacted", wantAuth: "Bearer api-key", wantProxyFetch: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			statusBody := `{"status":"completed","video":{"url":"` + tt.statusURL + `"}}`
			responses := []*http.Response{videoGrokHandlerResponse(http.StatusOK, "application/json", statusBody)}
			if !tt.wantProxyFetch {
				responses = append(responses,
					videoGrokHandlerResponse(http.StatusOK, "application/json", statusBody),
					videoGrokHandlerResponse(http.StatusPartialContent, "video/mp4", "video-bytes"),
				)
				responses[2].Header.Set("Content-Range", "bytes 0-10/11")
			}
			upstream := &videoGrokHandlerUpstream{responses: responses}
			provider := service.NewGrokVideoProvider(upstream, videoGrokTokenProvider("oauth-token"))
			upstreamID := "upstream-task"
			poll, err := provider.Poll(context.Background(), tt.account, service.VideoTask{UpstreamTaskID: &upstreamID})
			require.NoError(t, err)

			task := &service.VideoTask{
				RequestID: "vid_0123456789abcdef0123456789abcdef", UserID: 10, APIKeyID: 20,
				AccountID: tt.account.ID, Platform: service.PlatformGrok, Provider: service.PlatformGrok,
				Status: service.VideoTaskSucceeded, UpstreamTaskID: &upstreamID,
			}
			if poll.ResultURL != "" {
				task.ResultURL = &poll.ResultURL
			}
			tasks := &videoHandlerTaskStub{getOwned: func(context.Context, string, int64, int64) (*service.VideoTask, error) {
				return task, nil
			}}
			registry, err := service.NewVideoProviderRegistry(provider)
			require.NoError(t, err)
			fetcher := &videoContentFetcherStub{response: videoGrokHandlerResponse(http.StatusOK, "video/mp4", "public-bytes")}
			h := newVideoHandler(tasks, nil, videoAccountReaderStub{account: tt.account}, registry, fetcher, nil, nil)

			w := performVideoHandlerRequest(t, h.Content, http.MethodGet,
				"/v1/videos/vid_0123456789abcdef0123456789abcdef/content", "", ownedVideoAPIKey(10, 20, service.PlatformGrok))

			if tt.wantProxyFetch {
				require.Equal(t, http.StatusOK, w.Code)
				require.Equal(t, "public-bytes", w.Body.String())
				require.Equal(t, tt.statusURL, fetcher.rawURL)
				require.Len(t, upstream.requests, 1, "public signed content must not invoke authenticated OpenContent")
			} else {
				require.Equal(t, http.StatusPartialContent, w.Code)
				require.Equal(t, "video-bytes", w.Body.String())
				require.Zero(t, fetcher.calls, "protected relay content must not use the public URL fetcher")
				require.Len(t, upstream.requests, 3)
				require.Equal(t, tt.wantAuth, upstream.requests[1].Header.Get("Authorization"))
				require.Equal(t, tt.wantAuth, upstream.requests[2].Header.Get("Authorization"))
			}
		})
	}
}

type videoGrokTokenProvider string

func (p videoGrokTokenProvider) GetAccessToken(context.Context, *service.Account) (string, error) {
	return string(p), nil
}

type videoGrokHandlerUpstream struct {
	requests  []*http.Request
	responses []*http.Response
}

func (u *videoGrokHandlerUpstream) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.requests = append(u.requests, request)
	if len(u.responses) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	response := u.responses[0]
	u.responses = u.responses[1:]
	return response, nil
}

func (u *videoGrokHandlerUpstream) DoWithTLS(request *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(request, proxyURL, accountID, accountConcurrency)
}

func videoGrokHandlerResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
	}
}

type videoHandlerTaskStub struct {
	submitTask    *service.VideoTask
	submitErr     error
	submitCalls   int
	submitCommand service.VideoSubmitCommand
	getOwned      func(context.Context, string, int64, int64) (*service.VideoTask, error)
}

type videoBillingEligibilityStub struct {
	calls int
	err   error
}

func (s *videoBillingEligibilityStub) CheckBillingEligibility(context.Context, *service.User, *service.APIKey, *service.Group, *service.UserSubscription, string) error {
	s.calls++
	return s.err
}

func (s *videoHandlerTaskStub) Submit(_ context.Context, command service.VideoSubmitCommand) (*service.VideoTask, error) {
	s.submitCalls++
	s.submitCommand = command
	return s.submitTask, s.submitErr
}

func (s *videoHandlerTaskStub) GetOwned(ctx context.Context, requestID string, userID, apiKeyID int64) (*service.VideoTask, error) {
	if s.getOwned != nil {
		return s.getOwned(ctx, requestID, userID, apiKeyID)
	}
	return nil, service.ErrVideoTaskNotFound
}

type legacyVideoHandlerStub struct {
	statusCalls  int
	contentCalls int
	statusCode   int
	statusBody   string
}

func (s *legacyVideoHandlerStub) GrokVideoStatus(c *gin.Context) {
	s.statusCalls++
	status := s.statusCode
	if status == 0 {
		status = http.StatusNotFound
	}
	c.Data(status, "application/json", []byte(s.statusBody))
}

func (s *legacyVideoHandlerStub) GrokVideoContent(c *gin.Context) {
	s.contentCalls++
	c.Status(http.StatusOK)
}

type staticLegacyRouteGate bool

func (g staticLegacyRouteGate) CanRouteGrok(context.Context, *service.Group) (bool, error) {
	return bool(g), nil
}

type videoContentFetcherStub struct {
	response *http.Response
	err      error
	calls    int
	rawURL   string
	rangeHdr string
}

type videoAccountReaderStub struct {
	account *service.Account
	err     error
}

func (s videoAccountReaderStub) GetByID(context.Context, int64) (*service.Account, error) {
	return s.account, s.err
}

type videoContentProviderStub struct {
	name    string
	body    string
	headers http.Header
	length  int64
	status  int
	account *service.Account
	task    service.VideoTask
}

func (p *videoContentProviderStub) Name() string { return p.name }
func (p *videoContentProviderStub) Capabilities() service.VideoProviderCapabilities {
	return service.VideoProviderCapabilities{service.VideoOperationGeneration: {Text: true}}
}
func (p *videoContentProviderStub) Submit(context.Context, *service.Account, service.CanonicalVideoRequest, string) (service.VideoSubmitResult, error) {
	panic("unexpected submit")
}
func (p *videoContentProviderStub) RecoverSubmission(context.Context, *service.Account, service.VideoTask, string) (service.VideoSubmitResult, bool, error) {
	panic("unexpected recovery")
}
func (p *videoContentProviderStub) Poll(context.Context, *service.Account, service.VideoTask) (service.VideoPollResult, error) {
	panic("unexpected poll")
}
func (p *videoContentProviderStub) OpenContent(_ context.Context, account *service.Account, task service.VideoTask) (io.ReadCloser, http.Header, int64, error) {
	p.account, p.task = account, task
	return io.NopCloser(strings.NewReader(p.body)), p.headers.Clone(), p.length, nil
}

func (p *videoContentProviderStub) OpenContentWithStatus(_ context.Context, account *service.Account, task service.VideoTask) (io.ReadCloser, http.Header, int64, int, error) {
	p.account, p.task = account, task
	status := p.status
	if status == 0 {
		status = http.StatusOK
		if p.headers.Get("Content-Range") != "" {
			status = http.StatusPartialContent
		}
	}
	return io.NopCloser(strings.NewReader(p.body)), p.headers.Clone(), p.length, status, nil
}

func (f *videoContentFetcherStub) Fetch(_ context.Context, rawURL, rangeHeader string) (*http.Response, error) {
	f.calls++
	f.rawURL, f.rangeHdr = rawURL, rangeHeader
	return f.response, f.err
}

func newVideoHandlerForTest(
	tasks videoTaskApplication,
	billing videoBillingEligibility,
	accounts videoAccountReader,
	legacy legacyGrokVideoHandler,
	legacyGate legacyGrokRouteGate,
) *VideoHandler {
	return newVideoHandler(tasks, billing, accounts, nil, nil, legacy, legacyGate)
}

func ownedVideoAPIKey(userID, apiKeyID int64, platform string) *service.APIKey {
	groupID := int64(30)
	return &service.APIKey{
		ID: apiKeyID, UserID: userID, GroupID: &groupID, User: &service.User{ID: userID},
		Group: &service.Group{ID: groupID, Platform: platform, Status: service.StatusActive, Hydrated: true, AllowVideoGeneration: true},
	}
}

func performVideoHandlerRequest(t *testing.T, fn gin.HandlerFunc, method, path, body string, apiKey *service.APIKey) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID})
	if strings.Contains(path, "/videos/") && method == http.MethodGet {
		requestID := strings.TrimSuffix(path[strings.LastIndex(path, "/videos/")+len("/videos/"):], "/content")
		c.Params = gin.Params{{Key: "request_id", Value: requestID}}
	}
	fn(c)
	return w
}
