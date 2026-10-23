package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIRequestBodyLimitFailover_HTTP413ReturnsWithoutRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"model":"gpt-5.2","stream":false,"input":"hello"}`)

	for _, tc := range []struct {
		name        string
		accountType string
		passthrough bool
	}{
		{name: "oauth_native", accountType: AccountTypeOAuth},
		{name: "oauth_passthrough", accountType: AccountTypeOAuth, passthrough: true},
		{name: "api_key_native", accountType: AccountTypeAPIKey},
		{name: "api_key_passthrough", accountType: AccountTypeAPIKey, passthrough: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

			const upstreamBody = `{"error":{"message":"request body exceeds this account's 16MB proxy limit; secret=must-not-leak","type":"invalid_request_error"}}`
			body := &passthroughCloseTrackingReadCloser{Reader: strings.NewReader(upstreamBody)}
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusRequestEntityTooLarge,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
					"X-Request-Id": []string{"rid-body-limit"},
					"Retry-After":  []string{"30"},
				},
				Body: body,
			}}
			svc := &OpenAIGatewayService{
				cfg:          &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
				httpUpstream: upstream,
			}
			account := &Account{
				ID:          161,
				Name:        tc.name,
				Platform:    PlatformOpenAI,
				Type:        tc.accountType,
				Concurrency: 1,
				Credentials: map[string]any{
					"api_key":            "sk-test",
					"access_token":       "oauth-test",
					"chatgpt_account_id": "chatgpt-test",
					"base_url":           "https://api.example.test",
					"pool_mode":          true,
					"pool_mode_retry_status_codes": []any{
						float64(http.StatusRequestEntityTooLarge),
					},
				},
				Extra: map[string]any{
					"openai_passthrough":         tc.passthrough,
					"openai_responses_supported": true,
				},
				Status:      StatusActive,
				Schedulable: true,
			}

			result, err := svc.Forward(context.Background(), c, account, requestBody)

			require.Nil(t, result)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr), "413 must terminate the request rather than signal account failover")
			require.Len(t, upstream.requests, 1, "413 must not replay the payload on the same account")
			require.True(t, c.Writer.Written())
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
			require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, "Request payload is too large", gjson.Get(rec.Body.String(), "error.message").String())
			require.NotContains(t, rec.Body.String(), "must-not-leak")
			require.Empty(t, rec.Header().Get("Retry-After"))
			require.True(t, body.closed)
			if tc.passthrough && tc.accountType == AccountTypeAPIKey {
				require.Equal(t, requestBody, upstream.lastBody)
			} else {
				require.Equal(t, "gpt-5.2", gjson.GetBytes(upstream.lastBody, "model").String())
				if tc.accountType == AccountTypeOAuth {
					require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "input.0.content").String())
				} else {
					require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "input").String())
				}
			}
		})
	}
}

func TestOpenAIRequestBodyLimitFailover_CompatHTTP413ReturnsWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write compatErrorWriter
	}{
		{name: "chat_completions", write: writeChatCompletionsError},
		{name: "anthropic_messages", write: writeAnthropicError},
	} {
		for _, streamStarted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_started_%t", tc.name, streamStarted), func(t *testing.T) {
				c, rec := newOpenAIUpstreamErrorTestContext(t)
				if streamStarted {
					c.Writer.Header().Set("Content-Type", "text/event-stream")
					_, err := c.Writer.Write([]byte(": keepalive\n\n"))
					require.NoError(t, err)
					c.Writer.Flush()
				}
				account := newOpenAIUpstreamErrorTestAccount()
				account.Credentials = map[string]any{
					"custom_error_codes_enabled": true,
					"custom_error_codes":         []any{float64(http.StatusInternalServerError)},
				}
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				_, err := svc.handleCompatErrorResponse(
					newOpenAIUpstreamErrorResponse(http.StatusRequestEntityTooLarge, `{"error":{"message":"payload too large"}}`),
					c, account, tc.write, "gpt-5.2",
				)

				require.Error(t, err)
				var failoverErr *UpstreamFailoverError
				require.False(t, errors.As(err, &failoverErr))
				payload := rec.Body.String()
				if streamStarted {
					payload = stripKeepaliveComments(payload)
					require.Contains(t, payload, "data: ", "a committed SSE response must not receive bare JSON")
					payload = strings.TrimSpace(strings.SplitN(payload, "data: ", 2)[1])
					streamErr, ok := GetOpsStreamError(c)
					require.True(t, ok)
					require.Equal(t, http.StatusRequestEntityTooLarge, streamErr.IntendedStatus)
				} else {
					require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
				}
				require.Equal(t, "invalid_request_error", gjson.Get(payload, "error.type").String())
				require.Equal(t, "Request payload is too large", gjson.Get(payload, "error.message").String())
			})
		}
	}
}

func TestOpenAIRequestBodyLimitFailover_HTTP413AfterSSEStartedTerminatesStream(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, upstreamBody := range []string{
			`{"error":{"message":"payload too large"}}`,
			`{"error":{"code":"cyber_policy","message":"flagged for cyber policy"}}`,
		} {
			t.Run(fmt.Sprintf("passthrough_%t/%s", passthrough, upstreamBody), func(t *testing.T) {
				c, rec := newOpenAIUpstreamErrorTestContext(t)
				c.Writer.Header().Set("Content-Type", "text/event-stream")
				_, err := c.Writer.Write([]byte(": keepalive\n\n"))
				require.NoError(t, err)
				c.Writer.Flush()
				upstream := &httpUpstreamRecorder{resp: newOpenAIUpstreamErrorResponse(http.StatusRequestEntityTooLarge, upstreamBody)}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{
					ID: 163, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
					Credentials: map[string]any{"access_token": "oauth-test", "chatgpt_account_id": "chatgpt-test"},
					Extra:       map[string]any{"openai_passthrough": passthrough},
					Status:      StatusActive, Schedulable: true,
				}

				_, err = svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.2","stream":true,"input":"hello"}`))

				require.Error(t, err)
				require.Len(t, upstream.requests, 1)
				var failoverErr *UpstreamFailoverError
				require.False(t, errors.As(err, &failoverErr))
				events := parseCompactBridgeSSE(t, stripKeepaliveComments(rec.Body.String()))
				require.Len(t, events, 1)
				require.Equal(t, "response.failed", events[0][0])
				require.Equal(t, "invalid_request", gjson.Get(events[0][1], "response.error.code").String())
				require.Equal(t, "Request payload is too large", gjson.Get(events[0][1], "response.error.message").String())
				streamErr, ok := GetOpsStreamError(c)
				require.True(t, ok)
				require.Equal(t, http.StatusRequestEntityTooLarge, streamErr.IntendedStatus)
			})
		}
	}
}

