package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/stretchr/testify/require"
)

// seedanceFixtureUpstream is the HTTP boundary double: it records the exact
// request emitted by the adapter while serving a redacted Ark-shaped fixture.
type seedanceFixtureUpstream struct {
	response *http.Response
	err      error
	request  *http.Request
	body     []byte
}

type seedanceCloseTrackingBody struct {
	io.Reader
	closed bool
}

func (b *seedanceCloseTrackingBody) Close() error {
	b.closed = true
	return nil
}

func fixtureHTTPUpstream(t *testing.T, fixture string) *seedanceFixtureUpstream {
	t.Helper()
	body, err := os.ReadFile(fixture)
	require.NoError(t, err)
	return &seedanceFixtureUpstream{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}}
}

func (u *seedanceFixtureUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.request = req
	if req != nil && req.Body != nil {
		var err error
		u.body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(u.body))
	}
	return u.response, u.err
}

func (u *seedanceFixtureUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func seedanceAccount() *Account {
	return &Account{
		ID:          91,
		Platform:    PlatformVideo,
		Type:        AccountTypeAPIKey,
		Concurrency: 2,
		Extra:       map[string]any{"video_provider": VideoProviderSeedance},
		Credentials: map[string]any{
			"api_key":       "ark-key",
			"model_mapping": map[string]any{"seedance-2.0": "ep-seedance"},
		},
	}
}

func seedanceBoolPtr(value bool) *bool { return &value }

func seedancePublicResolver(context.Context, string) error { return nil }

func newSeedanceProvider(upstream HTTPUpstream) *SeedanceVideoProvider {
	return NewSeedanceVideoProvider(upstream, seedancePublicResolver)
}

func newSeedanceProviderWithResolver(upstream HTTPUpstream, resolver SeedanceResolvedIPValidator) *SeedanceVideoProvider {
	return NewSeedanceVideoProvider(upstream, resolver)
}

func seedanceResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// Catches a wrong Ark endpoint, missing bearer auth, omitted account mapping,
// or a payload that drifts from the documented task API contract.
func TestSeedanceProviderSubmitUsesArkTaskAPI(t *testing.T) {
	upstream := fixtureHTTPUpstream(t, "testdata/video/seedance/create_success.json")
	p := newSeedanceProvider(upstream)
	got, err := p.Submit(context.Background(), seedanceAccount(), CanonicalVideoRequest{
		Operation:       VideoOperationGeneration,
		Model:           "seedance-2.0",
		Prompt:          "camera pulls back",
		DurationSeconds: 5,
		Resolution:      "720p",
		AspectRatio:     "16:9",
		Audio:           seedanceBoolPtr(true),
	}, "submit-token")
	require.NoError(t, err)
	require.Equal(t, "cgt-2026-example", got.UpstreamTaskID)
	require.Equal(t, "/api/v3/contents/generations/tasks", upstream.request.URL.Path)
	require.Equal(t, "Bearer ark-key", upstream.request.Header.Get("Authorization"))
	require.JSONEq(t, `{"model":"ep-seedance","content":[{"type":"text","text":"camera pulls back"}],"duration":5,"resolution":"720p","ratio":"16:9","generate_audio":true}`, string(upstream.body))
}

// Catches status or result-field parsing that disagrees with the Ark task
// response shape.
func TestSeedanceProviderPollMapsSucceeded(t *testing.T) {
	p := newSeedanceProvider(fixtureHTTPUpstream(t, "testdata/video/seedance/get_succeeded.json"))
	taskID := "cgt-2026-example"
	got, err := p.Poll(context.Background(), seedanceAccount(), VideoTask{UpstreamTaskID: &taskID})
	require.NoError(t, err)
	require.Equal(t, VideoTaskSucceeded, got.Status)
	require.Equal(t, "https://example.volces.com/result.mp4", got.ResultURL)
	require.Equal(t, 5, got.ActualDurationSeconds)
}

// Catches content being reordered or encoded with a role/type that changes
// Seedance's media semantics, and catches provider options leaking through
// outside the request's explicit namespace.
func TestSeedanceProviderSubmitUsesStableContentOrderAndOptionAllowlist(t *testing.T) {
	upstream := fixtureHTTPUpstream(t, "testdata/video/seedance/create_success.json")
	account := seedanceAccount()
	account.Credentials["model_mapping"] = map[string]any{"seedance-2.0": "ep-ordered"}
	p := newSeedanceProvider(upstream)
	_, err := p.Submit(context.Background(), account, CanonicalVideoRequest{
		Operation:  VideoOperationGeneration,
		Model:      "seedance-2.0",
		Prompt:     "compose the scene",
		FirstFrame: []VideoAsset{{URL: "https://example.com/first.png"}},
		LastFrame:  []VideoAsset{{URL: "https://example.com/last.png"}},
		ReferenceImages: []VideoAsset{
			{URL: "https://example.com/reference-1.png"},
			{URL: "https://example.com/reference-2.png"},
		},
		ReferenceVideos: []VideoAsset{{URL: "https://example.com/reference.mp4"}},
		ProviderOptions: map[string]json.RawMessage{
			VideoProviderSeedance: json.RawMessage(`{"seed":7,"watermark":true,"return_last_frame":false,"service_tier":"priority","ignored":"no"}`),
		},
	}, "submit-token")
	require.NoError(t, err)

	var payload struct {
		Model   string `json:"model"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Role     string `json:"role"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
			VideoURL struct {
				URL string `json:"url"`
			} `json:"video_url"`
		} `json:"content"`
		Seed            int    `json:"seed"`
		Watermark       bool   `json:"watermark"`
		ReturnLastFrame bool   `json:"return_last_frame"`
		ServiceTier     string `json:"service_tier"`
		Ignored         any    `json:"ignored"`
	}
	require.NoError(t, json.Unmarshal(upstream.body, &payload))
	require.Equal(t, "ep-ordered", payload.Model)
	require.Len(t, payload.Content, 6)
	require.Equal(t, "text", payload.Content[0].Type)
	require.Equal(t, "compose the scene", payload.Content[0].Text)
	require.Equal(t, "image_url", payload.Content[1].Type)
	require.Equal(t, "first_frame", payload.Content[1].Role)
	require.Equal(t, "https://example.com/first.png", payload.Content[1].ImageURL.URL)
	require.Equal(t, "image_url", payload.Content[2].Type)
	require.Equal(t, "last_frame", payload.Content[2].Role)
	require.Equal(t, "https://example.com/last.png", payload.Content[2].ImageURL.URL)
	require.Equal(t, "reference_image", payload.Content[3].Role)
	require.Equal(t, "https://example.com/reference-1.png", payload.Content[3].ImageURL.URL)
	require.Equal(t, "reference_image", payload.Content[4].Role)
	require.Equal(t, "https://example.com/reference-2.png", payload.Content[4].ImageURL.URL)
	require.Equal(t, "video_url", payload.Content[5].Type)
	require.Equal(t, "reference_video", payload.Content[5].Role)
	require.Equal(t, "https://example.com/reference.mp4", payload.Content[5].VideoURL.URL)
	require.Equal(t, 7, payload.Seed)
	require.True(t, payload.Watermark)
	require.False(t, payload.ReturnLastFrame)
	require.Equal(t, "priority", payload.ServiceTier)
	require.Nil(t, payload.Ignored)
}

