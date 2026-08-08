package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type videoAccountTestRepo struct {
	AccountRepository
	account *Account
}

func (r *videoAccountTestRepo) GetByID(context.Context, int64) (*Account, error) {
	if r.account == nil {
		return nil, ErrAccountNotFound
	}
	return r.account, nil
}

type recordingVideoAccountProbe struct {
	credentialProbeCalls int
	generationCalls      int
	result               VideoAccountProbeResult
	err                  error
}

func (p *recordingVideoAccountProbe) ProbeCredentials(context.Context, *Account) (VideoAccountProbeResult, error) {
	p.credentialProbeCalls++
	return p.result, p.err
}

type rejectingVideoTestHTTPUpstream struct {
	calls int
}

func (u *rejectingVideoTestHTTPUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.calls++
	return nil, errors.New("video account test must not issue HTTP")
}

func (u *rejectingVideoTestHTTPUpstream) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls++
	return nil, errors.New("video account test must not issue HTTP")
}

func TestAccountTestVideoRoutesToCredentialProbeWithoutGenerationOrHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID:       91,
		Platform: PlatformVideo,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"api_key": "ark-key",
		},
		Extra: map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
	}
	probe := &recordingVideoAccountProbe{
		result: VideoAccountProbeResult{
			Code:   VideoAccountProbeNotSupportedCode,
			Status: VideoAccountProbeLocalConfigValidatedStatus,
		},
		err: ErrVideoAccountProbeNotSupported,
	}
	upstream := &rejectingVideoTestHTTPUpstream{}
	svc := &AccountTestService{
		accountRepo:       &videoAccountTestRepo{account: account},
		httpUpstream:      upstream,
		videoAccountProbe: probe,
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/91/test", nil)

	err := svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault)

	require.ErrorIs(t, err, ErrVideoAccountProbeNotSupported)
	require.Equal(t, 1, probe.credentialProbeCalls)
	require.Zero(t, probe.generationCalls)
	require.Zero(t, upstream.calls)
	require.Contains(t, recorder.Body.String(), `"code":"probe_not_supported"`)
	require.Contains(t, recorder.Body.String(), `"status":"local_config_validated"`)
	require.NotContains(t, recorder.Body.String(), `"success":true`)
	require.NotContains(t, recorder.Body.String(), "ark-key")
	require.NotContains(t, recorder.Body.String(), "claude")
}

func TestAccountTestVideoRejectsUnverifiedProbeResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID: 92, Platform: PlatformVideo, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"api_key": "ark-key"},
		Extra:       map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
	}
	probe := &recordingVideoAccountProbe{}
	upstream := &rejectingVideoTestHTTPUpstream{}
	svc := &AccountTestService{
		accountRepo:       &videoAccountTestRepo{account: account},
		httpUpstream:      upstream,
		videoAccountProbe: probe,
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/92/test", nil)

	err := svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault)

	require.Error(t, err)
	require.Equal(t, 1, probe.credentialProbeCalls)
	require.Zero(t, upstream.calls)
	require.Contains(t, recorder.Body.String(), `"code":"video_probe_unverified"`)
	require.NotContains(t, recorder.Body.String(), `"success":true`)
	require.NotContains(t, recorder.Body.String(), "ark-key")
}

func TestVideoAccountLocalProbeFailsClosedForSeedanceAndKling(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
	}{
		{
			name: "seedance",
			account: &Account{
				Platform:    PlatformVideo,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Credentials: map[string]any{"api_key": "ark-key"},
				Extra:       map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
			},
		},
		{
			name: "kling contract gate",
			account: &Account{
				Platform: PlatformVideo,
				Type:     AccountTypeAPIKey,
				Status:   StatusActive,
				Credentials: map[string]any{
					"access_key": "access",
					"secret_key": "secret",
				},
				Extra: map[string]any{"video_provider": VideoProviderKling, "model_mapping": map[string]any{"kling-3.0": "kling-v3"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := NewLocalVideoAccountProbe().ProbeCredentials(context.Background(), tt.account)
			require.ErrorIs(t, err, ErrVideoAccountProbeNotSupported)
			require.Equal(t, VideoAccountProbeNotSupportedCode, result.Code)
			require.Equal(t, VideoAccountProbeLocalConfigValidatedStatus, result.Status)
		})
	}
}

func TestVideoAccountLocalProbeRejectsInvalidFinalConfiguration(t *testing.T) {
	account := &Account{
		Platform:    PlatformVideo,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Credentials: map[string]any{"access_key": "access"},
		Extra:       map[string]any{"video_provider": VideoProviderKling, "model_mapping": map[string]any{"kling-3.0": "kling-v3"}},
	}

	result, err := NewLocalVideoAccountProbe().ProbeCredentials(context.Background(), account)

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrVideoAccountProbeNotSupported)
	require.Empty(t, result.Code)
}

func TestAccountTestVideoDisabledSecretlessAccountFailsStrictLocalCredentialValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID:          93,
		Platform:    PlatformVideo,
		Type:        AccountTypeAPIKey,
		Status:      StatusDisabled,
		Credentials: map[string]any{},
		Extra: map[string]any{
			VideoProviderExtraKey:     VideoProviderKling,
			VideoModelMappingExtraKey: map[string]any{"kling-3.0": "kling-v3"},
		},
	}
	upstream := &rejectingVideoTestHTTPUpstream{}
	svc := &AccountTestService{
		accountRepo:       &videoAccountTestRepo{account: account},
		httpUpstream:      upstream,
		videoAccountProbe: NewLocalVideoAccountProbe(),
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/93/test", nil)

	err := svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault)

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrVideoAccountProbeNotSupported)
	require.Zero(t, upstream.calls)
	require.Contains(t, recorder.Body.String(), `"code":"invalid_video_account_config"`)
	require.NotContains(t, recorder.Body.String(), VideoAccountProbeLocalConfigValidatedStatus)
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestVideoAccountLocalProbeRejectsUnapprovedStoredConfiguration(t *testing.T) {
	account := &Account{
		Platform: PlatformVideo,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"api_key": "ark-key",
			"region":  "unapproved",
		},
		Extra: map[string]any{"video_provider": VideoProviderSeedance, "model_mapping": map[string]any{"seedance-2.0": "endpoint-id"}},
	}

	result, err := NewLocalVideoAccountProbe().ProbeCredentials(context.Background(), account)

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrVideoAccountProbeNotSupported)
	require.Empty(t, result.Code)
}
