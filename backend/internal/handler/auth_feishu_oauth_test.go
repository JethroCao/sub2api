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

	entsql "entgo.io/ent/dialect/sql"
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
		service.SettingKeyFeishuConnectSyncEmail:               "true",
		service.SettingKeyFeishuConnectSyncDisplayName:         "true",
		service.SettingKeyFeishuConnectSyncDepartment:          "true",
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

func TestFeishuOAuthCallbackBindsExistingOrgMirrorAfterAutoCreate(t *testing.T) {
	originalFetch := fetchFeishuOAuthIdentity
	fetchFeishuOAuthIdentity = func(ctx context.Context, cfg feishuOAuthConfig, code string) (*feishuOAuthIdentity, error) {
		return &feishuOAuthIdentity{
			Token: feishuOAuthTokenResponse{Scope: "contact:user.base:readonly"},
			User: feishuUserInfo{
				Name:      "田晶晶",
				OpenID:    "ou_existing_org_user",
				UnionID:   "on_existing_org_user",
				UserID:    "feishu_existing_org_user",
				TenantKey: "tenant_test",
			},
		}, nil
	}
	t.Cleanup(func() { fetchFeishuOAuthIdentity = originalFetch })

	handler, client := newFeishuOAuthTestHandler(t, nil)
	t.Cleanup(func() { _ = client.Close() })
	createFeishuOrgMirrorTablesForTest(t, client)
	insertFeishuOrgMirrorUserForTest(t, client, "tenant_test", "ou_existing_org_user", "on_existing_org_user", "feishu_existing_org_user", "od-tech")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/feishu/callback?code=feishu-code&state=state-existing-org", nil)
	req.AddCookie(encodedCookie(feishuOAuthStateCookieName, "state-existing-org"))
	req.AddCookie(encodedCookie(feishuOAuthRedirectCookieName, "/dashboard"))
	req.AddCookie(encodedCookie(feishuOAuthIntentCookieName, oauthIntentLogin))
	req.AddCookie(encodedCookie(oauthPendingBrowserCookieName, "browser-existing-org"))
	c.Request = req

	handler.FeishuOAuthCallback(c)

	require.Equal(t, http.StatusFound, recorder.Code)
	fragment := parseOAuthRedirectFragment(t, recorder.Header().Get("Location"))
	require.NotEmpty(t, fragment.Get("access_token"))

	ctx := context.Background()
	createdUser, err := client.User.Query().
		Where(dbuser.EmailEQ("feishu-ou_existing_org_user@feishu-connect.invalid")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, createdUser.ID, loadFeishuOrgMirrorUserIDForTest(t, client, "tenant_test", "ou_existing_org_user"))
	require.Equal(t, createdUser.ID, loadFeishuOrgMirrorDepartmentUserIDForTest(t, client, "tenant_test", "ou_existing_org_user", "od-tech"))
}

func TestFeishuOAuthCallbackHonorsDisabledProfileAndDepartmentSync(t *testing.T) {
	originalFetch := fetchFeishuOAuthIdentity
	fetchFeishuOAuthIdentity = func(ctx context.Context, cfg feishuOAuthConfig, code string) (*feishuOAuthIdentity, error) {
		return &feishuOAuthIdentity{
			Token: feishuOAuthTokenResponse{Scope: "contact:user.base:readonly contact:user.email:readonly"},
			User: feishuUserInfo{
				Name:            "不应同步的姓名",
				OpenID:          "ou_sync_disabled",
				UnionID:         "on_sync_disabled",
				UserID:          "feishu_sync_disabled",
				TenantKey:       "tenant_test",
				EnterpriseEmail: "should-not-sync@example.com",
			},
		}, nil
	}
	t.Cleanup(func() { fetchFeishuOAuthIdentity = originalFetch })

	handler, client := newFeishuOAuthTestHandler(t, map[string]string{
		service.SettingKeyFeishuConnectSyncEmail:       "false",
		service.SettingKeyFeishuConnectSyncDisplayName: "false",
		service.SettingKeyFeishuConnectSyncDepartment:  "false",
	})
	t.Cleanup(func() { _ = client.Close() })
	createFeishuOrgMirrorTablesForTest(t, client)
	insertFeishuOrgMirrorUserForTest(t, client, "tenant_test", "ou_sync_disabled", "on_sync_disabled", "feishu_sync_disabled", "od-tech")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/feishu/callback?code=feishu-code&state=state-sync-disabled", nil)
	req.AddCookie(encodedCookie(feishuOAuthStateCookieName, "state-sync-disabled"))
	req.AddCookie(encodedCookie(feishuOAuthRedirectCookieName, "/dashboard"))
	req.AddCookie(encodedCookie(feishuOAuthIntentCookieName, oauthIntentLogin))
	req.AddCookie(encodedCookie(oauthPendingBrowserCookieName, "browser-sync-disabled"))
	c.Request = req

	handler.FeishuOAuthCallback(c)

	require.Equal(t, http.StatusFound, recorder.Code)
	fragment := parseOAuthRedirectFragment(t, recorder.Header().Get("Location"))
	require.NotEmpty(t, fragment.Get("access_token"))

	ctx := context.Background()
	createdUser, err := client.User.Query().
		Where(dbuser.EmailEQ("feishu-ou_sync_disabled@feishu-connect.invalid")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, "feishu_ou_sync_disabled", createdUser.Username)
	require.Zero(t, loadFeishuOrgMirrorUserIDForTest(t, client, "tenant_test", "ou_sync_disabled"))
	require.Zero(t, loadFeishuOrgMirrorDepartmentUserIDForTest(t, client, "tenant_test", "ou_sync_disabled", "od-tech"))
}