// Catches a future adapter that turns an unsafe account override into an
// outbound request instead of using the repository's safe URL validation.
func TestSeedanceProviderRejectsUnsafeAccountBaseURL(t *testing.T) {
	upstream := fixtureHTTPUpstream(t, "testdata/video/seedance/create_success.json")
	account := seedanceAccount()
	account.Credentials["base_url"] = "http://127.0.0.1:8080"
	_, err := newSeedanceProvider(upstream).Submit(context.Background(), account, CanonicalVideoRequest{
		Operation: VideoOperationGeneration, Model: "seedance-2.0", Prompt: "safe request",
	}, "submit-token")
	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "invalid_request", providerErr.Code)
	require.False(t, providerErr.Retryable)
	require.Nil(t, upstream.request)
	require.NotContains(t, err.Error(), "127.0.0.1")
}

// Catches incomplete official status handling, including a terminal cancelled
// task accidentally becoming retryable/unknown.
func TestSeedanceProviderPollMapsOfficialStatuses(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		body    string
		want    VideoTaskStatus
	}{
		{name: "queued", fixture: "get_queued.json", want: VideoTaskQueued},
		{name: "running", body: `{"id":"cgt-2026-example","status":"running","content":{},"resolution":"720p","ratio":"16:9","duration":5}`, want: VideoTaskRunning},
		{name: "succeeded", fixture: "get_succeeded.json", want: VideoTaskSucceeded},
		{name: "failed", fixture: "get_failed.json", want: VideoTaskFailed},
		{name: "cancelled", body: `{"id":"cgt-2026-example","status":"cancelled","content":{},"resolution":"720p","ratio":"16:9","duration":5}`, want: VideoTaskCancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstream *seedanceFixtureUpstream
			if tt.fixture != "" {
				upstream = fixtureHTTPUpstream(t, "testdata/video/seedance/"+tt.fixture)
			} else {
				upstream = &seedanceFixtureUpstream{response: seedanceResponse(http.StatusOK, tt.body)}
			}
			p := newSeedanceProvider(upstream)
			taskID := "cgt-2026-example"
			got, err := p.Poll(context.Background(), seedanceAccount(), VideoTask{UpstreamTaskID: &taskID})
			require.NoError(t, err)
			require.Equal(t, tt.want, got.Status)
		})
	}
}

