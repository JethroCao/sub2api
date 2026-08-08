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
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

// The six fixtures below are deterministic schema examples transcribed on
// 2026-08-08 from the official Kling Open Platform legacy API pages. They are
// not responses from paid tasks:
//   - https://kling.ai/document-api/api/video/1-6/text-to-video
//   - https://kling.ai/document-api/api/video/1-6/image-to-video
//   - https://kling.ai/document-api/api/video/1-6/video-extension

type klingFixedClock struct {
	mu  sync.RWMutex
	now time.Time
}

func newKlingFixedClock(now time.Time) *klingFixedClock { return &klingFixedClock{now: now} }

func (c *klingFixedClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *klingFixedClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type klingFixtureUpstream struct {
	mu           sync.Mutex
	fixture      []byte
	status       int
	err          error
	requests     []*http.Request
	bodies       [][]byte
	responseBody io.ReadCloser
	responses    [][]byte
}

func newKlingFixtureUpstream(t *testing.T, fixture string) *klingFixtureUpstream {
	t.Helper()
	body, err := os.ReadFile(fixture)
	require.NoError(t, err)
	return &klingFixtureUpstream{fixture: body, status: http.StatusOK}
}

func (u *klingFixtureUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if req != nil {
		clone := req.Clone(req.Context())
		clone.Header = req.Header.Clone()
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		u.requests = append(u.requests, clone)
		u.bodies = append(u.bodies, append([]byte(nil), body...))
	}
	if u.err != nil {
		return nil, u.err
	}
	body := append([]byte(nil), u.fixture...)
	if len(u.responses) > 0 {
		body = append([]byte(nil), u.responses[0]...)
		u.responses = u.responses[1:]
	}
	responseBody := u.responseBody
	if responseBody == nil {
		responseBody = io.NopCloser(bytes.NewReader(body))
	}
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: responseBody}, nil
}

func (u *klingFixtureUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *klingFixtureUpstream) lastRequest(t *testing.T) (*http.Request, []byte) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	require.NotEmpty(t, u.requests)
	return u.requests[len(u.requests)-1], append([]byte(nil), u.bodies[len(u.bodies)-1]...)
}

type klingCloseTrackingBody struct {
	io.Reader
	closed bool
}

func (b *klingCloseTrackingBody) Close() error {
	b.closed = true
	return nil
}

func klingAccount() *Account {
	return &Account{
		ID:          92,
		Platform:    PlatformVideo,
		Type:        AccountTypeAPIKey,
		Concurrency: 3,
		Extra:       map[string]any{"video_provider": VideoProviderKling},
		Credentials: map[string]any{
			"access_key":    "access",
			"secret_key":    "secret",
			"model_mapping": map[string]any{"kling-3.0": "kling-v3-upstream"},
		},
	}
}

func klingPublicResolver(context.Context, string) error { return nil }

func newTestKlingProvider(upstream HTTPUpstream, clock KlingClock) *KlingVideoProvider {
	return NewKlingVideoProvider(upstream, clock, klingPublicResolver)
}

func klingPollTask(operation VideoOperation, kind, taskID string) VideoTask {
	return VideoTask{
		Operation:      string(operation),
		UpstreamTaskID: &taskID,
		RequestPayload: []byte(`{"provider_task_kind":"` + kind + `"}`),
	}
}

func parseAndVerifyKlingJWT(t *testing.T, tokenString, secret string, now time.Time) *jwt.RegisteredClaims {
	t.Helper()
	claims := &jwt.RegisteredClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		require.Equal(t, jwt.SigningMethodHS256.Alg(), token.Method.Alg())
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithTimeFunc(func() time.Time { return now }))
	require.NoError(t, err)
	require.True(t, token.Valid)
	return claims
}

func TestKlingJWTClaims(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := newKlingFixedClock(now)
	token, err := SignKlingJWT("access", "secret", clock)
	require.NoError(t, err)
	claims := parseAndVerifyKlingJWT(t, token, "secret", now)
	require.Equal(t, "access", claims.Issuer)
	require.Equal(t, int64(1_800_001_800), claims.ExpiresAt.Unix())
	require.Equal(t, int64(1_799_999_995), claims.NotBefore.Unix())
}

