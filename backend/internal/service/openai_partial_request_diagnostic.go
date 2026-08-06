package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const openAIPartialMissingRequestBodyLogMessage = "openai.partial_missing_request_body"

func isOpenAIPartialMissingError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	errorCode := strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.code").String())
	errorParam := strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.param").String())
	if strings.EqualFold(errorCode, "MissingParameter") && strings.EqualFold(errorParam, "partial") {
		return true
	}

	message := strings.TrimSpace(upstreamMsg)
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.message").String())
	}
	lower := strings.ToLower(message)
	if !strings.Contains(lower, "partial") {
		return false
	}
	return strings.Contains(lower, "missing") ||
		strings.Contains(lower, "required") ||
		strings.Contains(lower, "requires")
}

func logOpenAIPartialMissingRequestBodies(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamStatusCode int,
	upstreamMsg string,
	upstreamErrorBody []byte,
	clientBody []byte,
	failedUpstreamBody []byte,
	originalModel string,
	upstreamModel string,
) {
	if !isOpenAIPartialMissingError(upstreamStatusCode, upstreamMsg, upstreamErrorBody) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	accountID := int64(0)
	accountName := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
	}

	errorCode := strings.TrimSpace(gjson.GetBytes(upstreamErrorBody, "error.code").String())
	errorParam := strings.TrimSpace(gjson.GetBytes(upstreamErrorBody, "error.param").String())
	errorMessage := strings.TrimSpace(upstreamMsg)
	if errorMessage == "" {
		errorMessage = strings.TrimSpace(gjson.GetBytes(upstreamErrorBody, "error.message").String())
	}

	baseFields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Bool(logger.OpsSystemLogSkipField, true),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.Int64("api_key_id", getAPIKeyIDFromContext(c)),
		zap.Int("upstream_status_code", upstreamStatusCode),
		zap.String("upstream_error_code", errorCode),
		zap.String("upstream_error_param", errorParam),
		zap.String("upstream_error_message", errorMessage),
		zap.String("original_model", strings.TrimSpace(originalModel)),
		zap.String("upstream_model", strings.TrimSpace(upstreamModel)),
	}

	logBody := func(bodyKind string, body []byte) {
		digest := sha256.Sum256(body)
		fields := append([]zap.Field{}, baseFields...)
		fields = append(fields,
			zap.String("body_kind", bodyKind),
			zap.Int("request_body_bytes", len(body)),
			zap.String("request_body_sha256", fmt.Sprintf("%x", digest)),
			zap.ByteString("request_body", body),
		)
		logger.FromContext(ctx).With(fields...).Warn(openAIPartialMissingRequestBodyLogMessage)
	}

	logBody("client", clientBody)
	logBody("upstream", failedUpstreamBody)
}
