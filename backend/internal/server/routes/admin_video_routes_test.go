package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAdminVideoPricingReplaceUsesStepUpMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	admin := engine.Group("/api/v1/admin")
	stepUpCalled := false
	stepUp := middleware.StepUpAuthMiddleware(func(c *gin.Context) {
		stepUpCalled = true
		c.AbortWithStatus(http.StatusTeapot)
	})
	registerAdminVideoRoutes(admin, &handler.Handlers{Admin: &handler.AdminHandlers{
		Video: adminhandler.NewVideoHandler(nil),
	}}, stepUp)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/groups/7/video-pricing-rules", nil)
	engine.ServeHTTP(recorder, request)

	require.True(t, stepUpCalled)
	require.Equal(t, http.StatusTeapot, recorder.Code)
}
