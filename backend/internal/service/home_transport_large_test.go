package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The observer must preserve every supported classifier input even when the
// useful fields follow an output string larger than its observation budget.
func TestHomeProjectionPreservesSemanticErrorShapes(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		status       int
	}{
		{"root_auth_message", `"message":"authentication_error"`, 401},
		{"root_rate_message", `"message":"rate_limit_exceeded"`, 429},
		{"root_permission_message", `"message":"permission denied"`, 403},
		{"root_context_code", `"code":"context_length_exceeded"`, 400},
		{"root_access_code", `"code":"account_deactivated"`, 403},
		{"detail_auth_string", `"detail":"unauthorized"`, 401},
		{"detail_rate_string", `"detail":"rate_limit_exceeded"`, 429},
		{"detail_access_code", `"detail":{"code":"workspace_disabled"}`, 403},
	} {
		for _, large := range []bool{false, true} {
			for _, sse := range []bool{false, true} {
				for _, chunk := range []int{1, 7, 8192} {
					t.Run(fmt.Sprintf("%s/large=%t/sse=%t/chunk=%d", tc.name, large, sse, chunk), func(t *testing.T) {
						output := ""
						if large {
							output = strings.Repeat("x", homeObserveLimit+100)
						}
						payload := `{"type":"error","output":"` + output + `",` + tc.fields + `}`
						direct := newHomeResponseObserver(200, http.Header{})
						direct.event([]byte(payload))
						require.Equal(t, tc.status, direct.end(true).StatusCode)
						header := http.Header{}
						if sse {
							header.Set("Content-Type", "text/event-stream")
							payload = "data: " + payload + "\n\ndata: [DONE]\n\n"
						}
						observer := newHomeResponseObserver(200, header)
						for start := 0; start < len(payload); start += chunk {
							observer.observe([]byte(payload[start:min(start+chunk, len(payload))]))
						}
						result := observer.end(true)
						require.True(t, result.Failed)
						require.Equal(t, tc.status, result.StatusCode)
					})
				}
			}
		}
	}
}

func TestHomeLargeTerminalResponsesPreserveOutcomeAndUsage(t *testing.T) {
	largeOutput := strings.Repeat("escaped\\\"image-base64-", 40000)
	for _, tc := range []struct {
		name, contentType, prefix, suffix string
		failed                            bool
		status                            int
	}{
		{"sse_completed_without_done", "text/event-stream", `data: {"response":{"output":[{"result":"`, `"}],"status":"completed","usage":{"input_tokens":71,"output_tokens":13,"total_tokens":84,"input_tokens_details":{"cached_tokens":9},"output_tokens_details":{"reasoning_tokens":4}}},"type":"response.completed"}` + "\n\n", false, 200},
		{"json_completed", "application/json", `{"output":[{"result":"`, `"}],"status":"completed","error":null,"usage":{"input_tokens":71,"output_tokens":13,"total_tokens":84,"input_tokens_details":{"cached_tokens":9},"output_tokens_details":{"reasoning_tokens":4}}}`, false, 200},
		{"sse_failed_followed_by_done", "text/event-stream", `data: {"response":{"output":[{"result":"`, `"}],"status":"failed","error":{"code":"rate_limit_exceeded","type":"usage_limit_reached","message":"private message","status_code":429},"usage":{"input_tokens":71,"output_tokens":13,"total_tokens":84,"input_tokens_details":{"cached_tokens":9},"output_tokens_details":{"reasoning_tokens":4}}},"type":"response.failed"}` + "\n\ndata: [DONE]\n\n", true, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := homeTransportSetup(t)
			payload := tc.prefix + largeOutput + tc.suffix
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, payload)
			}))
			defer upstream.Close()
			response, err := s.doOpenAIUpstream(homeTransportRequest(t, upstream.URL), "", homeTransportAccount())
			require.NoError(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.True(t, string(body) == payload, "all response bytes must pass through")
			transportHomeEventually(t, func() bool { return h.count("LPUSH", "usage") == 1 })
			h.mu.Lock()
			defer h.mu.Unlock()
			for _, record := range h.records {
				if record.args[0] == "LPUSH" && record.args[1] == "usage" {
					var report struct {
						Failed bool `json:"failed"`
						Fail   struct {
							StatusCode int    `json:"status_code"`
							Body       string `json:"body"`
						} `json:"fail"`
						Tokens struct {
							Input     int64 `json:"input_tokens"`
							Output    int64 `json:"output_tokens"`
							Total     int64 `json:"total_tokens"`
							Cached    int64 `json:"cached_tokens"`
							Reasoning int64 `json:"reasoning_tokens"`
						} `json:"tokens"`
					}
					require.NoError(t, json.Unmarshal([]byte(record.args[2]), &report))
					require.Equal(t, tc.failed, report.Failed)
					require.Equal(t, tc.status, report.Fail.StatusCode)
					require.Equal(t, int64(71), report.Tokens.Input)
					require.Equal(t, int64(13), report.Tokens.Output)
					require.Equal(t, int64(84), report.Tokens.Total)
					require.Equal(t, int64(9), report.Tokens.Cached)
					require.Equal(t, int64(4), report.Tokens.Reasoning)
					require.NotContains(t, record.args[2], "private message")
				}
			}
		})
	}
}

