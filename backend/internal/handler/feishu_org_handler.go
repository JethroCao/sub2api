package handler

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type FeishuOrgHandler struct {
	orgService *service.FeishuOrgPermissionService
	settingSvc *service.SettingService
}

func NewFeishuOrgHandler(orgService *service.FeishuOrgPermissionService, settingSvc *service.SettingService) *FeishuOrgHandler {
	return &FeishuOrgHandler{orgService: orgService, settingSvc: settingSvc}
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
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	policy := service.FeishuDeparturePolicy{
		Action:           service.FeishuDepartedUserActionAutoDisable,
		ThresholdCount:   service.FeishuDefaultDisableThresholdCount,
		ThresholdPercent: service.FeishuDefaultDisableThresholdPct,
	}
	if h.settingSvc != nil {
		settings, err := h.settingSvc.GetAllSettings(c.Request.Context())
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		policy = service.FeishuDeparturePolicy{
			Action:           settings.FeishuDepartedUserAction,
			ThresholdCount:   settings.FeishuSyncDisableThresholdCount,
			ThresholdPercent: settings.FeishuSyncDisableThresholdPercent,
		}
	}
	result, err := h.orgService.RunManualReconcile(c.Request.Context(), subject.UserID, policy)
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
