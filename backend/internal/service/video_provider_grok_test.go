package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

// These tests protect the Grok adapter's externally observable transport
// contract. They fail if a future change alters routing, auth, request bytes,
// upstream status mapping, or content download behavior.
type recordingHTTPUpstream struct {
	requests  []*http.Request
	bodies    [][]byte
	proxyURL  string
	responses []*http.Response
	err       error
}

type grokVideoCloseTrackingBody struct {
	io.Reader
	closed bool
}

func (b *grokVideoCloseTrackingBody) Close() error {
	b.closed = true
	return nil
}

func (u *recordingHTTPUpstream) Do(req *http.Request, proxyURL string, _ int64, _ int) (*http.Response, error) {
	u.requests = append(u.requests, req)
	u.proxyURL = proxyURL
	if req != nil && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		u.bodies = append(u.bodies, body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	if u.err != nil {
		return nil, u.err
	}
	if len(u.responses) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	response := u.responses[0]
	u.responses = u.responses[1:]
	return response, nil
}

func (u *recordingHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

type grokVideoTestTokenProvider string

func (p grokVideoTestTokenProvider) GetAccessToken(context.Context, *Account) (string, error) {
	return string(p), nil
}

func fakeGrokTokenProvider(token string) grokVideoTestTokenProvider {
	return grokVideoTestTokenProvider(token)
}

type countingGrokVideoTokenProvider struct {
	token string
	calls int
}

func (p *countingGrokVideoTokenProvider) GetAccessToken(context.Context, *Account) (string, error) {
	p.calls++
	return p.token, nil
}

func grokVideoTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func grokAPIKeyAccount() *Account {
	return &Account{
		ID:          81,
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Concurrency: 2,
		Credentials: map[string]any{
			"api_key":  "api-key",
			"base_url": "https://xai.test/v1",
		},
	}
}

func TestGrokVideoProviderSubmitPreservesCurrentWireContract(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	upstream := &recordingHTTPUpstream{responses: []*http.Response{grokVideoTestResponse(http.StatusOK, `{"request_id":"up_123"}`)}}
	provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))

	got, err := provider.Submit(context.Background(), grokAPIKeyAccount(), CanonicalVideoRequest{
		Operation:       VideoOperationGeneration,
		Model:           "grok-imagine-video-1.5",
		Prompt:          "waves",
		Resolution:      "720p",
		DurationSeconds: 10,
	}, "submit-token")

	require.NoError(t, err)
	require.Equal(t, "up_123", got.UpstreamTaskID)
	require.Equal(t, VideoTaskQueued, got.Status)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/v1/videos/generations", upstream.requests[0].URL.Path)
	require.Equal(t, "Bearer api-key", upstream.requests[0].Header.Get("Authorization"))
	require.JSONEq(t, `{"model":"grok-imagine-video","prompt":"waves","resolution":"720p","duration":10}`, string(upstream.bodies[0]))
}