// Catches malformed success bodies being mistaken for a usable submission or
// task, and proves every consumed HTTP response body is closed.
func TestSeedanceProviderRejectsMalformedResponsesAndClosesBodies(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(*SeedanceVideoProvider) error
	}{
		{
			name: "submit without task id",
			call: func(p *SeedanceVideoProvider) error {
				_, err := p.Submit(context.Background(), seedanceAccount(), CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "seedance-2.0", Prompt: "redacted"}, "submit-token")
				return err
			},
		},
		{
			name: "poll without status",
			call: func(p *SeedanceVideoProvider) error {
				taskID := "cgt-2026-example"
				_, err := p.Poll(context.Background(), seedanceAccount(), VideoTask{UpstreamTaskID: &taskID})
				return err
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := &seedanceCloseTrackingBody{Reader: strings.NewReader(`{"content":{}}`)}
			upstream := &seedanceFixtureUpstream{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}}
			err := tt.call(newSeedanceProvider(upstream))
			var providerErr VideoProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Equal(t, "provider_contract_error", providerErr.Code)
			require.True(t, body.closed)
			require.NotContains(t, err.Error(), "redacted")
		})
	}
}

// Catches retry policy drift at the submission boundary: sensitive/validation
// 400s must never retry; 429/5xx only retry when Ark proves no task was
// accepted; a sent-body timeout remains ambiguous.
func TestSeedanceProviderClassifiesSubmissionFailures(t *testing.T) {
	tests := []struct {
		name      string
		response  *http.Response
		err       error
		code      string
		retryable bool
		ambiguous bool
	}{
		{name: "validation 400", response: seedanceResponse(http.StatusBadRequest, `{"error":{"code":"InvalidParameter"}}`), code: "invalid_request"},
		{name: "sensitive 400", response: seedanceResponse(http.StatusBadRequest, `{"error":{"code":"InputTextSensitiveContentDetected"}}`), code: "content_rejected"},
		{name: "official quota rejection", response: seedanceResponse(http.StatusTooManyRequests, `{"error":{"code":"QuotaExceeded"}}`), code: "upstream_rate_limit", retryable: true},
		{name: "empty rate limit", response: seedanceResponse(http.StatusTooManyRequests, ``), code: "upstream_rate_limit", ambiguous: true},
		{name: "malformed rate limit", response: seedanceResponse(http.StatusTooManyRequests, `{`), code: "upstream_rate_limit", ambiguous: true},
		{name: "generic proxy rate limit", response: seedanceResponse(http.StatusTooManyRequests, `proxy overloaded`), code: "upstream_rate_limit", ambiguous: true},
		{name: "server error without id", response: seedanceResponse(http.StatusBadGateway, `{"error":{"code":"InternalError"}}`), code: "upstream_unavailable", ambiguous: true},
		{name: "rate limit accepted id error shape", response: seedanceResponse(http.StatusTooManyRequests, `{"id":"cgt-accepted","error":{"code":"QuotaExceeded"}}`), code: "upstream_rate_limit", ambiguous: true},
		{name: "timeout after body sent", err: context.DeadlineExceeded, code: "upstream_timeout", retryable: true, ambiguous: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &seedanceFixtureUpstream{response: tt.response, err: tt.err}
			_, err := newSeedanceProvider(upstream).Submit(context.Background(), seedanceAccount(), CanonicalVideoRequest{
				Operation: VideoOperationGeneration, Model: "seedance-2.0", Prompt: "private prompt",
			}, "submit-token")
			var providerErr VideoProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Equal(t, tt.code, providerErr.Code)
			require.Equal(t, tt.retryable, providerErr.Retryable)
			require.Equal(t, tt.ambiguous, providerErr.Ambiguous)
			require.NotContains(t, err.Error(), "private prompt")
		})
	}
}

