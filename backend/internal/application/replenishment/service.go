package replenishment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	registrationapp "github.com/owen891/grok2api/backend/internal/application/registration"
	accountdomain "github.com/owen891/grok2api/backend/internal/domain/account"
	registrationdomain "github.com/owen891/grok2api/backend/internal/domain/registration"
	"github.com/owen891/grok2api/backend/internal/observability"
	"github.com/owen891/grok2api/backend/internal/repository"
)

const (
	replenishmentLease   = 2 * time.Minute
	failureRetryInterval = 10 * time.Minute
	registrationTimeout  = 2 * time.Minute
	defaultDemandWindow  = 15 * time.Minute
	defaultVerifyGrace   = 2 * time.Minute
)

type Config struct {
	Enabled               bool
	DryRun                bool
	Provider              accountdomain.Provider
	Model                 string
	QuotaMode             string
	RegisterCount         int
	Cooldown              time.Duration
	RecoveryLeadTime      time.Duration
	MaxDailyRegistrations int
	Predictive            bool
	TargetEligible        int
	MinDemandRPM          float64
	DemandWindow          time.Duration
	VerificationGrace     time.Duration
}

type capacitySource interface {
	CapacitySnapshot(context.Context, accountdomain.Provider, string, string, time.Duration) (accountdomain.RoutingCapacity, error)
}

type registrationStarter interface {
	Status() (registrationapp.Status, error)
	Preflight(context.Context) registrationapp.PreflightResult
	Start(context.Context, registrationapp.StartInput) (registrationapp.Status, error)
}

type demandSource interface {
	CountRequestsSince(ctx context.Context, provider, upstreamModel string, since time.Time) (int64, error)
}

type Service struct {
	capacity        capacitySource
	states          repository.ReplenishmentRepository
	starter         registrationStarter
	demand          demandSource
	config          Config
	logger          *slog.Logger
	trigger         chan struct{}
	now             func() time.Time
	ownerMu         sync.Mutex
	ownedClaimToken string
}

func NewService(capacity capacitySource, states repository.ReplenishmentRepository, starter registrationStarter, config Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if config.RegisterCount <= 0 {
		config.RegisterCount = 1
	}
	if config.Cooldown <= 0 {
		config.Cooldown = 30 * time.Minute
	}
	if config.RecoveryLeadTime < 0 {
		config.RecoveryLeadTime = 0
	}
	if config.MaxDailyRegistrations <= 0 {
		config.MaxDailyRegistrations = 3
	}
	if config.DemandWindow <= 0 {
		config.DemandWindow = defaultDemandWindow
	}
	if config.VerificationGrace <= 0 {
		config.VerificationGrace = defaultVerifyGrace
	}
	return &Service{
		capacity: capacity, states: states, starter: starter, config: config, logger: logger,
		trigger: make(chan struct{}, 1), now: func() time.Time { return time.Now().UTC() },
	}
}

// SetDemandSource enables predictive low-watermark replenishment from persisted request demand.
func (s *Service) SetDemandSource(source demandSource) { s.demand = source }

// Request is non-blocking; registration is deliberately outside the request path.
func (s *Service) Request(_ context.Context, provider accountdomain.Provider, model, quotaMode string) {
	if !s.config.Enabled || provider != s.config.Provider || model != s.config.Model || quotaMode != s.config.QuotaMode {
		return
	}
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("registration_replenisher_started", "enabled", s.config.Enabled, "dry_run", s.config.DryRun,
		"scope", s.scope(), "cooldown", s.config.Cooldown, "daily_limit", s.config.MaxDailyRegistrations,
		"predictive", s.config.Predictive, "target_eligible", s.config.TargetEligible,
		"min_demand_rpm", s.config.MinDemandRPM, "demand_window", s.config.DemandWindow)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.trigger:
			s.ensure(ctx)
		case <-ticker.C:
			s.ensure(ctx)
		}
	}
}