func TestOpenAIRequestBodyLimitFailover_HTTP413DoesNotPenalizeAccountScheduler(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
	for _, path := range []string{"native", "passthrough", "chat_completions", "anthropic_messages"} {
		t.Run(path, func(t *testing.T) {
			c, _ := newOpenAIUpstreamErrorTestContext(t)
			cfg := &config.Config{}
			repo := &openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{openAIAdvancedSchedulerSettingKey: "true"}}
			upstream := &httpUpstreamRecorder{resp: newOpenAIUpstreamErrorResponse(http.StatusRequestEntityTooLarge,
				`{"error":{"message":"payload too large"}}`)}
			svc := &OpenAIGatewayService{
				cfg: cfg, httpUpstream: upstream,
				rateLimitService: &RateLimitService{settingService: NewSettingService(repo, cfg)},
			}
			account := &Account{
				ID: 165, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
				Credentials: map[string]any{"access_token": "oauth-test", "chatgpt_account_id": "chatgpt-test"},
				Extra:       map[string]any{"openai_passthrough": path == "passthrough"},
				Status:      StatusActive, Schedulable: true,
			}
			require.NotNil(t, svc.getOpenAIAccountScheduler(context.Background()))
			var err error
			switch path {
			case "chat_completions":
				_, err = svc.handleCompatErrorResponse(upstream.resp, c, account, writeChatCompletionsError, "gpt-5.2")
			case "anthropic_messages":
				_, err = svc.handleCompatErrorResponse(upstream.resp, c, account, writeAnthropicError, "gpt-5.2")
			default:
				_, err = svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.2","stream":false,"input":"hello"}`))
			}
			require.Error(t, err)
			require.False(t, svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.2", false, nil, fmt.Errorf("handler: %w", err)))
			errorRate, _, _ := svc.openaiAccountStats.snapshot(account.ID)
			require.Zero(t, errorRate, "a rejected payload must not lower the account's scheduling score")
			svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.2", false, nil, errors.New("upstream error: 502"))
			errorRate, _, _ = svc.openaiAccountStats.snapshot(account.ID)
			require.Greater(t, errorRate, float64(0), "non-413 failures must still be recorded")
		})
	}
}

