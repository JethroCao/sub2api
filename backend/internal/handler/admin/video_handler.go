package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type VideoHandler struct {
	service *service.AdminVideoService
}

func NewVideoHandler(adminVideoService *service.AdminVideoService) *VideoHandler {
	return &VideoHandler{service: adminVideoService}
}

func (h *VideoHandler) ListPricingRules(c *gin.Context) {
	groupID, ok := adminVideoPositiveID(c, "id")
	if !ok {
		return
	}
	rules, err := h.service.ListPricingRules(c.Request.Context(), groupID)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, rules)
}

type replaceVideoPricingRulesRequest struct {
	Rules []service.VideoPricingRuleInput `json:"rules" binding:"required"`
}

func (h *VideoHandler) ReplacePricingRules(c *gin.Context) {
	middleware.SetAuditAction(c, "admin.video.pricing.replace")
	groupID, ok := adminVideoPositiveID(c, "id")
	if !ok {
		return
	}
	if !requireAdminVideoIdempotencyKey(c) {
		return
	}
	var request replaceVideoPricingRulesRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.ErrorFrom(c, service.ErrVideoPricingRuleInvalid)
		return
	}
	payload := map[string]any{"target_group_id": groupID, "rules": request.Rules}
	executeAdminIdempotentJSON(c, "admin.video.pricing.replace", payload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		if err := h.service.ReplacePricingRules(ctx, groupID, request.Rules); err != nil {
			return nil, err
		}
		return h.service.ListPricingRules(ctx, groupID)
	})
}

