package accountinspection

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	inspectionapp "github.com/owen891/grok2api/backend/internal/application/accountinspection"
	"github.com/owen891/grok2api/backend/internal/domain/account"
	inspectiondomain "github.com/owen891/grok2api/backend/internal/domain/accountinspection"
	"github.com/owen891/grok2api/backend/internal/repository"
	"github.com/owen891/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *inspectionapp.Service }

func NewHandler(service *inspectionapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/account-inspections", h.list)
	router.GET("/account-inspections/latest", h.latest)
	router.POST("/account-inspections", h.start)
	router.GET("/account-inspections/:id", h.get)
	router.POST("/account-inspections/:id/cancel", h.cancel)
}

type startRequest struct {
	Provider        string   `json:"provider"`
	ModelRouteID    string   `json:"modelRouteId"`
	Mode            string   `json:"mode"`
	AccountIDs      []string `json:"accountIds"`
	Classifications []string `json:"classifications"`
	IncludeDisabled bool     `json:"includeDisabled"`
	Concurrency     int      `json:"concurrency"`
}

type runResponse struct {
	ID              string     `json:"id"`
	Provider        string     `json:"provider"`
	ModelRouteID    string     `json:"modelRouteId"`
	UpstreamModel   string     `json:"upstreamModel"`
	Mode            string     `json:"mode"`
	Status          string     `json:"status"`
	IncludeDisabled bool       `json:"includeDisabled"`
	Concurrency     int        `json:"concurrency"`
	Total           int        `json:"total"`
	Completed       int        `json:"completed"`
	CancelRequested bool       `json:"cancelRequested"`
	ErrorMessage    string     `json:"errorMessage,omitempty"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type resultResponse struct {
	AccountID              string     `json:"accountId"`
	Provider               string     `json:"provider"`
	AccountName            string     `json:"accountName"`
	AccountEmail           string     `json:"accountEmail,omitempty"`
	AccountEnabled         bool       `json:"accountEnabled"`
	AccountUpdatedAt       time.Time  `json:"accountUpdatedAt"`
	Model                  string     `json:"model"`
	Classification         string     `json:"classification"`
	SuggestedAction        string     `json:"suggestedAction"`
	Confidence             string     `json:"confidence"`
	FailureScope           string     `json:"failureScope,omitempty"`
	FailureAction          string     `json:"failureAction,omitempty"`
	HTTPStatus             int        `json:"httpStatus"`
	ErrorCode              string     `json:"errorCode,omitempty"`
	ErrorMessage           string     `json:"errorMessage,omitempty"`
	Attempts               int        `json:"attempts"`
	LatencyMilliseconds    int64      `json:"latencyMilliseconds"`
	QuotaExhausted         bool       `json:"quotaExhausted"`
	FreeQuotaExhausted     bool       `json:"freeQuotaExhausted"`
	ModelQuotaExhausted    bool       `json:"modelQuotaExhausted"`
	CredentialRejected     bool       `json:"credentialRejected"`
	PermanentAccountDenial bool       `json:"permanentAccountDenial"`
	ApplyStatus            string     `json:"applyStatus"`
	ApplyAttempts          int        `json:"applyAttempts"`
	ApplyError             string     `json:"applyError,omitempty"`
	AppliedAction          string     `json:"appliedAction,omitempty"`
	AppliedAt              *time.Time `json:"appliedAt,omitempty"`
	UpdatedAt              time.Time  `json:"updatedAt"`
}

func (h *Handler) start(c *gin.Context) {
	var body startRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidInput", "巡检参数无效")
		return
	}
	modelRouteID, err := strconv.ParseUint(body.ModelRouteID, 10, 64)
	if err != nil || modelRouteID == 0 {
		response.Error(c, http.StatusBadRequest, "invalidInput", "巡检模型无效")
		return
	}
	accountIDs, err := parseIDs(body.AccountIDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidInput", "巡检账号无效")
		return
	}
	classifications := make([]inspectiondomain.Classification, 0, len(body.Classifications))
	for _, value := range body.Classifications {
		classifications = append(classifications, inspectiondomain.Classification(value))
	}
	value, err := h.service.Start(c.Request.Context(), inspectionapp.StartInput{
		Provider: account.Provider(body.Provider), ModelRouteID: modelRouteID, Mode: inspectiondomain.RunMode(body.Mode),
		AccountIDs: accountIDs, Classifications: classifications, IncludeDisabled: body.IncludeDisabled, Concurrency: body.Concurrency,
	})
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusAccepted, newRunResponse(value))
}

func (h *Handler) list(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	values, err := h.service.List(c.Request.Context(), account.Provider(c.Query("provider")), limit)
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]runResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newRunResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}

func (h *Handler) latest(c *gin.Context) {
	value, err := h.service.Latest(c.Request.Context(), account.Provider(c.Query("provider")))
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newRunResponse(value))
}

func (h *Handler) get(c *gin.Context) {
	value, err := h.service.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "100"))
	results, total, err := h.service.Results(c.Request.Context(), value.ID, page, pageSize)
	if err != nil {
		h.writeError(c, err)
		return
	}
	summary, err := h.service.ResultSummary(c.Request.Context(), value.ID)
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]resultResponse, 0, len(results))
	for _, result := range results {
		items = append(items, newResultResponse(result))
	}
	summaryResponse := make(map[string]int, len(summary))
	for classification, count := range summary {
		summaryResponse[string(classification)] = count
	}
	response.Success(c, http.StatusOK, gin.H{"run": newRunResponse(value), "items": items, "summary": summaryResponse, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) cancel(c *gin.Context) {
	value, err := h.service.Cancel(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusAccepted, newRunResponse(value))
}

func parseIDs(values []string) ([]uint64, error) {
	result := make([]uint64, 0, len(values))
	for _, raw := range values {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || value == 0 {
			return nil, inspectionapp.ErrInvalidInput
		}
		result = append(result, value)
	}
	return result, nil
}

func newRunResponse(value inspectiondomain.Run) runResponse {
	return runResponse{
		ID: value.ID, Provider: string(value.Provider), ModelRouteID: strconv.FormatUint(value.ModelRouteID, 10), UpstreamModel: value.UpstreamModel,
		Mode: string(value.Mode), Status: string(value.Status), IncludeDisabled: value.IncludeDisabled, Concurrency: value.Concurrency,
		Total: value.Total, Completed: value.Completed, CancelRequested: value.CancelRequested, ErrorMessage: value.ErrorMessage,
		StartedAt: value.StartedAt, FinishedAt: value.FinishedAt, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func newResultResponse(value inspectiondomain.Result) resultResponse {
	return resultResponse{
		AccountID: strconv.FormatUint(value.AccountID, 10), Provider: string(value.Provider), AccountName: value.AccountName,
		AccountEmail: value.AccountEmail, AccountEnabled: value.AccountEnabled, AccountUpdatedAt: value.AccountUpdatedAt, Model: value.Model,
		Classification: string(value.Classification), SuggestedAction: string(value.SuggestedAction), Confidence: string(value.Confidence),
		FailureScope: value.FailureScope, FailureAction: value.FailureAction, HTTPStatus: value.HTTPStatus,
		ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage, Attempts: value.Attempts, LatencyMilliseconds: value.Latency.Milliseconds(),
		QuotaExhausted: value.QuotaExhausted, FreeQuotaExhausted: value.FreeQuotaExhausted, ModelQuotaExhausted: value.ModelQuotaExhausted,
		CredentialRejected: value.CredentialRejected, PermanentAccountDenial: value.PermanentAccountDenial,
		ApplyStatus: string(value.ApplyStatus), ApplyAttempts: value.ApplyAttempts, ApplyError: value.ApplyError,
		AppliedAction: value.AppliedAction, AppliedAt: value.AppliedAt, UpdatedAt: value.UpdatedAt,
	}
}

func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, inspectionapp.ErrInvalidInput):
		var validation *inspectionapp.InvalidInputError
		if errors.As(err, &validation) {
			switch validation.Reason {
			case "model_route_has_no_supported_accounts", "model_route_is_not_an_enabled_chat_route_for_provider", "provider_does_not_support_responses":
				response.Error(c, http.StatusConflict, "accountInspectionModelUnavailable", "所选探测模型当前没有可用账号，请选择可用的文本模型或先同步账号")
				return
			}
		}
		response.Error(c, http.StatusBadRequest, "accountInspectionInvalidInput", "巡检范围或参数无效，请重新选择")
	case errors.Is(err, inspectionapp.ErrNoTargets):
		response.Error(c, http.StatusBadRequest, "accountInspectionNoTargets", err.Error())
	case errors.Is(err, inspectionapp.ErrConflict), errors.Is(err, repository.ErrConflict):
		response.Error(c, http.StatusConflict, "accountInspectionConflict", err.Error())
	case errors.Is(err, repository.ErrNotFound):
		response.Error(c, http.StatusNotFound, "accountInspectionNotFound", "巡检任务不存在")
	default:
		response.Error(c, http.StatusInternalServerError, "accountInspectionFailed", err.Error())
	}
}