func TestKlingJWTRejectsMissingCredentialsWithoutEcho(t *testing.T) {
	for _, tt := range []struct{ access, secret string }{{"", "secret"}, {"access", ""}} {
		_, err := SignKlingJWT(tt.access, tt.secret, newKlingFixedClock(time.Unix(1_800_000_000, 0)))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
		require.NotContains(t, err.Error(), "access")
	}
}

func TestKlingContractFixturesRetainOfficialEnvelopeFields(t *testing.T) {
	creates := []string{"text_to_video_create.json", "image_to_video_create.json", "video_extend_create.json"}
	for _, name := range creates {
		raw, err := os.ReadFile("testdata/video/kling/" + name)
		require.NoError(t, err)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(raw, &envelope))
		for _, key := range []string{"code", "message", "request_id", "data"} {
			require.Contains(t, envelope, key)
		}
		data, ok := envelope["data"].(map[string]any)
		require.True(t, ok)
		for _, key := range []string{"task_id", "task_info", "task_status", "created_at", "updated_at"} {
			require.Contains(t, data, key)
		}
		taskInfo, ok := data["task_info"].(map[string]any)
		require.True(t, ok)
		require.Contains(t, taskInfo, "external_task_id")
	}

	queries := []string{"text_to_video_succeeded.json", "image_to_video_succeeded.json", "video_extend_succeeded.json"}
	for _, name := range queries {
		raw, err := os.ReadFile("testdata/video/kling/" + name)
		require.NoError(t, err)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(raw, &envelope))
		data, ok := envelope["data"].(map[string]any)
		require.True(t, ok)
		for _, key := range []string{"task_id", "task_status", "task_status_msg", "task_info", "task_result", "watermark_info", "final_unit_deduction", "final_balance_deduction", "created_at", "updated_at"} {
			require.Contains(t, data, key)
		}
		taskResult, ok := data["task_result"].(map[string]any)
		require.True(t, ok)
		videos, ok := taskResult["videos"].([]any)
		require.True(t, ok)
		require.NotEmpty(t, videos)
		video, ok := videos[0].(map[string]any)
		require.True(t, ok)
		for _, key := range []string{"id", "url", "watermark_url", "duration"} {
			require.Contains(t, video, key)
		}
		balanceDeduction, ok := data["final_balance_deduction"].(map[string]any)
		require.True(t, ok)
		for _, key := range []string{"quota", "list_price"} {
			require.Contains(t, balanceDeduction, key)
		}
	}

	raw, err := os.ReadFile("testdata/video/kling/video_extend_succeeded.json")
	require.NoError(t, err)
	var extension map[string]any
	require.NoError(t, json.Unmarshal(raw, &extension))
	data, ok := extension["data"].(map[string]any)
	require.True(t, ok)
	taskInfo, ok := data["task_info"].(map[string]any)
	require.True(t, ok)
	parent, ok := taskInfo["parent_video"].(map[string]any)
	require.True(t, ok)
	for _, key := range []string{"id", "url", "duration"} {
		require.Contains(t, parent, key)
	}
}

