package egressgroup

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	app "github.com/owen891/grok2api/backend/internal/application/egressgroup"
	domain "github.com/owen891/grok2api/backend/internal/domain/egress"
	"github.com/owen891/grok2api/backend/internal/repository"
	"github.com/owen891/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *app.Service }

func NewHandler(service *app.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/egress-groups", h.list)
	router.POST("/egress-groups", h.create)
	router.PUT("/egress-groups/:id", h.update)
	router.DELETE("/egress-groups/:id", h.delete)
	router.GET("/egress-groups/:id/members", h.members)
	router.POST("/egress-groups/:id/members", h.upsertMember)
	router.DELETE("/egress-groups/:id/members/:nodeId", h.deleteMember)
	router.POST("/egress-groups/:id/import", h.importNodes)
}

type groupRequest struct {
	Name            string  `json:"name"`
	Scope           string  `json:"scope"`
	Enabled         bool    `json:"enabled"`
	Strategy        string  `json:"strategy"`
	MaxConcurrency  int     `json:"maxConcurrency"`
	FallbackGroupID *uint64 `json:"fallbackGroupId,string"`
}

type memberRequest struct {
	NodeID         uint64 `json:"nodeId,string"`
	Weight         int    `json:"weight"`
	MaxConcurrency int    `json:"maxConcurrency"`
	Enabled        bool   `json:"enabled"`
	Priority       int    `json:"priority"`
}

type importRequest struct {
	Lines    []app.ImportLine `json:"lines"`
	Content  string           `json:"content"`
	DryRun   bool             `json:"dryRun"`
	Defaults memberRequest    `json:"defaults"`
}

func (h *Handler) list(c *gin.Context) {
	values, err := h.service.List(c.Request.Context(), domain.Scope(c.Query("scope")))
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "egressGroupListFailed", "读取代理组失败")
		return
	}
	items := make([]gin.H, 0, len(values))
	for _, value := range values {
		members, memberErr := h.service.Members(c.Request.Context(), value.ID)
		if memberErr != nil {
			response.Error(c, http.StatusInternalServerError, "egressGroupListFailed", "读取代理组成员失败")
			return
		}
		enabled := 0
		for _, member := range members {
			if member.Enabled {
				enabled++
			}
		}
		item := groupResponse(value)
		item["memberCount"], item["enabledMembers"] = len(members), enabled
		items = append(items, item)
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}

func (h *Handler) create(c *gin.Context) {
	var request groupRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Create(c.Request.Context(), request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, groupResponse(value))
}

func (h *Handler) update(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var request groupRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Update(c.Request.Context(), id, request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, groupResponse(value))
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) members(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	values, err := h.service.Members(c.Request.Context(), id)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "egressGroupMembersFailed", "读取代理组成员失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"items": values})
}

func (h *Handler) upsertMember(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var request memberRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.UpsertMember(c.Request.Context(), id, app.MemberInput{NodeID: request.NodeID, Weight: request.Weight, MaxConcurrency: request.MaxConcurrency, Enabled: request.Enabled, Priority: request.Priority})
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, value)
}

func (h *Handler) deleteMember(c *gin.Context) {
	groupID, ok := pathID(c, "id")
	if !ok {
		return
	}
	nodeID, ok := pathID(c, "nodeId")
	if !ok {
		return
	}
	if err := h.service.DeleteMember(c.Request.Context(), groupID, nodeID); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) importNodes(c *gin.Context) {
	groupID, ok := pathID(c, "id")
	if !ok {
		return
	}
	var request importRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	lines := request.Lines
	if len(lines) == 0 {
		for index, value := range splitLines(request.Content) {
			lines = append(lines, app.ImportLine{Line: index + 1, Value: value})
		}
	}
	results, err := h.service.Import(c.Request.Context(), groupID, lines, request.DryRun, app.MemberInput{Weight: request.Defaults.Weight, MaxConcurrency: request.Defaults.MaxConcurrency, Enabled: request.Defaults.Enabled, Priority: request.Defaults.Priority})
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"items": results})
}

func (r groupRequest) input() app.Input {
	return app.Input{Name: r.Name, Scope: domain.Scope(r.Scope), Enabled: r.Enabled, Strategy: domain.GroupStrategy(r.Strategy), MaxConcurrency: r.MaxConcurrency, FallbackGroupID: r.FallbackGroupID}
}
func groupResponse(value domain.Group) gin.H {
	result := gin.H{"id": strconv.FormatUint(value.ID, 10), "name": value.Name, "scope": value.Scope, "enabled": value.Enabled, "strategy": value.Strategy, "maxConcurrency": value.MaxConcurrency, "memberCount": 0, "enabledMembers": 0, "createdAt": value.CreatedAt, "updatedAt": value.UpdatedAt}
	if value.FallbackGroupID != nil {
		result["fallbackGroupId"] = strconv.FormatUint(*value.FallbackGroupID, 10)
	}
	return result
}
func splitLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if value := strings.TrimSpace(line); value != "" {
			lines = append(lines, value)
		}
	}
	return lines
}
func pathID(c *gin.Context, name string) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return 0, false
	}
	return id, true
}
func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, app.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidEgressGroup", err.Error())
	case errors.Is(err, app.ErrNotFound), errors.Is(err, repository.ErrNotFound):
		response.Error(c, http.StatusNotFound, "egressGroupNotFound", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressGroupOperationFailed", "代理组操作失败")
	}
}
