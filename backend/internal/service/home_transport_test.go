package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type homeTestHTTP struct{ client *http.Client }

func (h homeTestHTTP) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.client.Do(r)
}
func (h homeTestHTTP) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(r, p, id, n)
}
func homeTransportAccount() *Account {
	return &Account{ID: 7, Platform: "openai", Type: "oauth", Concurrency: 3, Extra: map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: transportHomeTestID}}
}
func homeTransportSetup(t *testing.T) (*transportHomeFakeHome, *OpenAIGatewayService) {
	h := transportHomeNewFake(t)
	client := transportHomeStartClient(t, h)
	return h, &OpenAIGatewayService{homeCredentials: NewHomeCredentialsService(nil, client), httpUpstream: homeTestHTTP{http.DefaultClient}}
}
func homeTransportRequest(t *testing.T, url string) *http.Request {
	r, e := http.NewRequestWithContext(context.Background(), "POST", url, strings.NewReader(`{"model":"gpt-5.1-codex","stream":true}`))
	require.NoError(t, e)
	r.Header.Set("Authorization", "Bearer stale-local-token")
	r.Header.Set("Chatgpt-Account-Id", "stale-account")
	return r
}
func TestHomeHTTPAdmissionDenialNeverSends(t *testing.T) {
	h, s := homeTransportSetup(t)
	var sent atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); w.Write([]byte(`{}`)) }))
	defer upstream.Close()
	held := []*homeclient.Lease{}
	for i := 0; i < 3; i++ {
		l, e := s.homeCredentials.Acquire(context.Background(), homeTransportAccount(), homeclient.Request{Model: transportHomeTestModel})
		require.NoError(t, e)
		held = append(held, l)
	}
	defer func() {
		for _, l := range held {
			l.Finish(homeclient.Result{})
		}
	}()
	resp, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err)
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.False(t, failure.ShouldReportAccountScheduleFailure())
	require.Zero(t, sent.Load())
	require.Equal(t, 4, h.count("RPOP", ""))
}
func TestHomeHTTPStreamHoldsLeaseAndReportsSemanticFailure(t *testing.T) {
	h, s := homeTransportSetup(t)
	release := make(chan struct{})
	received := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	req := homeTransportRequest(t, upstream.URL)
	resp, e := s.doOpenAIUpstream(req, "", homeTransportAccount())
	require.NoError(t, e)
	defer resp.Body.Close()
	hdr := <-received
	require.Equal(t, "Bearer access-token", hdr.Get("Authorization"))
	require.Equal(t, "account", hdr.Get("Chatgpt-Account-Id"))
	require.Equal(t, "Bearer stale-local-token", req.Header.Get("Authorization"))
	require.Equal(t, 1, h.count("RPOP", ""))
	require.Zero(t, h.count("LPUSH", "concurrency-release"))
	close(release)
	body, e := io.ReadAll(resp.Body)
	require.NoError(t, e)
	require.Contains(t, string(body), "response.failed")
	resp.Body.Close()
	resp.Body.Close()
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "usage") == 1 && h.count("LPUSH", "concurrency-release") == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.records {
		if rec.args[0] == "LPUSH" && rec.args[1] == "usage" {
			require.Contains(t, rec.args[2], `"failed":true`)
			require.Contains(t, rec.args[2], `"status_code":429`)
			require.NotContains(t, rec.args[2], "stale-local-token")
		}
	}
}
func TestHomeHTTPCancellationStopsUpstreamAndReleases(t *testing.T) {
	h, s := homeTransportSetup(t)
	stopped := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req := homeTransportRequest(t, upstream.URL).WithContext(ctx)
	resp, e := s.doOpenAIUpstream(req, "", homeTransportAccount())
	require.NoError(t, e)
	defer resp.Body.Close()
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream did not stop")
	}
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
}

