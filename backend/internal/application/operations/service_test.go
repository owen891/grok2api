package operations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	modeldomain "github.com/owen891/grok2api/backend/internal/domain/model"
	registrationdomain "github.com/owen891/grok2api/backend/internal/domain/registration"
	"github.com/owen891/grok2api/backend/internal/repository"
)

func TestSnapshotUsesSelectorCapacityAndReplenishmentState(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	next := now.Add(20 * time.Minute)
	service := NewService(
		modelSourceStub{routes: []modeldomain.Route{{ID: 7, PublicID: "Web/grok-imagine-image", Provider: account.ProviderWeb, UpstreamModel: "grok-imagine-image", Capability: modeldomain.CapabilityImage}}},
		capacitySourceStub{value: account.RoutingCapacity{Total: 5, Eligible: 2, Saturated: 1, QuotaExhausted: 2, InFlight: 3, TotalSlots: 8, AvailableSlots: 5}},
		quotaModeSourceStub{},
		&replenishmentSourceStub{state: registrationdomain.ReplenishmentState{Scope: "grok_web:grok-imagine-image:fast", Status: registrationdomain.ReplenishmentCooling, DailyStarts: 1, NextAttemptAt: &next, UpdatedAt: now}},
		ReplenishmentConfig{Enabled: true, Scope: "grok_web:grok-imagine-image:fast", MaxDailyRegistrations: 3},
	)
	service.now = func() time.Time { return now }
	service.TaskStarted("video_workers")

	snapshot, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 1 || snapshot.Routes[0].PublicModel != "grok-imagine-image" || snapshot.Routes[0].QuotaMode != "fast" {
		t.Fatalf("routes = %#v", snapshot.Routes)
	}
	if snapshot.Routes[0].Eligible != 2 || snapshot.Routes[0].Saturated != 1 || snapshot.Routes[0].AvailableSlots != 5 {
		t.Fatalf("capacity = %#v", snapshot.Routes[0])
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].State != "running" {
		t.Fatalf("tasks = %#v", snapshot.Tasks)
	}
	if snapshot.Replenishment.State != registrationdomain.ReplenishmentCooling || snapshot.Replenishment.DailyStarts != 1 || snapshot.Replenishment.NextAttemptAt == nil {
		t.Fatalf("replenishment = %#v", snapshot.Replenishment)
	}
}

func TestTaskTrackerPreservesFailureUntilSuccess(t *testing.T) {
	service := &Service{tasks: make(map[string]TaskStatus), now: time.Now}
	service.TaskStarted("quota_recovery")
	service.TaskFailed("quota_recovery", errors.New("temporary failure"), true)
	service.TaskHeartbeat("quota_recovery")
	failed := service.TaskSnapshot()[0]
	if failed.State != "degraded" || failed.ConsecutiveFailures != 1 || failed.RestartCount != 1 || failed.LastError == "" {
		t.Fatalf("failed = %#v", failed)
	}
	service.TaskSucceeded("quota_recovery", nil)
	succeeded := service.TaskSnapshot()[0]
	if succeeded.State != "running" || succeeded.ConsecutiveFailures != 0 || succeeded.LastError != "" || succeeded.LastSuccessAt == nil {
		t.Fatalf("succeeded = %#v", succeeded)
	}
}

func TestTaskScheduledHasDecodableInitialState(t *testing.T) {
	service := &Service{tasks: make(map[string]TaskStatus), now: time.Now}
	service.TaskScheduled("cleanup", time.Now().Add(time.Hour))
	status := service.TaskSnapshot()[0]
	if status.State != "running" || status.NextRunAt == nil {
		t.Fatalf("scheduled status=%#v", status)
	}
}

func TestTriggerReplenishmentQueuesConfiguredTrigger(t *testing.T) {
	service := NewService(nil, nil, nil, nil, ReplenishmentConfig{Enabled: true})
	trigger := &replenishmentTriggerStub{}
	service.SetReplenishmentTrigger(trigger)
	if err := service.TriggerReplenishment(context.Background()); err != nil {
		t.Fatal(err)
	}
	if trigger.calls != 1 {
		t.Fatalf("trigger calls = %d, want 1", trigger.calls)
	}
}

func TestTriggerReplenishmentRejectsDisabledOrUnavailableService(t *testing.T) {
	disabled := NewService(nil, nil, nil, nil, ReplenishmentConfig{})
	if err := disabled.TriggerReplenishment(context.Background()); !errors.Is(err, ErrReplenishmentDisabled) {
		t.Fatalf("disabled trigger error = %v", err)
	}

	unavailable := NewService(nil, nil, nil, nil, ReplenishmentConfig{Enabled: true})
	if err := unavailable.TriggerReplenishment(context.Background()); !errors.Is(err, ErrReplenishmentUnavailable) {
		t.Fatalf("unavailable trigger error = %v", err)
	}
}

type modelSourceStub struct{ routes []modeldomain.Route }

func (s modelSourceStub) ListConfiguredEnabled(context.Context) ([]modeldomain.Route, error) {
	return s.routes, nil
}

type capacitySourceStub struct{ value account.RoutingCapacity }

func (s capacitySourceStub) CapacitySnapshot(context.Context, account.Provider, string, string, time.Duration) (account.RoutingCapacity, error) {
	return s.value, nil
}

type quotaModeSourceStub struct{}

func (quotaModeSourceStub) QuotaMode(account.Provider, string) string { return "fast" }

type replenishmentTriggerStub struct {
	calls int
	err   error
}

func (s *replenishmentTriggerStub) Trigger(context.Context) error {
	s.calls++
	return s.err
}

type replenishmentSourceStub struct {
	state registrationdomain.ReplenishmentState
	err   error
}

func (s *replenishmentSourceStub) GetReplenishmentState(context.Context, string) (registrationdomain.ReplenishmentState, error) {
	return s.state, s.err
}

func (*replenishmentSourceStub) ClaimReplenishment(context.Context, repository.ReplenishmentClaim) (registrationdomain.ReplenishmentState, bool, error) {
	return registrationdomain.ReplenishmentState{}, false, nil
}

func (*replenishmentSourceStub) RenewReplenishment(context.Context, string, string, time.Time, time.Time) (bool, error) {
	return false, nil
}

func (*replenishmentSourceStub) ExpireReplenishment(context.Context, string, string, time.Time, time.Time, string) (bool, error) {
	return false, nil
}

func (*replenishmentSourceStub) FinishReplenishment(context.Context, string, string, registrationdomain.ReplenishmentStatus, time.Time, string, bool, time.Time) (bool, error) {
	return false, nil
}
