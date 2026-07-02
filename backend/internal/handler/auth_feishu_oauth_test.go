package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/ent/pendingauthsession"
	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newFeishuOAuthTestHandler(t *testing.T, extraSettings map[string]string) (*AuthHandler, *dbent.Client) {
	t.Helper()

	values := map[string]string{
		service.SettingKeyFeishuConnectEnabled:                 "true",
		service.SettingKeyFeishuConnectAppID:                   "cli_test",
		service.SettingKeyFeishuConnectAppSecret:               "secret_test",
		service.SettingKeyFeishuConnectRedirectURL:             "https://api.example.com/api/v1/auth/oauth/feishu/callback",
		service.SettingKeyFeishuConnectTenantRestrictionPolicy: "internal_only",
		service.SettingKeyFeishuConnectAllowedTenantKey:        "tenant_test",
	}
	for key, value := range extraSettings {
		values[key] = value
	}
	return newOAuthPendingFlowTestHandlerWithDependencies(t, oauthPendingFlowTestHandlerOptions{
		settingValues: values,
	})
}

func TestFeishuOAuthStartRedirectsWithClientIDAndScope(t *testing.T) {
	handler, client := newFeishuOAuthTestHandler(t, nil)
	t.Cleanup(func() { _ = client.Close() })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/feishu/start?redirect=/dashboard", nil)

	handler.FeishuOAuthStart(c)

	require.Equal(t, http.StatusFound, recorder.Code)
	location := recorder.Header().Get("Location")
	parsed, err := url.Parse(location)
	require.NoError(t, err)
	require.Equal(t, "accounts.feishu.cn", parsed.Host)
	require.Equal(t, "cli_test", parsed.Query().Get("client_id"))
	require.Equal(t, "code", parsed.Query().Get("response_type"))
	require.Equal(t, "https://api.example.com/api/v1/auth/oauth/feishu/callback", parsed.Query().Get("redirect_uri"))
	require.Contains(t, parsed.Query().Get("scope"), "contact:user.base:readonly")
	require.NotEmpty(t, parsed.Query().Get("state"))

	require.NotNil(t, findCookie(recorder.Result().Cookies(), feishuOAuthStateCookieName))
	require.NotNil(t, findCookie(recorder.Result().Cookies(), feishuOAuthRedirectCookieName))
	require.NotNil(t, findCookie(recorder.Result().Cookies(), feishuOAuthIntentCookieName))
	require.NotNil(t, findCookie(recorder.Result().Cookies(), oauthPendingBrowserCookieName))
}

func TestFeishuOAuthCallbackUsesExistingOpenIDIdentityWithoutEmail(t *testing.T) {
	originalFetch := fetchFeishuOAuthIdentity
	fetchFeishuOAuthIdentity = func(ctx context.Context, cfg feishuOAuthConfig, code string) (*feishuOAuthIdentity, error) {
		return &feishuOAuthIdentity{
			Token: feishuOAuthTokenResponse{
				Scope: "contact:user.base:readonly contact:user.email:readonly",
			},
			User: feishuUserInfo{
				Name:      "曹辰旭",
				OpenID:    "ou_open_id",
				UnionID:   "on_union_id",
				UserID:    "feishu_user_id",
				TenantKey: "tenant_test",
			},
		}, nil
	}
	t.Cleanup(func() { fetchFeishuOAuthIdentity = originalFetch })

	handler, client := newFeishuOAuthTestHandler(t, nil)
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	existingUser, err := client.User.Create().
		SetEmail("cao@example.com").
		SetUsername("曹辰旭").
		SetPasswordHash("hash").
		SetRole(service.RoleUser).
		SetStatus(service.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	_, err = client.AuthIdentity.Create().
		SetUserID(existingUser.ID).
		SetProviderType("feishu").
		SetProviderKey("tenant_test").
		SetProviderSubject("ou_open_id").
		SetMetadata(map[string]any{"open_id": "ou_open_id"}).
		Save(ctx)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/feishu/callback?code=feishu-code&state=state-123", nil)
	req.AddCookie(encodedCookie(feishuOAuthStateCookieName, "state-123"))
	req.AddCookie(encodedCookie(feishuOAuthRedirectCookieName, "/dashboard"))
	req.AddCookie(encodedCookie(feishuOAuthIntentCookieName, oauthIntentLogin))
	req.AddCookie(encodedCookie(oauthPendingBrowserCookieName, "browser-123"))
	c.Request = req

	handler.FeishuOAuthCallback(c)

	require.Equal(t, http.StatusFound, recorder.Code)
	require.Equal(t, "/auth/feishu/callback", recorder.Header().Get("Location"))

	sessionCookie := findCookie(recorder.Result().Cookies(), oauthPendingSessionCookieName)
	require.NotNil(t, sessionCookie)
	session, err := client.PendingAuthSession.Query().
		Where(pendingauthsession.SessionTokenEQ(decodeCookieValueForTest(t, sessionCookie.Value))).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, oauthIntentLogin, session.Intent)
	require.Equal(t, "feishu", session.ProviderType)
	require.Equal(t, "tenant_test", session.ProviderKey)
	require.Equal(t, "ou_open_id", session.ProviderSubject)
	require.NotNil(t, session.TargetUserID)
	require.Equal(t, existingUser.ID, *session.TargetUserID)
	require.Equal(t, existingUser.Email, session.ResolvedEmail)
	require.Equal(t, "ou_open_id", session.UpstreamIdentityClaims["open_id"])
	require.Equal(t, "ou_open_id", session.UpstreamIdentityClaims["subject"])
	require.Equal(t, "feishu_user_id", session.UpstreamIdentityClaims["user_id"])

	completion, ok := session.LocalFlowState[oauthCompletionResponseKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "/dashboard", completion["redirect"])
	require.Nil(t, completion["access_token"])
	require.Nil(t, completion["refresh_token"])
}

