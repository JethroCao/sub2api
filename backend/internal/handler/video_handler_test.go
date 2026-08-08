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
		{name: "unsupported", err: service.ErrVideoUnsupportedCapability, wantStatus: http.StatusBadRequest, wantType: "unsupported_capability"},
		{name: "pricing", err: service.ErrVideoPricingUnavailable, wantStatus: http.StatusServiceUnavailable, wantType: "video_pricing_unavailable"},
		{name: "balance", err: service.ErrVideoInsufficientBalance, wantStatus: http.StatusPaymentRequired, wantType: "insufficient_balance"},
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

func TestVideoSubmitRejectsKlingWhileProviderIsDormant(t *testing.T) {
	tasks := &videoHandlerTaskStub{}
	h := newVideoHandlerForTest(tasks, nil, nil, nil, nil)
	w := performVideoHandlerRequest(t, h.Generate, http.MethodPost, "/v1/videos/generations",
		`{"model":"kling-3.0","prompt":"waves"}`, ownedVideoAPIKey(10, 20, service.PlatformVideo))
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, "unsupported_capability", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
	require.Zero(t, tasks.submitCalls)
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

type videoHandlerTaskStub struct {
	submitTask    *service.VideoTask
	submitErr     error
	submitCalls   int
	submitCommand service.VideoSubmitCommand
	getOwned      func(context.Context, string, int64, int64) (*service.VideoTask, error)
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