func TestKlingProviderSubmitRoutesAndBodies(t *testing.T) {
	clock := newKlingFixedClock(time.Unix(1_800_000_000, 0))
	tests := []struct {
		name    string
		fixture string
		request CanonicalVideoRequest
		path    string
		want    string
	}{
		{
			name: "text to video", fixture: "text_to_video_create.json", path: "/v1/videos/text2video",
			request: CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "animate", DurationSeconds: 5, AspectRatio: "16:9"},
			want:    `{"model_name":"kling-v3-upstream","prompt":"animate","duration":"5","aspect_ratio":"16:9","external_task_id":"submit_example"}`,
		},
		{
			name: "image to video", fixture: "image_to_video_create.json", path: "/v1/videos/image2video",
			request: CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "animate", FirstFrame: []VideoAsset{{URL: "https://example.com/first.png"}}, LastFrame: []VideoAsset{{URL: "https://example.com/last.png"}}, DurationSeconds: 5},
			want:    `{"model_name":"kling-v3-upstream","image":"https://example.com/first.png","image_tail":"https://example.com/last.png","prompt":"animate","duration":"5","external_task_id":"submit_example"}`,
		},
		{
			name: "video extension", fixture: "video_extend_create.json", path: "/v1/videos/video-extend",
			request: CanonicalVideoRequest{Operation: VideoOperationExtension, Model: "kling-3.0", Prompt: "continue", ReferenceVideos: []VideoAsset{{URL: "https://example.com/source.mp4"}}, ProviderOptions: map[string]json.RawMessage{VideoProviderKling: json.RawMessage(`{"video_id":"source_video_example","negative_prompt":"blur","cfg_scale":0.5,"watermark":true}`)}},
			want:    `{"video_id":"source_video_example","prompt":"continue","negative_prompt":"blur","cfg_scale":0.5,"watermark_info":{"enabled":true},"external_task_id":"submit_example"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newKlingFixtureUpstream(t, "testdata/video/kling/"+tt.fixture)
			got, err := newTestKlingProvider(upstream, clock).Submit(context.Background(), klingAccount(), tt.request, "submit_example")
			require.NoError(t, err)
			require.Equal(t, "kling_task_example", got.UpstreamTaskID)
			require.Equal(t, VideoTaskQueued, got.Status)
			require.Equal(t, "submitted", got.UpstreamStatus)
			req, body := upstream.lastRequest(t)
			require.Equal(t, http.MethodPost, req.Method)
			require.Equal(t, "api-singapore.klingai.com", req.URL.Hostname())
			require.Equal(t, tt.path, req.URL.Path)
			require.Equal(t, "application/json", req.Header.Get("Content-Type"))
			require.True(t, strings.HasPrefix(req.Header.Get("Authorization"), "Bearer "))
			require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
			require.True(t, HTTPUpstreamResolvedIPValidationRequired(req.Context()))
			require.JSONEq(t, tt.want, string(body))
		})
	}
}

func TestKlingProviderPollRoutesAndParsesOfficialFixtures(t *testing.T) {
	clock := newKlingFixedClock(time.Unix(1_800_000_000, 0))
	for _, tt := range []struct {
		name      string
		operation VideoOperation
		request   CanonicalVideoRequest
		fixture   string
		path      string
	}{
		{name: "text", operation: VideoOperationGeneration, request: CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "text"}, fixture: "text_to_video_succeeded.json", path: "/v1/videos/text2video/kling_task_example"},
		{name: "image", operation: VideoOperationGeneration, request: CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", FirstFrame: []VideoAsset{{URL: "https://example.com/first.png"}}}, fixture: "image_to_video_succeeded.json", path: "/v1/videos/image2video/kling_task_example"},
		{name: "extension", operation: VideoOperationExtension, request: CanonicalVideoRequest{Operation: VideoOperationExtension, Model: "kling-3.0", ReferenceVideos: []VideoAsset{{URL: "https://example.com/source.mp4"}}, ProviderOptions: map[string]json.RawMessage{VideoProviderKling: json.RawMessage(`{"video_id":"source"}`)}}, fixture: "video_extend_succeeded.json", path: "/v1/videos/video-extend/kling_task_example"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newKlingFixtureUpstream(t, "testdata/video/kling/"+tt.fixture)
			provider := newTestKlingProvider(upstream, clock)
			kind, err := klingProviderTaskKind(tt.request)
			require.NoError(t, err)
			got, err := provider.Poll(context.Background(), klingAccount(), klingPollTask(tt.operation, kind, "kling_task_example"))
			require.NoError(t, err)
			require.Equal(t, VideoTaskSucceeded, got.Status)
			require.Equal(t, "succeed", got.UpstreamStatus)
			require.Equal(t, "https://cdn.example.com/video.mp4", got.ResultURL)
			require.Equal(t, 5, got.ActualDurationSeconds)
			req, _ := upstream.lastRequest(t)
			require.Equal(t, http.MethodGet, req.Method)
			require.Equal(t, tt.path, req.URL.Path)
		})
	}
}

func TestKlingProviderMapsOfficialStatuses(t *testing.T) {
	for _, tt := range []struct {
		status string
		want   VideoTaskStatus
	}{
		{"submitted", VideoTaskQueued},
		{"processing", VideoTaskRunning},
		{"succeed", VideoTaskSucceeded},
		{"failed", VideoTaskFailed},
		{"unexpected", VideoTaskUnknown},
	} {
		require.Equal(t, tt.want, klingVideoTaskStatus(tt.status))
	}
}

func TestKlingProviderRejectsEditAndUnsupportedShapes(t *testing.T) {
	provider := newTestKlingProvider(newKlingFixtureUpstream(t, "testdata/video/kling/text_to_video_create.json"), newKlingFixedClock(time.Unix(1_800_000_000, 0)))
	requests := []CanonicalVideoRequest{
		{Operation: VideoOperationEdit, Model: "kling-3.0", ReferenceVideos: []VideoAsset{{URL: "https://example.com/source.mp4"}}},
		{Operation: VideoOperationGeneration, Model: "kling-3.0", ReferenceImages: []VideoAsset{{URL: "https://example.com/reference.png"}}},
		{Operation: VideoOperationGeneration, Model: "kling-3.0", FirstFrame: []VideoAsset{{URL: "https://example.com/1.png"}, {URL: "https://example.com/2.png"}}},
	}
	for _, request := range requests {
		_, err := provider.Submit(context.Background(), klingAccount(), request, "submit-token")
		var providerErr VideoProviderError
		require.ErrorAs(t, err, &providerErr)
		require.Equal(t, "unsupported_capability", providerErr.Code)
		require.False(t, providerErr.Retryable)
		require.False(t, providerErr.Ambiguous)
	}
}

func TestKlingProviderOptionsAreStrictlyAllowlisted(t *testing.T) {
	upstream := newKlingFixtureUpstream(t, "testdata/video/kling/text_to_video_create.json")
	provider := newTestKlingProvider(upstream, newKlingFixedClock(time.Unix(1_800_000_000, 0)))
	request := CanonicalVideoRequest{
		Operation: VideoOperationGeneration,
		Model:     "kling-3.0",
		Prompt:    "animate",
		ProviderOptions: map[string]json.RawMessage{
			VideoProviderKling: json.RawMessage(`{"negative_prompt":"blur","mode":"pro","sound":"off"}`),
		},
	}
	_, err := provider.Submit(context.Background(), klingAccount(), request, "submit_example")
	require.NoError(t, err)
	_, body := upstream.lastRequest(t)
	require.JSONEq(t, `{"model_name":"kling-v3-upstream","prompt":"animate","negative_prompt":"blur","mode":"pro","sound":"off","external_task_id":"submit_example"}`, string(body))

	request.ProviderOptions[VideoProviderKling] = json.RawMessage(`{"callback_url":"https://attacker.example/callback"}`)
	_, err = provider.Submit(context.Background(), klingAccount(), request, "submit_example")
	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "invalid_request", providerErr.Code)
	require.Len(t, upstream.requests, 1)
}

func TestNormalizeVideoRequestAcceptsOnlyKlingProviderOptionAllowlist(t *testing.T) {
	request, err := NormalizeVideoRequest(VideoOperationExtension, "application/json", []byte(`{
		"model":"kling-3.0",
		"video":"https://example.com/source.mp4",
		"provider_options":{"kling":{"video_id":"video_example","negative_prompt":"blur","mode":"pro","sound":"off","cfg_scale":0.5,"watermark":true}}
	}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"video_id":"video_example","negative_prompt":"blur","mode":"pro","sound":"off","cfg_scale":0.5,"watermark":true}`, string(request.ProviderOptions[VideoProviderKling]))

	_, err = NormalizeVideoRequest(VideoOperationGeneration, "application/json", []byte(`{"model":"kling-3.0","prompt":"animate","provider_options":{"kling":{"callback_url":"https://attacker.example/callback"}}}`))
	require.ErrorIs(t, err, ErrVideoInvalidRequest)
}

func TestKlingProviderRecoveryContractGateDoesNotQueryUpstream(t *testing.T) {
	upstream := newKlingFixtureUpstream(t, "testdata/video/kling/image_to_video_succeeded.json")
	task := klingPollTask(VideoOperationGeneration, klingTaskKindImageToVideo, "kling_task_example")
	got, found, err := newTestKlingProvider(upstream, newKlingFixedClock(time.Unix(1_800_000_000, 0))).RecoverSubmission(context.Background(), klingAccount(), task, "submit_example")
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, got)
	require.Empty(t, upstream.requests)
}

func TestKlingRecoveryRouteUsesOnlyPersistedHintAfterAssetRedaction(t *testing.T) {
	tests := []struct {
		name    string
		request CanonicalVideoRequest
		kind    string
		want    string
	}{
		{
			name: "data image generation",
			request: CanonicalVideoRequest{
				Operation:  VideoOperationGeneration,
				FirstFrame: []VideoAsset{{URL: "data:image/png;base64,aW1hZ2U="}},
			},
			kind: klingTaskKindImageToVideo,
			want: klingImageToVideoPath,
		},
		{
			name: "signed source extension",
			request: CanonicalVideoRequest{
				Operation:       VideoOperationExtension,
				ReferenceVideos: []VideoAsset{{URL: "https://cdn.example.com/source.mp4?signature=private&expires=1800000000"}},
				ProviderOptions: map[string]json.RawMessage{VideoProviderKling: json.RawMessage(`{"video_id":"source_video_example"}`)},
			},
			kind: klingTaskKindVideoExtend,
			want: klingVideoExtendPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recoveryPayload := minimizedVideoRecoveryPayload(VideoProviderKling, tt.request)
			require.NotContains(t, string(recoveryPayload.Bytes()), "data:image")
			require.NotContains(t, string(recoveryPayload.Bytes()), "signature")
			acceptedPayload := minimizedVideoPollPayload(VideoProviderKling, tt.request)
			require.JSONEq(t, `{"provider_task_kind":"`+tt.kind+`"}`, string(acceptedPayload.Bytes()))

			for _, payload := range []MinimizedVideoPayload{recoveryPayload, acceptedPayload} {
				restored := VideoTask{Operation: string(tt.request.Operation), RequestPayload: payload.Bytes()}
				kind, err := klingTaskKindFromDurableTask(restored)
				require.NoError(t, err)
				require.Equal(t, tt.want, klingPathForTaskKind(kind))
			}
		})
	}
}

func TestKlingRecoveryRouteFailsClosedOnMissingMalformedOrConflictingHint(t *testing.T) {
	tests := []VideoTask{
		{Operation: string(VideoOperationGeneration)},
		{Operation: string(VideoOperationGeneration), RequestPayload: []byte(`{`)},
		{Operation: string(VideoOperationGeneration), RequestPayload: []byte(`{}`)},
		{Operation: string(VideoOperationGeneration), RequestPayload: []byte(`{"provider_task_kind":"unknown"}`)},
		{Operation: string(VideoOperationExtension), RequestPayload: []byte(`{"provider_task_kind":"image2video"}`)},
	}
	for _, task := range tests {
		kind, err := klingTaskKindFromDurableTask(task)
		require.Error(t, err)
		require.Empty(t, kind)
	}
}

func TestKlingProviderRejectsUnsafeAccountBaseURLAndResolvedDestination(t *testing.T) {
	clock := newKlingFixedClock(time.Unix(1_800_000_000, 0))
	for _, raw := range []string{"http://127.0.0.1:8080", "https://user@example.com", "https://example.com?token=secret", "https://example.com/#fragment"} {
		upstream := newKlingFixtureUpstream(t, "testdata/video/kling/text_to_video_create.json")
		account := klingAccount()
		account.Credentials["base_url"] = raw
		_, err := newTestKlingProvider(upstream, clock).Submit(context.Background(), account, CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "safe"}, "submit-token")
		var providerErr VideoProviderError
		require.ErrorAs(t, err, &providerErr)
		require.Equal(t, "invalid_request", providerErr.Code)
		require.Empty(t, upstream.requests)
		require.NotContains(t, err.Error(), raw)
	}

	upstream := newKlingFixtureUpstream(t, "testdata/video/kling/text_to_video_create.json")
	account := klingAccount()
	account.Credentials["base_url"] = "https://kling-relay.example"
	resolver := func(_ context.Context, host string) error {
		require.Equal(t, "kling-relay.example", host)
		return urlvalidator.ValidateResolvedIPs([]net.IP{net.ParseIP("127.0.0.1")})
	}
	_, err := NewKlingVideoProvider(upstream, clock, resolver).Submit(context.Background(), account, CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "safe"}, "submit-token")
	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "invalid_request", providerErr.Code)
	require.Empty(t, upstream.requests)
}

func TestKlingProviderClosesBodiesAndRejectsMalformedSuccess(t *testing.T) {
	body := &klingCloseTrackingBody{Reader: strings.NewReader(`{"code":0,"message":"success","request_id":"req_example","data":{"task_status":"submitted"}}`)}
	upstream := &klingFixtureUpstream{status: http.StatusOK, responseBody: body}
	_, err := newTestKlingProvider(upstream, newKlingFixedClock(time.Unix(1_800_000_000, 0))).Submit(context.Background(), klingAccount(), CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "private prompt"}, "submit-token")
	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "provider_contract_error", providerErr.Code)
	require.True(t, providerErr.Ambiguous)
	require.True(t, body.closed)
	require.NotContains(t, err.Error(), "private prompt")
	require.NotContains(t, err.Error(), "access")
	require.NotContains(t, err.Error(), "secret")
}

func TestKlingProviderClassifiesSubmissionAndPollFailures(t *testing.T) {
	clock := newKlingFixedClock(time.Unix(1_800_000_000, 0))
	for _, tt := range []struct {
		name      string
		status    int
		err       error
		code      string
		retryable bool
		ambiguous bool
	}{
		{name: "bad request", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "authentication", status: http.StatusUnauthorized, code: "upstream_authentication"},
		{name: "rate limit", status: http.StatusTooManyRequests, code: "upstream_rate_limit", ambiguous: true},
		{name: "server error", status: http.StatusBadGateway, code: "upstream_unavailable", ambiguous: true},
		{name: "transport timeout", err: context.DeadlineExceeded, code: "upstream_timeout", retryable: true, ambiguous: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &klingFixtureUpstream{status: tt.status, err: tt.err, fixture: []byte(`{"code":1,"message":"redacted"}`)}
			_, err := newTestKlingProvider(upstream, clock).Submit(context.Background(), klingAccount(), CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "private prompt"}, "submit-token")
			var providerErr VideoProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Equal(t, tt.code, providerErr.Code)
			require.Equal(t, tt.retryable, providerErr.Retryable)
			require.Equal(t, tt.ambiguous, providerErr.Ambiguous)
			require.NotContains(t, err.Error(), "private prompt")
		})
	}

	upstream := &klingFixtureUpstream{status: http.StatusTooManyRequests, fixture: []byte(`{"code":1,"message":"redacted"}`)}
	_, err := newTestKlingProvider(upstream, clock).Poll(context.Background(), klingAccount(), klingPollTask(VideoOperationGeneration, klingTaskKindTextToVideo, "task"))
	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "upstream_rate_limit", providerErr.Code)
	require.True(t, providerErr.Retryable)
	require.False(t, providerErr.Ambiguous)
}

func TestKlingProviderUnavailableBeforeSendIsNotAmbiguous(t *testing.T) {
	provider := NewKlingVideoProvider(nil, newKlingFixedClock(time.Unix(1_800_000_000, 0)))
	_, err := provider.Submit(context.Background(), klingAccount(), CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "animate"}, "submit_example")
	var providerErr VideoProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "upstream_unavailable", providerErr.Code)
	require.True(t, providerErr.Retryable)
	require.False(t, providerErr.Ambiguous)
}

func TestKlingProviderPollFailsClosedOnMissingOrInconsistentDurableRouteHint(t *testing.T) {
	provider := newTestKlingProvider(newKlingFixtureUpstream(t, "testdata/video/kling/text_to_video_succeeded.json"), newKlingFixedClock(time.Unix(1_800_000_000, 0)))
	taskID := "kling_task_example"
	for _, task := range []VideoTask{
		{Operation: string(VideoOperationGeneration), UpstreamTaskID: &taskID},
		{Operation: string(VideoOperationGeneration), UpstreamTaskID: &taskID, RequestPayload: []byte(`{"provider_task_kind":"video-extend"}`)},
		{Operation: string(VideoOperationExtension), UpstreamTaskID: &taskID, RequestPayload: []byte(`{"provider_task_kind":"image2video"}`)},
		{Operation: string(VideoOperationGeneration), UpstreamTaskID: &taskID, RequestPayload: []byte(`{"provider_task_kind":"unknown"}`)},
	} {
		_, err := provider.Poll(context.Background(), klingAccount(), task)
		var providerErr VideoProviderError
		require.ErrorAs(t, err, &providerErr)
		require.Equal(t, "provider_contract_error", providerErr.Code)
		require.False(t, providerErr.Ambiguous)
	}
	upstream, ok := provider.upstream.(*klingFixtureUpstream)
	require.True(t, ok)
	require.Empty(t, upstream.requests)
}

func TestKlingRecoveryPayloadPersistsStrictRouteHintAndVideoID(t *testing.T) {
	request := CanonicalVideoRequest{
		Operation:       VideoOperationExtension,
		Model:           "kling-3.0",
		ReferenceVideos: []VideoAsset{{URL: "https://example.com/source.mp4"}},
		ProviderOptions: map[string]json.RawMessage{VideoProviderKling: json.RawMessage(`{"video_id":"video_example"}`)},
	}
	payload := minimizedVideoRecoveryPayload(VideoProviderKling, request)
	require.JSONEq(t, `{"input_video_ref":"https://example.com/source.mp4","provider_task_kind":"video-extend","video_id":"video_example"}`, string(payload.Bytes()))
}

func TestKlingAcceptedTaskPayloadRetainsOnlyRouteHint(t *testing.T) {
	request := CanonicalVideoRequest{
		Operation:       VideoOperationExtension,
		Model:           "kling-3.0",
		Prompt:          "private prompt",
		ReferenceVideos: []VideoAsset{{URL: "https://example.com/source.mp4"}},
		ProviderOptions: map[string]json.RawMessage{VideoProviderKling: json.RawMessage(`{"video_id":"video_example"}`)},
	}
	payload := minimizedVideoPollPayload(VideoProviderKling, request)
	require.JSONEq(t, `{"provider_task_kind":"video-extend"}`, string(payload.Bytes()))
	require.NotContains(t, string(payload.Bytes()), "private prompt")
	require.NotContains(t, string(payload.Bytes()), "source.mp4")
	require.NotContains(t, string(payload.Bytes()), "video_example")
	require.Empty(t, minimizedVideoPollPayload(VideoProviderSeedance, request).Bytes())
}

func TestKlingProviderPollUsesPersistedMinimalRouteHintAfterRestart(t *testing.T) {
	upstream := newKlingFixtureUpstream(t, "testdata/video/kling/image_to_video_succeeded.json")
	provider := newTestKlingProvider(upstream, newKlingFixedClock(time.Unix(1_800_000_000, 0)))
	request := CanonicalVideoRequest{
		Operation:  VideoOperationGeneration,
		Model:      "kling-3.0",
		Prompt:     "private prompt",
		FirstFrame: []VideoAsset{{URL: "https://example.com/start.png"}},
	}
	payload := minimizedVideoPollPayload(VideoProviderKling, request)
	taskID := "kling_task_example"
	restored := VideoTask{
		Operation:      string(VideoOperationGeneration),
		UpstreamTaskID: &taskID,
		RequestPayload: payload.Bytes(),
	}

	result, err := provider.Poll(context.Background(), klingAccount(), restored)

	require.NoError(t, err)
	require.Equal(t, VideoTaskSucceeded, result.Status)
	req, _ := upstream.lastRequest(t)
	require.Equal(t, "/v1/videos/image2video/kling_task_example", req.URL.Path)
}

func TestKlingProviderCachesJWTConcurrentlyAndRefreshesFiveMinutesBeforeExpiry(t *testing.T) {
	clock := newKlingFixedClock(time.Unix(1_800_000_000, 0))
	upstream := newKlingFixtureUpstream(t, "testdata/video/kling/text_to_video_create.json")
	provider := newTestKlingProvider(upstream, clock)
	request := CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "animate"}

	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := provider.Submit(context.Background(), klingAccount(), request, "submit_example")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	upstream.mu.Lock()
	require.Len(t, upstream.requests, workers)
	firstToken := upstream.requests[0].Header.Get("Authorization")
	for _, req := range upstream.requests {
		require.Equal(t, firstToken, req.Header.Get("Authorization"))
	}
	upstream.mu.Unlock()

	clock.Advance(25*time.Minute - time.Second)
	_, err := provider.Submit(context.Background(), klingAccount(), request, "submit_example")
	require.NoError(t, err)
	req, _ := upstream.lastRequest(t)
	require.Equal(t, firstToken, req.Header.Get("Authorization"))

	clock.Advance(time.Second)
	_, err = provider.Submit(context.Background(), klingAccount(), request, "submit_example")
	require.NoError(t, err)
	req, _ = upstream.lastRequest(t)
	require.NotEqual(t, firstToken, req.Header.Get("Authorization"))
}

func TestKlingProviderCapabilitiesFailClosedWithoutModelConfiguration(t *testing.T) {
	require.Empty(t, NewKlingVideoProvider(nil, newKlingFixedClock(time.Unix(1_800_000_000, 0))).Capabilities())
}

func TestKlingProviderOpenContentPreservesSignedURLWithoutJWT(t *testing.T) {
	upstream := &klingFixtureUpstream{status: http.StatusOK, responseBody: io.NopCloser(strings.NewReader("video"))}
	resultURL := "https://cdn.example.com/video.mp4?Expires=123&Signature=redacted"
	body, _, _, err := newTestKlingProvider(upstream, newKlingFixedClock(time.Unix(1_800_000_000, 0))).OpenContent(context.Background(), klingAccount(), VideoTask{ResultURL: &resultURL})
	require.NoError(t, err)
	defer func() { require.NoError(t, body.Close()) }()
	req, _ := upstream.lastRequest(t)
	require.Empty(t, req.Header.Get("Authorization"))
	require.Equal(t, "Expires=123&Signature=redacted", req.URL.RawQuery)
	require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
	require.True(t, HTTPUpstreamResolvedIPValidationRequired(req.Context()))
}

func TestKlingProviderErrorsNeverLeakCredentialOrJWT(t *testing.T) {
	account := klingAccount()
	account.Credentials["access_key"] = "highly-sensitive-access"
	account.Credentials["secret_key"] = "highly-sensitive-secret"
	upstream := &klingFixtureUpstream{err: errors.New("Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature highly-sensitive-secret")}
	_, err := newTestKlingProvider(upstream, newKlingFixedClock(time.Unix(1_800_000_000, 0))).Submit(context.Background(), account, CanonicalVideoRequest{Operation: VideoOperationGeneration, Model: "kling-3.0", Prompt: "private prompt"}, "submit-token")
	require.Error(t, err)
	for _, sensitive := range []string{"highly-sensitive-access", "highly-sensitive-secret", "eyJhbGci", "private prompt"} {
		require.NotContains(t, err.Error(), sensitive)
	}
}
