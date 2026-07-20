package operations

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	modeldomain "github.com/owen891/grok2api/backend/internal/domain/model"
	registrationdomain "github.com/owen891/grok2api/backend/internal/domain/registration"
	"github.com/owen891/grok2api/backend/internal/observability"
	"github.com/owen891/grok2api/backend/internal/repository"
)

const defaultRecoveryLead = 10 * time.Minute

var (
	ErrReplenishmentDisabled    = errors.New("automatic replenishment is disabled")
	ErrReplenishmentUnavailable = errors.New("automatic replenishment trigger is unavailable")
)

type capacitySource interface {
	CapacitySnapshot(context.Context, account.Provider, string, string, time.Duration) (account.RoutingCapacity, error)
}

type quotaModeSource interface {
	QuotaMode(account.Provider, string) string
}

type modelSource interface {
	ListConfiguredEnabled(context.Context) ([]modeldomain.Route, error)
}

type replenishmentTrigger interface {
	Trigger(context.Context) error
}

type ReplenishmentConfig struct {
	Enabled               bool
	DryRun                bool
	Scope                 string
	MaxDailyRegistrations int
	Predictive            bool
	TargetEligible        int
	MinDemandRPM          float64
	DemandWindow          time.Duration
	VerificationGrace     time.Duration
}

type RouteCapacity struct {
	RouteID          uint64           `json:"routeId,string"`
	PublicModel      string           `json:"publicModel"`
	Provider         account.Provider `json:"provider"`
	UpstreamModel    string           `json:"upstreamModel"`
	Capability       string           `json:"capability"`
	QuotaMode        string           `json:"quotaMode,omitempty"`
	Total            int              `json:"total"`
	Eligible         int              `json:"eligible"`
	Saturated        int              `json:"saturated"`
	Disabled         int              `json:"disabled"`
	ReauthRequired   int              `json:"reauthRequired"`
	QuotaExhausted   int              `json:"quotaExhausted"`
	RecoveringSoon   int              `json:"recoveringSoon"`
	Cooling          int              `json:"cooling"`
	ModelCooling     int              `json:"modelCooling"`
	Unsupported      int              `json:"unsupported"`
	InFlight         int              `json:"inFlight"`
	TotalSlots       int              `json:"totalSlots"`
	AvailableSlots   int              `json:"availableSlots"`
	Unlimited        int              `json:"unlimited"`
	EarliestRecovery *time.Time       `json:"earliestRecovery,omitempty"`
}

