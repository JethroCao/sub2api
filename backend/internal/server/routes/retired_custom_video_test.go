package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRetiredCustomVideoPlatformHasNoPublicContentAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterGatewayRoutes(router, &handler.Handlers{
		Gateway: &handler.GatewayHandler{}, OpenAIGateway: &handler.OpenAIGatewayHandler{},
		AsyncImage: handler.NewAsyncImageHandler(nil, nil),
	}, servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		id := int64(1)
		c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{
			GroupID: &id, Group: &service.Group{ID: id, Platform: "video", Status: service.StatusActive, Hydrated: true},
		})
		c.Next()
	}), nil, nil, nil, nil, nil, &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1024 * 1024}})
	for _, path := range []string{
		"/v1/videos/vid_0123456789abcdef0123456789abcdef/content",
		"/videos/vid_0123456789abcdef0123456789abcdef/content",
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotFound, w.Code, "retired custom video request must not enter any provider: %s", path)
	}
}
