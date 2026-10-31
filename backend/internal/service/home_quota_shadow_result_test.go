package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/homeclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type homeQuotaResultRepo struct {
	*homeProbeRepo
	writes    chan context.Context
	proceed   chan struct{}
	completed chan struct{}
	accepted  atomic.Int32
}

func (r *homeQuotaResultRepo) UpdateExtra(ctx context.Context, id int64, _ map[string]any) error {
	r.writes <- ctx
	<-r.proceed
	defer close(r.completed)
	if expected, guarded := HomeResultCredentialID(ctx); guarded {
		selectedID, _ := HomeResultAccountID(ctx)
		ownerID, _ := HomeResultOwnerID(ctx)
		selected, err := r.GetByID(ctx, selectedID)
		if err != nil || selected.ParentAccountID == nil || *selected.ParentAccountID != ownerID || id != selectedID {
			return ErrHomeOwnershipChanged
		}
		owner, err := r.GetByID(ctx, ownerID)
		if err != nil {
			return err
		}
		current, err := owner.HomeCredentialID()
		if err != nil || current != expected {
			return ErrHomeOwnershipChanged
		}
	}
	r.accepted.Add(1)
	return nil
}

func TestHomeShadowQuotaResultRetainsBindingAndNotifiesAfterPersistence(t *testing.T) {
	for _, scenario := range []string{"stable", "unbind", "rebind", "reparent", "native"} {
		t.Run(scenario, func(t *testing.T) {
			owner, selected, base, change := homeProbeAccounts(true, scenario)
			repo := &homeQuotaResultRepo{homeProbeRepo: base, writes: make(chan context.Context, 1), proceed: make(chan struct{}), completed: make(chan struct{})}
			released := false
			defer func() {
				if !released {
					close(repo.proceed)
				}
			}()
			client := &homeMetadataClient{}
			client.refresh = func(id, observed string) (homeclient.Credential, error) {
				return homeclient.Credential{ID: id, AccessToken: "old-home-at", AccountID: "old-home-account", ExpiresAt: time.Now().Add(time.Hour)}, nil
			}
			home := NewHomeCredentialsService(repo, client)
			notifier := &OpenAIQuotaAutoResetService{ctx: t.Context(), queue: make(chan int64, 4)}
			setOpenAIAutoResetNotifier(notifier)
			defer clearOpenAIAutoResetNotifier(notifier)
			var sends int
			quota := NewOpenAIQuotaService(repo, nil, &OpenAITokenProvider{accountRepo: repo, homeCredentials: home}, homeAuxiliaryHTTPFactory(func(r *http.Request) (int, string) {
				sends++
				if scenario == "native" {
					require.Equal(t, "Bearer native-token", r.Header.Get("Authorization"))
				} else {
					require.Equal(t, "Bearer old-home-at", r.Header.Get("Authorization"))
				}
				if sends == 1 {
					change()
					if scenario == "reparent" {
						nextOwner := *owner
						nextOwner.ID = 9
						repo.replace(&nextOwner)
						nextShadow := *selected
						nextShadow.ParentAccountID = &nextOwner.ID
						repo.replace(&nextShadow)
					}
					return http.StatusOK, `{"additional_rate_limits":[{"metered_feature":"codex_bengalfox","rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_after_seconds":3600}}}]}`
				}
				return http.StatusOK, `{}`
			}), nil)
			svc := &AccountUsageService{accountRepo: repo, homeCredentials: home, openAIQuotaService: quota}
			_, err := svc.getOpenAIUsage(t.Context(), selected, true)
			require.NoError(t, err)
			select {
			case writeCtx := <-repo.writes:
				id, hasID := HomeResultCredentialID(writeCtx)
				if scenario == "native" {
					assert.False(t, hasID)
				} else {
					assert.True(t, hasID, "the actual shadow quota response must retain its original binding")
					assert.Equal(t, transportHomeTestID, id)
					selectedID, hasSelected := HomeResultAccountID(writeCtx)
					ownerID, hasOwner := HomeResultOwnerID(writeCtx)
					assert.True(t, hasSelected)
					assert.True(t, hasOwner)
					assert.Equal(t, selected.ID, selectedID)
					assert.Equal(t, owner.ID, ownerID)
				}
			case <-time.After(time.Second):
				t.Fatal("quota snapshot write was not attempted")
			}
			assert.Empty(t, notifier.queue, "reset wakeup must wait for successful expected-binding persistence")
			close(repo.proceed)
			released = true
			<-repo.completed
			if scenario == "stable" || scenario == "native" {
				require.EqualValues(t, 1, repo.accepted.Load())
				select {
				case id := <-notifier.queue:
					require.Equal(t, owner.ID, id, "wakeup uses the owner captured by the actual quota read")
				case <-time.After(time.Second):
					t.Fatal("successful quota snapshot did not notify its owner")
				}
			} else {
				assert.Zero(t, repo.accepted.Load(), "a replaced binding must reject the old quota result")
				select {
				case id := <-notifier.queue:
					t.Errorf("rejected snapshot notified account %d", id)
				case <-time.After(20 * time.Millisecond):
				}
			}
			require.Equal(t, 2, sends, "one quota read and its existing credit-details read; no replay")
			require.Zero(t, client.acquires.Load(), "quota metadata reads must not acquire inference slots")
		})
	}
}
