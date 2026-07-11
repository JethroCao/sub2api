package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// CheckFeishuPreflight returns local Feishu configuration readiness without calling Feishu.
// POST /api/v1/admin/settings/feishu/preflight
func (h *SettingHandler) CheckFeishuPreflight(c *gin.Context) {
	result, err := h.settingService.CheckFeishuPreflight(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