func TestOpenAIRequestBodyLimitFailover_HTTP413OverridesRetryableErrorMessages(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"pool_mode":                    true,
		"pool_mode_retry_status_codes": []any{float64(http.StatusRequestEntityTooLarge)},
	}}
	svc := &OpenAIGatewayService{}
	for _, body := range []string{
		`{"error":{"message":"request body too large"}}`,
		`{"error":{"message":"server is overloaded","code":"server_is_overloaded"}}`,
		`{"error":{"message":"account deactivated","code":"account_deactivated"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(account, http.StatusRequestEntityTooLarge, "", []byte(body)))
			require.False(t, shouldFailoverOpenAIPassthroughResponse(account, http.StatusRequestEntityTooLarge, []byte(body)))
		})
	}
}

func TestOpenAIRequestBodyLimitFailover_CompactHTTP413DoesNotRetryFallbackModel(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough_%t", passthrough), func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.5","stream":false,"instructions":"compact-test","input":[{"type":"message","role":"user","content":"hello"},{"type":"compaction_trigger"}]}`)
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			MarkOpenAINativeCompactionV2(c)
			upstream := &httpUpstreamRecorder{resp: newOpenAIUpstreamErrorResponse(http.StatusRequestEntityTooLarge,
				`{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`)}
			svc := &OpenAIGatewayService{
				cfg:          &config.Config{Gateway: config.GatewayConfig{OpenAICompactModel: "gpt-5.4"}},
				httpUpstream: upstream,
			}
			account := &Account{
				ID: 164, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
				Credentials: map[string]any{"access_token": "oauth-test", "chatgpt_account_id": "chatgpt-test"},
				Extra:       map[string]any{"openai_passthrough": passthrough},
				Status:      StatusActive, Schedulable: true,
			}

			result, err := svc.Forward(context.Background(), c, account, body)

			require.Nil(t, result)
			require.Error(t, err)
			require.Len(t, upstream.requests, 1, "HTTP413 must not replay a compact payload using another model")
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr))
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
			require.Contains(t, gjson.Get(rec.Body.String(), "error.message").String(), "maximum context length exceeded")
		})
	}
}

func TestOpenAIRequestBodyLimitFailover_ContextWindow413DoesNotSwitchAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"model":"gpt-5.2","stream":false,"input":"hello"}`)

	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough_%t", passthrough), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

			const upstreamBody = `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error"}}`
			body := &passthroughCloseTrackingReadCloser{Reader: strings.NewReader(upstreamBody)}
			svc := &OpenAIGatewayService{
				cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
				httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusRequestEntityTooLarge,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       body,
				}},
			}
			account := &Account{
				ID: 162, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.example.test"},
				Extra: map[string]any{
					"openai_passthrough":         passthrough,
					"openai_responses_supported": true,
				},
				Status: StatusActive, Schedulable: true,
			}

			result, err := svc.Forward(context.Background(), c, account, requestBody)

			require.Nil(t, result)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr), "context-window failures are deterministic request errors")
			require.True(t, c.Writer.Written())
			require.Contains(t, rec.Body.String(), "exceeds the context window")
			require.True(t, body.closed)
		})
	}
}
