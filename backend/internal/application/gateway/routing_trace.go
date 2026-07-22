package gateway

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/domain/audit"
	"github.com/owen891/grok2api/backend/internal/observability"
)

const maxRoutingTraceEvents = 32

type routingTraceContextKey struct{}

type RoutingTrace struct {
	mu        sync.Mutex
	Version   int                 `json:"version"`
	RouteID   string              `json:"routeId"`
	Provider  account.Provider    `json:"provider"`
	Model     string              `json:"model"`
	QuotaMode string              `json:"quotaMode,omitempty"`
	StartedAt time.Time           `json:"startedAt"`
	Events    []RoutingTraceEvent `json:"events"`
}

type routingTracePayload struct {
	Version   int                 `json:"version"`
	RouteID   string              `json:"routeId"`
	Provider  account.Provider    `json:"provider"`
	Model     string              `json:"model"`
	QuotaMode string              `json:"quotaMode,omitempty"`
	StartedAt time.Time           `json:"startedAt"`
	Events    []RoutingTraceEvent `json:"events"`
}

type RoutingTraceEvent struct {
	Type              string `json:"type"`
	ElapsedMS         int64  `json:"elapsedMs"`
	Attempt           int    `json:"attempt,omitempty"`
	AccountID         string `json:"accountId,omitempty"`
	Selection         string `json:"selection,omitempty"`
	Total             int    `json:"total,omitempty"`
	Excluded          int    `json:"excluded,omitempty"`
	Eligible          int    `json:"eligible,omitempty"`
	Probe             int    `json:"probe,omitempty"`
	Disabled          int    `json:"disabled,omitempty"`
	ReauthRequired    int    `json:"reauthRequired,omitempty"`
	InferenceDenied   int    `json:"inferenceDenied,omitempty"`
	Cooling           int    `json:"cooling,omitempty"`
	ModelCooling      int    `json:"modelCooling,omitempty"`
	QuotaExhausted    int    `json:"quotaExhausted,omitempty"`
	Unsupported       int    `json:"unsupported,omitempty"`
	Reason            string `json:"reason,omitempty"`
	Stage             string `json:"stage,omitempty"`
	StatusCode        int    `json:"statusCode,omitempty"`
	ErrorCode         string `json:"errorCode,omitempty"`
	Action            string `json:"action,omitempty"`
	Scope             string `json:"scope,omitempty"`
	DurationMS        int64  `json:"durationMs,omitempty"`
	AccountScoped     bool   `json:"accountScoped,omitempty"`
	QuotaStateChanged bool   `json:"quotaStateChanged,omitempty"`
}

func newRoutingTrace(routeID uint64, provider account.Provider, model, quotaMode string) *RoutingTrace {
	return &RoutingTrace{
		Version: 1, RouteID: strconv.FormatUint(routeID, 10), Provider: provider,
		Model: strings.TrimSpace(model), QuotaMode: strings.TrimSpace(quotaMode), StartedAt: time.Now().UTC(),
		Events: make([]RoutingTraceEvent, 0, 8),
	}
}

func routingTraceFromJSON(value string) *RoutingTrace {
	if value == "" || len(value) > 16_384 {
		return nil
	}
	var payload routingTracePayload
	if json.Unmarshal([]byte(value), &payload) != nil || payload.Version != 1 || payload.RouteID == "" || len(payload.Events) > maxRoutingTraceEvents {
		return nil
	}
	if payload.StartedAt.IsZero() {
		payload.StartedAt = time.Now().UTC()
	}
	return &RoutingTrace{
		Version: payload.Version, RouteID: payload.RouteID, Provider: payload.Provider, Model: payload.Model,
		QuotaMode: payload.QuotaMode, StartedAt: payload.StartedAt, Events: payload.Events,
	}
}

func withRoutingTrace(ctx context.Context, trace *RoutingTrace) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, routingTraceContextKey{}, trace)
}

func routingTraceFromContext(ctx context.Context) *RoutingTrace {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Value(routingTraceContextKey{}).(*RoutingTrace)
	return value
}

