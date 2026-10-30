package homeclient

import (
	"encoding/json"
	"time"
)

type releaseKey struct{ id, model string }
type releaseCount struct{ latest, acked int64 }
type releasePayload struct {
	CredentialID string `json:"credential_id"`
	Model        string `json:"model"`
	Seq          int64  `json:"release_seq"`
}

func (c *Client) flushReleases(g *generation) bool {
	// Serialise snapshots/ACKs so a delayed old ACK cannot regress progress.
	g.releaseMu.Lock()
	defer g.releaseMu.Unlock()
	c.mu.Lock()
	var batch []releasePayload
	for k, v := range g.ledger {
		if v.latest > v.acked {
			batch = append(batch, releasePayload{k.id, k.model, v.latest})
		}
	}
	failed := g.failed
	c.mu.Unlock()
	if failed {
		return false
	}
	ok := true
	for _, p := range batch {
		raw, _ := json.Marshal(p)
		f, _, e := c.exchange(g.ctx, g, commandTimeout, "LPUSH", "concurrency-release", string(raw))
		if e != nil || f.kind != ':' || f.number != 1 {
			ok = false
			continue
		}
		c.mu.Lock()
		if v := g.ledger[releaseKey{p.CredentialID, p.Model}]; v != nil && p.Seq > v.acked {
			v.acked = p.Seq
		}
		notify(g.changed)
		c.mu.Unlock()
	}
	return ok
}
func (c *Client) releaseLoop(g *generation) {
	delay := g.policy.Flush
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-g.wake:
		case <-t.C:
		}
		if c.flushReleases(g) {
			delay = g.policy.Flush
		} else {
			delay *= 2
			if delay > g.policy.MaxBackoff {
				delay = g.policy.MaxBackoff
			}
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(delay)
	}
}
