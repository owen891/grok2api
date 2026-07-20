package system

import (
	"net/http"
	"strings"

	updatecheckapp "github.com/owen891/grok2api/backend/internal/application/updatecheck"
	"github.com/owen891/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	publicAPIBaseURL string
	versionCheck     *updatecheckapp.Service
}

func NewHandler(publicAPIBaseURL string, versionCheck *updatecheckapp.Service) *Handler {
	return &Handler{publicAPIBaseURL: strings.TrimRight(publicAPIBaseURL, "/"), versionCheck: versionCheck}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/system", h.get)
	router.GET("/system/version", h.getVersion)
	router.POST("/system/update/check", h.checkVersion)
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, gin.H{"publicApiBaseURL": h.publicAPIBaseURL})
}

func (h *Handler) getVersion(c *gin.Context) {
	if h.versionCheck == nil {
		response.Error(c, http.StatusServiceUnavailable, "updateCheckUnavailable", "版本检查暂不可用")
		return
	}
	response.Success(c, http.StatusOK, h.versionCheck.Snapshot())
}

func (h *Handler) checkVersion(c *gin.Context) {
	if h.versionCheck == nil {
		response.Error(c, http.StatusServiceUnavailable, "updateCheckUnavailable", "版本检查暂不可用")
		return
	}
	response.Success(c, http.StatusOK, h.versionCheck.Check(c.Request.Context()))
}