// Catches a future recovery implementation that resubmits or invents a list
// query without a verified Ark-side submission-token field.
func TestSeedanceProviderDoesNotRecoverAmbiguousSubmission(t *testing.T) {
	upstream := fixtureHTTPUpstream(t, "testdata/video/seedance/create_success.json")
	got, found, err := newSeedanceProvider(upstream).RecoverSubmission(context.Background(), seedanceAccount(), VideoTask{}, "submit-token")
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, got)
	require.Nil(t, upstream.request)
}

// Catches a provider-wide default that lets unconfigured models inherit media
// features instead of requiring a model-keyed VideoCapabilityCatalog entry.
func TestSeedanceProviderCapabilitiesFailClosedWithoutModelConfiguration(t *testing.T) {
	capabilities := NewSeedanceVideoProvider(nil).Capabilities()
	require.Empty(t, capabilities)
}

// Catches capability defaults that bypass configured per-model restrictions.
func TestSeedanceModelCapabilityCatalogIsExplicitAndFailClosed(t *testing.T) {
	catalog := VideoCapabilityCatalog{
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-text-only"): {
			VideoOperationGeneration: {Text: true},
		},
		VideoModelCapabilityKey(VideoProviderSeedance, "seedance-rich"): {
			VideoOperationGeneration: {Text: true, FirstFrame: true, LastFrame: true, FirstAndLastFrame: true, ReferenceImages: true, ReferenceVideos: true, Audio: true},
		},
	}
	require.NoError(t, catalog.Validate(VideoProviderSeedance, CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "seedance-text-only", Prompt: "text"}))
	require.ErrorIs(t, catalog.Validate(VideoProviderSeedance, CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "seedance-text-only", Prompt: "text", Audio: seedanceBoolPtr(true)}), ErrVideoUnsupportedCapability)
	require.NoError(t, catalog.Validate(VideoProviderSeedance, CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "seedance-rich", Prompt: "text", FirstFrame: []VideoAsset{{URL: "https://example.com/first.png"}}, LastFrame: []VideoAsset{{URL: "https://example.com/last.png"}}, ReferenceImages: []VideoAsset{{URL: "https://example.com/ref.png"}}, ReferenceVideos: []VideoAsset{{URL: "https://example.com/ref.mp4"}}, Audio: seedanceBoolPtr(true)}))
	require.ErrorIs(t, catalog.Validate(VideoProviderSeedance, CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "seedance-unknown", Prompt: "text"}), ErrVideoUnsupportedCapability)
}