func (s *Service) ensure(ctx context.Context) {
	if !s.config.Enabled || s.capacity == nil || s.states == nil || s.starter == nil {
		return
	}
	status, err := s.starter.Status()
	if err != nil {
		s.logger.Warn("registration_replenish_status_failed", "error", err)
		return
	}
	now := s.now().UTC()
	if s.reconcileLifecycle(ctx, status, now) {
		return
	}
	snapshot, err := s.capacity.CapacitySnapshot(ctx, s.config.Provider, s.config.Model, s.config.QuotaMode, s.config.RecoveryLeadTime)
	if err != nil {
		s.logger.Warn("registration_replenish_capacity_failed", "error", err)
		observability.ObserveReplenishmentFailure(s.scope(), "capacity_read")
		return
	}
	triggered, triggerReason, demandRPM := s.shouldReplenish(ctx, snapshot, now)
	s.logger.Debug("registration_replenish_evaluated",
		"eligible", snapshot.Eligible, "exhausted", snapshot.QuotaExhausted,
		"recovering_soon", snapshot.RecoveringSoon, "cooling", snapshot.Cooling,
		"model_cooling", snapshot.ModelCooling, "unsupported", snapshot.Unsupported,
		"triggered", triggered, "trigger_reason", triggerReason, "demand_rpm", demandRPM)
	if !triggered {
		return
	}
	operationCtx, cancel := context.WithTimeout(ctx, registrationTimeout)
	defer cancel()
	preflight := s.starter.Preflight(operationCtx)
	if !preflight.OK {
		s.logger.Warn("registration_replenish_preflight_failed", "failures", preflightFailures(preflight))
		observability.ObserveReplenishmentFailure(s.scope(), "preflight")
		return
	}
	claimToken, err := newClaimToken()
	if err != nil {
		s.logger.Warn("registration_replenish_token_failed", "error", err)
		return
	}
	nextAt := now.Add(s.config.Cooldown)
	state, claimed, err := s.states.ClaimReplenishment(ctx, repository.ReplenishmentClaim{
		Scope: s.scope(), ClaimToken: claimToken, Now: now, LeaseUntil: now.Add(replenishmentLease), NextAt: nextAt,
		DailyLimit: s.config.MaxDailyRegistrations, BaselineEligible: snapshot.Eligible,
	})
	if err != nil {
		s.logger.Warn("registration_replenish_claim_failed", "error", err)
		return
	}
	if !claimed {
		s.logger.Debug("registration_replenish_skipped", "status", state.Status, "daily_starts", state.DailyStarts, "next_attempt_at", state.NextAttemptAt)
		return
	}
	s.setOwnedClaim(state.ClaimToken)
	s.logger.Warn("registration_replenish_claimed",
		"eligible", snapshot.Eligible, "exhausted", snapshot.QuotaExhausted,
		"count", s.config.RegisterCount, "daily_starts", state.DailyStarts, "dry_run", s.config.DryRun,
		"trigger_reason", triggerReason, "demand_rpm", demandRPM)
	observability.ObserveReplenishmentTrigger(s.scope(), triggerReason, s.config.DryRun)
	observability.SetReplenishmentState(s.scope(), string(registrationdomain.ReplenishmentStarting))
	if s.config.DryRun {
		s.finish(ctx, state.ClaimToken, registrationdomain.ReplenishmentCooling, nextAt, "", true, now)
		return
	}
	if _, err := s.starter.Start(operationCtx, registrationapp.StartInput{Count: s.config.RegisterCount, Threads: 1, AccountType: "web"}); err != nil {
		retryAt := now.Add(min(failureRetryInterval, s.config.Cooldown))
		s.finish(ctx, state.ClaimToken, registrationdomain.ReplenishmentFailed, retryAt, err.Error(), true, now)
		observability.ObserveReplenishmentFailure(s.scope(), "start")
		s.logger.Warn("registration_replenish_start_failed", "error", err, "retry_at", retryAt)
		return
	}
	s.finish(ctx, state.ClaimToken, registrationdomain.ReplenishmentRunning, nextAt, "", false, now)
	s.logger.Warn("registration_replenish_started", "count", s.config.RegisterCount, "next_attempt_at", nextAt)
}

