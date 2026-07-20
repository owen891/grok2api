package replenishment

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	registrationapp "github.com/owen891/grok2api/backend/internal/application/registration"
	"github.com/owen891/grok2api/backend/internal/domain/account"
	registrationdomain "github.com/owen891/grok2api/backend/internal/domain/registration"
	"github.com/owen891/grok2api/backend/internal/infra/persistence/relational"
	"github.com/owen891/grok2api/backend/internal/repository"
)

type starterStub struct {
	mu        sync.Mutex
	status    registrationapp.Status
	starts    []registrationapp.StartInput
	preflight *registrationapp.PreflightResult
	statusErr error
	startErr  error
}

func (s *starterStub) Status() (registrationapp.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.statusErr
}
func (s *starterStub) Preflight(context.Context) registrationapp.PreflightResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preflight == nil {
		return registrationapp.PreflightResult{OK: true}
	}
	return *s.preflight
}
func (s *starterStub) Start(_ context.Context, input registrationapp.StartInput) (registrationapp.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, input)
	if s.startErr != nil {
		return s.status, s.startErr
	}
	s.status.Running = true
	return s.status, nil
}

func (s *starterStub) Starts() []registrationapp.StartInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]registrationapp.StartInput(nil), s.starts...)
}

func (s *starterStub) SetStatus(value registrationapp.Status) {
	s.mu.Lock()
	s.status = value
	s.mu.Unlock()
}

type capacityStub struct {
	mu    sync.Mutex
	value account.RoutingCapacity
	calls int
}

func (s *capacityStub) Set(value account.RoutingCapacity) {
	s.mu.Lock()
	s.value = value
	s.mu.Unlock()
}

func (s *capacityStub) CapacitySnapshot(context.Context, account.Provider, string, string, time.Duration) (account.RoutingCapacity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.value, nil
}

func (s *capacityStub) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type demandStub struct {
	count int64
	err   error
	calls int
}

func (s *demandStub) CountRequestsSince(context.Context, string, string, time.Time) (int64, error) {
	s.calls++
	return s.count, s.err
}

func openStateRepository(t *testing.T) (*relational.Database, *relational.ReplenishmentRepository) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "replenishment.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		database.Close()
		t.Fatal(err)
	}
	return database, relational.NewReplenishmentRepository(database)
}

func testConfig() Config {
	return Config{
		Enabled: true, Provider: account.ProviderWeb, Model: "grok-imagine-image", QuotaMode: "fast",
		RegisterCount: 1, Cooldown: 30 * time.Minute, RecoveryLeadTime: 10 * time.Minute, MaxDailyRegistrations: 3,
	}
}

func TestServiceDisabledNeverEvaluatesOrRegisters(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 1}}
	starter := &starterStub{}
	config := testConfig()
	config.Enabled = false
	service := NewService(capacity, states, starter, config, nil)
	service.ensure(context.Background())
	if starts := starter.Starts(); capacity.Calls() != 0 || len(starts) != 0 {
		t.Fatalf("disabled service evaluated=%d starts=%#v", capacity.Calls(), starts)
	}
	if _, err := states.GetReplenishmentState(context.Background(), service.scope()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("disabled state error = %v", err)
	}
}

func TestServiceStartsOneWebRegistrationForExhaustedCapacity(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 2}}
	starter := &starterStub{}
	service := NewService(capacity, states, starter, testConfig(), nil)
	service.ensure(context.Background())
	service.ensure(context.Background())
	starts := starter.Starts()
	if len(starts) != 1 || starts[0].AccountType != "web" || starts[0].Count != 1 || starts[0].Threads != 1 {
		t.Fatalf("registration starts = %#v", starts)
	}
	state, err := states.GetReplenishmentState(context.Background(), service.scope())
	if err != nil || state.Status != registrationdomain.ReplenishmentRunning || state.DailyStarts != 1 {
		t.Fatalf("state = %#v, err=%v", state, err)
	}
}

func TestServiceDryRunPersistsCooldownAndDailyLimitAcrossRestart(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 1}}
	config := testConfig()
	config.DryRun = true
	config.MaxDailyRegistrations = 1
	first := NewService(capacity, states, &starterStub{}, config, nil)
	first.now = func() time.Time { return now }
	first.ensure(context.Background())

	secondStarter := &starterStub{}
	second := NewService(capacity, states, secondStarter, config, nil)
	second.now = func() time.Time { return now.Add(time.Hour) }
	second.ensure(context.Background())
	if starts := secondStarter.Starts(); len(starts) != 0 {
		t.Fatalf("restart bypassed daily limit: %#v", starts)
	}
	state, err := states.GetReplenishmentState(context.Background(), second.scope())
	if err != nil || state.DailyStarts != 1 || state.Status != registrationdomain.ReplenishmentCooling {
		t.Fatalf("state = %#v, err=%v", state, err)
	}
}

