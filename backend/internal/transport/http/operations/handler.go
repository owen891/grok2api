package operations

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	operationsapp "github.com/owen891/grok2api/backend/internal/application/operations"
	"github.com/owen891/grok2api/backend/internal/shared/response"
)

type Handler struct{ service *operationsapp.Service }

func NewHandler(service *operationsapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/operations", h.get)
	router.POST("/operations/replenishment/trigger", h.triggerReplenishment)
}

func (h *Handler) get(c *gin.Context) {
	result, err := h.service.Snapshot(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "operationsLoadFailed", "读取运行状态失败")
		return
	}
	response.Success(c, http.StatusOK, result)
}

func (h *Handler) triggerReplenishment(c *gin.Context) {
	if err := h.service.TriggerReplenishment(c.Request.Context()); err != nil {
		switch {
		case errors.Is(err, operationsapp.ErrReplenishmentDisabled):
			response.Error(c, http.StatusConflict, "replenishmentDisabled", err.Error())
		case errors.Is(err, operationsapp.ErrReplenishmentUnavailable):
			response.Error(c, http.StatusServiceUnavailable, "replenishmentUnavailable", err.Error())
		default:
			response.Error(c, http.StatusInternalServerError, "replenishmentTriggerFailed", "触发自动补号失败")
		}
		return
	}
	response.Success(c, http.StatusAccepted, gin.H{"triggered": true})
}
