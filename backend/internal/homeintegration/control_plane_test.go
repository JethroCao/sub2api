//go:build homeintegration

package homeintegration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
)

// Only the isolated deploy/tests/home_control_plane_integration.py harness
// creates this file. It contains synthetic tokens and private fixture paths.
type fixture struct {
	SourceTag     string              `json:"source_tag"`
	SourceCommit  string              `json:"source_commit"`
	ManagementURL string              `json:"management_url"`
	ManagementKey string              `json:"management_key"`
	CredentialID  string              `json:"credential_id"`
	Model         string              `json:"model"`
	AccessToken   string              `json:"access_token"`
	PreviousToken string              `json:"previous_token"`
	AccountID     string              `json:"account_id"`
	Clients       []homeclient.Config `json:"clients"`
}

type management struct {
	fixture fixture
	client  *http.Client
}

func (m management) get(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.fixture.ManagementURL+path, nil)
	if err != nil {
		return errors.New("invalid fixture management request")
	}
	req.Header.Set("Authorization", "Bearer "+m.fixture.ManagementKey)
	response, err := m.client.Do(req)
	if err != nil {
		return errors.New("fixture management request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fixture management HTTP status %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return errors.New("invalid fixture management JSON")
	}
	return nil
}

func eventually(t *testing.T, description string, limit time.Duration, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}