func (h *VideoHandler) ListTasks(c *gin.Context) {
	query, err := parseAdminVideoTaskQuery(c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if h == nil || h.service == nil {
		response.ErrorFrom(c, infraerrors.ServiceUnavailable("ADMIN_VIDEO_UNAVAILABLE", "admin video service is unavailable"))
		return
	}
	tasks, total, err := h.service.ListTasks(c.Request.Context(), query)
	if response.ErrorFrom(c, err) {
		return
	}
	items := make([]map[string]any, 0, len(tasks))
	for i := range tasks {
		items = append(items, adminVideoTaskResponse(&tasks[i]))
	}
	response.Success(c, map[string]any{"items": items, "total": total, "limit": query.Limit, "offset": query.Offset})
}

func (h *VideoHandler) GetTask(c *gin.Context) {
	detail, err := h.service.GetTask(c.Request.Context(), strings.TrimSpace(c.Param("request_id")))
	if response.ErrorFrom(c, err) {
		return
	}
	events := make([]map[string]any, 0, len(detail.Events))
	for _, event := range detail.Events {
		var payload any
		if bytes := event.Payload.Bytes(); len(bytes) > 0 {
			_ = json.Unmarshal(bytes, &payload)
		}
		events = append(events, map[string]any{"id": event.ID, "event_type": event.EventType, "payload": payload, "created_at": event.CreatedAt})
	}
	response.Success(c, map[string]any{"task": adminVideoTaskResponse(&detail.Task), "events": events, "result_url_summary": detail.ResultURLSummary})
}

func (h *VideoHandler) Reconcile(c *gin.Context) {
	if !requireAdminVideoIdempotencyKey(c) {
		return
	}
	var request struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.ErrorFrom(c, service.ErrVideoTaskInvalidRequest)
		return
	}
	subject, ok := adminVideoSubject(c)
	if !ok {
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	payload := adminVideoTargetPayload(requestID, map[string]any{"reason": request.Reason})
	executeAdminIdempotentJSON(c, "admin.video.reconcile", payload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		result, err := h.service.Reconcile(ctx, requestID, service.AdminVideoReconcileCommand{
			ActorUserID: subject.UserID, AuditRequestID: adminVideoAuditRequestID(c), Reason: request.Reason,
			IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if err != nil {
			return nil, err
		}
		return adminVideoActionResponse(result), nil
	})
}

func (h *VideoHandler) Refund(c *gin.Context) {
	if !requireAdminVideoIdempotencyKey(c) {
		return
	}
	var request struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.ErrorFrom(c, service.ErrVideoTaskInvalidRequest)
		return
	}
	subject, ok := adminVideoSubject(c)
	if !ok {
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	payload := adminVideoTargetPayload(requestID, map[string]any{"reason": request.Reason})
	executeAdminIdempotentJSON(c, "admin.video.refund", payload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		result, err := h.service.Refund(ctx, requestID, service.AdminVideoRefundCommand{
			ActorUserID: subject.UserID, AuditRequestID: adminVideoAuditRequestID(c), Reason: request.Reason,
			IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if err != nil {
			return nil, err
		}
		return adminVideoActionResponse(result), nil
	})
}

func (h *VideoHandler) Complete(c *gin.Context) {
	if !requireAdminVideoIdempotencyKey(c) {
		return
	}
	var request service.AdminVideoCompleteCommand
	if err := c.ShouldBindJSON(&request); err != nil {
		response.ErrorFrom(c, service.ErrVideoTaskInvalidRequest)
		return
	}
	subject, ok := adminVideoSubject(c)
	if !ok {
		return
	}
	request.ActorUserID = subject.UserID
	request.AuditRequestID = adminVideoAuditRequestID(c)
	request.IdempotencyKey = c.GetHeader("Idempotency-Key")
	requestID := strings.TrimSpace(c.Param("request_id"))
	// The coordinator receives the request shape only as an ephemeral fingerprint;
	// the durable repository stores only hashes and a query-free URL summary.
	payload := adminVideoTargetPayload(requestID, request)
	executeAdminIdempotentJSON(c, "admin.video.complete", payload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		result, err := h.service.Complete(ctx, requestID, request)
		if err != nil {
			return nil, err
		}
		return adminVideoActionResponse(result), nil
	})
}

func adminVideoTargetPayload(requestID string, command any) map[string]any {
	return map[string]any{
		"target_request_id": strings.TrimSpace(requestID),
		"command":           command,
	}
}

func requireAdminVideoIdempotencyKey(c *gin.Context) bool {
	key, err := service.NormalizeIdempotencyKey(c.GetHeader("Idempotency-Key"))
	if err != nil {
		response.ErrorFrom(c, err)
		return false
	}
	if key == "" {
		response.ErrorFrom(c, service.ErrAdminVideoIdempotencyKey)
		return false
	}
	return true
}

func parseAdminVideoTaskQuery(c *gin.Context) (service.VideoTaskListQuery, error) {
	query := service.VideoTaskListQuery{RequestID: strings.TrimSpace(c.Query("request_id")), Provider: strings.TrimSpace(c.Query("provider")), ExternalModel: strings.TrimSpace(c.Query("model")), Operation: strings.TrimSpace(c.Query("operation")), Limit: 50}
	if len(query.ExternalModel) > 128 || len(query.RequestID) > 36 ||
		(query.Provider != "" && query.Provider != service.PlatformGrok && query.Provider != service.VideoProviderSeedance && query.Provider != service.VideoProviderKling) ||
		(query.Operation != "" && query.Operation != string(service.VideoOperationGeneration) && query.Operation != string(service.VideoOperationEdit) && query.Operation != string(service.VideoOperationExtension)) {
		return query, service.ErrVideoTaskInvalidRequest
	}
	parseID := func(name string) (*int64, error) {
		raw := strings.TrimSpace(c.Query(name))
		if raw == "" {
			return nil, nil
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			return nil, service.ErrVideoTaskInvalidRequest
		}
		return &value, nil
	}
	var err error
	if query.UserID, err = parseID("user_id"); err != nil {
		return query, err
	}
	if query.APIKeyID, err = parseID("api_key_id"); err != nil {
		return query, err
	}
	if query.AccountID, err = parseID("account_id"); err != nil {
		return query, err
	}
	if query.GroupID, err = parseID("group_id"); err != nil {
		return query, err
	}
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		query.Status = service.VideoTaskStatus(raw)
		if !service.IsVideoTaskStatus(query.Status) {
			return query, service.ErrVideoTaskInvalidRequest
		}
	}
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		query.Limit, err = strconv.Atoi(raw)
		if err != nil || query.Limit < 1 || query.Limit > 200 {
			return query, service.ErrVideoTaskInvalidRequest
		}
	}
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		query.Offset, err = strconv.Atoi(raw)
		if err != nil || query.Offset < 0 || query.Offset > 1_000_000 {
			return query, service.ErrVideoTaskInvalidRequest
		}
	}
	parseTime := func(name string) (*time.Time, error) {
		raw := strings.TrimSpace(c.Query(name))
		if raw == "" {
			return nil, nil
		}
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, service.ErrVideoTaskInvalidRequest
		}
		value = value.UTC()
		return &value, nil
	}
	if query.CreatedAfter, err = parseTime("created_after"); err != nil {
		return query, err
	}
	if query.CreatedBefore, err = parseTime("created_before"); err != nil {
		return query, err
	}
	if query.CreatedAfter != nil && query.CreatedBefore != nil && !query.CreatedAfter.Before(*query.CreatedBefore) {
		return query, service.ErrVideoTaskInvalidRequest
	}
	return query, nil
}

