package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type VideoPublicHandler interface {
	Generate(*gin.Context)
	Edit(*gin.Context)
	Extend(*gin.Context)
	Status(*gin.Context)
	Content(*gin.Context)
}

type videoTaskApplication interface {
	Submit(context.Context, service.VideoSubmitCommand) (*service.VideoTask, error)
	GetOwned(context.Context, string, int64, int64) (*service.VideoTask, error)
}

type videoBillingEligibility interface {
	CheckBillingEligibility(context.Context, *service.User, *service.APIKey, *service.Group, *service.UserSubscription, string) error
}

type videoAccountReader interface {
	GetByID(context.Context, int64) (*service.Account, error)
}

type videoContentFetcher interface {
	Fetch(context.Context, string, string) (*http.Response, error)
}

type legacyGrokVideoHandler interface {
	GrokVideoGeneration(*gin.Context)
	GrokVideoEdit(*gin.Context)
	GrokVideoExtension(*gin.Context)
	GrokVideoStatus(*gin.Context)
	GrokVideoContent(*gin.Context)
}

type legacyGrokRouteGate interface {
	CanRouteGrok(context.Context, *service.Group) (bool, error)
}

type VideoHandler struct {
	tasks                         videoTaskApplication
	billing                       videoBillingEligibility
	accounts                      videoAccountReader
	providers                     *service.VideoProviderRegistry
	fetcher                       videoContentFetcher
	legacy                        legacyGrokVideoHandler
	legacyRoute                   legacyGrokRouteGate
	audit                         *OpenAIGatewayHandler
	durableGrokSubmissionsEnabled bool
}

func NewVideoHandler(
	tasks *service.VideoTaskService,
	billing *service.BillingCacheService,
	accounts service.AccountRepository,
	providers *service.VideoProviderRegistry,
	fetcher *service.VideoContentFetcher,
	legacy *OpenAIGatewayHandler,
	composite *service.CompositeRouteResolver,
	cfg *config.Config,
) *VideoHandler {
	durableGrokEnabled := cfg != nil && cfg.Video.Enabled && cfg.Video.GrokEnabled
	return newVideoHandler(tasks, billing, accounts, providers, fetcher, legacy, videoCompositeLegacyRouteGate{resolver: composite}, durableGrokEnabled)
}

func newVideoHandler(
	tasks videoTaskApplication,
	billing videoBillingEligibility,
	accounts videoAccountReader,
	providers *service.VideoProviderRegistry,
	fetcher videoContentFetcher,
	legacy legacyGrokVideoHandler,
	legacyRoute legacyGrokRouteGate,
	durableGrokSubmissionsEnabled bool,
) *VideoHandler {
	handler := &VideoHandler{
		tasks: tasks, billing: billing, accounts: accounts, providers: providers,
		fetcher: fetcher, legacy: legacy, legacyRoute: legacyRoute,
		durableGrokSubmissionsEnabled: durableGrokSubmissionsEnabled,
	}
	if openAI, ok := legacy.(*OpenAIGatewayHandler); ok {
		handler.audit = openAI
	}
	return handler
}

func (h *VideoHandler) Generate(c *gin.Context) { h.submit(c, service.VideoOperationGeneration) }
func (h *VideoHandler) Edit(c *gin.Context)     { h.submit(c, service.VideoOperationEdit) }
func (h *VideoHandler) Extend(c *gin.Context)   { h.submit(c, service.VideoOperationExtension) }