func TestServiceSkipsWhenCapacityWillRecoverSoon(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 2, RecoveringSoon: 1}}
	starter := &starterStub{}
	service := NewService(capacity, states, starter, testConfig(), nil)
	service.ensure(context.Background())
	if starts := starter.Starts(); len(starts) != 0 {
		t.Fatalf("recovering capacity started registration: %#v", starts)
	}
}

func TestServicePreflightFailureDoesNotConsumeDailyLimit(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 1}}
	preflight := registrationapp.PreflightResult{OK: false, Checks: []registrationapp.PreflightCheck{{Name: "proxy", Detail: "unreachable"}}}
	starter := &starterStub{preflight: &preflight}
	service := NewService(capacity, states, starter, testConfig(), nil)
	service.ensure(context.Background())
	if starts := starter.Starts(); len(starts) != 0 {
		t.Fatalf("preflight failure started registration: %#v", starts)
	}
	if _, err := states.GetReplenishmentState(context.Background(), service.scope()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("preflight failure consumed state: %v", err)
	}
}

func TestServiceReconcilesWorkerCompletion(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 1}}
	starter := &starterStub{}
	service := NewService(capacity, states, starter, testConfig(), nil)
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	service.ensure(context.Background())
	exitCode := 0
	starter.SetStatus(registrationapp.Status{Running: false, ExitCode: &exitCode, Progress: registrationapp.Progress{Succeeded: 1, AccountCount: 1}})
	service.now = func() time.Time { return now.Add(time.Minute) }
	service.ensure(context.Background())
	state, err := states.GetReplenishmentState(context.Background(), service.scope())
	if err != nil || state.Status != registrationdomain.ReplenishmentVerifying || state.ClaimToken == "" || state.DailyStarts != 1 {
		t.Fatalf("verifying state = %#v, err=%v", state, err)
	}
	capacity.Set(account.RoutingCapacity{Eligible: 1})
	service.now = func() time.Time { return now.Add(90 * time.Second) }
	service.ensure(context.Background())
	state, err = states.GetReplenishmentState(context.Background(), service.scope())
	if err != nil || state.Status != registrationdomain.ReplenishmentCooling || state.ClaimToken != "" || state.DailyStarts != 1 {
		t.Fatalf("completed state = %#v, err=%v", state, err)
	}
}

func TestServiceFailsWhenRegistrationAddsNoRoutableCapacity(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 1}}
	starter := &starterStub{}
	config := testConfig()
	config.VerificationGrace = time.Minute
	service := NewService(capacity, states, starter, config, nil)
	service.now = func() time.Time { return now }
	service.ensure(context.Background())
	exitCode := 0
	starter.SetStatus(registrationapp.Status{ExitCode: &exitCode, Progress: registrationapp.Progress{Succeeded: 1, AccountCount: 1}})
	service.now = func() time.Time { return now.Add(time.Minute) }
	service.ensure(context.Background())
	service.now = func() time.Time { return now.Add(2*time.Minute + time.Second) }
	service.ensure(context.Background())
	state, err := states.GetReplenishmentState(context.Background(), service.scope())
	if err != nil || state.Status != registrationdomain.ReplenishmentFailed || state.ClaimToken != "" || !strings.Contains(state.LastError, "no routable capacity") {
		t.Fatalf("failed verification state = %#v, err=%v", state, err)
	}
}

func TestServicePredictiveTriggerRequiresDemandThreshold(t *testing.T) {
	for name, count := range map[string]int64{"below": 19, "at_threshold": 20} {
		t.Run(name, func(t *testing.T) {
			database, states := openStateRepository(t)
			defer database.Close()
			capacity := &capacityStub{value: account.RoutingCapacity{Eligible: 1}}
			starter := &starterStub{}
			config := testConfig()
			config.Predictive = true
			config.TargetEligible = 2
			config.MinDemandRPM = 2
			config.DemandWindow = 10 * time.Minute
			service := NewService(capacity, states, starter, config, nil)
			demand := &demandStub{count: count}
			service.SetDemandSource(demand)
			service.ensure(context.Background())
			wantStarts := 0
			if name == "at_threshold" {
				wantStarts = 1
			}
			if starts := starter.Starts(); len(starts) != wantStarts || demand.calls != 1 {
				t.Fatalf("starts=%#v demand_calls=%d", starts, demand.calls)
			}
			if wantStarts == 1 {
				state, err := states.GetReplenishmentState(context.Background(), service.scope())
				if err != nil || state.BaselineEligible != 1 {
					t.Fatalf("predictive state=%#v err=%v", state, err)
				}
			}
		})
	}
}

func TestServicePredictiveDisabledPreservesLegacyTrigger(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	capacity := &capacityStub{value: account.RoutingCapacity{Eligible: 1}}
	starter := &starterStub{}
	service := NewService(capacity, states, starter, testConfig(), nil)
	demand := &demandStub{count: 1000}
	service.SetDemandSource(demand)
	service.ensure(context.Background())
	if starts := starter.Starts(); len(starts) != 0 || demand.calls != 0 {
		t.Fatalf("legacy mode starts=%#v demand_calls=%d", starts, demand.calls)
	}
}