func createFeishuOrgMirrorTablesForTest(t *testing.T, client *dbent.Client) {
	t.Helper()

	execFeishuOAuthTestSQL(t, client, `
CREATE TABLE IF NOT EXISTS feishu_org_users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NULL,
	tenant_key TEXT NOT NULL DEFAULT '',
	open_id TEXT NOT NULL,
	union_id TEXT NOT NULL DEFAULT '',
	feishu_user_id TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	email TEXT NOT NULL DEFAULT '',
	employee_no TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'active',
	primary_open_department_id TEXT NOT NULL DEFAULT '',
	manager_open_id TEXT NOT NULL DEFAULT '',
	department_open_ids TEXT NOT NULL DEFAULT '[]',
	raw TEXT NOT NULL DEFAULT '{}',
	last_synced_at TIMESTAMP NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(tenant_key, open_id)
)`)
	execFeishuOAuthTestSQL(t, client, `
CREATE TABLE IF NOT EXISTS feishu_user_departments (
	tenant_key TEXT NOT NULL DEFAULT '',
	open_id TEXT NOT NULL,
	open_department_id TEXT NOT NULL,
	user_id INTEGER NULL,
	is_primary BOOLEAN NOT NULL DEFAULT false,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (tenant_key, open_id, open_department_id)
)`)
}

func insertFeishuOrgMirrorUserForTest(t *testing.T, client *dbent.Client, tenantKey string, openID string, unionID string, feishuUserID string, departmentID string) {
	t.Helper()

	execFeishuOAuthTestSQL(t, client, `
INSERT INTO feishu_org_users (
	user_id, tenant_key, open_id, union_id, feishu_user_id, name, status,
	primary_open_department_id, department_open_ids, raw, last_synced_at, updated_at
) VALUES (
	NULL, ?, ?, ?, ?, '田晶晶', 'active', ?, '["' || ? || '"]', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
)
ON CONFLICT(tenant_key, open_id) DO UPDATE SET
	user_id = NULL,
	union_id = excluded.union_id,
	feishu_user_id = excluded.feishu_user_id,
	primary_open_department_id = excluded.primary_open_department_id,
	department_open_ids = excluded.department_open_ids,
	updated_at = CURRENT_TIMESTAMP`, tenantKey, openID, unionID, feishuUserID, departmentID, departmentID)
	execFeishuOAuthTestSQL(t, client, `
INSERT INTO feishu_user_departments (
	tenant_key, open_id, open_department_id, user_id, is_primary, updated_at
) VALUES (?, ?, ?, NULL, true, CURRENT_TIMESTAMP)
ON CONFLICT(tenant_key, open_id, open_department_id) DO UPDATE SET
	user_id = NULL,
	is_primary = excluded.is_primary,
	updated_at = CURRENT_TIMESTAMP`, tenantKey, openID, departmentID)
}

func execFeishuOAuthTestSQL(t *testing.T, client *dbent.Client, query string, args ...any) {
	t.Helper()

	var result entsql.Result
	require.NoError(t, client.Driver().Exec(context.Background(), query, args, &result))
}

func loadFeishuOrgMirrorUserIDForTest(t *testing.T, client *dbent.Client, tenantKey string, openID string) int64 {
	t.Helper()

	return loadFeishuOrgMirrorIDForTest(t, client, `SELECT COALESCE(user_id, 0) FROM feishu_org_users WHERE tenant_key = ? AND open_id = ?`, tenantKey, openID)
}

func loadFeishuOrgMirrorDepartmentUserIDForTest(t *testing.T, client *dbent.Client, tenantKey string, openID string, departmentID string) int64 {
	t.Helper()

	return loadFeishuOrgMirrorIDForTest(t, client, `SELECT COALESCE(user_id, 0) FROM feishu_user_departments WHERE tenant_key = ? AND open_id = ? AND open_department_id = ?`, tenantKey, openID, departmentID)
}

func loadFeishuOrgMirrorIDForTest(t *testing.T, client *dbent.Client, query string, args ...any) int64 {
	t.Helper()

	var rows entsql.Rows
	require.NoError(t, client.Driver().Query(context.Background(), query, args, &rows))
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var id int64
	require.NoError(t, rows.Scan(&id))
	require.NoError(t, rows.Err())
	return id
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