func TestGrokVideoProviderSubmitPreservesImageAliasesAndMutationEndpoints(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	tests := []struct {
		name    string
		request CanonicalVideoRequest
		path    string
		body    string
	}{
		{
			name: "image to video keeps 1.5 model and canonical image URL",
			request: CanonicalVideoRequest{
				Operation: VideoOperationGeneration, Model: "grok-imagine-video-1.5", Prompt: "animate",
				FirstFrame: []VideoAsset{{URL: "data:image/png;base64,aW1n"}},
			},
			path: "/v1/videos/generations",
			body: `{"model":"grok-imagine-video-1.5","prompt":"animate","image":{"url":"data:image/png;base64,aW1n"}}`,
		},
		{
			name: "edit",
			request: CanonicalVideoRequest{
				Operation: VideoOperationEdit, Model: "grok-imagine-video", Prompt: "continue", DurationSeconds: 6,
				ReferenceVideos: []VideoAsset{{URL: "https://example.com/in.mp4"}},
			},
			path: "/v1/videos/edits",
			body: `{"model":"grok-imagine-video","prompt":"continue","video":{"url":"https://example.com/in.mp4"},"duration":6}`,
		},
		{
			name: "extension",
			request: CanonicalVideoRequest{
				Operation: VideoOperationExtension, Model: "grok-imagine-video", Prompt: "continue", DurationSeconds: 6,
				ReferenceVideos: []VideoAsset{{URL: "https://example.com/in.mp4"}},
			},
			path: "/v1/videos/extensions",
			body: `{"model":"grok-imagine-video","prompt":"continue","video":{"url":"https://example.com/in.mp4"},"duration":6}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &recordingHTTPUpstream{responses: []*http.Response{grokVideoTestResponse(http.StatusOK, `{"request_id":"up_123"}`)}}
			provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))

			_, err := provider.Submit(context.Background(), grokAPIKeyAccount(), tt.request, "submit-token")

			require.NoError(t, err)
			require.Equal(t, tt.path, upstream.requests[0].URL.Path)
			require.JSONEq(t, tt.body, string(upstream.bodies[0]))
		})
	}
}

func TestGrokVideoProviderUsesOAuthCustomBaseURLAndProxy(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	proxyID := int64(9)
	account := &Account{
		ID: 82, Platform: PlatformGrok, Type: AccountTypeOAuth, ProxyID: &proxyID,
		Proxy: &Proxy{Protocol: "http", Host: "proxy.example", Port: 8080},
		Credentials: map[string]any{
			"access_token": "stored-oauth-token", "base_url": "https://relay.example/v1",
		},
	}
	upstream := &recordingHTTPUpstream{responses: []*http.Response{grokVideoTestResponse(http.StatusOK, `{"request_id":"up_123"}`)}}
	provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("oauth-token"))

	_, err := provider.Submit(context.Background(), account, CanonicalVideoRequest{
		Operation: VideoOperationGeneration, Model: "grok-imagine-video", Prompt: "waves",
	}, "submit-token")

	require.NoError(t, err)
	require.Equal(t, "https://relay.example/v1/videos/generations", upstream.requests[0].URL.String())
	require.Equal(t, "Bearer oauth-token", upstream.requests[0].Header.Get("Authorization"))
	require.Empty(t, upstream.requests[0].Header.Get("X-Grok-Client-Version"))
	require.Equal(t, "http://proxy.example:8080", upstream.proxyURL)
}

func TestGrokVideoProviderSubmitAppliesAccountMappingAfterBuiltinNormalization(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	account := grokAPIKeyAccount()
	account.Credentials["model_mapping"] = map[string]any{"grok-imagine-video": "vendor-video"}
	upstream := &recordingHTTPUpstream{responses: []*http.Response{grokVideoTestResponse(http.StatusOK, `{"request_id":"up_123"}`)}}
	provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))

	_, err := provider.Submit(context.Background(), account, CanonicalVideoRequest{
		Operation: VideoOperationGeneration, Model: "grok-imagine-video-1.5", Prompt: "waves",
	}, "submit-token")

	require.NoError(t, err)
	require.JSONEq(t, `{"model":"vendor-video","prompt":"waves"}`, string(upstream.bodies[0]))
}

func TestGrokVideoProviderPollMapsUpstreamStatus(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	tests := []struct {
		name   string
		body   string
		status VideoTaskStatus
		url    string
	}{
		{name: "queued", body: `{"status":"queued"}`, status: VideoTaskQueued},
		{name: "running", body: `{"status":"in_progress"}`, status: VideoTaskRunning},
		{name: "completed", body: `{"status":"completed","video":{"url":"https://vidgen.x.ai/signed/video.mp4"}}`, status: VideoTaskSucceeded, url: "https://vidgen.x.ai/signed/video.mp4"},
		{name: "failed", body: `{"status":"failed"}`, status: VideoTaskFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &recordingHTTPUpstream{responses: []*http.Response{grokVideoTestResponse(http.StatusOK, tt.body)}}
			provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))

			taskID := "up_123"
			got, err := provider.Poll(context.Background(), grokAPIKeyAccount(), VideoTask{UpstreamTaskID: &taskID})

			require.NoError(t, err)
			require.Equal(t, tt.status, got.Status)
			require.Equal(t, tt.url, got.ResultURL)
			require.Equal(t, "/v1/videos/up_123", upstream.requests[0].URL.Path)
			require.Equal(t, http.MethodGet, upstream.requests[0].Method)
			require.Empty(t, upstream.bodies)
		})
	}
}

func TestGrokVideoProviderPollPersistsOnlyStrictPublicVidgenURL(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	oauthAccount := &Account{
		ID: 82, Platform: PlatformGrok, Type: AccountTypeOAuth,
		Credentials: map[string]any{"base_url": "https://relay.example/v1"},
	}
	for _, tt := range []struct {
		name    string
		account *Account
		body    string
		wantURL string
		wantErr bool
	}{
		{name: "public vidgen", account: grokAPIKeyAccount(), body: `{"status":"completed","video":{"url":"https://vidgen.x.ai/signed/video.mp4?token=redacted"}}`, wantURL: "https://vidgen.x.ai/signed/video.mp4?token=redacted"},
		{name: "root public vidgen compatibility", account: grokAPIKeyAccount(), body: `{"status":"completed","url":"https://vidgen.x.ai/signed/root-video.mp4?token=redacted"}`, wantURL: "https://vidgen.x.ai/signed/root-video.mp4?token=redacted"},
		{name: "api key relay", account: grokAPIKeyAccount(), body: `{"status":"completed","video":{"url":"https://xai.test/v1/videos/up_123/content"}}`},
		{name: "root api key relay compatibility", account: grokAPIKeyAccount(), body: `{"status":"completed","url":"https://xai.test/v1/videos/up_123/content"}`},
		{name: "oauth relay", account: oauthAccount, body: `{"status":"completed","video":{"url":"https://relay.example/v1/videos/up_123/content"}}`},
		{name: "relative protected relay", account: grokAPIKeyAccount(), body: `{"status":"completed","video":{"url":"/v1/videos/up_123/content"}}`},
		{name: "unsupported public origin", account: grokAPIKeyAccount(), body: `{"status":"completed","video":{"url":"https://attacker.example/video.mp4"}}`, wantErr: true},
		{name: "unsupported root public origin", account: grokAPIKeyAccount(), body: `{"status":"completed","url":"https://attacker.example/video.mp4"}`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &recordingHTTPUpstream{responses: []*http.Response{grokVideoTestResponse(http.StatusOK, tt.body)}}
			provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))
			upstreamID := "up_123"

			got, err := provider.Poll(context.Background(), tt.account, VideoTask{UpstreamTaskID: &upstreamID})
			if tt.wantErr {
				var providerErr VideoProviderError
				require.ErrorAs(t, err, &providerErr)
				require.Equal(t, "provider_contract_error", providerErr.Code)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantURL, got.ResultURL)
		})
	}
}

func TestGrokVideoProviderPollRejectsRedirectStatusResponse(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	body := &grokVideoCloseTrackingBody{Reader: strings.NewReader(`{"status":"completed"}`)}
	upstream := &recordingHTTPUpstream{responses: []*http.Response{{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{"https://unexpected.example/videos/up_123"}},
		Body:       body,
	}}}
	provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))

	taskID := "up_123"
	_, err := provider.Poll(context.Background(), grokAPIKeyAccount(), VideoTask{UpstreamTaskID: &taskID})

	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "upstream_error", providerErr.Code)
	require.False(t, providerErr.Retryable)
	require.True(t, body.closed)
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.requests[0].Context()))
}

func TestGrokVideoProviderClassifiesFailoverAndDoesNotRecoverAmbiguousSubmission(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	upstream := &recordingHTTPUpstream{responses: []*http.Response{grokVideoTestResponse(http.StatusTooManyRequests, `{"error":{"message":"limited"}}`)}}
	provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))

	_, err := provider.Submit(context.Background(), grokAPIKeyAccount(), CanonicalVideoRequest{
		Operation: VideoOperationGeneration, Model: "grok-imagine-video", Prompt: "waves",
	}, "submit-token")
	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "upstream_rate_limit", providerErr.Code)
	require.True(t, providerErr.Retryable)
	require.False(t, providerErr.Ambiguous)

	recovered, found, recoverErr := provider.RecoverSubmission(context.Background(), grokAPIKeyAccount(), VideoTask{}, "submit-token")
	require.NoError(t, recoverErr)
	require.False(t, found)
	require.Zero(t, recovered)
}

func TestGrokVideoProviderOpenContentPreservesRangeAndTemporaryVidgenURL(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	upstream := &recordingHTTPUpstream{responses: []*http.Response{
		grokVideoTestResponse(http.StatusOK, `{"status":"completed","video":{"url":"https://vidgen.x.ai/signed-token/xai-video-task.mp4"}}`),
		{
			StatusCode: http.StatusPartialContent,
			Header: http.Header{
				"Content-Type":   []string{"video/mp4"},
				"Content-Length": []string{"13"},
				"Content-Range":  []string{"bytes 0-12/100"},
			},
			Body: io.NopCloser(strings.NewReader("video-payload")),
		},
	}}
	provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))
	upstreamID := "up_123"

	body, headers, length, err := provider.OpenContent(
		WithGrokVideoContentRange(context.Background(), "bytes=0-12"),
		grokAPIKeyAccount(),
		VideoTask{UpstreamTaskID: &upstreamID},
	)

	require.NoError(t, err)
	defer func() { require.NoError(t, body.Close()) }()
	payload, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "video-payload", string(payload))
	require.Equal(t, int64(13), length)
	require.Equal(t, "video/mp4", headers.Get("Content-Type"))
	require.Equal(t, "bytes 0-12/100", headers.Get("Content-Range"))
	require.Len(t, upstream.requests, 2)
	require.Equal(t, "/v1/videos/up_123", upstream.requests[0].URL.Path)
	require.Equal(t, "Bearer api-key", upstream.requests[0].Header.Get("Authorization"))
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.requests[0].Context()))
	require.Equal(t, "https://vidgen.x.ai/signed-token/xai-video-task.mp4", upstream.requests[1].URL.String())
	require.Empty(t, upstream.requests[1].Header.Get("Authorization"))
	require.Equal(t, "bytes=0-12", upstream.requests[1].Header.Get("Range"))
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.requests[1].Context()))
}

func TestGrokVideoProviderOpenContentWithStatusPreservesRequestedRangeNotSatisfiable(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	upstream := &recordingHTTPUpstream{responses: []*http.Response{
		grokVideoTestResponse(http.StatusOK, `{"status":"completed"}`),
		{
			StatusCode: http.StatusRequestedRangeNotSatisfiable,
			Header:     http.Header{"Content-Range": []string{"bytes */100"}},
			Body:       io.NopCloser(strings.NewReader("")),
		},
	}}
	provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))
	upstreamID := "up_123"

	body, headers, _, status, err := provider.OpenContentWithStatus(
		WithGrokVideoContentRange(context.Background(), "bytes=200-300"),
		grokAPIKeyAccount(), VideoTask{UpstreamTaskID: &upstreamID},
	)

	require.NoError(t, err)
	defer func() { require.NoError(t, body.Close()) }()
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, status)
	require.Equal(t, "bytes */100", headers.Get("Content-Range"))
	require.Equal(t, "bytes=200-300", upstream.requests[1].Header.Get("Range"))
}

func TestGrokVideoProviderOpenContentRejectsRedirectContentResponse(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	redirectBody := &grokVideoCloseTrackingBody{Reader: strings.NewReader("redirect")}
	upstream := &recordingHTTPUpstream{responses: []*http.Response{
		grokVideoTestResponse(http.StatusOK, `{"status":"completed","video":{"url":"https://vidgen.x.ai/signed-token/xai-video-task.mp4"}}`),
		{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://unexpected.example/video.mp4"}},
			Body:       redirectBody,
		},
	}}
	provider := NewGrokVideoProvider(upstream, fakeGrokTokenProvider("token"))
	upstreamID := "up_123"

	body, headers, length, err := provider.OpenContent(context.Background(), grokAPIKeyAccount(), VideoTask{UpstreamTaskID: &upstreamID})

	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "upstream_error", providerErr.Code)
	require.False(t, providerErr.Retryable)
	require.Nil(t, body)
	require.Nil(t, headers)
	require.Zero(t, length)
	require.True(t, redirectBody.closed)
	require.Len(t, upstream.requests, 2)
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.requests[1].Context()))
}

func TestGrokVideoProviderOpenContentReusesOAuthCredentialForStatusAndRelay(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	tokens := &countingGrokVideoTokenProvider{token: "oauth-token"}
	upstream := &recordingHTTPUpstream{responses: []*http.Response{
		grokVideoTestResponse(http.StatusOK, `{"status":"completed"}`),
		{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"video/mp4"}}, Body: io.NopCloser(strings.NewReader("video-payload"))},
	}}
	provider := NewGrokVideoProvider(upstream, tokens)
	upstreamID := "up_123"
	account := &Account{
		ID: 91, Platform: PlatformGrok, Type: AccountTypeOAuth,
		Credentials: map[string]any{"base_url": "https://relay.example/v1"},
	}

	body, _, _, err := provider.OpenContent(context.Background(), account, VideoTask{UpstreamTaskID: &upstreamID})

	require.NoError(t, err)
	defer func() { require.NoError(t, body.Close()) }()
	require.Equal(t, 1, tokens.calls)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, "Bearer oauth-token", upstream.requests[0].Header.Get("Authorization"))
	require.Equal(t, "Bearer oauth-token", upstream.requests[1].Header.Get("Authorization"))
}
