package admin

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type deepHomeManualRepo struct {
	service.AccountRepository
	current *service.Account
}

func (r *deepHomeManualRepo) GetByID(context.Context, int64) (*service.Account, error) {
	return r.current, nil
}

type deepHomeManualOAuth struct{ calls int }

func (c *deepHomeManualOAuth) ExchangeCode(context.Context, string, string, string, string, string) (*openai.TokenResponse, error) {
	panic("unexpected exchange")
}
func (c *deepHomeManualOAuth) RefreshToken(ctx context.Context, rt, proxy string) (*openai.TokenResponse, error) {
	return c.RefreshTokenWithClientID(ctx, rt, proxy, "")
}
func (c *deepHomeManualOAuth) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	c.calls++
	return &openai.TokenResponse{AccessToken: "rotated-native-at", RefreshToken: "rotated-native-rt", ExpiresIn: 3600}, nil
}

type deepHomeManualAdmin struct {
	service.AdminService
	updates int
}

func (s *deepHomeManualAdmin) UpdateAccount(context.Context, int64, *service.UpdateAccountInput) (*service.Account, error) {
	s.updates++
	panic("unexpected for observed bug")
}
func TestHomeManualRefreshRejectsUnboundSnapshotBeforeNativeRotation(t *testing.T) {
	stale := &service.Account{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{service.OpenAICredentialSourceKey: "home", service.HomeCredentialIDKey: "eb1b3cb1-3de2-47a4-b833-2b8e6644ad6e"}}
	native := &service.Account{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"access_token": "native-at", "refresh_token": "native-rt"}}
	repo := &deepHomeManualRepo{current: native}
	client := &deepHomeManualOAuth{}
	admin := &deepHomeManualAdmin{}
	oauth := service.ProvideOpenAIOAuthService(nil, repo, nil, client, nil)
	defer oauth.Stop()
	handler := &AccountHandler{adminService: admin, openaiOAuthService: oauth}
	_, _, err := handler.refreshSingleAccount(context.Background(), stale)
	require.ErrorIs(t, err, service.ErrHomeOwnershipChanged)
	require.Zero(t, client.calls, "stale Home refresh must not consume a native RT that its caller will discard")
	require.Zero(t, admin.updates)
}
