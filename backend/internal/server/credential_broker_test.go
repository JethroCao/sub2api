package server

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

func TestCredentialBrokerRouteDefaultsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerCredentialBrokerRoutes(r.Group("/api/v1"), &handler.Handlers{CredentialBroker: handler.NewCredentialBrokerHandler(nil, &config.Config{})})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/credential-broker/openai/4/token", nil))
	require.Equal(t, 404, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.JSONEq(t, `{"error":{"type":"not_found","message":"not_found"}}`, w.Body.String())
}
