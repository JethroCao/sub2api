package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/oauth"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type feishuOAuthConfig = config.FeishuConnectConfig

const (
	feishuOAuthCookiePath         = "/api/v1/auth/oauth/feishu"
	feishuOAuthStateCookieName    = "feishu_oauth_state"
	feishuOAuthRedirectCookieName = "feishu_oauth_redirect"
	feishuOAuthIntentCookieName   = "feishu_oauth_intent"
	feishuOAuthBindUserCookieName = "feishu_oauth_bind_user"
	feishuOAuthCookieMaxAgeSec    = 10 * 60
	feishuOAuthDefaultRedirectTo  = "/dashboard"
	feishuOAuthDefaultFrontendCB  = "/auth/feishu/callback"
	feishuOAuthProviderKey        = "feishu"
)

type feishuOAuthTokenResponse struct {
	Code         int    `json:"code"`
	Msg          string `json:"msg"`
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type feishuUserInfoResponse struct {
	Code int            `json:"code"`
	Msg  string         `json:"msg"`
	Data feishuUserInfo `json:"data"`
}

type feishuUserInfo struct {
	AvatarBig       string `json:"avatar_big"`
	AvatarMiddle    string `json:"avatar_middle"`
	AvatarThumb     string `json:"avatar_thumb"`
	AvatarURL       string `json:"avatar_url"`
	Email           string `json:"email"`
	EnterpriseEmail string `json:"enterprise_email"`
	EnName          string `json:"en_name"`
	Name            string `json:"name"`
	OpenID          string `json:"open_id"`
	TenantKey       string `json:"tenant_key"`
	UnionID         string `json:"union_id"`
	UserID          string `json:"user_id"`
	EmployeeNo      string `json:"employee_no"`
}

type feishuOAuthIdentity struct {
	Token feishuOAuthTokenResponse
	User  feishuUserInfo
}

var fetchFeishuOAuthIdentity = defaultFetchFeishuOAuthIdentity

func (h *AuthHandler) getFeishuOAuthConfig(ctx context.Context) (config.FeishuConnectConfig, error) {
	if h != nil && h.settingSvc != nil {
		return h.settingSvc.GetFeishuConnectOAuthConfig(ctx)
	}
	if h == nil || h.cfg == nil {
		return config.FeishuConnectConfig{}, infraerrors.ServiceUnavailable("CONFIG_NOT_READY", "config not loaded")
	}
	if !h.cfg.Feishu.Enabled {
		return config.FeishuConnectConfig{}, infraerrors.NotFound("OAUTH_DISABLED", "feishu oauth login is disabled")
	}
	return h.cfg.Feishu, nil
}

func feishuSetCookie(c *gin.Context, name string, value string, maxAgeSec int, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     feishuOAuthCookiePath,
		MaxAge:   maxAgeSec,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func feishuClearCookie(c *gin.Context, name string, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     feishuOAuthCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// FeishuOAuthStart starts the Feishu OAuth login flow.
// GET /api/v1/auth/oauth/feishu/start?redirect=/dashboard&intent=login
func (h *AuthHandler) FeishuOAuthStart(c *gin.Context) {
	cfg, err := h.getFeishuOAuthConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	state, err := oauth.GenerateState()
	if err != nil {
		response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_STATE_GEN_FAILED", "failed to generate oauth state").WithCause(err))
		return
	}
	redirectTo := sanitizeFrontendRedirectPath(c.Query("redirect"))
	if redirectTo == "" {
		redirectTo = feishuOAuthDefaultRedirectTo
	}
	browserSessionKey, err := generateOAuthPendingBrowserSession()
	if err != nil {
		response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_BROWSER_SESSION_GEN_FAILED", "failed to generate oauth browser session").WithCause(err))
		return
	}

	secureCookie := isRequestHTTPS(c)
	feishuSetCookie(c, feishuOAuthStateCookieName, encodeCookieValue(state), feishuOAuthCookieMaxAgeSec, secureCookie)
	feishuSetCookie(c, feishuOAuthRedirectCookieName, encodeCookieValue(redirectTo), feishuOAuthCookieMaxAgeSec, secureCookie)
	intent := normalizeOAuthIntent(c.Query("intent"))
	feishuSetCookie(c, feishuOAuthIntentCookieName, encodeCookieValue(intent), feishuOAuthCookieMaxAgeSec, secureCookie)
	captureOAuthPromoCode(c, secureCookie)
	setOAuthPendingBrowserCookie(c, browserSessionKey, secureCookie)
	clearOAuthPendingSessionCookie(c, secureCookie)
	if intent == oauthIntentBindCurrentUser {
		bindCookieValue, err := h.buildOAuthBindUserCookieFromContext(c)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		feishuSetCookie(c, feishuOAuthBindUserCookieName, encodeCookieValue(bindCookieValue), feishuOAuthCookieMaxAgeSec, secureCookie)
	} else {
		feishuClearCookie(c, feishuOAuthBindUserCookieName, secureCookie)
	}

	authURL, err := buildFeishuAuthorizeURL(cfg, state)
	if err != nil {
		response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_BUILD_URL_FAILED", "failed to build feishu authorization url").WithCause(err))
		return
	}
	c.Redirect(http.StatusFound, authURL)
}

// FeishuOAuthCallback handles Feishu OAuth callback and creates a browser-bound
// pending session, keeping token issuance on the existing pending OAuth exchange.
// GET /api/v1/auth/oauth/feishu/callback?code=...&state=...
func (h *AuthHandler) FeishuOAuthCallback(c *gin.Context) {
	cfg, cfgErr := h.getFeishuOAuthConfig(c.Request.Context())
	if cfgErr != nil {
		response.ErrorFrom(c, cfgErr)
		return
	}

	frontendCallback := strings.TrimSpace(cfg.FrontendRedirectURL)
	if frontendCallback == "" {
		frontendCallback = feishuOAuthDefaultFrontendCB
	}
	if providerErr := strings.TrimSpace(c.Query("error")); providerErr != "" {
		redirectOAuthError(c, frontendCallback, "provider_error", providerErr, c.Query("error_description"))
		return
	}

	code := strings.TrimSpace(c.Query("code"))
	state := strings.TrimSpace(c.Query("state"))
	if code == "" || state == "" {
		redirectOAuthError(c, frontendCallback, "missing_params", "missing code/state", "")
		return
	}

	secureCookie := isRequestHTTPS(c)
	defer func() {
		feishuClearCookie(c, feishuOAuthStateCookieName, secureCookie)
		feishuClearCookie(c, feishuOAuthRedirectCookieName, secureCookie)
		feishuClearCookie(c, feishuOAuthIntentCookieName, secureCookie)
		feishuClearCookie(c, feishuOAuthBindUserCookieName, secureCookie)
		clearOAuthPromoCodeCookie(c, secureCookie)
	}()

	expectedState, err := readCookieDecoded(c, feishuOAuthStateCookieName)
	if err != nil || expectedState == "" || state != expectedState {
		redirectOAuthError(c, frontendCallback, "invalid_state", "invalid oauth state", "")
		return
	}
	redirectTo, _ := readCookieDecoded(c, feishuOAuthRedirectCookieName)
	redirectTo = sanitizeFrontendRedirectPath(redirectTo)
	if redirectTo == "" {
		redirectTo = feishuOAuthDefaultRedirectTo
	}
	intent, _ := readCookieDecoded(c, feishuOAuthIntentCookieName)
	intent = normalizeOAuthIntent(intent)
	browserSessionKey, _ := readOAuthPendingBrowserCookie(c)
	if strings.TrimSpace(browserSessionKey) == "" {
		redirectOAuthError(c, frontendCallback, "missing_browser_session", "missing oauth browser session", "")
		return
	}

	identity, err := fetchFeishuOAuthIdentity(c.Request.Context(), cfg, code)
	if err != nil {
		log.Printf("[Feishu OAuth] identity fetch failed: %v", err)
		redirectOAuthError(c, frontendCallback, "provider_error", "feishu_identity_fetch_failed", singleLine(err.Error()))
		return
	}
	if !checkFeishuTenantAllowed(cfg, identity.User.TenantKey) {
		redirectOAuthError(c, frontendCallback, "tenant_rejected", "", "")
		return
	}

	providerSubject := feishuProviderSubject(identity.User)
	if providerSubject == "" {
		redirectOAuthError(c, frontendCallback, "missing_subject", "missing feishu identity subject", "")
		return
	}

	email := feishuPrimaryEmail(identity.User)
	syntheticEmail := buildFeishuSyntheticEmail(providerSubject)
	resolvedEmail := email
	if resolvedEmail == "" {
		resolvedEmail = syntheticEmail
	}
	username := firstNonEmpty(identity.User.Name, identity.User.EnName, feishuFallbackUsername(providerSubject))
	identityRef := service.PendingAuthIdentityKey{
		ProviderType:    "feishu",
		ProviderKey:     feishuOAuthProviderKey,
		ProviderSubject: providerSubject,
	}
	upstreamClaims := buildFeishuUpstreamClaims(identity, resolvedEmail, syntheticEmail, username)

	if intent == oauthIntentBindCurrentUser {
		targetUserID, err := h.readOAuthBindUserIDFromCookie(c, feishuOAuthBindUserCookieName)
		if err != nil {
			redirectOAuthError(c, frontendCallback, "invalid_state", "invalid oauth bind target", "")
			return
		}
		if err := h.createOAuthPendingSession(c, oauthPendingSessionPayload{
			Intent:                 oauthIntentBindCurrentUser,
			Identity:               identityRef,
			TargetUserID:           &targetUserID,
			ResolvedEmail:          resolvedEmail,
			RedirectTo:             redirectTo,
			BrowserSessionKey:      browserSessionKey,
			UpstreamIdentityClaims: upstreamClaims,
			CompletionResponse:     map[string]any{"redirect": redirectTo},
		}); err != nil {
			redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
			return
		}
		redirectToFrontendCallback(c, frontendCallback)
		return
	}

	existingIdentityUser, err := h.findOAuthIdentityUser(c.Request.Context(), identityRef)
	if err != nil {
		redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
		return
	}
	if existingIdentityUser != nil {
		if err := h.createOAuthPendingSession(c, oauthPendingSessionPayload{
			Intent:                 oauthIntentLogin,
			Identity:               identityRef,
			TargetUserID:           &existingIdentityUser.ID,
			ResolvedEmail:          existingIdentityUser.Email,
			RedirectTo:             redirectTo,
			BrowserSessionKey:      browserSessionKey,
			UpstreamIdentityClaims: upstreamClaims,
			CompletionResponse:     map[string]any{"redirect": redirectTo},
		}); err != nil {
			redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
			return
		}
		redirectToFrontendCallback(c, frontendCallback)
		return
	}

	signupBlocked := h.isFeishuSignupBlocked(c.Request.Context(), cfg)
	if signupBlocked {
		if err := h.createOAuthPendingSession(c, oauthPendingSessionPayload{
			Intent:                 oauthIntentLogin,
			Identity:               identityRef,
			ResolvedEmail:          resolvedEmail,
			RedirectTo:             redirectTo,
			BrowserSessionKey:      browserSessionKey,
			UpstreamIdentityClaims: upstreamClaims,
			CompletionResponse: map[string]any{
				"step":                      "bind_login_required",
				"existing_account_bindable": true,
				"create_account_allowed":    false,
				"redirect":                  redirectTo,
			},
		}); err != nil {
			redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
			return
		}
		redirectToFrontendCallback(c, frontendCallback)
		return
	}

	compatEmailUser, err := h.findFeishuCompatEmailUser(c.Request.Context(), email)
	if err != nil {
		redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
		return
	}
	emailVerificationRequired := h != nil && h.authService != nil && h.authService.IsEmailVerifyEnabled(c.Request.Context())
	forceEmailOnSignup := h.isForceEmailOnThirdPartySignup(c.Request.Context())
	if compatEmailUser == nil && !emailVerificationRequired && !forceEmailOnSignup && h != nil && h.authService != nil {
		if err := h.ensureBackendModeAllowsNewUserLogin(c.Request.Context()); err != nil {
			redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
			return
		}
		tokenPair, user, err := h.authService.LoginOrRegisterOAuthWithTokenPairAndPromoCode(
			c.Request.Context(),
			resolvedEmail,
			username,
			"",
			"",
			readOAuthPromoCode(c),
			"feishu",
		)
		if err == nil {
			if err := applyPendingOAuthBinding(
				c.Request.Context(),
				h.entClient(),
				h.authService,
				h.userService,
				&dbent.PendingAuthSession{
					Intent:                 oauthIntentLogin,
					ProviderType:           identityRef.ProviderType,
					ProviderKey:            identityRef.ProviderKey,
					ProviderSubject:        identityRef.ProviderSubject,
					ResolvedEmail:          resolvedEmail,
					UpstreamIdentityClaims: upstreamClaims,
				},
				nil,
				&user.ID,
				true,
				false,
			); err != nil {
				redirectOAuthError(c, frontendCallback, "session_error", "failed to bind oauth identity", "")
				return
			}
			h.authService.RecordSuccessfulLogin(c.Request.Context(), user.ID)
			clearOAuthPendingSessionCookie(c, secureCookie)
			clearOAuthPendingBrowserCookie(c, secureCookie)
			redirectOAuthTokenPair(c, frontendCallback, tokenPair, redirectTo)
			return
		}
		if !errors.Is(err, service.ErrOAuthInvitationRequired) {
			redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
			return
		}
	}
	if err := h.createFeishuOAuthChoicePendingSession(
		c,
		identityRef,
		resolvedEmail,
		resolvedEmail,
		redirectTo,
		browserSessionKey,
		upstreamClaims,
		email,
		compatEmailUser,
		forceEmailOnSignup,
	); err != nil {
		redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
		return
	}
	redirectToFrontendCallback(c, frontendCallback)
}

func buildFeishuAuthorizeURL(cfg config.FeishuConnectConfig, state string) (string, error) {
	base := strings.TrimSpace(cfg.AuthorizeURL)
	if base == "" {
		return "", infraerrors.InternalServer("FEISHU_AUTHORIZE_URL_EMPTY", "feishu authorize_url not configured")
	}
	redirectURI := strings.TrimSpace(cfg.RedirectURL)
	if redirectURI == "" {
		return "", infraerrors.InternalServer("FEISHU_REDIRECT_URL_EMPTY", "feishu redirect_url not configured")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", infraerrors.InternalServer("FEISHU_AUTHORIZE_URL_PARSE_FAILED", "failed to parse feishu authorize_url").WithCause(err)
	}
	q := u.Query()
	q.Set("client_id", strings.TrimSpace(cfg.AppID))
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	if scopes := strings.TrimSpace(cfg.Scopes); scopes != "" {
		q.Set("scope", scopes)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func defaultFetchFeishuOAuthIdentity(ctx context.Context, cfg feishuOAuthConfig, code string) (*feishuOAuthIdentity, error) {
	tokenResp, err := feishuExchangeCode(ctx, cfg, code)
	if err != nil {
		return nil, err
	}
	userInfo, err := feishuFetchUserInfo(ctx, cfg, tokenResp.AccessToken)
	if err != nil {
		return nil, err
	}
	return &feishuOAuthIdentity{Token: *tokenResp, User: *userInfo}, nil
}

func feishuExchangeCode(ctx context.Context, cfg feishuOAuthConfig, code string) (*feishuOAuthTokenResponse, error) {
	body := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     strings.TrimSpace(cfg.AppID),
		"client_secret": strings.TrimSpace(cfg.AppSecret),
		"code":          strings.TrimSpace(code),
		"redirect_uri":  strings.TrimSpace(cfg.RedirectURL),
	}
	if scopes := strings.TrimSpace(cfg.Scopes); scopes != "" {
		body["scope"] = scopes
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(cfg.TokenURL), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("feishu token endpoint status %d: %s", resp.StatusCode, truncateLogValue(string(raw), 1024))
	}
	var out feishuOAuthTokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("feishu token error code=%d msg=%s", out.Code, out.Msg)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return nil, fmt.Errorf("feishu token response missing access_token")
	}
	return &out, nil
}

func feishuFetchUserInfo(ctx context.Context, cfg feishuOAuthConfig, accessToken string) (*feishuUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(cfg.UserInfoURL), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("feishu userinfo endpoint status %d: %s", resp.StatusCode, truncateLogValue(string(raw), 1024))
	}
	var out feishuUserInfoResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("feishu userinfo error code=%d msg=%s", out.Code, out.Msg)
	}
	return &out.Data, nil
}

func feishuProviderSubject(user feishuUserInfo) string {
	return strings.TrimSpace(firstNonEmpty(user.UnionID, user.OpenID, user.UserID))
}

func feishuPrimaryEmail(user feishuUserInfo) string {
	return strings.TrimSpace(strings.ToLower(firstNonEmpty(user.EnterpriseEmail, user.Email)))
}

func buildFeishuSyntheticEmail(subject string) string {
	return "feishu-" + strings.ToLower(strings.TrimSpace(subject)) + service.FeishuConnectSyntheticEmailDomain
}

func feishuFallbackUsername(subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "feishu_user"
	}
	if len([]rune(subject)) > 32 {
		return "feishu_" + string([]rune(subject)[:32])
	}
	return "feishu_" + subject
}

