package registration

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	registrationapp "github.com/owen891/grok2api/backend/internal/application/registration"
	"github.com/owen891/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type controller interface {
	Status() (registrationapp.Status, error)
	Start(context.Context, registrationapp.StartInput) (registrationapp.Status, error)
	Stop(context.Context) (registrationapp.Status, error)
	Logs(int) (registrationapp.LogResult, error)
	Settings() (registrationapp.WorkerSettings, error)
	UpdateSettings(registrationapp.WorkerSettingsPatch) (registrationapp.WorkerSettings, error)
	Preflight(context.Context) registrationapp.PreflightResult
}

type Handler struct{ controller controller }

func NewHandler(controller controller) *Handler { return &Handler{controller: controller} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/registration", h.status)
	router.GET("/registration/logs", h.logs)
	router.GET("/registration/config", h.settings)
	router.PUT("/registration/config", h.updateSettings)
	router.GET("/registration/preflight", h.preflight)
	router.POST("/registration/start", h.start)
	router.POST("/registration/stop", h.stop)
}

func (h *Handler) status(c *gin.Context) {
	value, err := h.controller.Status()
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, value)
}

func (h *Handler) logs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "500"))
	value, err := h.controller.Logs(limit)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, value)
}

func (h *Handler) settings(c *gin.Context) {
	value, err := h.controller.Settings()
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, value)
}

func (h *Handler) updateSettings(c *gin.Context) {
	var request registrationapp.WorkerSettingsPatch
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.controller.UpdateSettings(request)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, value)
}

func (h *Handler) preflight(c *gin.Context) {
	response.Success(c, http.StatusOK, h.controller.Preflight(c.Request.Context()))
}

func (h *Handler) start(c *gin.Context) {
	var request registrationapp.StartInput
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.controller.Start(c.Request.Context(), request)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusAccepted, value)
}

func (h *Handler) stop(c *gin.Context) {
	value, err := h.controller.Stop(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, value)
}

func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, registrationapp.ErrRunning):
		response.Error(c, http.StatusConflict, "registrationRunning", err.Error())
	case errors.Is(err, registrationapp.ErrNotConfigured):
		response.Error(c, http.StatusBadRequest, "registrationNotConfigured", err.Error())
	case errors.Is(err, registrationapp.ErrInvalidInput):
		response.Error(c, http.StatusUnprocessableEntity, "invalidRegistrationInput", err.Error())
	case errors.Is(err, registrationapp.ErrPreflight):
		response.Error(c, http.StatusBadRequest, "registrationPreflightFailed", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "registrationOperationFailed", "注册任务操作失败")
	}
}