func TestHomeJSONProjectionAcrossChunkBoundaries(t *testing.T) {
	payload := `{"output":[{"text":"fake \\\"type\\\":\\\"response.failed\\\" { ] \\u0078"},[null,true,false,-12.34e+5]],"response":{"status":"completed","usage":{"input_tokens":17,"output_tokens":5,"input_tokens_details":{"cached_tokens":3}}},"\u0074ype":"response.completed"}`
	require.True(t, json.Valid([]byte(payload)))
	for _, size := range []int{1, 2, 3, 7, 31, 4096} {
		p := &homeJSONProjector{}
		for start := 0; start < len(payload); start += size {
			end := start + size
			if end > len(payload) {
				end = len(payload)
			}
			p.feed([]byte(payload[start:end]))
		}
		raw := p.finish()
		require.NotEmpty(t, raw, "chunk size %d", size)
		require.True(t, json.Valid(raw))
		var projected map[string]any
		require.NoError(t, json.Unmarshal(raw, &projected))
		_, hasOutput := projected["output"]
		require.False(t, hasOutput)
		o := newHomeResponseObserver(200, http.Header{})
		o.event(raw)
		r := o.end(true)
		require.False(t, r.Failed)
		require.True(t, o.terminal)
		require.Equal(t, int64(17), r.InputTokens)
		require.Equal(t, int64(3), r.CachedTokens)
	}
	for _, malformed := range []string{`{"output":[1,],"status":"completed"}`, `{"output":"bad\q","status":"completed"}`, `{"output":01,"status":"completed"}`, `{"output":1e+,"status":"completed"}`, `{"status":"completed"}garbage`, `{"output":{"unclosed":"string"}`} {
		p := &homeJSONProjector{}
		p.feed([]byte(malformed))
		require.Empty(t, p.finish(), malformed)
	}
}
func TestHomeLargeObservationMemoryBound(t *testing.T) {
	for _, sse := range []bool{false, true} {
		header := http.Header{}
		if sse {
			header.Set("Content-Type", "text/event-stream")
		}
		o := newHomeResponseObserver(200, header)
		prefix := `{"output":[{"image":"`
		if sse {
			prefix = "data: " + prefix
		}
		o.observe([]byte(prefix))
		assertBound := func() {
			t.Helper()
			retained := cap(o.pending)
			if o.parser != nil {
				raw, err := json.Marshal(o.parser.root)
				require.NoError(t, err)
				retained += len(raw) + cap(o.parser.token)
				for _, frame := range o.parser.stack {
					retained += len(frame.key)
				}
			}
			require.Less(t, retained, homeObserveLimit)
		}
		chunk := []byte(strings.Repeat("x", 8192))
		for i := 0; i < 2048; i++ {
			o.observe(chunk)
			assertBound()
		}
		o.observe([]byte(`"}],"error":{"message":"`))
		// Even a very large selected message must not hide the later error code.
		for i := 0; i < 128; i++ {
			o.observe(chunk)
			assertBound()
		}
		o.observe([]byte(`","code":"rate_limit_exceeded","status_code":429},"status":"failed","usage":{"input_tokens":21,"output_tokens":8}}`))
		if sse {
			o.observe([]byte("\n\ndata: [DONE]\n\n"))
		}
		r := o.end(true)
		assertBound()
		require.True(t, r.Failed)
		require.Equal(t, 429, r.StatusCode)
		require.Equal(t, int64(21), r.InputTokens)
		require.Equal(t, int64(8), r.OutputTokens)
	}
}
