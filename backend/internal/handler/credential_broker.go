package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type credentialBrokerIssuer interface {
	Enabled() bool
	Issue(context.Context, string, int64, string) (*service.CredentialBrokerToken, error)
}

// CredentialBrokerHandler is a separate node-to-node API, not a user API key API.
type CredentialBrokerHandler struct {
	broker         credentialBrokerIssuer
	trustedProxies []*net.IPNet
}

func NewCredentialBrokerHandler(broker *service.CredentialBrokerService, cfg *config.Config) *CredentialBrokerHandler {
	h := &CredentialBrokerHandler{broker: broker}
	if cfg != nil && cfg.CredentialBroker.Validate() == nil {
		for _, cidr := range cfg.CredentialBroker.TrustedProxyCIDRs {
			if _, n, err := net.ParseCIDR(cidr); err == nil {
				h.trustedProxies = append(h.trustedProxies, n)
			}
		}
	}
	return h
}

func (h *CredentialBrokerHandler) secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if len(r.Header.Values("X-Forwarded-Proto")) != 1 || r.Header.Get("X-Forwarded-Proto") != "https" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	for _, n := range h.trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func brokerHTTPError(c *gin.Context, status int, code string, retry int) {
	if retry > 0 {
		c.Header("Retry-After", strconv.Itoa(retry))
	}
	c.JSON(status, gin.H{"error": gin.H{"type": code, "message": code}})
}

func (h *CredentialBrokerHandler) Issue(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	if h == nil || h.broker == nil || !h.broker.Enabled() {
		brokerHTTPError(c, 404, "not_found", 0)
		return
	}
	if !h.secure(c.Request) {
		brokerHTTPError(c, 403, "https_required", 0)
		return
	}
	auth := c.Request.Header.Values("Authorization")
	if len(auth) != 1 {
		brokerHTTPError(c, 401, "unauthorized", 0)
		return
	}
	parts := strings.Fields(auth[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		brokerHTTPError(c, 401, "unauthorized", 0)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		brokerHTTPError(c, 400, "invalid_request", 0)
		return
	}
	if contentType := c.GetHeader("Content-Type"); contentType != "" {
		media, _, err := mime.ParseMediaType(contentType)
		if err != nil || media != "application/json" {
			brokerHTTPError(c, 400, "invalid_request", 0)
			return
		}
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2048)
	defer c.Request.Body.Close()
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var body *struct {
		RejectedRevision string `json:"rejected_revision"`
	}
	err = decoder.Decode(&body)
	if err != nil && !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			brokerHTTPError(c, 413, "request_too_large", 0)
		} else {
			brokerHTTPError(c, 400, "invalid_request", 0)
		}
		return
	}
	if body == nil && err == nil {
		brokerHTTPError(c, 400, "invalid_request", 0)
		return
	}
	if err == nil {
		var extra any
		if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			brokerHTTPError(c, 400, "invalid_request", 0)
			return
		}
	}
	rejected := ""
	if body != nil {
		rejected = body.RejectedRevision
	}
	token, err := h.broker.Issue(c.Request.Context(), parts[1], id, rejected)
	if err != nil {
		var e *service.CredentialBrokerError
		if errors.As(err, &e) {
			brokerHTTPError(c, e.Status, e.Code, e.RetryAfter)
		} else {
			brokerHTTPError(c, 503, "broker_unavailable", 1)
		}
		return
	}
	c.JSON(http.StatusOK, token)
}