func buildFeishuUpstreamClaims(identity *feishuOAuthIdentity, resolvedEmail string, syntheticEmail string, username string) map[string]any {
	user := feishuUserInfo{}
	scope := ""
	if identity != nil {
		user = identity.User
		scope = identity.Token.Scope
	}
	claims := map[string]any{
		"email":                  strings.TrimSpace(resolvedEmail),
		"username":               strings.TrimSpace(username),
		"subject":                feishuProviderSubject(user),
		"union_id":               strings.TrimSpace(user.UnionID),
		"open_id":                strings.TrimSpace(user.OpenID),
		"user_id":                strings.TrimSpace(user.UserID),
		"tenant_key":             strings.TrimSpace(user.TenantKey),
		"employee_no":            strings.TrimSpace(user.EmployeeNo),
		"scope":                  strings.TrimSpace(scope),
		"suggested_display_name": firstNonEmpty(user.Name, user.EnName, username),
		"suggested_avatar_url":   firstNonEmpty(user.AvatarURL, user.AvatarThumb, user.AvatarMiddle, user.AvatarBig),
	}
	if syntheticEmail != "" && !strings.EqualFold(strings.TrimSpace(syntheticEmail), strings.TrimSpace(resolvedEmail)) {
		claims["synthetic_email"] = strings.TrimSpace(syntheticEmail)
	}
	return claims
}

