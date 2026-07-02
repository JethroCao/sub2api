package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newFeishuSettingsHandler(repo *settingHandlerRepoStub) *SettingHandler {
	svc := service.NewSettingService(repo, &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}})
	return NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
}

func TestAdminSettingsExposeFeishuConnectFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &settingHandlerRepoStub{values: map[string]string{
		service.SettingKeyEmailPasswordLoginEnabled:               "false",
		service.SettingKeyAdminEmailLoginFallbackEnabled:          "true",
		service.SettingKeyFeishuConnectEnabled:                    "true",
		service.SettingKeyFeishuConnectAppID:                      "cli_test",
		service.SettingKeyFeishuConnectAppSecret:                  "secret",
		service.SettingKeyFeishuConnectRedirectURL:                "https://example.com/oauth/feishu/callback",
		service.SettingKeyFeishuConnectTenantRestrictionPolicy:    "internal_only",
		service.SettingKeyFeishuConnectAllowedTenantKey:           "tenant_test",
		service.SettingKeyFeishuConnectBypassRegistration:         "true",
		service.SettingKeyFeishuConnectSyncEmail:                  "true",
		service.SettingKeyFeishuConnectSyncDisplayName:            "true",
		service.SettingKeyFeishuConnectSyncDepartment:             "true",
		service.SettingKeyFeishuOrgSyncEnabled:                    "true",
		service.SettingKeyFeishuDepartedUserAction:                "auto_disable",
		service.SettingKeyFeishuSyncDisableThresholdCount:         "10",
		service.SettingKeyFeishuSyncDisableThresholdPercent:       "20",
		service.SettingKeyAuthSourceDefaultFeishuBalance:          "3.5",
		service.SettingKeyAuthSourceDefaultFeishuConcurrency:      "7",
		service.SettingKeyAuthSourceDefaultFeishuGrantOnSignup:    "true",
		service.SettingKeyAuthSourceDefaultFeishuGrantOnFirstBind: "false",
	}}
	handler := newFeishuSettingsHandler(repo)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)

	handler.GetSettings(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, false, got.Data["email_password_login_enabled"])
	require.Equal(t, true, got.Data["admin_email_login_fallback_enabled"])
	require.Equal(t, true, got.Data["feishu_connect_enabled"])
	require.Equal(t, "cli_test", got.Data["feishu_connect_app_id"])
	require.Equal(t, true, got.Data["feishu_connect_app_secret_configured"])
	require.NotContains(t, got.Data, "feishu_connect_app_secret")
	require.Equal(t, "internal_only", got.Data["feishu_connect_tenant_restriction_policy"])
	require.Equal(t, "tenant_test", got.Data["feishu_connect_allowed_tenant_key"])
	require.Equal(t, true, got.Data["feishu_connect_bypass_registration"])
	require.Equal(t, true, got.Data["feishu_org_sync_enabled"])
	require.Equal(t, "auto_disable", got.Data["feishu_departed_user_action"])
	require.Equal(t, float64(10), got.Data["feishu_sync_disable_threshold_count"])
	require.Equal(t, float64(20), got.Data["feishu_sync_disable_threshold_percent"])
	require.Equal(t, 3.5, got.Data["auth_source_default_feishu_balance"])
	require.Equal(t, float64(7), got.Data["auth_source_default_feishu_concurrency"])
	require.Equal(t, true, got.Data["auth_source_default_feishu_grant_on_signup"])
}

func TestFeishuPreflightReportsMissingCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &settingHandlerRepoStub{values: map[string]string{
		service.SettingKeyEmailPasswordLoginEnabled:            "false",
		service.SettingKeyAdminEmailLoginFallbackEnabled:       "true",
		service.SettingKeyFeishuConnectEnabled:                 "true",
		service.SettingKeyFeishuConnectTenantRestrictionPolicy: "internal_only",
	}}
	handler := newFeishuSettingsHandler(repo)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/settings/feishu/preflight", bytes.NewReader(nil))

	handler.CheckFeishuPreflight(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Data struct {
			OK    bool `json:"ok"`
			Login struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"login"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.False(t, got.Data.OK)
	require.Equal(t, "error", got.Data.Login.Status)
	require.Equal(t, "FEISHU_CONFIG_INCOMPLETE", got.Data.Login.Reason)
}

func TestFeishuPreflightReportsDefaultGrantWarning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &settingHandlerRepoStub{values: map[string]string{
		service.SettingKeyEmailPasswordLoginEnabled:            "false",
		service.SettingKeyAdminEmailLoginFallbackEnabled:       "true",
		service.SettingKeyFeishuConnectEnabled:                 "true",
		service.SettingKeyFeishuConnectAppID:                   "cli_test",
		service.SettingKeyFeishuConnectAppSecret:               "secret",
		service.SettingKeyFeishuConnectRedirectURL:             "https://example.com/oauth/feishu/callback",
		service.SettingKeyFeishuConnectTenantRestrictionPolicy: "internal_only",
	}}
	handler := newFeishuSettingsHandler(repo)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/settings/feishu/preflight", bytes.NewReader(nil))

	handler.CheckFeishuPreflight(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Data struct {
			OK    bool `json:"ok"`
			Login struct {
				Status string `json:"status"`
			} `json:"login"`
			Warnings []struct {
				Reason string `json:"reason"`
			} `json:"warnings"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.True(t, got.Data.OK)
	require.Equal(t, "ok", got.Data.Login.Status)
	require.Contains(t, preflightWarningReasons(got.Data.Warnings), "FEISHU_DEFAULT_GRANT_UNCONFIGURED")
}

func preflightWarningReasons(items []struct {
	Reason string `json:"reason"`
}) []string {
	reasons := make([]string, 0, len(items))
	for _, item := range items {
		reasons = append(reasons, item.Reason)
	}
	return reasons
}
