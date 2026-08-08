package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAdminVideoRefundRequiresIdempotencyKeyBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "request_id", Value: "vid_1"}}
	context.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
	context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/video/tasks/vid_1/refund", strings.NewReader(`{"reason":"provider confirmed absent"}`))
	context.Request.Header.Set("Content-Type", "application/json")

	NewVideoHandler(nil).Refund(context)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "IDEMPOTENCY_KEY_REQUIRED")
}

func TestAdminVideoCompleteRequiresIdempotencyKeyBeforePayloadSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "request_id", Value: "vid_1"}}
	context.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
	context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/video/tasks/vid_1/complete", strings.NewReader(`{
		"reason":"verified", "provider_task_id":"provider-1",
		"result_url":"https://cdn.example.com/v.mp4?token=secret",
		"duration_seconds":6, "resolution":"720p", "final_amount":1.5
	}`))
	context.Request.Header.Set("Content-Type", "application/json")

	NewVideoHandler(nil).Complete(context)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "IDEMPOTENCY_KEY_REQUIRED")
	require.NotContains(t, recorder.Body.String(), "token=secret")
}

func TestAdminVideoListRejectsUnboundedPageSize(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/video/tasks?limit=10000", nil)

	NewVideoHandler(nil).ListTasks(context)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestAdminVideoPricingReplaceRequiresIdempotencyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "id", Value: "7"}}
	context.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/groups/7/video-pricing-rules", strings.NewReader(`{"rules":[]}`))
	context.Request.Header.Set("Content-Type", "application/json")

	NewVideoHandler(nil).ReplacePricingRules(context)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "IDEMPOTENCY_KEY_REQUIRED")
	action, ok := context.Get("audit_action")
	require.True(t, ok)
	require.Equal(t, "admin.video.pricing.replace", action)
}

func TestAdminVideoActionFingerprintBindsTargetRequestID(t *testing.T) {
	payload := map[string]any{"reason": "provider confirmed absent"}
	first, err := service.BuildIdempotencyFingerprint(
		http.MethodPost,
		"/api/v1/admin/video/tasks/:request_id/refund",
		"admin:7",
		adminVideoTargetPayload("vid_1", payload),
	)
	require.NoError(t, err)
	second, err := service.BuildIdempotencyFingerprint(
		http.MethodPost,
		"/api/v1/admin/video/tasks/:request_id/refund",
		"admin:7",
		adminVideoTargetPayload("vid_2", payload),
	)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
}