func TestServiceClaimsOnceAcrossConcurrentInstances(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 3}}
	starter := &starterStub{}
	first := NewService(capacity, states, starter, testConfig(), nil)
	second := NewService(capacity, states, starter, testConfig(), nil)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, service := range []*Service{first, second} {
		workers.Add(1)
		go func(value *Service) {
			defer workers.Done()
			<-start
			value.ensure(context.Background())
		}(service)
	}
	close(start)
	workers.Wait()
	if starts := starter.Starts(); len(starts) != 1 {
		t.Fatalf("concurrent starts = %#v", starts)
	}
	state, err := states.GetReplenishmentState(context.Background(), first.scope())
	if err != nil || state.DailyStarts != 1 {
		t.Fatalf("state = %#v, err=%v", state, err)
	}
}

func TestServiceNonOwnerDoesNotReconcileAnotherInstancesWorker(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 1}}
	ownerStarter := &starterStub{}
	observerStarter := &starterStub{}
	owner := NewService(capacity, states, ownerStarter, testConfig(), nil)
	observer := NewService(capacity, states, observerStarter, testConfig(), nil)
	owner.now = func() time.Time { return now }
	observer.now = func() time.Time { return now.Add(30 * time.Second) }
	owner.ensure(context.Background())
	state, err := states.GetReplenishmentState(context.Background(), owner.scope())
	if err != nil || state.Status != registrationdomain.ReplenishmentRunning || state.LeaseUntil == nil {
		t.Fatalf("owner state=%#v err=%v", state, err)
	}
	originalLease := *state.LeaseUntil
	observer.ensure(context.Background())
	state, err = states.GetReplenishmentState(context.Background(), owner.scope())
	if err != nil || state.Status != registrationdomain.ReplenishmentRunning || state.ClaimToken == "" || len(observerStarter.Starts()) != 0 {
		t.Fatalf("observer changed owner state=%#v starts=%#v err=%v", state, observerStarter.Starts(), err)
	}
	owner.now = func() time.Time { return now.Add(time.Minute) }
	owner.ensure(context.Background())
	state, err = states.GetReplenishmentState(context.Background(), owner.scope())
	if err != nil || state.LeaseUntil == nil || !state.LeaseUntil.After(originalLease) {
		t.Fatalf("owner lease was not renewed state=%#v err=%v", state, err)
	}
}

func TestServiceRecoversExpiredClaimFromCrashedOwner(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 1}}
	owner := NewService(capacity, states, &starterStub{}, testConfig(), nil)
	owner.now = func() time.Time { return now }
	owner.ensure(context.Background())
	recovery := NewService(capacity, states, &starterStub{}, testConfig(), nil)
	recovery.now = func() time.Time { return now.Add(replenishmentLease + time.Second) }
	recovery.ensure(context.Background())
	state, err := states.GetReplenishmentState(context.Background(), owner.scope())
	if err != nil || state.Status != registrationdomain.ReplenishmentFailed || state.ClaimToken != "" || !strings.Contains(state.LastError, "owner lease expired") {
		t.Fatalf("recovered state=%#v err=%v", state, err)
	}
}

func TestServiceDatabaseFailureDoesNotStartWorker(t *testing.T) {
	database, states := openStateRepository(t)
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 1}}
	starter := &starterStub{}
	service := NewService(capacity, states, starter, testConfig(), nil)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	service.ensure(context.Background())
	if starts := starter.Starts(); len(starts) != 0 {
		t.Fatalf("database outage started worker: %#v", starts)
	}
}

func TestServiceWorkerFailureReleasesClaimAndRetriesAfterBackoff(t *testing.T) {
	database, states := openStateRepository(t)
	defer database.Close()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	capacity := &capacityStub{value: account.RoutingCapacity{QuotaExhausted: 1}}
	starter := &starterStub{}
	service := NewService(capacity, states, starter, testConfig(), nil)
	service.now = func() time.Time { return now }
	service.ensure(context.Background())
	exitCode := 17
	starter.SetStatus(registrationapp.Status{ExitCode: &exitCode, LastError: &registrationapp.Failure{Code: "worker_failed", Message: "temporary failure"}})
	service.now = func() time.Time { return now.Add(time.Minute) }
	service.ensure(context.Background())
	state, err := states.GetReplenishmentState(context.Background(), service.scope())
	if err != nil || state.Status != registrationdomain.ReplenishmentFailed || state.ClaimToken != "" || state.NextAttemptAt == nil {
		t.Fatalf("failed state=%#v err=%v", state, err)
	}
	service.now = func() time.Time { return now.Add(12 * time.Minute) }
	service.ensure(context.Background())
	state, err = states.GetReplenishmentState(context.Background(), service.scope())
	if err != nil || len(starter.Starts()) != 2 || state.Status != registrationdomain.ReplenishmentRunning || state.DailyStarts != 2 {
		t.Fatalf("retry starts=%#v state=%#v err=%v", starter.Starts(), state, err)
	}
}
