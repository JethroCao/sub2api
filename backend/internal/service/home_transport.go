package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	"github.com/tidwall/gjson"
)

// HomeControlError identifies admission/control-plane failures. It is not a
// proxy fault and must not poison account transport health.
type HomeControlError struct{ Cause error }

func (e *HomeControlError) Error() string { return "Home admission is unavailable" }
func (e *HomeControlError) Unwrap() error { return e.Cause }
func homeControlError(err error) error {
	if err == nil {
		return nil
	}
	var existing *HomeControlError
	if errors.As(err, &existing) {
		return err
	}
	return &HomeControlError{err}
}

func homeAttemptOwner(ctx context.Context, repo AccountRepository, home *HomeCredentialsService, account *Account) (*Account, error) {
	if home != nil {
		return home.CurrentOwner(ctx, account)
	}
	return currentHomeCredentialOwner(ctx, repo, account)
}
func beginHomeAttempt(ctx context.Context, home *HomeCredentialsService, account *Account, model string) (*homeclient.Lease, error) {
	lease, err := home.Acquire(ctx, account, homeclient.Request{Model: model})
	return lease, homeControlError(err)
}
func applyHomeHeaders(headers http.Header, c homeclient.Credential) {
	// Header overrides and stale request snapshots may never override Home identity.
	for key := range headers {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Chatgpt-Account-Id") || strings.EqualFold(key, "OpenAI-Organization") {
			delete(headers, key)
		}
	}
	headers.Set("Authorization", "Bearer "+c.AccessToken)
	headers.Set("Chatgpt-Account-Id", c.AccountID)
}
func homeHeaderIdentity(headers http.Header) [32]byte {
	return sha256.Sum256([]byte(headers.Get("Authorization") + "\x00" + headers.Get("Chatgpt-Account-Id")))
}
func homeCredentialIdentity(c homeclient.Credential) [32]byte {
	h := make(http.Header)
	applyHomeHeaders(h, c)
	return homeHeaderIdentity(h)
}

func doHomeHTTP(request *http.Request, account *Account, home *HomeCredentialsService, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	clone := request.Clone(request.Context())
	var reader io.ReadCloser
	var err error
	if request.GetBody != nil {
		reader, err = request.GetBody()
	} else if request.Body != nil {
		// Rare non-replayable callers still retain the exact original bytes.
		var raw []byte
		raw, err = io.ReadAll(request.Body)
		_ = request.Body.Close()
		clone.Body = io.NopCloser(bytes.NewReader(raw))
		reader = io.NopCloser(bytes.NewReader(raw))
	}
	if err != nil {
		return nil, homeControlError(err)
	}
	if reader == nil {
		return nil, homeControlError(errors.New("missing upstream model"))
	}
	raw, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, homeControlError(err)
	}
	model := strings.TrimSpace(gjson.GetBytes(raw, "model").String())
	if model == "" {
		return nil, homeControlError(errors.New("missing upstream model"))
	}
	lease, err := beginHomeAttempt(request.Context(), home, account, model)
	if err != nil {
		return nil, err
	}
	recoveryAccount := homeAttemptRecoveryAccount(account, lease.Credential)
	clone = clone.WithContext(lease.Context())
	applyHomeHeaders(clone.Header, lease.Credential)
	response, err := send(clone)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		lease.Finish(homeclient.Result{Failed: true, ErrorCode: "transport_error"})
		return nil, err
	}
	if response == nil || response.Body == nil {
		lease.Finish(homeclient.Result{Failed: true, ErrorCode: "empty_response"})
		return response, nil
	}
	observer := newHomeResponseObserver(response.StatusCode, response.Header)
	response.Body = newHomeHTTPBody(response.Body, lease, observer, func(result homeclient.Result) {
		recoverHomeAttempt(home, recoveryAccount, lease.Credential.AccessToken, result)
	})
	return response, nil
}

// Observation projects the small control/usage schema while streaming past
// arbitrarily large output strings. No whole body or SSE line is retained.
const homeObserveLimit = 256 << 10

type homeResponseObserver struct {
	result                homeclient.Result
	terminal              bool
	sse                   bool
	pending               []byte // at most the five-byte SSE data: prefix
	parser                *homeJSONProjector
	lineData, lineIgnored bool
	doneMarkerPos         int
	doneMarkerInvalid     bool
}

