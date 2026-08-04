package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupHandlerCreateAcceptsVideoPlatformAndPermission(t *testing.T) {
	router, adminSvc := setupAdminRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/groups", bytes.NewBufferString(
		`{"name":"video","platform":"video","rate_multiplier":1,"allow_video_generation":true}`,
	))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, adminSvc.lastCreateGroupInput)
	require.Equal(t, service.PlatformVideo, adminSvc.lastCreateGroupInput.Platform)
	require.True(t, adminSvc.lastCreateGroupInput.AllowVideoGeneration)
}

func TestGroupHandlerUpdateAcceptsVideoPlatformAndPermission(t *testing.T) {
	router, adminSvc := setupAdminRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/groups/2", bytes.NewBufferString(
		`{"platform":"video","allow_video_generation":false}`,
	))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, adminSvc.lastUpdateGroupInput)
	require.Equal(t, service.PlatformVideo, adminSvc.lastUpdateGroupInput.Platform)
	require.NotNil(t, adminSvc.lastUpdateGroupInput.AllowVideoGeneration)
	require.False(t, *adminSvc.lastUpdateGroupInput.AllowVideoGeneration)
}

func TestGroupHandlerCompositeRouteAcceptsVideoTarget(t *testing.T) {
	router, adminSvc := setupAdminRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/groups/2/composite-routes", bytes.NewBufferString(
		`{"public_model":"video-model","target_platform":"video","upstream_model":"provider-model","enabled":true}`,
	))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.NotNil(t, adminSvc.lastCompositeRouteInput)
	require.Equal(t, service.PlatformVideo, adminSvc.lastCompositeRouteInput.TargetPlatform)
}
