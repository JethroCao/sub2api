package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func NormalizeFeishuTenantRestrictionPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case FeishuTenantRestrictionNone:
		return FeishuTenantRestrictionNone
	default:
		return FeishuTenantRestrictionInternalOnly
	}
}

func normalizeFeishuDepartedUserAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case FeishuDepartedUserActionAutoDisable:
		return FeishuDepartedUserActionAutoDisable
	default:
		return FeishuDepartedUserActionAutoDisable
	}
}

func normalizeFeishuSystemSettings(settings *SystemSettings) {
	if settings == nil {
		return
	}
	settings.FeishuConnectAppID = strings.TrimSpace(settings.FeishuConnectAppID)
	settings.FeishuConnectAppSecret = strings.TrimSpace(settings.FeishuConnectAppSecret)
	settings.FeishuConnectRedirectURL = strings.TrimSpace(settings.FeishuConnectRedirectURL)
	settings.FeishuConnectTenantRestrictionPolicy = NormalizeFeishuTenantRestrictionPolicy(settings.FeishuConnectTenantRestrictionPolicy)
	settings.FeishuConnectAllowedTenantKey = strings.TrimSpace(settings.FeishuConnectAllowedTenantKey)
	settings.FeishuDepartedUserAction = normalizeFeishuDepartedUserAction(settings.FeishuDepartedUserAction)
	if settings.FeishuSyncDisableThresholdCount <= 0 {
		settings.FeishuSyncDisableThresholdCount = FeishuDefaultDisableThresholdCount
	}
	if settings.FeishuSyncDisableThresholdPercent <= 0 {
		settings.FeishuSyncDisableThresholdPercent = FeishuDefaultDisableThresholdPct
	}
	if settings.FeishuConnectTenantRestrictionPolicy != FeishuTenantRestrictionInternalOnly {
		settings.FeishuConnectBypassRegistration = false
		settings.FeishuOrgSyncEnabled = false
	}
}

func (s *SettingService) ValidateLoginEntryAvailability(ctx context.Context, settings *SystemSettings) error {
	_ = ctx
	if settings == nil {
		return nil
	}
	normalizeFeishuSystemSettings(settings)

	if settings.FeishuConnectEnabled && !feishuConnectConfigComplete(settings) {
		return infraerrors.BadRequest(
			"FEISHU_CONFIG_INCOMPLETE",
			"feishu login requires app id, app secret, redirect url, and tenant restriction policy",
		)
	}

	hasUserLoginEntry := settings.EmailPasswordLoginEnabled ||
		settings.LinuxDoConnectEnabled ||
		settings.DingTalkConnectEnabled ||
		settings.FeishuConnectEnabled ||
		settings.WeChatConnectEnabled ||
		settings.OIDCConnectEnabled ||
		settings.GitHubOAuthEnabled ||
		settings.GoogleOAuthEnabled
	if !hasUserLoginEntry {
		return infraerrors.BadRequest(
			"LOGIN_ENTRY_UNAVAILABLE",
			"at least one user login entry must remain available",
		)
	}
	if !settings.EmailPasswordLoginEnabled && !settings.AdminEmailLoginFallbackEnabled {
		return infraerrors.BadRequest(
			"ADMIN_LOGIN_FALLBACK_REQUIRED",
			"admin email fallback login must remain enabled when normal email login is disabled",
		)
	}
	return nil
}

func feishuConnectConfigComplete(settings *SystemSettings) bool {
	if settings == nil {
		return false
	}
	return strings.TrimSpace(settings.FeishuConnectAppID) != "" &&
		strings.TrimSpace(settings.FeishuConnectAppSecret) != "" &&
		strings.TrimSpace(settings.FeishuConnectRedirectURL) != "" &&
		strings.TrimSpace(settings.FeishuConnectTenantRestrictionPolicy) != ""
}

func defaultFeishuConnectConfig(base config.FeishuConnectConfig) config.FeishuConnectConfig {
	if strings.TrimSpace(base.AuthorizeURL) == "" {
		base.AuthorizeURL = "https://accounts.feishu.cn/open-apis/authen/v1/authorize"
	}
	if strings.TrimSpace(base.TokenURL) == "" {
		base.TokenURL = "https://accounts.feishu.cn/oauth/v3/token"
	}
	if strings.TrimSpace(base.UserInfoURL) == "" {
		base.UserInfoURL = "https://open.feishu.cn/open-apis/authen/v1/user_info"
	}
	if strings.TrimSpace(base.Scopes) == "" {
		base.Scopes = "contact:user.base:readonly contact:user.email:readonly contact:user.employee_id:readonly"
	}
	if strings.TrimSpace(base.FrontendRedirectURL) == "" {
		base.FrontendRedirectURL = "/auth/feishu/callback"
	}
	base.TenantRestrictionPolicy = NormalizeFeishuTenantRestrictionPolicy(base.TenantRestrictionPolicy)
	base.DepartedUserAction = normalizeFeishuDepartedUserAction(base.DepartedUserAction)
	if base.DisableThresholdCount <= 0 {
		base.DisableThresholdCount = FeishuDefaultDisableThresholdCount
	}
	if base.DisableThresholdPercent <= 0 {
		base.DisableThresholdPercent = FeishuDefaultDisableThresholdPct
	}
	return base
}