func newHomeResponseObserver(status int, headers http.Header) *homeResponseObserver {
	o := &homeResponseObserver{result: homeclient.Result{StatusCode: status, Failed: status >= 400, RequestID: headers.Get("X-Request-Id")}}
	o.sse = strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/event-stream")
	o.result.Stream = o.sse
	if seconds, e := strconv.ParseFloat(headers.Get("Retry-After"), 64); e == nil && seconds > 0 {
		o.result.RetryAfter = time.Duration(seconds * float64(time.Second))
	}
	return o
}
func (o *homeResponseObserver) observe(data []byte) {
	if !o.sse {
		if o.parser == nil {
			o.parser = &homeJSONProjector{}
		}
		o.parser.feed(data)
		return
	}
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			o.observeLine(data)
			return
		}
		o.observeLine(data[:i])
		o.finishLine()
		data = data[i+1:]
	}
}
func (o *homeResponseObserver) observeLine(data []byte) {
	if o.lineIgnored {
		return
	}
	for !o.lineData && len(data) > 0 {
		ch := data[0]
		data = data[1:]
		if len(o.pending) == 0 && (ch == ' ' || ch == '\t' || ch == '\r') {
			continue
		}
		if ch != "data:"[len(o.pending)] {
			o.lineIgnored = true
			return
		}
		o.pending = append(o.pending, ch)
		if len(o.pending) == 5 {
			o.lineData = true
			o.parser = &homeJSONProjector{}
		}
	}
	if !o.lineData {
		return
	}
	if !o.doneMarkerInvalid {
		for _, ch := range data {
			if ch == ' ' || ch == '\t' || ch == '\r' {
				if o.doneMarkerPos == 0 || o.doneMarkerPos == 6 {
					continue
				}
			}
			if o.doneMarkerPos >= 6 || ch != "[DONE]"[o.doneMarkerPos] {
				o.doneMarkerInvalid = true
				break
			}
			o.doneMarkerPos++
		}
	}
	o.parser.feed(data)
}
func (o *homeResponseObserver) finishLine() {
	if o.lineData {
		if !o.doneMarkerInvalid && o.doneMarkerPos == 6 {
			o.terminal = true
		} else if raw := o.parser.finish(); len(raw) > 0 {
			o.event(raw)
		}
	}
	o.parser = nil
	o.pending = o.pending[:0]
	o.lineData = false
	o.lineIgnored = false
	o.doneMarkerPos = 0
	o.doneMarkerInvalid = false
}
func (o *homeResponseObserver) event(raw []byte) {
	if bytes.Equal(raw, []byte("[DONE]")) {
		o.terminal = true
		return
	}
	if !gjson.ValidBytes(raw) {
		return
	}
	typ := gjson.GetBytes(raw, "type").String()
	status := gjson.GetBytes(raw, "response.status").String()
	if status == "" {
		status = gjson.GetBytes(raw, "status").String()
	}
	if typ == "error" || typ == "response.failed" || status == "failed" || (gjson.GetBytes(raw, "error").Exists() && gjson.GetBytes(raw, "error").Type != gjson.Null) {
		o.result.Failed = true
		message := extractUpstreamErrorMessage(raw)
		if o.result.StatusCode < 400 {
			o.result.StatusCode = openAIStreamFailedEventSemanticStatus(raw, message)
		}
		code := openAIStreamFailedEventErrorCode(raw)
		if code == "" {
			code = extractUpstreamErrorCode(raw)
		}
		if code == "" {
			code = gjson.GetBytes(raw, "error.type").String()
		}
		if code == "" {
			code = gjson.GetBytes(raw, "response.error.type").String()
		}
		o.result.ErrorCode = code
		o.terminal = true
	}
	if isOpenAIWSTerminalEvent(typ) || typ == "response.done" || status == "completed" || status == "incomplete" {
		o.terminal = true
	}
	if typ == "response.incomplete" || status == "incomplete" {
		o.result.Failed = true
		o.result.ErrorCode = "response_incomplete"
	}
	usage := gjson.GetBytes(raw, "response.usage")
	if !usage.Exists() {
		usage = gjson.GetBytes(raw, "usage")
	}
	if usage.Exists() {
		o.result.InputTokens = usage.Get("input_tokens").Int()
		o.result.OutputTokens = usage.Get("output_tokens").Int()
		o.result.TotalTokens = usage.Get("total_tokens").Int()
		o.result.CachedTokens = usage.Get("input_tokens_details.cached_tokens").Int()
		o.result.ReasoningTokens = usage.Get("output_tokens_details.reasoning_tokens").Int()
	}
	if d := gjson.GetBytes(raw, "error.resets_in_seconds").Int(); d > 0 {
		o.result.RetryAfter = time.Duration(d) * time.Second
	}
	if d := gjson.GetBytes(raw, "error.resets_at").Int(); d > 0 {
		o.result.ResetAt = time.Unix(d, 0)
	}
}
func (o *homeResponseObserver) end(eof bool) homeclient.Result {
	if o.sse {
		o.finishLine()
	} else if o.parser != nil {
		if raw := o.parser.finish(); len(raw) > 0 {
			o.event(raw)
		}
	}
	if o.sse && !o.terminal {
		o.result.Failed = true
		o.result.ErrorCode = "stream_interrupted"
	} else if !eof && !o.terminal {
		o.result.Failed = true
		if o.result.ErrorCode == "" {
			o.result.ErrorCode = "transport_interrupted"
		}
	}
	return o.result
}