func adminVideoTaskResponse(task *service.VideoTask) map[string]any {
	if task == nil {
		return nil
	}
	return map[string]any{
		"request_id": task.RequestID, "user_id": task.UserID, "api_key_id": task.APIKeyID,
		"group_id": task.GroupID, "account_id": task.AccountID, "provider": task.Provider,
		"operation": task.Operation, "external_model": task.ExternalModel, "status": task.Status,
		"upstream_status": task.UpstreamStatus, "result_url_summary": safeAdminVideoResultURL(task.ResultURL),
		"result_duration_seconds": task.ResultDurationSeconds, "result_width": task.ResultWidth, "result_height": task.ResultHeight,
		"pricing_unit": task.PricingUnit, "unit_price": task.UnitPrice, "estimated_units": task.EstimatedUnits,
		"estimated_amount": task.EstimatedAmount, "frozen_amount": task.FrozenAmount, "settled_amount": task.SettledAmount,
		"billing_status": task.BillingStatus, "last_error_code": task.LastErrorCode,
		"next_poll_at": task.NextPollAt, "lease_expires_at": task.LeaseExpiresAt,
		"created_at": task.CreatedAt, "updated_at": task.UpdatedAt, "submitted_at": task.SubmittedAt,
		"started_at": task.StartedAt, "finished_at": task.FinishedAt, "settled_at": task.SettledAt,
	}
}

func adminVideoActionResponse(result *service.AdminVideoActionResult) any {
	if result == nil {
		return nil
	}
	return map[string]any{"task": adminVideoTaskResponse(&result.Task), "replayed": result.Replayed}
}

func safeAdminVideoResultURL(raw *string) string {
	if raw == nil {
		return ""
	}
	value := *raw
	if index := strings.IndexAny(value, "?#"); index >= 0 {
		value = value[:index]
	}
	return value
}

func adminVideoPositiveID(c *gin.Context, name string) (int64, bool) {
	value, err := strconv.ParseInt(strings.TrimSpace(c.Param(name)), 10, 64)
	if err != nil || value <= 0 {
		response.ErrorFrom(c, service.ErrVideoTaskInvalidRequest)
		return 0, false
	}
	return value, true
}

func adminVideoSubject(c *gin.Context) (middleware.AuthSubject, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Error(c, http.StatusUnauthorized, "admin authentication is required")
		return middleware.AuthSubject{}, false
	}
	return subject, true
}

func adminVideoAuditRequestID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	value, _ := c.Request.Context().Value(ctxkey.RequestID).(string)
	return strings.TrimSpace(value)
}