// This catches wire, lifecycle, accounting, and reporting incompatibilities
// between the production homeclient and an unmodified Home v1.1.7 process.
func TestHomeControlPlaneInteroperability(t *testing.T) {
	path := os.Getenv("SUB2_HOME_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("requires isolated Home fixture; run deploy/tests/home_control_plane_integration.py")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read isolated Home fixture config")
	}
	var cfg fixture
	if json.Unmarshal(raw, &cfg) != nil || cfg.SourceTag != "v1.1.7" || cfg.SourceCommit == "" || len(cfg.Clients) != 2 || cfg.CredentialID == "" || cfg.AccessToken == "" || cfg.PreviousToken == "" || cfg.Model == "" {
		t.Fatal("invalid isolated Home fixture config")
	}
	managementURL, err := url.Parse(cfg.ManagementURL)
	if err != nil || managementURL.Scheme != "http" || !net.ParseIP(managementURL.Hostname()).IsLoopback() {
		t.Fatal("fixture management endpoint must be a loopback HTTP address")
	}
	for _, client := range cfg.Clients {
		host, _, err := net.SplitHostPort(client.Address)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			t.Fatal("fixture Home endpoint must be loopback")
		}
	}
	if cfg.Clients[0].CertFile == cfg.Clients[1].CertFile {
		t.Fatal("fixture clients must use distinct certificates")
	}
	t.Logf("Home source %s (%s); two independent mTLS clients", cfg.SourceTag, cfg.SourceCommit)
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	t.Cleanup(cancel)
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	admin := management{cfg, &http.Client{Transport: transport, Timeout: 5 * time.Second}}
	var clients []*homeclient.Client
	var runs []<-chan error
	var leases []*homeclient.Lease
	t.Cleanup(func() {
		for _, lease := range leases {
			lease.Finish(homeclient.Result{StatusCode: 200})
		}
		for _, client := range clients {
			if err := client.Close(); err != nil {
				t.Errorf("client shutdown failed: %v", err)
			}
		}
		for _, run := range runs {
			select {
			case err := <-run:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("membership supervisor failed: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("membership supervisor did not stop")
			}
		}
	})
	start := func(config homeclient.Config) *homeclient.Client {
		t.Helper()
		client, err := homeclient.New(config)
		if err != nil {
			t.Fatalf("create client: %v", err)
		}
		if client.Ready() {
			t.Fatal("client ready before membership bootstrap")
		}
		if _, err := client.Acquire(ctx, homeclient.Request{CredentialID: cfg.CredentialID, Model: cfg.Model}); !errors.Is(err, homeclient.ErrNotReady) {
			t.Fatal("client admitted a request before Run")
		}
		done := make(chan error, 1)
		clients = append(clients, client)
		runs = append(runs, done)
		go func() { done <- client.Run(ctx) }()
		eventually(t, "acknowledged membership readiness", 35*time.Second, client.Ready)
		return client
	}
	acquireModel := func(client *homeclient.Client, model string) *homeclient.Lease {
		t.Helper()
		lease, err := client.Acquire(ctx, homeclient.Request{CredentialID: cfg.CredentialID, Model: model, SessionID: "fixture-session"})
		if err != nil {
			t.Fatalf("pinned admission: %v", err)
		}
		leases = append(leases, lease)
		if lease.Credential.ID != cfg.CredentialID || lease.Credential.AuthIndex != cfg.CredentialID || lease.Credential.AccessToken != cfg.AccessToken || lease.Credential.AccountID != cfg.AccountID || !lease.Credential.ExpiresAt.After(time.Now()) {
			t.Fatal("admitted credential identity, token, account, or expiration differs from the fixture")
		}
		if lease.Context().Err() != nil {
			t.Fatal("new admission context is already canceled")
		}
		return lease
	}
	acquire := func(client *homeclient.Client) *homeclient.Lease {
		t.Helper()
		return acquireModel(client, cfg.Model)
	}
	count := func(want int) {
		t.Helper()
		eventually(t, fmt.Sprintf("authoritative admitted count %d", want), 10*time.Second, func() bool {
			var response struct {
				Items []struct {
					CredentialID string `json:"credential_id"`
					Admitted     int    `json:"admitted_in_flight"`
					Remaining    int    `json:"remaining"`
				} `json:"items"`
			}
			if admin.get(ctx, "/credentials/concurrency", &response) != nil {
				return false
			}
			for _, item := range response.Items {
				if item.CredentialID == cfg.CredentialID {
					return item.Admitted == want && item.Remaining == 3-want
				}
			}
			return false
		})
	}
	a, b := start(cfg.Clients[0]), start(cfg.Clients[1])
	first, second, third := acquire(a), acquire(a), acquire(b)
	count(3)
	_, err = b.Acquire(ctx, homeclient.Request{CredentialID: cfg.CredentialID, Model: cfg.Model})
	var refused *homeclient.Error
	if !errors.As(err, &refused) || refused.Type != "credential_concurrency_exceeded" || !refused.Retryable || refused.RetryAfter <= 0 {
		t.Fatalf("fourth admission must return typed shared-limit refusal: %v", err)
	}
	t.Log("shared cap 3 and typed fourth-request refusal verified")
	// Refresh is an auxiliary operation. It must work even while every model
	// admission slot is occupied, and must preserve those occupied slots.
	refreshed, err := a.Refresh(ctx, cfg.CredentialID, cfg.PreviousToken)
	if err != nil || refreshed.ID != cfg.CredentialID || refreshed.AccessToken != cfg.AccessToken || refreshed.AccountID != cfg.AccountID {
		t.Fatalf("refresh must adopt the stored newer token while capacity is full: %v", err)
	}
	_, err = a.Refresh(ctx, "00000000-0000-4000-8000-000000000001", cfg.PreviousToken)
	if !errors.As(err, &refused) || refused.Type != "auth_not_found" {
		t.Fatalf("missing UUID must return typed auth_not_found: %v", err)
	}
	count(3)
	t.Log("observed-token refresh and missing-UUID error verified without a model admission slot")

	requestID := "home-interop-" + fmt.Sprint(time.Now().UnixNano())
	result := homeclient.Result{RequestID: requestID, StatusCode: 200, Endpoint: "/v1/responses", InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	first.Finish(result)
	first.Finish(result)
	count(2)
	fourth := acquire(b)
	count(3)
	eventually(t, "persisted single usage report with fixture tokens", 10*time.Second, func() bool {
		var response struct {
			Items []struct {
				RequestID  string `json:"request_id"`
				Failed     bool   `json:"failed"`
				Credential struct {
					ID string `json:"credential_id"`
				} `json:"credential"`
				Tokens struct {
					Total int `json:"total_tokens"`
				} `json:"tokens"`
			} `json:"items"`
		}
		if admin.get(ctx, "/usage/records?request_id="+url.QueryEscape(requestID), &response) != nil || len(response.Items) != 1 {
			return false
		}
		item := response.Items[0]
		return item.RequestID == requestID && !item.Failed && item.Credential.ID == cfg.CredentialID && item.Tokens.Total == 15
	})
	second.Finish(homeclient.Result{StatusCode: 200})
	third.Finish(homeclient.Result{StatusCode: 200})
	fourth.Finish(homeclient.Result{StatusCode: 200})
	count(0)
	t.Log("Finish restored capacity, duplicate Finish was safe, and usage persisted once")

	// Home strips a recognized reasoning suffix from concurrency.model while
	// preserving the upstream model. Both requests consume the same model group.
	suffixed := acquireModel(a, cfg.Model+"(high)")
	count(1)
	suffixed.Finish(homeclient.Result{StatusCode: http.StatusOK})
	count(0)
	t.Log("reasoning-suffixed dispatch accepted and its canonical accounting tuple released")

	_, err = a.Acquire(ctx, homeclient.Request{CredentialID: "00000000-0000-4000-8000-000000000001", Model: cfg.Model})
	if !errors.As(err, &refused) || refused.Type == "credential_concurrency_exceeded" {
		t.Fatalf("unknown pin must be refused by Home instead of rotating to an allowed credential: %v", err)
	}
	count(0)

	active := acquire(b)
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case <-active.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the active lease")
	}
	active.Finish(homeclient.Result{StatusCode: 200})
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("graceful Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not drain the completed lease")
	}
	if b.Ready() {
		t.Fatal("closed membership remains ready")
	}
	count(0)
	fresh := start(cfg.Clients[1]) // Same certificate, new process membership; no takeover.
	next := acquire(fresh)
	count(1)
	next.Finish(homeclient.Result{StatusCode: 200})
	count(0) // A new membership must restart its cumulative release ledger at one.
	t.Log("Close cancellation/drain and fresh-membership release ledger verified")

	// Keep cooldown last: the fixture never clears Home's scheduler state or
	// waits for a real provider reset. Client A must observe client B's result.
	limited := acquire(fresh)
	failureID := "home-interop-limited-" + fmt.Sprint(time.Now().UnixNano())
	resetAt := time.Now().Add(time.Minute)
	limited.Finish(homeclient.Result{
		RequestID: failureID, StatusCode: http.StatusTooManyRequests, Failed: true,
		ErrorCode: "usage_limit_reached", ResetAt: resetAt,
	})
	count(0)
	eventually(t, "persisted single 429 usage report", 10*time.Second, func() bool {
		var response struct {
			Items []struct {
				RequestID  string `json:"request_id"`
				Failed     bool   `json:"failed"`
				StatusCode int    `json:"status_code"`
				Credential struct {
					ID string `json:"credential_id"`
				} `json:"credential"`
			} `json:"items"`
		}
		if admin.get(ctx, "/usage/records?request_id="+url.QueryEscape(failureID), &response) != nil || len(response.Items) != 1 {
			return false
		}
		item := response.Items[0]
		return item.RequestID == failureID && item.Failed && item.StatusCode == http.StatusTooManyRequests && item.Credential.ID == cfg.CredentialID
	})
	if !time.Now().Before(resetAt) {
		t.Fatal("fixture cooldown expired before admission verification")
	}
	unexpected, err := a.Acquire(ctx, homeclient.Request{CredentialID: cfg.CredentialID, Model: cfg.Model})
	if unexpected != nil {
		// Clean up an unexpected admission without reporting a second failure.
		unexpected.Finish(homeclient.Result{StatusCode: http.StatusOK})
	}
	if !errors.As(err, &refused) || refused.Type != "model_cooldown" || !refused.Retryable || refused.RetryAfter <= 0 {
		t.Fatalf("reported 429 must cause typed shared model cooldown: %v", err)
	}
	count(0)
	t.Log("failed 429 usage persisted, released its lease, and imposed shared model cooldown")
}