func (h *VideoHandler) submit(c *gin.Context, operation service.VideoOperation) {
	apiKey, ok := videoOwnerAPIKey(c)
	if !ok {
		videoError(c, http.StatusUnauthorized, "invalid_request_error", "Invalid API key")
		return
	}
	if h.useLegacyGrokSubmission(apiKey.Group) {
		switch operation {
		case service.VideoOperationGeneration:
			h.legacy.GrokVideoGeneration(c)
		case service.VideoOperationEdit:
			h.legacy.GrokVideoEdit(c)
		case service.VideoOperationExtension:
			h.legacy.GrokVideoExtension(c)
		default:
			videoError(c, http.StatusNotFound, "not_found_error", "Videos API is not supported for this operation")
		}
		return
	}
	if apiKey.Group == nil || !apiKey.Group.AllowVideoGeneration {
		videoError(c, http.StatusForbidden, "unsupported_capability", "Video generation is not allowed for this group")
		return
	}
	if h == nil || h.tasks == nil {
		videoError(c, http.StatusServiceUnavailable, "upstream_error", "Video service is unavailable")
		return
	}

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			videoError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body is too large")
			return
		}
		videoError(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	request, err := service.NormalizeVideoRequest(operation, c.GetHeader("Content-Type"), body)
	if err != nil {
		videoSubmissionError(c, err)
		return
	}
	publicModel := request.Model
	if model, exists := service.RequestedPublicModelFromContext(c.Request.Context()); exists {
		publicModel = model
		request.Model = model
	}
	platform, provider, err := videoSubmissionRoute(c.Request.Context(), apiKey.Group, publicModel)
	if err != nil {
		videoSubmissionError(c, err)
		return
	}
	if h.audit != nil {
		moderationBody := service.ParseGrokMediaRequest(c.GetHeader("Content-Type"), body).ModerationBody()
		if len(moderationBody) > 0 {
			subject, _ := middleware.GetAuthSubjectFromContext(c)
			decision := h.audit.checkSecurityAudit(c, nil, apiKey, subject, service.ContentModerationProtocolOpenAIImages, publicModel, moderationBody)
			if decision != nil && !decision.AllowNextStage {
				h.audit.openAISecurityAuditError(c, decision)
				return
			}
		}
	}

	subscription, _ := middleware.GetSubscriptionFromContext(c)
	if h.billing != nil {
		if err := h.billing.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, platform); err != nil {
			videoBillingError(c, err)
			return
		}
	}
	billingMode := "balance"
	if subscription != nil {
		billingMode = "subscription"
	}
	task, err := h.tasks.Submit(c.Request.Context(), service.VideoSubmitCommand{
		UserID: apiKey.UserID, APIKeyID: apiKey.ID, Group: apiKey.Group,
		Subscription: subscription, Platform: platform, Provider: provider,
		BillingMode: billingMode, IdempotencyKey: c.GetHeader("Idempotency-Key"), Request: request,
	})
	if err != nil {
		videoSubmissionError(c, err)
		return
	}
	if task == nil || !service.IsVideoRequestID(task.RequestID) {
		videoError(c, http.StatusBadGateway, "upstream_error", "Video submission failed")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Location", videoContentStatusPath(c.Request.URL.Path, task.RequestID))
	c.JSON(videoSubmissionHTTPStatus(task.Status), videoTaskResponse(c, task))
}

func (h *VideoHandler) useLegacyGrokSubmission(group *service.Group) bool {
	return h != nil && h.legacy != nil && !h.durableGrokSubmissionsEnabled &&
		group != nil && group.Platform == service.PlatformGrok
}

