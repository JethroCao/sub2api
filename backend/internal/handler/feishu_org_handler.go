package handler

import (
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type FeishuOrgHandler struct {
	orgService   *service.FeishuOrgPermissionService
	settingSvc   *service.SettingService
	usageService *service.UsageService
}

func NewFeishuOrgHandler(orgService *service.FeishuOrgPermissionService, settingSvc *service.SettingService, usageService *service.UsageService) *FeishuOrgHandler {
	return &FeishuOrgHandler{orgService: orgService, settingSvc: settingSvc, usageService: usageService}
}

type feishuSetUserGroupsRequest struct {
	GroupIDs []int64 `json:"group_ids"`
	Reason   string  `json:"reason"`
}

type feishuSetDepartmentGroupsRequest struct {
	TenantKey string  `json:"tenant_key"`
	GroupIDs  []int64 `json:"group_ids"`
	Reason    string  `json:"reason"`
}

func (h *FeishuOrgHandler) ListDepartments(c *gin.Context) {
	if h == nil || h.orgService == nil {
		response.InternalError(c, "Feishu organization permission service is not configured")
		return
	}
	result, err := h.orgService.ListDepartments(c.Request.Context(), parseFeishuOrgListInput(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *FeishuOrgHandler) ListUsers(c *gin.Context) {
	if h == nil || h.orgService == nil {
		response.InternalError(c, "Feishu organization permission service is not configured")
		return
	}
	result, err := h.orgService.ListUsers(c.Request.Context(), parseFeishuOrgListInput(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *FeishuOrgHandler) ListSyncRuns(c *gin.Context) {
	if h == nil || h.orgService == nil {
		response.InternalError(c, "Feishu organization permission service is not configured")
		return
	}
	result, err := h.orgService.ListSyncRuns(c.Request.Context(), parseFeishuOrgListInput(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *FeishuOrgHandler) RunManualReconcile(c *gin.Context) {
	if h == nil || h.orgService == nil {
		response.InternalError(c, "Feishu organization permission service is not configured")
		return
	}
	if h.settingSvc == nil {
		response.InternalError(c, "Feishu settings service is not configured")
		return
	}
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	cfg, err := h.settingSvc.GetFeishuConnectOAuthConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	policy := service.FeishuDeparturePolicy{
		Action:           cfg.DepartedUserAction,
		ThresholdCount:   cfg.DisableThresholdCount,
		ThresholdPercent: cfg.DisableThresholdPercent,
	}
	result, err := h.orgService.RunFeishuOrgSync(c.Request.Context(), subject.UserID, cfg, policy)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *FeishuOrgHandler) ListManagedUsers(c *gin.Context) {
	if h == nil || h.orgService == nil {
		response.InternalError(c, "Feishu organization permission service is not configured")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Unauthorized")
		return
	}
	result, err := h.orgService.ListManagerUsers(c.Request.Context(), subject.UserID, parseFeishuOrgListInput(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *FeishuOrgHandler) ManagerAccess(c *gin.Context) {
	if h == nil || h.orgService == nil {
		response.InternalError(c, "Feishu organization permission service is not configured")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Unauthorized")
		return
	}
	hasAccess, err := h.orgService.HasManagerScope(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"has_access": hasAccess})
}

// SetManagedUserGroupGrants lets a Feishu department manager update the
// department-manager source grants for a user inside their Feishu scope.
func (h *FeishuOrgHandler) SetManagedUserGroupGrants(c *gin.Context) {
	if h == nil || h.orgService == nil {
		response.InternalError(c, "Feishu organization permission service is not configured")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Unauthorized")
		return
	}
	targetUserID, ok := parseFeishuOrgUserIDParam(c)
	if !ok {
		return
	}
	var req feishuSetUserGroupsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.orgService.SetDepartmentManagerUserGroupGrants(c.Request.Context(), service.FeishuDepartmentManagerAssignmentInput{
		ManagerUserID: subject.UserID,
		TargetUserID:  targetUserID,
		GroupIDs:      req.GroupIDs,
		Reason:        req.Reason,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

type feishuManagerUsageFilters struct {
	Filters   usagestats.UsageLogFilters
	StartTime time.Time
	EndTime   time.Time
}

func (h *FeishuOrgHandler) ListManagerUsage(c *gin.Context) {
	parsed, ok := h.parseManagerUsageFilters(c, false)
	if !ok {
		return
	}
	page, pageSize := response.ParsePagination(c)
	records, result, err := h.usageService.ListWithFilters(c.Request.Context(), pagination.PaginationParams{
		Page:      page,
		PageSize:  pageSize,
		SortBy:    c.DefaultQuery("sort_by", "created_at"),
		SortOrder: c.DefaultQuery("sort_order", "desc"),
	}, parsed.Filters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]dto.UsageLog, 0, len(records))
	for i := range records {
		out = append(out, *dto.UsageLogFromService(&records[i]))
	}
	response.Paginated(c, out, result.Total, result.Page, result.PageSize)
}

func (h *FeishuOrgHandler) ManagerUsageStats(c *gin.Context) {
	parsed, ok := h.parseManagerUsageFilters(c, true)
	if !ok {
		return
	}
	stats, err := h.usageService.GetStatsWithFilters(c.Request.Context(), parsed.Filters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	stats.TotalAccountCost = nil
	stats.UpstreamEndpoints = nil
	stats.EndpointPaths = nil
	response.Success(c, stats)
}

func (h *FeishuOrgHandler) ManagerDashboardStats(c *gin.Context) {
	parsed, ok := h.parseManagerUsageFilters(c, false)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	totalStats, err := h.usageService.GetStatsWithFilters(ctx, parsed.Filters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	userTZ := c.Query("timezone")
	now := timezone.NowInUserLocation(userTZ)
	todayStart := timezone.StartOfDayInUserLocation(now, userTZ)
	todayEnd := timezone.StartOfDayInUserLocation(now.AddDate(0, 0, 1), userTZ)
	todayFilters := parsed.Filters
	todayFilters.StartTime = &todayStart
	todayFilters.EndTime = &todayEnd
	todayStats, err := h.usageService.GetStatsWithFilters(ctx, todayFilters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	fiveMinutesAgo := now.Add(-5 * time.Minute)
	perfFilters := parsed.Filters
	perfFilters.StartTime = &fiveMinutesAgo
	perfFilters.EndTime = &now
	perfStats, err := h.usageService.GetStatsWithFilters(ctx, perfFilters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, usagestats.UserDashboardStats{
		TotalRequests:            totalStats.TotalRequests,
		TotalInputTokens:         totalStats.TotalInputTokens,
		TotalOutputTokens:        totalStats.TotalOutputTokens,
		TotalCacheCreationTokens: totalStats.TotalCacheCreationTokens,
		TotalCacheReadTokens:     totalStats.TotalCacheReadTokens,
		TotalTokens:              totalStats.TotalTokens,
		TotalCost:                totalStats.TotalCost,
		TotalActualCost:          totalStats.TotalActualCost,
		TodayRequests:            todayStats.TotalRequests,
		TodayInputTokens:         todayStats.TotalInputTokens,
		TodayOutputTokens:        todayStats.TotalOutputTokens,
		TodayCacheCreationTokens: todayStats.TotalCacheCreationTokens,
		TodayCacheReadTokens:     todayStats.TotalCacheReadTokens,
		TodayTokens:              todayStats.TotalTokens,
		TodayCost:                todayStats.TotalCost,
		TodayActualCost:          todayStats.TotalActualCost,
		AverageDurationMs:        totalStats.AverageDurationMs,
		Rpm:                      perfStats.TotalRequests / 5,
		Tpm:                      perfStats.TotalTokens / 5,
	})
}

func (h *FeishuOrgHandler) ManagerUsageTrend(c *gin.Context) {
	parsed, ok := h.parseManagerUsageFilters(c, true)
	if !ok {
		return
	}
	granularity := c.DefaultQuery("granularity", "day")
	trend, err := h.usageService.GetUsageTrendWithFilters(c.Request.Context(), parsed.StartTime, parsed.EndTime, granularity, parsed.Filters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"trend":       trend,
		"start_date":  parsed.StartTime.Format("2006-01-02"),
		"end_date":    parsed.EndTime.Add(-24 * time.Hour).Format("2006-01-02"),
		"granularity": granularity,
	})
}

func (h *FeishuOrgHandler) ManagerUsageModels(c *gin.Context) {
	parsed, ok := h.parseManagerUsageFilters(c, true)
	if !ok {
		return
	}
	modelSource := strings.TrimSpace(c.Query("model_source"))
	if modelSource != "" && modelSource != usagestats.ModelSourceRequested {
		response.BadRequest(c, "Invalid model_source, manager usage only supports requested")
		return
	}
	stats, err := h.usageService.GetModelStatsWithFiltersBySource(c.Request.Context(), parsed.StartTime, parsed.EndTime, parsed.Filters, usagestats.ModelSourceRequested)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"models":     userModelStatsFromUsageStats(stats),
		"start_date": parsed.StartTime.Format("2006-01-02"),
		"end_date":   parsed.EndTime.Add(-24 * time.Hour).Format("2006-01-02"),
	})
}

func (h *FeishuOrgHandler) ManagerUsageSnapshotV2(c *gin.Context) {
	parsed, ok := h.parseManagerUsageFilters(c, true)
	if !ok {
		return
	}
	granularity := strings.TrimSpace(c.DefaultQuery("granularity", "day"))
	if granularity != "hour" {
		granularity = "day"
	}
	includeTrend, ok := parseBoolQueryWithDefault(c, "include_trend", true)
	if !ok {
		return
	}
	includeModels, ok := parseBoolQueryWithDefault(c, "include_model_stats", true)
	if !ok {
		return
	}
	includeGroups, ok := parseBoolQueryWithDefault(c, "include_group_stats", true)
	if !ok {
		return
	}

	resp := gin.H{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"start_date":   parsed.StartTime.Format("2006-01-02"),
		"end_date":     parsed.EndTime.Add(-24 * time.Hour).Format("2006-01-02"),
		"granularity":  granularity,
	}
	if includeTrend {
		trend, err := h.usageService.GetUsageTrendWithFilters(c.Request.Context(), parsed.StartTime, parsed.EndTime, granularity, parsed.Filters)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		resp["trend"] = trend
	}
	if includeModels {
		models, err := h.usageService.GetModelStatsWithFiltersBySource(c.Request.Context(), parsed.StartTime, parsed.EndTime, parsed.Filters, usagestats.ModelSourceRequested)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		resp["models"] = userModelStatsFromUsageStats(models)
	}
	if includeGroups {
		groups, err := h.usageService.GetGroupStatsWithFilters(c.Request.Context(), parsed.StartTime, parsed.EndTime, parsed.Filters)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		resp["groups"] = userGroupStatsFromUsageStats(groups)
	}
	response.Success(c, resp)
}

func (h *FeishuOrgHandler) parseManagerUsageFilters(c *gin.Context, requireRange bool) (*feishuManagerUsageFilters, bool) {
	if h == nil || h.orgService == nil || h.usageService == nil {
		response.InternalError(c, "Feishu organization usage service is not configured")
		return nil, false
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Unauthorized")
		return nil, false
	}
	managedUserIDs, err := h.orgService.ListManagerLocalUserIDs(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return nil, false
	}
	if userIDStr := strings.TrimSpace(c.Query("user_id")); userIDStr != "" {
		userID, err := strconv.ParseInt(userIDStr, 10, 64)
		if err != nil || userID <= 0 {
			response.BadRequest(c, "Invalid user_id")
			return nil, false
		}
		if !int64SliceContains(managedUserIDs, userID) {
			response.Forbidden(c, "Not authorized to access this user's usage records")
			return nil, false
		}
		managedUserIDs = []int64{userID}
	}

	var groupID int64
	if groupIDStr := strings.TrimSpace(c.Query("group_id")); groupIDStr != "" {
		id, err := strconv.ParseInt(groupIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid group_id")
			return nil, false
		}
		groupID = id
	}

	var requestType *int16
	var stream *bool
	if requestTypeStr := strings.TrimSpace(c.Query("request_type")); requestTypeStr != "" {
		parsed, err := service.ParseUsageRequestType(requestTypeStr)
		if err != nil {
			response.BadRequest(c, err.Error())
			return nil, false
		}
		value := int16(parsed)
		requestType = &value
	} else if streamStr := strings.TrimSpace(c.Query("stream")); streamStr != "" {
		parsed, err := strconv.ParseBool(streamStr)
		if err != nil {
			response.BadRequest(c, "Invalid stream value, use true or false")
			return nil, false
		}
		stream = &parsed
	}

	var billingType *int8
	if billingTypeStr := strings.TrimSpace(c.Query("billing_type")); billingTypeStr != "" {
		id, err := strconv.ParseInt(billingTypeStr, 10, 8)
		if err != nil {
			response.BadRequest(c, "Invalid billing_type")
			return nil, false
		}
		parsed := int8(id)
		billingType = &parsed
	}
	billingMode := strings.TrimSpace(c.Query("billing_mode"))
	if billingMode != "" && !service.BillingMode(billingMode).IsValid() {
		response.BadRequest(c, "Invalid billing_mode")
		return nil, false
	}

	userTZ := c.Query("timezone")
	now := timezone.NowInUserLocation(userTZ)
	startDateStr := strings.TrimSpace(c.Query("start_date"))
	endDateStr := strings.TrimSpace(c.Query("end_date"))
	var startTime, endTime time.Time
	var startPtr, endPtr *time.Time
	if startDateStr != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", startDateStr, userTZ)
		if err != nil {
			response.BadRequest(c, "Invalid start_date format, use YYYY-MM-DD")
			return nil, false
		}
		startTime = t
		startPtr = &startTime
	}
	if endDateStr != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", endDateStr, userTZ)
		if err != nil {
			response.BadRequest(c, "Invalid end_date format, use YYYY-MM-DD")
			return nil, false
		}
		endTime = t.AddDate(0, 0, 1)
		endPtr = &endTime
	}
	if requireRange {
		if startPtr == nil {
			switch c.DefaultQuery("period", "") {
			case "today":
				startTime = timezone.StartOfDayInUserLocation(now, userTZ)
			case "week":
				startTime = now.AddDate(0, 0, -7)
			case "month":
				startTime = now.AddDate(0, -1, 0)
			default:
				startTime = timezone.StartOfDayInUserLocation(now.AddDate(0, 0, -7), userTZ)
			}
			startPtr = &startTime
		}
		if endPtr == nil {
			if strings.TrimSpace(c.Query("period")) != "" {
				endTime = now
			} else {
				endTime = timezone.StartOfDayInUserLocation(now.AddDate(0, 0, 1), userTZ)
			}
			endPtr = &endTime
		}
	}

	return &feishuManagerUsageFilters{
		Filters: usagestats.UsageLogFilters{
			UserIDs:           managedUserIDs,
			GroupID:           groupID,
			Model:             strings.TrimSpace(c.Query("model")),
			ModelFilterSource: usagestats.ModelSourceRequested,
			RequestType:       requestType,
			Stream:            stream,
			BillingType:       billingType,
			BillingMode:       billingMode,
			StartTime:         startPtr,
			EndTime:           endPtr,
		},
		StartTime: derefTime(startPtr),
		EndTime:   derefTime(endPtr),
	}, true
}

func int64SliceContains(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// SetUserOverrideGroupGrants lets a super admin update the super-admin override
// source grants for a user. The admin middleware protects the route.
func (h *FeishuOrgHandler) SetUserOverrideGroupGrants(c *gin.Context) {
	if h == nil || h.orgService == nil {
		response.InternalError(c, "Feishu organization permission service is not configured")
		return
	}
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	targetUserID, ok := parseFeishuOrgUserIDParam(c)
	if !ok {
		return
	}
	var req feishuSetUserGroupsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.orgService.SetUserGroupGrants(c.Request.Context(), service.FeishuSetUserGroupGrantsInput{
		ActorUserID:  subject.UserID,
		TargetUserID: targetUserID,
		Source:       service.FeishuGrantSourceSuperAdmin,
		GroupIDs:     req.GroupIDs,
		Reason:       req.Reason,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// SetDepartmentGroupPool lets a super admin configure which groups can be
// assigned to employees whose primary Feishu department is this department.
func (h *FeishuOrgHandler) SetDepartmentGroupPool(c *gin.Context) {
	if h == nil || h.orgService == nil {
		response.InternalError(c, "Feishu organization permission service is not configured")
		return
	}
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	deptID := c.Param("department_id")
	if deptID == "" {
		response.BadRequest(c, "Invalid department ID")
		return
	}
	var req feishuSetDepartmentGroupsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.orgService.SetDepartmentGroupPool(c.Request.Context(), service.FeishuSetDepartmentGroupPoolInput{
		ActorUserID:      subject.UserID,
		TenantKey:        req.TenantKey,
		OpenDepartmentID: deptID,
		GroupIDs:         req.GroupIDs,
		Reason:           req.Reason,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func parseFeishuOrgUserIDParam(c *gin.Context) (int64, bool) {
	raw := c.Param("id")
	if raw == "" {
		raw = c.Param("user_id")
	}
	userID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || userID <= 0 {
		response.BadRequest(c, "Invalid user ID")
		return 0, false
	}
	return userID, true
}

func parseFeishuOrgListInput(c *gin.Context) service.FeishuOrgListInput {
	return service.FeishuOrgListInput{
		TenantKey: c.Query("tenant_key"),
		Query:     c.Query("q"),
		Limit:     parseFeishuOrgIntQuery(c.Query("limit")),
		Offset:    parseFeishuOrgIntQuery(c.Query("offset")),
	}
}

func parseFeishuOrgIntQuery(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return value
}
