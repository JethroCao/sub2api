package homeclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCanonicalAccountingModelPreservesAdmissionAndRelease(t *testing.T) {
	// Home v1.1.7 normalizes only the accounting key; the upstream model
	// retains its requested suffix/case. Literal expectations follow that wire contract.
	for _, tc := range []struct{ model, key string }{
		{testModel + "(high)", testModel},
		{strings.ToUpper(testModel), testModel},
		{testModel + "(none)", testModel},
		{testModel + "(auto)", testModel},
		{testModel + "(minimal)", testModel},
		{testModel + "(low)", testModel},
		{testModel + "(medium)", testModel},
		{testModel + "(xhigh)", testModel},
		{testModel + "(max)", testModel},
		{testModel + "(-1)", testModel},
		{testModel + "(0000000001)", testModel},
		{testModel + "(2147483647)", testModel},
		{testModel + "(2147483648)", testModel + "(2147483648)"},
		{testModel + "(custom)", testModel + "(custom)"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			h := newFake(t)
			h.mutate = func(reply map[string]any) {
				reply["model"] = tc.model
				reply["concurrency"].(map[string]any)["model"] = tc.key
			}
			c := startClient(t, h)
			lease, err := c.Acquire(context.Background(), Request{CredentialID: testID, Model: tc.model})
			if err != nil {
				t.Fatalf("valid canonical accounting tuple rejected: %v", err)
			}
			if lease.Context().Err() != nil || !c.Ready() {
				t.Fatal("valid canonical model fenced admission")
			}
			lease.Finish(Result{})
			eventually(t, func() bool { return h.count("LPUSH", "concurrency-release") > 0 })
			h.mu.Lock()
			defer h.mu.Unlock()
			for _, r := range h.records {
				if r.args[0] == "LPUSH" && r.args[1] == "concurrency-release" {
					var p releasePayload
					if json.Unmarshal([]byte(r.args[2]), &p) != nil || p.Model != tc.key || p.Seq != 1 {
						t.Fatalf("release did not preserve returned canonical tuple: %+v", p)
					}
				}
			}
		})
	}
}

func TestShutdownDrainsQueuedUsage(t *testing.T) {
	for _, viaParentCancel := range []bool{false, true} {
		name := "Close"
		if viaParentCancel {
			name = "RunContext"
		}
		t.Run(name, func(t *testing.T) {
			h := newFake(t)
			block := make(chan struct{})
			h.usageBlock = block
			defer func() {
				select {
				case <-block:
				default:
					close(block)
				}
			}()
			c, err := New(h.configForClient())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runDone := make(chan error, 1)
			go func() { runDone <- c.Run(ctx) }()
			t.Cleanup(func() { _ = c.Close() })
			eventually(t, c.Ready)
			first := acquire(t, c)
			first.Finish(Result{RequestID: "first"})
			eventually(t, func() bool { return h.count("LPUSH", "usage") == 1 })
			second := acquire(t, c)
			second.Finish(Result{RequestID: "last-failure", Failed: true, StatusCode: 429, ErrorCode: "usage_limit_reached", RetryAfter: time.Minute})
			c.mu.Lock()
			g := c.current
			c.mu.Unlock()
			done := make(chan error, 1)
			go func() {
				if viaParentCancel {
					cancel()
					<-runDone
				}
				done <- c.Close()
			}()
			<-g.work.Done()
			select {
			case err := <-done:
				t.Fatalf("shutdown returned before queued failure could be reported: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			close(block)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("shutdown did not finish after usage ACKs")
			}
			if got := h.count("LPUSH", "usage"); got != 2 {
				t.Fatalf("shutdown lost or repeated completed usage: %d", got)
			}
		})
	}
}

func TestShutdownUsageDrainRespectsCancelBoundWithoutReplay(t *testing.T) {
	h := newFake(t)
	block := make(chan struct{})
	h.usageBlock = block
	defer close(block)
	c := startClient(t, h)
	lease := acquire(t, c)
	lease.Finish(Result{Failed: true, StatusCode: 429})
	eventually(t, func() bool { return h.count("LPUSH", "usage") == 1 })
	started := time.Now()
	if err := c.Close(); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("unacknowledged usage must exhaust the bounded drain: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("shutdown exceeded its 300 ms cancel bound: %v", elapsed)
	}
	if got := h.count("LPUSH", "usage"); got != 1 {
		t.Fatalf("ambiguous usage replayed: %d", got)
	}
}
