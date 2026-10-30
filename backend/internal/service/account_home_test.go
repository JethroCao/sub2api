package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

const testHomeID = "eb1b3cb1-3de2-47a4-b833-2b8e6644ad6e"

func homeTestExtra() map[string]any {
	return map[string]any{"openai_credential_source": "home", "home_credential_id": testHomeID}
}
func TestHomeBindingStripsNativeCredentialsAndPreservesLocalIdentity(t *testing.T) {
	repo := &upstreamBillingProbeAdminRepo{&upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{7: {ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, GroupIDs: []int64{3}, Credentials: map[string]any{"access_token": "old", "refresh_token": "secret", "model_mapping": map[string]any{"a": "b"}}}}}}
	svc := &adminServiceImpl{accountRepo: repo}
	a, err := svc.UpdateAccount(context.Background(), 7, &UpdateAccountInput{Extra: homeTestExtra()})
	require.NoError(t, err)
	require.NotContains(t, a.Credentials, "refresh_token")
	require.NotContains(t, a.Credentials, "access_token")
	require.Equal(t, int64(7), a.ID)
	require.Equal(t, []int64{3}, a.GroupIDs)
	require.Contains(t, a.Credentials, "model_mapping")
	a, err = svc.UpdateAccount(context.Background(), 7, &UpdateAccountInput{Extra: map[string]any{}, Credentials: map[string]any{"refresh_token": "stale"}})
	require.NoError(t, err)
	require.Equal(t, "home", a.Extra["openai_credential_source"])
	require.NotContains(t, a.Credentials, "refresh_token")
}
func TestHomeBindingRejectsInvalidAndConflictingAccounts(t *testing.T) {
	for _, tc := range []struct {
		name, platform, typ string
		extra, creds        map[string]any
	}{
		{"invalid uuid", PlatformOpenAI, AccountTypeOAuth, map[string]any{"openai_credential_source": "home", "home_credential_id": "bad"}, nil},
		{"unknown source", PlatformOpenAI, AccountTypeOAuth, map[string]any{"openai_credential_source": "other", "home_credential_id": testHomeID}, nil},
		{"wrong provider", PlatformAnthropic, AccountTypeOAuth, homeTestExtra(), nil},
		{"wrong type", PlatformOpenAI, AccountTypeAPIKey, homeTestExtra(), nil},
		{"pat", PlatformOpenAI, AccountTypeOAuth, homeTestExtra(), map[string]any{"auth_mode": "personalAccessToken"}},
		{"legacy pat", PlatformOpenAI, AccountTypeOAuth, homeTestExtra(), map[string]any{"openai_auth_mode": "personal_access_token"}},
		{"agent", PlatformOpenAI, AccountTypeOAuth, homeTestExtra(), map[string]any{"agent_private_key": "secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &upstreamBillingProbeAccountRepo{}
			_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{Name: "test", Platform: tc.platform, Type: tc.typ, Extra: tc.extra, Credentials: tc.creds, SkipDefaultGroupBind: true})
			require.Error(t, err)
			require.Empty(t, repo.accounts)
		})
	}
}
func TestHomeBindingGenericMutationsRejected(t *testing.T) {
	svc := &adminServiceImpl{}
	require.Error(t, svc.UpdateAccountExtra(context.Background(), 1, homeTestExtra()))
	_, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{Extra: homeTestExtra()})
	require.Error(t, err)
}
func TestHomeBindingRefresherExcludedEvenWithRT(t *testing.T) {
	r := NewOpenAITokenRefresher(nil, nil)
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: homeTestExtra(), Credentials: map[string]any{"refresh_token": "old"}}
	require.False(t, r.CanRefresh(a))
}

func TestHomeBindingReauthRaceRejectsCurrentOwner(t *testing.T) {
	a := homeTestAccount()
	ctx := WithNativeCredentialMutation(context.Background())
	require.ErrorIs(t, ProtectHomeAccountWrite(ctx, homeTestExtra(), a), ErrHomeOwnershipChanged)
}
func TestHomeBindingShadowResolvesOwnerAndRejectsIndependentPair(t *testing.T) {
	parent := homeTestAccount()
	id := parent.ID
	shadow := &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &id, QuotaDimension: QuotaDimensionSpark}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{7: parent, 8: shadow}}
	owner, err := currentHomeCredentialOwner(context.Background(), repo, shadow)
	require.NoError(t, err)
	require.Equal(t, parent.ID, owner.ID)
	shadow.Extra = homeTestExtra()
	_, err = currentHomeCredentialOwner(context.Background(), repo, shadow)
	require.ErrorIs(t, err, ErrHomeBinding)
}
func TestHomeBindingHeaderOverridesCannotReplaceIdentity(t *testing.T) {
	a := homeTestAccount()
	a.Credentials = map[string]any{"access_token": "home-at", "chatgpt_account_id": "home-account", "header_override_enabled": true, "header_overrides": map[string]any{"authorization": "Bearer injected", "chatgpt-account-id": "injected"}}
	h := http.Header{}
	a.ApplyHeaderOverrides(h)
	require.Equal(t, "Bearer home-at", h.Get("Authorization"))
	require.Equal(t, "home-account", h.Get("ChatGPT-Account-ID"))
}