func TestFeishuOAuthCallbackAutoCreatesSyntheticUserWithFeishuSource(t *testing.T) {
	originalFetch := fetchFeishuOAuthIdentity
	fetchFeishuOAuthIdentity = func(ctx context.Context, cfg feishuOAuthConfig, code string) (*feishuOAuthIdentity, error) {
		return &feishuOAuthIdentity{
			Token: feishuOAuthTokenResponse{Scope: "contact:user.base:readonly"},
			User: feishuUserInfo{
				Name:      "新员工",
				OpenID:    "ou_new_open",
				UnionID:   "on_new_union",
				UserID:    "feishu_new_user",
				TenantKey: "tenant_test",
			},
		}, nil
	}
	t.Cleanup(func() { fetchFeishuOAuthIdentity = originalFetch })

	handler, client := newFeishuOAuthTestHandler(t, nil)
	t.Cleanup(func() { _ = client.Close() })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/feishu/callback?code=feishu-code&state=state-new", nil)
	req.AddCookie(encodedCookie(feishuOAuthStateCookieName, "state-new"))
	req.AddCookie(encodedCookie(feishuOAuthRedirectCookieName, "/dashboard"))
	req.AddCookie(encodedCookie(feishuOAuthIntentCookieName, oauthIntentLogin))
	req.AddCookie(encodedCookie(oauthPendingBrowserCookieName, "browser-new"))
	c.Request = req

	handler.FeishuOAuthCallback(c)

	require.Equal(t, http.StatusFound, recorder.Code)
	fragment := parseOAuthRedirectFragment(t, recorder.Header().Get("Location"))
	require.NotEmpty(t, fragment.Get("access_token"))
	require.NotEmpty(t, fragment.Get("refresh_token"))
	require.Equal(t, "/dashboard", fragment.Get("redirect"))

	ctx := context.Background()
	createdUser, err := client.User.Query().
		Where(dbuser.EmailEQ("feishu-ou_new_open@feishu-connect.invalid")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, "feishu", createdUser.SignupSource)

	identity, err := client.AuthIdentity.Query().
		Where(
			authidentity.ProviderTypeEQ("feishu"),
			authidentity.ProviderKeyEQ("tenant_test"),
			authidentity.ProviderSubjectEQ("ou_new_open"),
		).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, createdUser.ID, identity.UserID)
	require.Equal(t, "ou_new_open", identity.Metadata["open_id"])
}

func TestFeishuOAuthCallbackRejectsUnboundExistingEmail(t *testing.T) {
	originalFetch := fetchFeishuOAuthIdentity
	fetchFeishuOAuthIdentity = func(ctx context.Context, cfg feishuOAuthConfig, code string) (*feishuOAuthIdentity, error) {
		return &feishuOAuthIdentity{
			Token: feishuOAuthTokenResponse{Scope: "contact:user.base:readonly contact:user.email:readonly"},
			User: feishuUserInfo{
				Name:            "已有员工",
				OpenID:          "ou_unbound_open",
				UnionID:         "on_unbound_union",
				UserID:          "feishu_unbound_user",
				TenantKey:       "tenant_test",
				EnterpriseEmail: "existing@example.com",
			},
		}, nil
	}
	t.Cleanup(func() { fetchFeishuOAuthIdentity = originalFetch })

	handler, client := newFeishuOAuthTestHandler(t, nil)
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	_, err := client.User.Create().
		SetEmail("existing@example.com").
		SetUsername("existing").
		SetPasswordHash("hash").
		SetRole(service.RoleUser).
		SetStatus(service.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/feishu/callback?code=feishu-code&state=state-existing", nil)
	req.AddCookie(encodedCookie(feishuOAuthStateCookieName, "state-existing"))
	req.AddCookie(encodedCookie(feishuOAuthRedirectCookieName, "/dashboard"))
	req.AddCookie(encodedCookie(feishuOAuthIntentCookieName, oauthIntentLogin))
	req.AddCookie(encodedCookie(oauthPendingBrowserCookieName, "browser-existing"))
	c.Request = req

	handler.FeishuOAuthCallback(c)

	require.Equal(t, http.StatusFound, recorder.Code)
	fragment := parseOAuthRedirectFragment(t, recorder.Header().Get("Location"))
	require.Equal(t, "identity_unbound", fragment.Get("error"))
	require.Contains(t, fragment.Get("error_message"), "feishu identity is not bound")
	require.Empty(t, fragment.Get("access_token"))
	require.Nil(t, findCookie(recorder.Result().Cookies(), oauthPendingSessionCookieName))

	count, err := client.AuthIdentity.Query().
		Where(
			authidentity.ProviderTypeEQ("feishu"),
			authidentity.ProviderKeyEQ("tenant_test"),
			authidentity.ProviderSubjectEQ("ou_unbound_open"),
		).
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, count)
}