func (t *RoutingTrace) record(value RoutingTraceEvent) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.Events) >= maxRoutingTraceEvents {
		return
	}
	value.ElapsedMS = max(int64(0), time.Since(t.StartedAt).Milliseconds())
	value.Reason = truncateTraceText(value.Reason, 160)
	value.ErrorCode = truncateTraceText(value.ErrorCode, 100)
	value.Action = truncateTraceText(value.Action, 100)
	t.Events = append(t.Events, value)
}

func (t *RoutingTrace) JSON() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	payload := routingTracePayload{
		Version: t.Version, RouteID: t.RouteID, Provider: t.Provider, Model: t.Model,
		QuotaMode: t.QuotaMode, StartedAt: t.StartedAt, Events: append([]RoutingTraceEvent(nil), t.Events...),
	}
	t.mu.Unlock()
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > 16_384 {
		return ""
	}
	return string(encoded)
}

func recordRoutingPool(ctx context.Context, value RoutingTraceEvent) {
	if trace := routingTraceFromContext(ctx); trace != nil {
		value.Type = "candidate_pool"
		trace.record(value)
		observability.ObserveRoutingPool(string(trace.Provider), trace.Model, map[string]int{
			"total": value.Total, "eligible": value.Eligible, "probe": value.Probe, "disabled": value.Disabled,
			"reauth_required": value.ReauthRequired, "inference_denied": value.InferenceDenied, "cooling": value.Cooling,
			"model_cooling": value.ModelCooling, "quota_exhausted": value.QuotaExhausted, "unsupported": value.Unsupported,
		})
	}
}

func recordRoutingSelection(ctx context.Context, accountID uint64, selection string) {
	if trace := routingTraceFromContext(ctx); trace != nil {
		trace.record(RoutingTraceEvent{Type: "selected", AccountID: strconv.FormatUint(accountID, 10), Selection: selection})
		observability.ObserveRoutingSelection(string(trace.Provider), trace.Model, selection)
		if shadowAccountID, ok := trace.shadowAccountID(); ok {
			result := "mismatch"
			if shadowAccountID == accountID {
				result = "match"
			}
			observability.ObserveShadowSelection(string(trace.Provider), trace.Model, result)
		}
	}
}

func recordRoutingShadowSelection(ctx context.Context, accountID uint64) {
	if trace := routingTraceFromContext(ctx); trace != nil {
		trace.record(RoutingTraceEvent{Type: "shadow_selection", AccountID: strconv.FormatUint(accountID, 10), Selection: "performance_score"})
		observability.ObserveShadowSelection(string(trace.Provider), trace.Model, "recommended")
	}
}

func recordRoutingFailure(ctx context.Context, reason SelectionUnavailableReason) {
	if trace := routingTraceFromContext(ctx); trace != nil {
		trace.record(RoutingTraceEvent{Type: "selection_failed", Reason: string(reason)})
		observability.ObserveRoutingFailure(string(trace.Provider), trace.Model, "selection", string(reason))
	}
}

func (t *RoutingTrace) shadowAccountID() (uint64, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for index := len(t.Events) - 1; index >= 0; index-- {
		if t.Events[index].Type != "shadow_selection" {
			continue
		}
		value, err := strconv.ParseUint(t.Events[index].AccountID, 10, 64)
		return value, err == nil && value > 0
	}
	return 0, false
}

func (t *RoutingTrace) recordAttempt(attempt int, accountID uint64, stage string, status int, errorCode, action string, scope FailureScope, duration time.Duration, accountScoped, quotaStateChanged bool) {
	if t == nil {
		return
	}
	t.record(RoutingTraceEvent{
		Type: "attempt", Attempt: attempt, AccountID: strconv.FormatUint(accountID, 10), Stage: stage,
		StatusCode: status, ErrorCode: errorCode, Action: action, DurationMS: max(int64(0), duration.Milliseconds()),
		Scope: string(scope), AccountScoped: accountScoped, QuotaStateChanged: quotaStateChanged,
	})
	if errorCode != "" {
		observability.ObserveRoutingFailure(string(t.Provider), t.Model, string(scope), action)
	}
}

func applyRoutingTrace(record *audit.Record, trace *RoutingTrace) {
	if record != nil && trace != nil {
		record.RoutingTraceJSON = trace.JSON()
	}
}

func truncateTraceText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