type TaskStatus struct {
	Name                string     `json:"name"`
	State               string     `json:"state"`
	StartedAt           *time.Time `json:"startedAt,omitempty"`
	LastRunAt           *time.Time `json:"lastRunAt,omitempty"`
	LastHeartbeatAt     *time.Time `json:"lastHeartbeatAt,omitempty"`
	LastSuccessAt       *time.Time `json:"lastSuccessAt,omitempty"`
	LastFailureAt       *time.Time `json:"lastFailureAt,omitempty"`
	NextRunAt           *time.Time `json:"nextRunAt,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	RestartCount        int        `json:"restartCount"`
	LastError           string     `json:"lastError,omitempty"`
}

type ReplenishmentStatus struct {
	Enabled                  bool                                   `json:"enabled"`
	DryRun                   bool                                   `json:"dryRun"`
	Scope                    string                                 `json:"scope"`
	MaxDailyRegistrations    int                                    `json:"maxDailyRegistrations"`
	Predictive               bool                                   `json:"predictive"`
	TargetEligible           int                                    `json:"targetEligible"`
	MinDemandRPM             float64                                `json:"minDemandRPM"`
	DemandWindowSeconds      int64                                  `json:"demandWindowSeconds"`
	VerificationGraceSeconds int64                                  `json:"verificationGraceSeconds"`
	State                    registrationdomain.ReplenishmentStatus `json:"state"`
	LastTriggerAt            *time.Time                             `json:"lastTriggerAt,omitempty"`
	NextAttemptAt            *time.Time                             `json:"nextAttemptAt,omitempty"`
	DailyStarts              int                                    `json:"dailyStarts"`
	LastError                string                                 `json:"lastError,omitempty"`
	UpdatedAt                *time.Time                             `json:"updatedAt,omitempty"`
}

type Snapshot struct {
	GeneratedAt   time.Time           `json:"generatedAt"`
	Routes        []RouteCapacity     `json:"routes"`
	Tasks         []TaskStatus        `json:"tasks"`
	Replenishment ReplenishmentStatus `json:"replenishment"`
}

type Service struct {
	models        modelSource
	capacity      capacitySource
	quotaModes    quotaModeSource
	replenishment repository.ReplenishmentRepository
	replenishCfg  ReplenishmentConfig
	trigger       replenishmentTrigger

	mu    sync.RWMutex
	tasks map[string]TaskStatus
	now   func() time.Time
}

func NewService(models modelSource, capacity capacitySource, quotaModes quotaModeSource, replenishment repository.ReplenishmentRepository, replenishCfg ReplenishmentConfig) *Service {
	return &Service{
		models: models, capacity: capacity, quotaModes: quotaModes, replenishment: replenishment,
		replenishCfg: replenishCfg, tasks: make(map[string]TaskStatus), now: func() time.Time { return time.Now().UTC() },
	}
}

// SetReplenishmentTrigger connects the bounded manual wake-up command without
// giving the operations API direct access to registration internals.
func (s *Service) SetReplenishmentTrigger(trigger replenishmentTrigger) {
	s.mu.Lock()
	s.trigger = trigger
	s.mu.Unlock()
}

// TriggerReplenishment schedules an immediate policy evaluation. It does not
// bypass configured capacity thresholds, cooldowns, or daily limits.
func (s *Service) TriggerReplenishment(ctx context.Context) error {
	if !s.replenishCfg.Enabled {
		return ErrReplenishmentDisabled
	}
	s.mu.RLock()
	trigger := s.trigger
	s.mu.RUnlock()
	if trigger == nil {
		return ErrReplenishmentUnavailable
	}
	return trigger.Trigger(ctx)
}

func (s *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	routes, err := s.models.ListConfiguredEnabled(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	observability.ResetRouteCapacity()
	result := Snapshot{GeneratedAt: s.now(), Routes: make([]RouteCapacity, 0, len(routes)), Tasks: s.TaskSnapshot()}
	for _, route := range routes {
		quotaMode := ""
		if s.quotaModes != nil {
			quotaMode = s.quotaModes.QuotaMode(route.Provider, route.UpstreamModel)
		}
		capacity, err := s.capacity.CapacitySnapshot(ctx, route.Provider, route.UpstreamModel, quotaMode, defaultRecoveryLead)
		if err != nil {
			return Snapshot{}, err
		}
		result.Routes = append(result.Routes, newRouteCapacity(route, quotaMode, capacity))
	}
	result.Replenishment = s.replenishmentSnapshot(ctx)
	return result, nil
}

func newRouteCapacity(route modeldomain.Route, quotaMode string, value account.RoutingCapacity) RouteCapacity {
	return RouteCapacity{
		RouteID: route.ID, PublicModel: modeldomain.ExternalPublicID(route.Provider, route.PublicID), Provider: route.Provider,
		UpstreamModel: route.UpstreamModel, Capability: string(route.Capability), QuotaMode: quotaMode,
		Total: value.Total, Eligible: value.Eligible, Saturated: value.Saturated, Disabled: value.Disabled,
		ReauthRequired: value.ReauthRequired, QuotaExhausted: value.QuotaExhausted, RecoveringSoon: value.RecoveringSoon,
		Cooling: value.Cooling, ModelCooling: value.ModelCooling, Unsupported: value.Unsupported, InFlight: value.InFlight,
		TotalSlots: value.TotalSlots, AvailableSlots: value.AvailableSlots, Unlimited: value.Unlimited,
		EarliestRecovery: value.EarliestRecovery,
	}
}

func (s *Service) replenishmentSnapshot(ctx context.Context) ReplenishmentStatus {
	result := ReplenishmentStatus{
		Enabled: s.replenishCfg.Enabled, DryRun: s.replenishCfg.DryRun, Scope: s.replenishCfg.Scope,
		MaxDailyRegistrations: s.replenishCfg.MaxDailyRegistrations, Predictive: s.replenishCfg.Predictive,
		TargetEligible: s.replenishCfg.TargetEligible, MinDemandRPM: s.replenishCfg.MinDemandRPM,
		DemandWindowSeconds: int64(s.replenishCfg.DemandWindow.Seconds()), VerificationGraceSeconds: int64(s.replenishCfg.VerificationGrace.Seconds()),
		State: registrationdomain.ReplenishmentIdle,
	}
	if s.replenishment == nil || strings.TrimSpace(s.replenishCfg.Scope) == "" {
		observability.SetReplenishmentState(result.Scope, string(result.State))
		return result
	}
	state, err := s.replenishment.GetReplenishmentState(ctx, s.replenishCfg.Scope)
	if errors.Is(err, repository.ErrNotFound) {
		observability.SetReplenishmentState(result.Scope, string(result.State))
		return result
	}
	if err != nil {
		result.State = registrationdomain.ReplenishmentFailed
		result.LastError = "读取自动补号状态失败"
		observability.SetReplenishmentState(result.Scope, string(result.State))
		return result
	}
	updatedAt := state.UpdatedAt
	result.State, result.LastTriggerAt, result.NextAttemptAt = state.Status, state.LastTriggerAt, state.NextAttemptAt
	result.DailyStarts, result.LastError, result.UpdatedAt = state.DailyStarts, state.LastError, &updatedAt
	observability.SetReplenishmentState(result.Scope, string(result.State))
	return result
}

func (s *Service) TaskStarted(name string) {
	now := s.now()
	s.updateTask(name, func(value *TaskStatus) {
		value.State, value.StartedAt, value.LastRunAt, value.LastHeartbeatAt = "running", &now, &now, &now
	})
	observability.SetBackgroundTaskState(name, "running")
}

func (s *Service) TaskHeartbeat(name string) {
	now := s.now()
	s.updateTask(name, func(value *TaskStatus) {
		value.LastHeartbeatAt = &now
	})
}

func (s *Service) TaskSucceeded(name string, nextRunAt *time.Time) {
	now := s.now()
	s.updateTask(name, func(value *TaskStatus) {
		value.State, value.LastSuccessAt, value.LastHeartbeatAt, value.NextRunAt = "running", &now, &now, nextRunAt
		value.ConsecutiveFailures, value.LastError = 0, ""
	})
	observability.SetBackgroundTaskState(name, "running")
}

func (s *Service) TaskFailed(name string, err error, restarting bool) {
	now := s.now()
	s.updateTask(name, func(value *TaskStatus) {
		value.State, value.LastFailureAt, value.LastHeartbeatAt = "degraded", &now, &now
		value.ConsecutiveFailures++
		if restarting {
			value.RestartCount++
		}
		value.LastError = boundedError(err)
	})
	observability.SetBackgroundTaskState(name, "degraded")
	observability.ObserveBackgroundTaskFailure(name, restarting)
}

func (s *Service) TaskScheduled(name string, nextRunAt time.Time) {
	s.updateTask(name, func(value *TaskStatus) {
		value.NextRunAt = &nextRunAt
		if value.State == "" {
			value.State = "running"
		}
	})
	observability.SetBackgroundTaskState(name, "running")
}

func (s *Service) TaskStopped(name string) {
	now := s.now()
	s.updateTask(name, func(value *TaskStatus) {
		value.State, value.LastHeartbeatAt, value.NextRunAt = "stopped", &now, nil
	})
	observability.SetBackgroundTaskState(name, "stopped")
}

func (s *Service) TaskSnapshot() []TaskStatus {
	s.mu.RLock()
	result := make([]TaskStatus, 0, len(s.tasks))
	for _, value := range s.tasks {
		result = append(result, value)
	}
	s.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (s *Service) updateTask(name string, update func(*TaskStatus)) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	s.mu.Lock()
	value := s.tasks[name]
	value.Name = name
	update(&value)
	s.tasks[name] = value
	s.mu.Unlock()
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(err.Error())
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}