func checkFeishuTenantAllowed(cfg config.FeishuConnectConfig, tenantKey string) bool {
	if cfg.TenantRestrictionPolicy != service.FeishuTenantRestrictionInternalOnly {
		return true
	}
	allowedTenant := strings.TrimSpace(cfg.AllowedTenantKey)
	if allowedTenant == "" {
		return true
	}
	return strings.EqualFold(allowedTenant, strings.TrimSpace(tenantKey))
}

func (h *AuthHandler) isFeishuSignupBlocked(ctx context.Context, cfg config.FeishuConnectConfig) bool {
	if h.settingSvc == nil {
		return false
	}
	if h.settingSvc.IsRegistrationEnabled(ctx) {
		return false
	}
	if cfg.BypassRegistration && cfg.TenantRestrictionPolicy == service.FeishuTenantRestrictionInternalOnly {
		return false
	}
	return true
}

func (h *AuthHandler) findFeishuCompatEmailUser(ctx context.Context, email string) (*dbent.User, error) {
	client := h.entClient()
	if client == nil {
		return nil, infraerrors.ServiceUnavailable("PENDING_AUTH_NOT_READY", "pending auth service is not ready")
	}
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" ||
		strings.HasSuffix(email, service.LinuxDoConnectSyntheticEmailDomain) ||
		strings.HasSuffix(email, service.OIDCConnectSyntheticEmailDomain) ||
		strings.HasSuffix(email, service.WeChatConnectSyntheticEmailDomain) ||
		strings.HasSuffix(email, service.DingTalkConnectSyntheticEmailDomain) ||
		strings.HasSuffix(email, service.FeishuConnectSyntheticEmailDomain) {
		return nil, nil
	}
	userEntity, err := findUserByNormalizedEmail(ctx, client, email)
	if err != nil {
		if errors.Is(err, service.ErrUserNotFound) {
			return nil, nil
		}
		return nil, infraerrors.InternalServer("COMPAT_EMAIL_LOOKUP_FAILED", "failed to look up compat email user").WithCause(err)
	}
	return userEntity, nil
}