type homeHTTPBody struct {
	body        io.ReadCloser
	lease       *homeclient.Lease
	observer    *homeResponseObserver
	mu          sync.Mutex
	once        sync.Once
	done        chan struct{}
	afterFinish func(homeclient.Result)
}

func newHomeHTTPBody(body io.ReadCloser, lease *homeclient.Lease, o *homeResponseObserver, afterFinish func(homeclient.Result)) *homeHTTPBody {
	b := &homeHTTPBody{body: body, lease: lease, observer: o, done: make(chan struct{}), afterFinish: afterFinish}
	// The watcher closes the real body before releasing. Finish cancels Context;
	// done prevents that cancellation from recursively finalizing this owner.
	go func() {
		select {
		case <-lease.Context().Done():
			b.finish(false)
		case <-b.done:
		}
	}()
	return b
}
func (b *homeHTTPBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.mu.Lock()
	b.observer.observe(p[:n])
	b.mu.Unlock()
	// Keep reading after a terminal event: [DONE], trailers and compatibility
	// frames still belong to the caller. EOF or its explicit Close ends transport.
	if err != nil {
		b.finish(errors.Is(err, io.EOF))
	}
	return n, err
}
func (b *homeHTTPBody) Close() error { b.finish(false); return nil }
func (b *homeHTTPBody) finish(eof bool) {
	b.once.Do(func() {
		close(b.done)
		// Closing/canceling the underlying transport must precede Finish, even when
		// an application only read a bounded error prefix or abandoned a stream.
		_ = b.body.Close()
		b.mu.Lock()
		result := b.observer.end(eof)
		b.mu.Unlock()
		b.lease.Finish(result)
		if b.afterFinish != nil {
			b.afterFinish(result)
		}
	})
}

// Admission may resolve a newer binding than the selected account snapshot.
// Pin recovery to that admitted UUID and retain the selected local ID. Recover
// rereads that ID and resolves any persisted shadow parent before checking the
// UUID. The expected binding itself must be an ordinary owner shape: shadow
// rows cannot carry Home binding metadata.
func homeAttemptRecoveryAccount(account *Account, credential homeclient.Credential) *Account {
	return &Account{
		ID: account.ID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Extra: map[string]any{OpenAICredentialSourceKey: "home", HomeCredentialIDKey: credential.ID},
	}
}

// Credential recovery is metadata work owned by Home. It starts after release
// and never replays the request that received a401.
func recoverHomeAttempt(home *HomeCredentialsService, account *Account, at string, result homeclient.Result) {
	if result.StatusCode != http.StatusUnauthorized || home == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = home.Recover(ctx, account, at)
	}()
}

func homeRequestFailure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	var control *HomeControlError
	if !errors.As(err, &control) {
		return err
	}
	status := http.StatusServiceUnavailable
	reason := GatewayFailureReason("home_control_unavailable")
	headers := make(http.Header)
	var refusal *homeclient.Error
	if errors.As(err, &refusal) {
		if refusal.Type == "credential_concurrency_exceeded" {
			status = http.StatusTooManyRequests
			reason = "home_capacity"
		}
		if refusal.RetryAfter > 0 {
			headers.Set("Retry-After", strconv.FormatInt(int64((refusal.RetryAfter+time.Second-1)/time.Second), 10))
		}
	}
	return &UpstreamFailoverError{StatusCode: status, ResponseHeaders: headers, ResponseBody: []byte(`{"error":{"type":"home_admission_unavailable","message":"Shared upstream capacity is unavailable"}}`), Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeProvider, Reason: reason, NextAccountAction: NextAccountRetry, RequestScopedTransient: true, ClientStatusCode: status, ClientMessage: "Shared upstream capacity is unavailable"}
}
func isHomeRequestFailure(err error) bool {
	var failure *UpstreamFailoverError
	return errors.As(err, &failure) && (failure.Reason == "home_control_unavailable" || failure.Reason == "home_capacity")
}
