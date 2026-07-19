package operations

import (
	"net/http"

	operationsapp "github.com/chenyme/grok2api/backend/internal/application/operations"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *operationsapp.Service }

func NewHandler(service *operationsapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) { router.GET("/operations", h.get) }

func (h *Handler) get(c *gin.Context) {
	result, err := h.service.Snapshot(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "operationsLoadFailed", "读取运行状态失败")
		return
	}
	response.Success(c, http.StatusOK, result)
}