func (h *AuthHandler) createFeishuOAuthChoicePendingSession(
	c *gin.Context,
	identity service.PendingAuthIdentityKey,
	suggestedEmail string,
	resolvedEmail string,
	redirectTo string,
	browserSessionKey string,
	upstreamClaims map[string]any,
	compatEmail string,
	compatEmailUser *dbent.User,
	forceEmailOnSignup bool,
) error {
	suggestionEmail := strings.TrimSpace(suggestedEmail)
	canonicalEmail := strings.TrimSpace(resolvedEmail)
	if suggestionEmail == "" {
		suggestionEmail = canonicalEmail
	}
	completionResponse := map[string]any{
		"step":                      oauthPendingChoiceStep,
		"adoption_required":         true,
		"redirect":                  strings.TrimSpace(redirectTo),
		"email":                     suggestionEmail,
		"resolved_email":            canonicalEmail,
		"existing_account_email":    "",
		"existing_account_bindable": false,
		"create_account_allowed":    true,
		"force_email_on_signup":     forceEmailOnSignup,
		"choice_reason":             "third_party_signup",
	}
	if strings.TrimSpace(compatEmail) != "" {
		completionResponse["compat_email"] = strings.TrimSpace(compatEmail)
	}
	if compatEmailUser != nil {
		completionResponse["email"] = strings.TrimSpace(compatEmailUser.Email)
		completionResponse["existing_account_email"] = strings.TrimSpace(compatEmailUser.Email)
		completionResponse["existing_account_bindable"] = true
		completionResponse["choice_reason"] = "compat_email_match"
	}
	if forceEmailOnSignup && compatEmailUser == nil {
		completionResponse["choice_reason"] = "force_email_on_signup"
	}
	resolvedChoiceEmail := suggestionEmail
	if compatEmailUser != nil {
		resolvedChoiceEmail = strings.TrimSpace(compatEmailUser.Email)
	}
	var targetUserID *int64
	if compatEmailUser != nil && compatEmailUser.ID > 0 {
		targetUserID = &compatEmailUser.ID
	}
	return h.createOAuthPendingSession(c, oauthPendingSessionPayload{
		Intent:                 oauthIntentLogin,
		Identity:               identity,
		TargetUserID:           targetUserID,
		ResolvedEmail:          resolvedChoiceEmail,
		RedirectTo:             redirectTo,
		BrowserSessionKey:      browserSessionKey,
		UpstreamIdentityClaims: upstreamClaims,
		CompletionResponse:     completionResponse,
	})
}