func TestHomeHTTPFailsClosedOfflineAndWrongPin(t *testing.T) {
	for _, failure := range []string{"offline", "wrong_pin"} {
		t.Run(failure, func(t *testing.T) {
			h, s := homeTransportSetup(t)
			var sends atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends.Add(1) }))
			defer upstream.Close()
			if failure == "offline" {
				require.NoError(t, s.homeCredentials.client.(*homeclient.Client).Close())
			} else {
				h.mu.Lock()
				h.mutate = func(reply map[string]any) {
					reply["auth"].(map[string]any)["id"] = "00000000-0000-0000-0000-000000000001"
				}
				h.mu.Unlock()
			}
			_, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
			require.Error(t, err)
			require.Zero(t, sends.Load())
		})
	}
}
func TestHomeHTTPNonStreamingErrorKeepsStatusAndCooldown(t *testing.T) {
	h, s := homeTransportSetup(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"private prompt must not escape","resets_in_seconds":120}}`)
	}))
	defer upstream.Close()
	resp, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "usage") == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.args[0] == "LPUSH" && r.args[1] == "usage" {
			require.Contains(t, r.args[2], `"status_code":429`)
			require.Contains(t, r.args[2], `usage_limit_reached`)
			require.Contains(t, r.args[2], `resets_in_seconds`)
			require.NotContains(t, r.args[2], "private prompt")
		}
	}
}
func TestHomeHTTPEarlyCloseAndMembershipLossStopUpstream(t *testing.T) {
	for _, ending := range []string{"close", "membership_loss"} {
		t.Run(ending, func(t *testing.T) {
			h, s := homeTransportSetup(t)
			stopped := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(stopped)
			}))
			defer upstream.Close()
			resp, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
			require.NoError(t, err)
			defer resp.Body.Close()
			if ending == "close" {
				resp.Body.Close()
				resp.Body.Close()
			} else {
				h.mu.Lock()
				h.heartbeat = false
				h.mu.Unlock()
			}
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("live upstream survived admission loss")
			}
			if ending == "close" {
				transportHomeEventually(t, func() bool { return h.count("LPUSH", "concurrency-release") == 1 })
			}
		})
	}
}
func TestHomeHTTPNativeAccountRemainsDirect(t *testing.T) {
	h, s := homeTransportSetup(t)
	account := homeTransportAccount()
	account.Extra = nil
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer stale-local-token", r.Header.Get("Authorization"))
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	resp, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", account)
	require.NoError(t, err)
	resp.Body.Close()
	require.Zero(t, h.count("RPOP", ""))
}

func TestHomeHTTP401RecoversAdmittedTokenAfterRelease(t *testing.T) {
	h, s := homeTransportSetup(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
	}))
	defer upstream.Close()
	resp, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	transportHomeEventually(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, r := range h.records {
			if r.args[0] == "GET" && strings.Contains(r.args[1], "access_token_sha256") {
				return true
			}
		}
		return false
	})
	require.Equal(t, 1, h.count("RPOP", ""), "auth recovery must not replay a model request")
}

func TestHomeHTTPCompletedNullErrorIsSuccess(t *testing.T) {
	h, s := homeTransportSetup(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"completed","error":null,"usage":{"input_tokens":11,"output_tokens":3,"total_tokens":14}}`)
	}))
	defer upstream.Close()
	resp, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "usage") == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.args[0] == "LPUSH" && r.args[1] == "usage" {
			require.Contains(t, r.args[2], `"failed":false`)
			require.Contains(t, r.args[2], `"input_tokens":11`)
		}
	}
}

func TestHomeHTTPObserverPreservesAllBytesAfterTerminal(t *testing.T) {
	_, s := homeTransportSetup(t)
	payload := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n" + strings.Repeat(":padding\n", 10000) + "data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, payload)
	}))
	defer upstream.Close()
	resp, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.True(t, payload == string(body), "observer truncated stream: want %d bytes, got %d", len(payload), len(body))
}

func TestHomeQuotaHeaderProbeNeedsAdmission(t *testing.T) {
	h, s := homeTransportSetup(t)
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { connects.Add(1); w.WriteHeader(502) }))
	defer proxy.Close()
	parsed, e := url.Parse(proxy.URL)
	require.NoError(t, e)
	port, e := strconv.Atoi(parsed.Port())
	require.NoError(t, e)
	account := homeTransportAccount()
	id := int64(9)
	account.ProxyID = &id
	account.Proxy = &Proxy{ID: id, Protocol: "http", Host: parsed.Hostname(), Port: port}
	held := []*homeclient.Lease{}
	for i := 0; i < 3; i++ {
		l, e := s.homeCredentials.Acquire(context.Background(), account, homeclient.Request{Model: transportHomeTestModel})
		require.NoError(t, e)
		held = append(held, l)
	}
	defer func() {
		for _, l := range held {
			l.Finish(homeclient.Result{})
		}
	}()
	usage := &AccountUsageService{homeCredentials: s.homeCredentials}
	_, err := usage.probeOpenAICodexSnapshot(context.Background(), account)
	require.Error(t, err)
	require.Zero(t, connects.Load())
	require.Equal(t, 4, h.count("RPOP", ""))
}

// Only the destination changes; request headers/body and the real HTTP transport
// still pass through the production attempt gate.
type homeRedirectHTTP struct{ target string }