func TestSeedanceProviderPollRejectsEmptyTaskID(t *testing.T) {
	emptyTaskID := " "
	_, err := newSeedanceProvider(fixtureHTTPUpstream(t, "testdata/video/seedance/get_queued.json")).Poll(context.Background(), seedanceAccount(), VideoTask{UpstreamTaskID: &emptyTaskID})
	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "invalid_request", providerErr.Code)
	require.False(t, errors.Is(err, context.DeadlineExceeded))
}

// Catches signed Ark result URLs being rejected merely because their required
// signature lives in the query string, or an Ark API key leaking to storage.
func TestSeedanceProviderOpenContentPreservesSignedResultURLWithoutAuth(t *testing.T) {
	upstream := &seedanceFixtureUpstream{response: &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"video/mp4"}},
		ContentLength: 5,
		Body:          io.NopCloser(strings.NewReader("video")),
	}}
	resultURL := "https://example.volces.com/result.mp4?X-Expires=123&X-Signature=redacted"
	body, headers, length, err := newSeedanceProvider(upstream).OpenContent(context.Background(), seedanceAccount(), VideoTask{ResultURL: &resultURL})
	require.NoError(t, err)
	defer func() { require.NoError(t, body.Close()) }()
	content, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "video", string(content))
	require.Equal(t, int64(5), length)
	require.Equal(t, "video/mp4", headers.Get("Content-Type"))
	require.Equal(t, "X-Expires=123&X-Signature=redacted", upstream.request.URL.RawQuery)
	require.Empty(t, upstream.request.Header.Get("Authorization"))
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.request.Context()))
	require.True(t, HTTPUpstreamResolvedIPValidationRequired(upstream.request.Context()))
}

// Catches a hostname that passes syntax checks but resolves to a protected
// destination; the resolver must deny it before an upstream request is sent.
func TestSeedanceProviderRejectsUnsafeResolvedDestinations(t *testing.T) {
	for _, tt := range []struct {
		name string
		ip   string
	}{
		{name: "loopback", ip: "127.0.0.1"},
		{name: "private", ip: "10.0.0.8"},
		{name: "link local", ip: "169.254.1.8"},
		{name: "reserved", ip: "192.0.2.8"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := fixtureHTTPUpstream(t, "testdata/video/seedance/create_success.json")
			resolver := func(_ context.Context, host string) error {
				require.Equal(t, "ark-relay.example", host)
				return urlvalidator.ValidateResolvedIPs([]net.IP{net.ParseIP(tt.ip)})
			}
			account := seedanceAccount()
			account.Credentials["base_url"] = "https://ark-relay.example"
			_, err := newSeedanceProviderWithResolver(upstream, resolver).Submit(context.Background(), account, CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "seedance-2.0", Prompt: "safe"}, "submission-token")
			var providerErr VideoProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Equal(t, "invalid_request", providerErr.Code)
			require.Nil(t, upstream.request)
		})
	}
}

// Catches signed content URLs bypassing the same resolver-time protection as
// custom Ark base URLs; no content request may be sent after a bad resolution.
func TestSeedanceProviderRejectsUnsafeResolvedSignedContentURL(t *testing.T) {
	upstream := &seedanceFixtureUpstream{response: seedanceResponse(http.StatusOK, "video")}
	resolver := func(_ context.Context, host string) error {
		require.Equal(t, "signed-relay.example", host)
		return urlvalidator.ValidateResolvedIPs([]net.IP{net.ParseIP("127.0.0.1")})
	}
	resultURL := "https://signed-relay.example/result.mp4?X-Signature=redacted"
	_, _, _, err := newSeedanceProviderWithResolver(upstream, resolver).OpenContent(context.Background(), seedanceAccount(), VideoTask{ResultURL: &resultURL})
	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "invalid_request", providerErr.Code)
	require.Nil(t, upstream.request)
}