func (h *VideoHandler) Status(c *gin.Context) {
	apiKey, ok := videoOwnerAPIKey(c)
	if !ok {
		videoError(c, http.StatusUnauthorized, "invalid_request_error", "Invalid API key")
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	task, handled := h.getOwnedOrLegacy(c, apiKey, requestID, false)
	if handled {
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, videoTaskResponse(c, task))
}

func (h *VideoHandler) Content(c *gin.Context) {
	apiKey, ok := videoOwnerAPIKey(c)
	if !ok {
		videoError(c, http.StatusUnauthorized, "invalid_request_error", "Invalid API key")
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	task, handled := h.getOwnedOrLegacy(c, apiKey, requestID, true)
	if handled {
		return
	}
	if task.Status != service.VideoTaskSucceeded {
		videoError(c, http.StatusNotFound, "not_found_error", "Video content is not available")
		return
	}

	if task.ResultURL != nil && strings.TrimSpace(*task.ResultURL) != "" {
		if h.fetcher == nil {
			videoError(c, http.StatusBadGateway, "upstream_error", "Video content is unavailable")
			return
		}
		response, err := h.fetcher.Fetch(c.Request.Context(), strings.TrimSpace(*task.ResultURL), c.GetHeader("Range"))
		if err != nil {
			videoSubmissionError(c, err)
			return
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent && response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			videoError(c, http.StatusBadGateway, "upstream_error", "Video content upstream failed")
			return
		}
		copyVideoContentHeaders(c, response.Header)
		c.Status(response.StatusCode)
		_, _ = io.Copy(c.Writer, response.Body)
		return
	}
	h.openProviderContent(c, task)
}

func (h *VideoHandler) getOwnedOrLegacy(c *gin.Context, apiKey *service.APIKey, requestID string, content bool) (*service.VideoTask, bool) {
	if h == nil || h.tasks == nil || requestID == "" {
		videoError(c, http.StatusNotFound, "not_found_error", "Video request not found")
		return nil, true
	}
	task, err := h.tasks.GetOwned(c.Request.Context(), requestID, apiKey.UserID, apiKey.ID)
	if err == nil && task != nil {
		return task, false
	}
	if !errors.Is(err, service.ErrVideoTaskNotFound) {
		videoError(c, http.StatusBadGateway, "upstream_error", "Video task lookup failed")
		return nil, true
	}
	if service.IsVideoRequestID(requestID) {
		videoError(c, http.StatusNotFound, "not_found_error", "Video request not found")
		return nil, true
	}
	canLegacy, routeErr := h.canRouteLegacyGrok(c.Request.Context(), apiKey.Group)
	if routeErr != nil {
		videoError(c, http.StatusBadGateway, "upstream_error", "Video task lookup failed")
		return nil, true
	}
	if !canLegacy || h.legacy == nil {
		videoError(c, http.StatusNotFound, "not_found_error", "Video request not found")
		return nil, true
	}
	if content {
		h.legacy.GrokVideoContent(c)
	} else {
		h.legacy.GrokVideoStatus(c)
	}
	return nil, true
}

func (h *VideoHandler) canRouteLegacyGrok(ctx context.Context, group *service.Group) (bool, error) {
	if group == nil {
		return false, nil
	}
	if group.Platform == service.PlatformGrok {
		return true, nil
	}
	if group.Platform != service.PlatformComposite || h.legacyRoute == nil {
		return false, nil
	}
	return h.legacyRoute.CanRouteGrok(ctx, group)
}

func (h *VideoHandler) openProviderContent(c *gin.Context, task *service.VideoTask) {
	if h.accounts == nil || h.providers == nil || task == nil || task.AccountID <= 0 {
		videoError(c, http.StatusBadGateway, "upstream_error", "Video content is unavailable")
		return
	}
	account, err := h.accounts.GetByID(c.Request.Context(), task.AccountID)
	if err != nil || account == nil || account.ID != task.AccountID {
		videoError(c, http.StatusBadGateway, "upstream_error", "Video content is unavailable")
		return
	}
	provider, ok := h.providers.Get(task.Provider)
	if !ok || provider == nil {
		videoError(c, http.StatusBadGateway, "upstream_error", "Video content is unavailable")
		return
	}
	ctx := service.WithGrokVideoContentRange(c.Request.Context(), c.GetHeader("Range"))
	body, headers, length, status, err := openVideoProviderContent(ctx, provider, account, *task)
	if err != nil {
		videoSubmissionError(c, err)
		return
	}
	if body == nil {
		videoError(c, http.StatusBadGateway, "upstream_error", "Video content is unavailable")
		return
	}
	defer func() { _ = body.Close() }()
	if status != http.StatusOK && status != http.StatusPartialContent && status != http.StatusRequestedRangeNotSatisfiable {
		videoError(c, http.StatusBadGateway, "upstream_error", "Video content upstream failed")
		return
	}
	copyVideoContentHeaders(c, headers)
	if length >= 0 && c.Writer.Header().Get("Content-Length") == "" {
		c.Header("Content-Length", strconv.FormatInt(length, 10))
	}
	c.Status(status)
	c.Writer.WriteHeaderNow()
	_, _ = io.Copy(c.Writer, body)
}

func openVideoProviderContent(ctx context.Context, provider service.VideoProvider, account *service.Account, task service.VideoTask) (io.ReadCloser, http.Header, int64, int, error) {
	if statusProvider, ok := provider.(service.VideoContentStatusProvider); ok {
		return statusProvider.OpenContentWithStatus(ctx, account, task)
	}
	body, headers, length, err := provider.OpenContent(ctx, account, task)
	status := http.StatusOK
	if headers != nil && headers.Get("Content-Range") != "" {
		status = http.StatusPartialContent
	}
	return body, headers, length, status, err
}

func videoOwnerAPIKey(c *gin.Context) (*service.APIKey, bool) {
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil || apiKey.ID <= 0 || apiKey.UserID <= 0 || apiKey.Group == nil || apiKey.Group.ID <= 0 {
		return nil, false
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID != apiKey.UserID {
		return nil, false
	}
	return apiKey, true
}

func videoSubmissionRoute(ctx context.Context, group *service.Group, model string) (string, string, error) {
	if group == nil {
		return "", "", service.ErrVideoGenerationNotAllowed
	}
	platform := group.Platform
	if group.Platform == service.PlatformComposite {
		resolved, ok := service.ResolvedTargetPlatformFromContext(ctx)
		if !ok {
			return "", "", service.ErrVideoUnsupportedCapability
		}
		platform = resolved
	}
	switch platform {
	case service.PlatformGrok:
		return platform, service.PlatformGrok, nil
	case service.PlatformVideo:
		routeModel := model
		if upstreamModel, ok := service.ResolvedUpstreamModelFromContext(ctx); ok {
			routeModel = upstreamModel
		}
		normalized := strings.ToLower(strings.TrimSpace(routeModel))
		switch {
		case strings.HasPrefix(normalized, "seedance-"):
			return platform, service.VideoProviderSeedance, nil
		case strings.HasPrefix(normalized, "kling-"):
			return "", "", service.ErrVideoUnsupportedCapability
		default:
			return "", "", service.ErrVideoUnsupportedCapability
		}
	default:
		return "", "", service.ErrVideoUnsupportedCapability
	}
}

func videoSubmissionHTTPStatus(status service.VideoTaskStatus) int {
	switch status {
	case service.VideoTaskCreated, service.VideoTaskSubmitting, service.VideoTaskSubmitted,
		service.VideoTaskQueued, service.VideoTaskRunning, service.VideoTaskUnknown:
		return http.StatusAccepted
	default:
		return http.StatusOK
	}
}

func videoTaskResponse(c *gin.Context, task *service.VideoTask) gin.H {
	response := gin.H{
		"id": task.RequestID, "request_id": task.RequestID, "status": task.Status,
		"provider": task.Provider, "model": task.ExternalModel,
		"created_at": task.CreatedAt.Unix(), "updated_at": task.UpdatedAt.Unix(),
	}
	if task.SubmittedAt != nil {
		response["submitted_at"] = task.SubmittedAt.Unix()
	}
	if task.StartedAt != nil {
		response["started_at"] = task.StartedAt.Unix()
	}
	if task.FinishedAt != nil {
		response["finished_at"] = task.FinishedAt.Unix()
	}
	if task.ResultURLExpiresAt != nil {
		response["expires_at"] = task.ResultURLExpiresAt.Unix()
	}
	if task.ResultURL != nil && strings.TrimSpace(*task.ResultURL) != "" {
		response["video_url"] = strings.TrimSpace(*task.ResultURL)
	} else if task.Status == service.VideoTaskSucceeded && c != nil {
		response["video_url"] = videoContentPath(c.Request.URL.Path, task.RequestID)
	}
	if task.ResultContentType != nil {
		response["content_type"] = *task.ResultContentType
	}
	if task.ResultDurationSeconds != nil {
		response["duration"] = *task.ResultDurationSeconds
	}
	if task.ResultWidth != nil {
		response["width"] = *task.ResultWidth
	}
	if task.ResultHeight != nil {
		response["height"] = *task.ResultHeight
	}
	if task.LastErrorCode != nil {
		errorBody := gin.H{"code": strings.ToLower(strings.TrimSpace(*task.LastErrorCode))}
		if task.LastErrorMessage != nil && strings.TrimSpace(*task.LastErrorMessage) != "" {
			errorBody["message"] = strings.TrimSpace(*task.LastErrorMessage)
		}
		response["error"] = errorBody
	}
	return response
}

func videoSubmissionError(c *gin.Context, err error) {
	status, errorType, message := videoErrorDetails(err)
	videoError(c, status, errorType, message)
}

func videoErrorDetails(err error) (int, string, string) {
	switch {
	case errors.Is(err, service.ErrVideoRequestBodyTooLarge):
		return http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body is too large"
	case errors.Is(err, service.ErrVideoInvalidRequest), errors.Is(err, service.ErrVideoInvalidMedia),
		errors.Is(err, service.ErrVideoTooManyAssets), errors.Is(err, service.ErrVideoTaskInvalidRequest),
		errors.Is(err, service.ErrIdempotencyKeyInvalid):
		return http.StatusBadRequest, "invalid_request_error", "Invalid video request"
	case errors.Is(err, service.ErrVideoIdempotencyConflict):
		return http.StatusConflict, "idempotency_conflict", "Idempotency key conflicts with an existing video request"
	case errors.Is(err, service.ErrVideoUnsupportedCapability), errors.Is(err, service.ErrVideoGenerationNotAllowed),
		errors.Is(err, service.ErrVideoProviderDisabled):
		return http.StatusBadRequest, "unsupported_capability", "Unsupported video capability"
	case errors.Is(err, service.ErrVideoPricingUnavailable), errors.Is(err, service.ErrVideoPricingInvalid):
		return http.StatusServiceUnavailable, "video_pricing_unavailable", "Video pricing is unavailable"
	case errors.Is(err, service.ErrVideoInsufficientBalance), errors.Is(err, service.ErrVideoSubscriptionQuotaExceeded),
		errors.Is(err, service.ErrInsufficientBalance):
		return http.StatusForbidden, "insufficient_balance", "Insufficient balance"
	case errors.Is(err, service.ErrNoAvailableAccounts):
		return http.StatusServiceUnavailable, "no_available_account", "No available video account"
	case errors.Is(err, service.ErrVideoTaskNotFound):
		return http.StatusNotFound, "not_found_error", "Video request not found"
	case errors.Is(err, service.ErrVideoContentUnsafeURL), errors.Is(err, service.ErrVideoContentTooManyRedirects),
		errors.Is(err, service.ErrVideoContentTooLarge):
		return http.StatusBadGateway, "upstream_error", "Video content is unavailable"
	}
	var providerErr service.VideoProviderError
	if errors.As(err, &providerErr) {
		if providerErr.Code == "unsupported_capability" {
			return http.StatusBadRequest, "unsupported_capability", "Unsupported video capability"
		}
		if providerErr.Code == "invalid_request" || providerErr.Code == "invalid_media" {
			return http.StatusBadRequest, "invalid_request_error", "Invalid video request"
		}
		if providerErr.Code == "upstream_rate_limit" || providerErr.HTTPStatus == http.StatusTooManyRequests {
			return http.StatusTooManyRequests, "rate_limit_error", "Upstream video rate limit exceeded"
		}
		status := providerErr.HTTPStatus
		if status < 500 {
			status = http.StatusBadGateway
		}
		return status, "upstream_error", "Upstream video request failed"
	}
	return http.StatusBadGateway, "upstream_error", "Video request failed"
}

func videoBillingError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrInsufficientBalance) || errors.Is(err, service.ErrVideoInsufficientBalance) {
		videoError(c, http.StatusForbidden, "insufficient_balance", "Insufficient balance")
		return
	}
	status, _, _, retryAfter := billingErrorDetails(err)
	if retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfter))
	}
	if status == http.StatusTooManyRequests {
		videoError(c, http.StatusTooManyRequests, "rate_limit_error", "Video request rate limit exceeded")
		return
	}
	if status == http.StatusServiceUnavailable {
		videoError(c, http.StatusServiceUnavailable, "upstream_error", "Video billing service is unavailable")
		return
	}
	videoError(c, http.StatusForbidden, "insufficient_balance", "Video billing eligibility check failed")
}

func videoError(c *gin.Context, status int, errorType, message string) {
	c.JSON(status, gin.H{"error": gin.H{"type": errorType, "message": message}})
}

var videoContentResponseHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag",
	"Last-Modified", "Content-Disposition",
}

func copyVideoContentHeaders(c *gin.Context, upstream http.Header) {
	for _, name := range videoContentResponseHeaders {
		if value := upstream.Get(name); value != "" {
			c.Header(name, value)
		}
	}
}

func videoContentStatusPath(requestPath, requestID string) string {
	if strings.HasPrefix(requestPath, "/v1/") {
		return "/v1/videos/" + requestID
	}
	return "/videos/" + requestID
}

func videoContentPath(requestPath, requestID string) string {
	return videoContentStatusPath(requestPath, requestID) + "/content"
}

type videoCompositeLegacyRouteGate struct {
	resolver *service.CompositeRouteResolver
}

func (g videoCompositeLegacyRouteGate) CanRouteGrok(ctx context.Context, group *service.Group) (bool, error) {
	if group == nil || group.Platform != service.PlatformComposite || g.resolver == nil {
		return false, nil
	}
	return g.resolver.CanRoutePlatform(ctx, group.ID, service.PlatformGrok)
}