// reconcileLifecycle returns true while an existing replenishment owns this evaluation cycle.
func (s *Service) reconcileLifecycle(ctx context.Context, worker registrationapp.Status, now time.Time) bool {
	state, err := s.states.GetReplenishmentState(ctx, s.scope())
	if errors.Is(err, repository.ErrNotFound) {
		return worker.Running
	}
	if err != nil {
		s.logger.Warn("registration_replenish_state_read_failed", "error", err)
		return true
	}
	if state.ClaimToken == "" {
		observability.SetReplenishmentState(s.scope(), string(state.Status))
		return worker.Running
	}
	observability.SetReplenishmentState(s.scope(), string(state.Status))
	if !s.ownsClaim(state.ClaimToken) {
		if state.LeaseUntil != nil && state.LeaseUntil.After(now) {
			return true
		}
		retryAt := now.Add(min(failureRetryInterval, s.config.Cooldown))
		updated, expireErr := s.states.ExpireReplenishment(ctx, s.scope(), state.ClaimToken, now, retryAt, "replenishment owner lease expired")
		if expireErr != nil {
			s.logger.Warn("registration_replenish_owner_expire_failed", "error", expireErr)
		} else if updated {
			observability.SetReplenishmentState(s.scope(), string(registrationdomain.ReplenishmentFailed))
			observability.ObserveReplenishmentFailure(s.scope(), "owner_lease")
			s.logger.Warn("registration_replenish_owner_expired", "retry_at", retryAt)
		}
		return true
	}
	if worker.Running {
		if state.Status == registrationdomain.ReplenishmentStarting {
			s.finish(ctx, state.ClaimToken, registrationdomain.ReplenishmentRunning, valueOr(state.NextAttemptAt, now.Add(s.config.Cooldown)), "", false, now)
		}
		s.renewLease(ctx, state.ClaimToken, now)
		return true
	}
	if state.Status == registrationdomain.ReplenishmentStarting {
		if state.LeaseUntil == nil || state.LeaseUntil.After(now) {
			return true
		}
		retryAt := now.Add(min(failureRetryInterval, s.config.Cooldown))
		s.finish(ctx, state.ClaimToken, registrationdomain.ReplenishmentFailed, retryAt, "registration start lease expired", true, now)
		observability.ObserveReplenishmentFailure(s.scope(), "start_lease")
		s.logger.Warn("registration_replenish_start_expired", "retry_at", retryAt)
		return true
	}
	if state.Status == registrationdomain.ReplenishmentVerifying {
		s.verifyCapacity(ctx, state, now)
		return true
	}
	if state.Status != registrationdomain.ReplenishmentRunning {
		return false
	}
	if failure := workerFailure(worker); failure != "" {
		retryAt := now.Add(min(failureRetryInterval, s.config.Cooldown))
		s.finish(ctx, state.ClaimToken, registrationdomain.ReplenishmentFailed, retryAt, failure, true, now)
		observability.ObserveReplenishmentFailure(s.scope(), "worker")
		s.logger.Warn("registration_replenish_failed", "error", failure, "succeeded", worker.Progress.Succeeded, "failed", worker.Progress.Failed, "retry_at", retryAt)
		return true
	}
	nextAt := valueOr(state.NextAttemptAt, now.Add(s.config.Cooldown))
	s.finish(ctx, state.ClaimToken, registrationdomain.ReplenishmentVerifying, nextAt, "waiting for routable capacity", false, now)
	s.renewLease(ctx, state.ClaimToken, now)
	s.logger.Info("registration_replenish_verifying", "succeeded", worker.Progress.Succeeded, "failed", worker.Progress.Failed,
		"account_count", worker.Progress.AccountCount, "verification_grace", s.config.VerificationGrace)
	return true
}

func (s *Service) verifyCapacity(ctx context.Context, state registrationdomain.ReplenishmentState, now time.Time) {
	snapshot, err := s.capacity.CapacitySnapshot(ctx, s.config.Provider, s.config.Model, s.config.QuotaMode, 0)
	if err == nil && snapshot.Eligible > state.BaselineEligible {
		nextAt := valueOr(state.NextAttemptAt, now.Add(s.config.Cooldown))
		s.finish(ctx, state.ClaimToken, registrationdomain.ReplenishmentCooling, nextAt, "", true, now)
		s.logger.Info("registration_replenish_verified", "eligible", snapshot.Eligible, "baseline_eligible", state.BaselineEligible,
			"available_slots", snapshot.AvailableSlots, "next_attempt_at", nextAt)
		return
	}
	if now.Sub(state.UpdatedAt) < s.config.VerificationGrace {
		if err != nil {
			s.logger.Warn("registration_replenish_verification_read_failed", "error", err)
		}
		s.renewLease(ctx, state.ClaimToken, now)
		return
	}
	failure := "registration completed but no routable capacity appeared"
	if err != nil {
		failure = "capacity verification failed: " + err.Error()
	}
	retryAt := now.Add(min(failureRetryInterval, s.config.Cooldown))
	s.finish(ctx, state.ClaimToken, registrationdomain.ReplenishmentFailed, retryAt, failure, true, now)
	observability.ObserveReplenishmentFailure(s.scope(), "verification")
	s.logger.Warn("registration_replenish_verification_failed", "error", failure, "retry_at", retryAt)
}

