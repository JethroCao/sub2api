package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestVideoAccountHandlerReadResponseExposesMetadataWithoutSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := newStubAdminService()
	stub.getAccountResult = &service.Account{
		ID:       41,
		Name:     "seedance",
		Platform: service.PlatformVideo,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"api_key":    "api-secret",
			"access_key": "access-secret",
			"secret_key": "secret-secret",
			"base_url":   "https://ark.example.com",
		},
		Extra: map[string]any{
			"video_provider":              service.VideoProviderSeedance,
			"model_mapping":               map[string]any{"seedance-2.0": "ep-seedance"},
			"video_disabled_capabilities": []any{"audio"},
		},
	}
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/accounts/:id", handler.GetByID)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/41", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	var body struct {
		Data struct {
			Credentials       map[string]any  `json:"credentials"`
			CredentialsStatus map[string]bool `json:"credentials_status"`
			VideoProvider     string          `json:"video_provider"`
			Capabilities      []string        `json:"video_capabilities"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, service.VideoProviderSeedance, body.Data.VideoProvider)
	require.Contains(t, body.Data.Capabilities, "generation")
	require.NotContains(t, body.Data.Capabilities, "audio")
	require.Equal(t, "https://ark.example.com", body.Data.Credentials["base_url"])
	require.True(t, body.Data.CredentialsStatus["has_api_key"])
	require.True(t, body.Data.CredentialsStatus["has_access_key"])
	require.True(t, body.Data.CredentialsStatus["has_secret_key"])
	require.NotContains(t, recorder.Body.String(), "api-secret")
	require.NotContains(t, recorder.Body.String(), "access-secret")
	require.NotContains(t, recorder.Body.String(), "secret-secret")
}

func TestVideoAccountResponseFieldsAreOmittedForNonVideoAccounts(t *testing.T) {
	handler := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	out := handler.accountResponseFromService(&service.Account{
		ID: 1, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "secret"},
	})

	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "video_provider")
	require.NotContains(t, string(raw), "video_capabilities")
	require.NotContains(t, string(raw), "secret")
}

func TestVideoAccountHandlerCreateMapsAdministrationFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := newStubAdminService()
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts", handler.Create)
	body := []byte(`{
		"name":"seedance",
		"platform":"video",
		"type":"apikey",
		"credentials":{"api_key":"ark-key","base_url":"https://ark.example.com"},
		"extra":{"video_provider":"seedance","model_mapping":{"seedance-2.0":"ep-seedance"},"video_disabled_capabilities":["audio"]},
		"proxy_id":9,"concurrency":3,"priority":4,"status":"active","group_ids":[7,8]
	}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, stub.createdAccounts, 1)
	input := stub.createdAccounts[0]
	require.Equal(t, service.PlatformVideo, input.Platform)
	require.Equal(t, service.AccountTypeAPIKey, input.Type)
	require.Equal(t, "ark-key", input.Credentials["api_key"])
	require.Equal(t, "https://ark.example.com", input.Credentials["base_url"])
	require.Equal(t, service.VideoProviderSeedance, input.Extra["video_provider"])
	require.Equal(t, float64(9), float64(*input.ProxyID))
	require.Equal(t, 3, input.Concurrency)
	require.Equal(t, 4, input.Priority)
	require.Equal(t, "active", input.Status)
	require.Equal(t, []int64{7, 8}, input.GroupIDs)
}

func TestVideoAccountHandlerUpdateMapsExplicitDisableAndSecretClear(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := newStubAdminService()
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.PUT("/accounts/:id", handler.Update)
	body := []byte(`{
		"status":"inactive",
		"credentials":{"access_key":null,"secret_key":""},
		"extra":{"video_provider":"kling","model_mapping":{"kling-3.0":"kling-v3"}},
		"proxy_id":0,"concurrency":0,"group_ids":[]
	}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/accounts/42", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotNil(t, stub.lastUpdateAccountInput)
	input := stub.lastUpdateAccountInput
	require.Equal(t, "inactive", input.Status)
	require.Contains(t, input.Credentials, "access_key")
	require.Nil(t, input.Credentials["access_key"])
	require.Equal(t, "", input.Credentials["secret_key"])
	require.Equal(t, service.VideoProviderKling, input.Extra["video_provider"])
	require.NotNil(t, input.ProxyID)
	require.Zero(t, *input.ProxyID)
	require.NotNil(t, input.Concurrency)
	require.Zero(t, *input.Concurrency)
	require.NotNil(t, input.GroupIDs)
	require.Empty(t, *input.GroupIDs)
}