type FeishuPreflightCapability struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type FeishuPreflightWarning struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type FeishuPreflightResult struct {
	OK                bool                      `json:"ok"`
	Login             FeishuPreflightCapability `json:"login"`
	Email             FeishuPreflightCapability `json:"email"`
	OrgSync           FeishuPreflightCapability `json:"org_sync"`
	DepartedDetection FeishuPreflightCapability `json:"departed_detection"`
	ManagerRelation   FeishuPreflightCapability `json:"manager_relation"`
	Warnings          []FeishuPreflightWarning  `json:"warnings"`
}

func (s *SettingService) CheckFeishuPreflight(ctx context.Context) (*FeishuPreflightResult, error) {
	settings, err := s.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	authDefaults, err := s.GetAuthSourceDefaultSettings(ctx)
	if err != nil {
		return nil, err
	}

	result := &FeishuPreflightResult{
		OK: true,
		Login: FeishuPreflightCapability{
			Status:  "ok",
			Message: "feishu login configuration is complete",
		},
		Email: FeishuPreflightCapability{
			Status:  "disabled",
			Message: "email sync is disabled",
		},
		OrgSync: FeishuPreflightCapability{
			Status:  "disabled",
			Message: "feishu org sync is disabled",
		},
		DepartedDetection: FeishuPreflightCapability{
			Status:  "disabled",
			Message: "departed user detection requires org sync",
		},
		ManagerRelation: FeishuPreflightCapability{
			Status:  "disabled",
			Message: "manager relation sync requires org sync",
		},
	}
	if !settings.FeishuConnectEnabled {
		result.OK = false
		result.Login = FeishuPreflightCapability{
			Status:  "disabled",
			Reason:  "FEISHU_DISABLED",
			Message: "feishu login is disabled",
		}
		return result, nil
	}
	if !feishuConnectConfigComplete(settings) {
		result.OK = false
		result.Login = FeishuPreflightCapability{
			Status:  "error",
			Reason:  "FEISHU_CONFIG_INCOMPLETE",
			Message: "feishu app id, app secret, redirect url, or tenant restriction policy is missing",
		}
	}
	if settings.FeishuConnectSyncEmail {
		result.Email = FeishuPreflightCapability{
			Status:  "warning",
			Reason:  "FEISHU_EMAIL_SCOPE_UNVERIFIED",
			Message: "email sync is enabled; runtime verification still depends on Feishu scope and tenant email availability",
		}
	}
	if settings.FeishuOrgSyncEnabled {
		result.OrgSync = FeishuPreflightCapability{
			Status:  "warning",
			Reason:  "FEISHU_ORG_SCOPE_UNVERIFIED",
			Message: "org sync is enabled; runtime verification still depends on Feishu contact scopes",
		}
		if settings.FeishuDepartedUserAction == FeishuDepartedUserActionAutoDisable {
			result.DepartedDetection = FeishuPreflightCapability{
				Status:  "warning",
				Reason:  "FEISHU_DEPARTED_SCOPE_UNVERIFIED",
				Message: "departed users will auto-disable locally after org sync verifies access",
			}
		}
		result.ManagerRelation = FeishuPreflightCapability{
			Status:  "warning",
			Reason:  "FEISHU_MANAGER_SCOPE_UNVERIFIED",
			Message: "manager relation sync depends on Feishu employee data scope",
		}
	}
	if authDefaults == nil || !feishuDefaultGrantConfigured(authDefaults.Feishu) {
		result.Warnings = append(result.Warnings, FeishuPreflightWarning{
			Reason:  "FEISHU_DEFAULT_GRANT_UNCONFIGURED",
			Message: "feishu source default grant is not configured; auto-created Feishu users will fall back to system defaults",
		})
	}
	return result, nil
}

func feishuDefaultGrantConfigured(settings ProviderDefaultGrantSettings) bool {
	return settings.Balance > 0 ||
		len(settings.Subscriptions) > 0 ||
		settings.GrantOnSignup ||
		settings.GrantOnFirstBind ||
		len(settings.PlatformQuotas) > 0
}