func (s *Service) shouldReplenish(ctx context.Context, snapshot accountdomain.RoutingCapacity, now time.Time) (bool, string, float64) {
	if snapshot.Eligible == 0 && snapshot.QuotaExhausted > 0 && snapshot.RecoveringSoon == 0 {
		return true, "quota_exhausted", 0
	}
	if !s.config.Predictive || snapshot.Eligible > s.config.TargetEligible || s.demand == nil {
		return false, "", 0
	}
	count, err := s.demand.CountRequestsSince(ctx, string(s.config.Provider), s.config.Model, now.Add(-s.config.DemandWindow))
	if err != nil {
		s.logger.Warn("registration_replenish_demand_failed", "error", err)
		observability.ObserveReplenishmentFailure(s.scope(), "demand_read")
		return false, "", 0
	}
	rpm := float64(count) / s.config.DemandWindow.Minutes()
	if rpm < s.config.MinDemandRPM {
		return false, "", rpm
	}
	return true, "predictive_low_watermark", rpm
}

func (s *Service) finish(ctx context.Context, claimToken string, status registrationdomain.ReplenishmentStatus, nextAt time.Time, lastError string, release bool, now time.Time) {
	updated, err := s.states.FinishReplenishment(ctx, s.scope(), claimToken, status, nextAt, lastError, release, now)
	if err != nil {
		s.logger.Error("registration_replenish_state_write_failed", "status", status, "error", err)
	} else if !updated {
		s.clearOwnedClaim(claimToken)
		s.logger.Debug("registration_replenish_stale_finish_ignored", "status", status)
	} else {
		observability.SetReplenishmentState(s.scope(), string(status))
		if release {
			s.clearOwnedClaim(claimToken)
		}
	}
}

func (s *Service) renewLease(ctx context.Context, claimToken string, now time.Time) {
	updated, err := s.states.RenewReplenishment(ctx, s.scope(), claimToken, now.Add(replenishmentLease), now)
	if err != nil {
		s.logger.Warn("registration_replenish_lease_renew_failed", "error", err)
	} else if !updated {
		s.clearOwnedClaim(claimToken)
		s.logger.Warn("registration_replenish_lease_lost")
	}
}

func (s *Service) setOwnedClaim(claimToken string) {
	s.ownerMu.Lock()
	s.ownedClaimToken = claimToken
	s.ownerMu.Unlock()
}

func (s *Service) ownsClaim(claimToken string) bool {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	return claimToken != "" && s.ownedClaimToken == claimToken
}

func (s *Service) clearOwnedClaim(claimToken string) {
	s.ownerMu.Lock()
	if s.ownedClaimToken == claimToken {
		s.ownedClaimToken = ""
	}
	s.ownerMu.Unlock()
}

func (s *Service) scope() string {
	return fmt.Sprintf("%s:%s:%s", s.config.Provider, strings.TrimSpace(s.config.Model), strings.TrimSpace(s.config.QuotaMode))
}

// Scope returns the stable capacity key used by persisted replenishment state.
func (s *Service) Scope() string { return s.scope() }

func newClaimToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func preflightFailures(value registrationapp.PreflightResult) []string {
	result := make([]string, 0)
	for _, check := range value.Checks {
		if !check.OK {
			result = append(result, check.Name+": "+check.Detail)
		}
	}
	return result
}

func workerFailure(value registrationapp.Status) string {
	if value.LastError != nil {
		return strings.TrimSpace(value.LastError.Code + ": " + value.LastError.Message)
	}
	if value.ExitCode == nil {
		return "registration worker stopped without an exit result"
	}
	if *value.ExitCode != 0 {
		return fmt.Sprintf("registration worker exited with code %d", *value.ExitCode)
	}
	return ""
}

func valueOr(value *time.Time, fallback time.Time) time.Time {
	if value == nil || value.IsZero() {
		return fallback
	}
	return value.UTC()
}