func (h homeRedirectHTTP) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	c := r.Clone(r.Context())
	u, _ := url.Parse(h.target)
	c.URL = u
	return http.DefaultClient.Do(c)
}
func (h homeRedirectHTTP) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(r, p, id, n)
}
func TestHomeHTTPCompactInnerRetryAdmitsActualModelEachTime(t *testing.T) {
	h, s := homeTransportSetup(t)
	var sends atomic.Int32
	models := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		models <- gjson.GetBytes(raw, "model").String()
		w.Header().Set("Content-Type", "application/json")
		if sends.Add(1) == 1 {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"context window exceeded"}}`)
			return
		}
		io.WriteString(w, `{"id":"resp_compact","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	s.httpUpstream = homeRedirectHTTP{upstream.URL}
	s.cfg = &config.Config{Gateway: config.GatewayConfig{OpenAICompactModel: "gpt-5.4"}}
	body := []byte(`{"model":"gpt-5.5","stream":false,"instructions":"compact-test","input":[{"type":"message","role":"user","content":"hello"},{"type":"compaction_trigger"}]}`)
	c := newOpenAICompactFallbackTestContext(t, "/v1/responses")
	c.Request.Body = io.NopCloser(strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	MarkOpenAINativeCompactionV2(c)
	result, err := s.Forward(context.Background(), c, homeTransportAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int32(2), sends.Load())
	require.Equal(t, "gpt-5.5", <-models)
	require.Equal(t, "gpt-5.4", <-models)
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "usage") == 2 })
	h.mu.Lock()
	defer h.mu.Unlock()
	var admitted []string
	for _, r := range h.records {
		if r.args[0] == "RPOP" {
			var req struct {
				Model string `json:"model"`
			}
			require.NoError(t, json.Unmarshal([]byte(r.args[1]), &req))
			admitted = append(admitted, req.Model)
		}
	}
	require.Equal(t, []string{"gpt-5.5", "gpt-5.4"}, admitted)
}

func TestHomeEarlyCredentialFailureUsesBoundedFailover(t *testing.T) {
	h, s := homeTransportSetup(t)
	require.NoError(t, s.homeCredentials.client.(*homeclient.Client).Close())
	_, _, err := s.GetRequestCredential(context.Background(), nil, homeTransportAccount())
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.True(t, failover.ShouldRetryNextAccount())
	require.False(t, failover.ShouldReportAccountScheduleFailure())
	require.Equal(t, 503, failover.StatusCode)
	require.Zero(t, h.count("RPOP", ""))
}
func TestHomeTransportControlFailureIsNotProxyFault(t *testing.T) {
	_, s := homeTransportSetup(t)
	a := homeTransportAccount()
	err := homeControlError(errors.New("connection refused"))
	require.False(t, classifyUpstreamTransportError(err).Persistent)
	converted := s.handleOpenAIUpstreamTransportError(context.Background(), nil, a, err, false)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, converted, &failover)
	require.False(t, failover.ShouldReportAccountScheduleFailure())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, s.handleOpenAIUpstreamTransportError(ctx, nil, a, homeControlError(context.Canceled), false), context.Canceled)
}

func TestHomeAccountTestTransportBypassesCredentialPlugin(t *testing.T) {
	h, s := homeTransportSetup(t)
	manager := &PluginManager{}
	manager.route.Store(&pluginRoute{pluginID: 1, rolloutPercent: 100, unavailable: "plugin must not receive Home credentials"})
	var sends atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		io.WriteString(w, `{"status":"completed"}`)
	}))
	defer upstream.Close()
	tests := &AccountTestService{homeCredentials: s.homeCredentials, pluginManager: manager, httpUpstream: homeTestHTTP{http.DefaultClient}}
	for _, tls := range []bool{false, true} {
		resp, err := tests.doOpenAIAccountTestUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount(), tls)
		require.NoError(t, err)
		_, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		resp.Body.Close()
	}
	s.pluginManager = manager
	resp, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, int32(3), sends.Load())
	require.Equal(t, 3, h.count("RPOP", ""))
	transportHomeEventually(t, func() bool { return h.count("LPUSH", "usage") == 3 })
}
func TestHomeObserverMemoryIsBoundedAndFailureAfterLargeDeltaStillVisible(t *testing.T) {
	o := newHomeResponseObserver(200, http.Header{"Content-Type": []string{"text/event-stream"}})
	o.observe([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\""))
	chunk := []byte(strings.Repeat("x", 8192))
	for i := 0; i < 1024; i++ {
		o.observe(chunk)
		require.LessOrEqual(t, len(o.pending), homeObserveLimit)
	}
	o.observe([]byte("\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n"))
	result := o.end(true)
	require.True(t, result.Failed)
	require.Equal(t, 429, result.StatusCode)
}

func TestHomeHTTPFreshOwnerOverridesStaleSnapshot(t *testing.T) {
	h, s := homeTransportSetup(t)
	current := homeTransportAccount()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{current.ID: current}}
	s.accountRepo = repo
	s.homeCredentials = NewHomeCredentialsService(repo, s.homeCredentials.client)
	stale := *current
	stale.Extra = nil
	stale.Credentials = map[string]any{"access_token": "poisoned-cache", "chatgpt_account_id": "old-owner"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		require.Equal(t, "account", r.Header.Get("Chatgpt-Account-Id"))
		io.WriteString(w, `{"status":"completed"}`)
	}))
	defer upstream.Close()
	resp, e := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", &stale)
	require.NoError(t, e)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, 1, h.count("RPOP", ""))
	require.Equal(t, int64(7), stale.ID)
	require.Equal(t, "poisoned-cache", stale.GetOpenAIAccessToken())
	require.Nil(t, stale.Extra)
}
