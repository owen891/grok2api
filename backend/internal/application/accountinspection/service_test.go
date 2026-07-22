package accountinspection

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	accountapp "github.com/owen891/grok2api/backend/internal/application/account"
	"github.com/owen891/grok2api/backend/internal/application/gateway"
	"github.com/owen891/grok2api/backend/internal/domain/account"
	inspectiondomain "github.com/owen891/grok2api/backend/internal/domain/accountinspection"
	modeldomain "github.com/owen891/grok2api/backend/internal/domain/model"
	"github.com/owen891/grok2api/backend/internal/infra/persistence/relational"
	"github.com/owen891/grok2api/backend/internal/infra/provider"
	"github.com/owen891/grok2api/backend/internal/infra/runtime/memory"
	"github.com/owen891/grok2api/backend/internal/infra/security"
)

func TestInspectionAutomaticallyAppliesHighConfidenceAction(t *testing.T) {
	service, accountRepo, runs, route, credential, adapter := newInspectionTestService(t)
	adapter.set(credential.ID, http.StatusUnauthorized, `{"error":{"code":"invalid_token","message":"token expired"}}`)
	run, err := service.Start(context.Background(), StartInput{
		Provider: account.ProviderBuild, ModelRouteID: route.ID, Mode: inspectiondomain.RunModeFull, Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.processAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	finished, err := runs.GetInspectionRun(context.Background(), run.ID)
	if err != nil || finished.Status != inspectiondomain.RunStatusCompleted || finished.Completed != 1 {
		t.Fatalf("finished=%#v err=%v", finished, err)
	}
	results, total, err := runs.ListInspectionResults(context.Background(), run.ID, 0, 10)
	if err != nil || total != 1 || len(results) != 1 || results[0].Classification != inspectiondomain.ClassificationReauth || results[0].SuggestedAction != inspectiondomain.ActionRequireReauth || results[0].Confidence != inspectiondomain.ConfidenceHigh || results[0].ApplyStatus != inspectiondomain.ApplyStatusApplied || results[0].ApplyAttempts != 1 || results[0].AppliedAt == nil || results[0].AppliedAction != string(inspectiondomain.ActionRequireReauth) {
		t.Fatalf("results=%#v total=%d err=%v", results, total, err)
	}
	current, err := accountRepo.Get(context.Background(), credential.ID)
	if err != nil || current.AuthStatus != account.AuthStatusReauthRequired || !current.Enabled {
		t.Fatalf("automatically applied account=%#v err=%v", current, err)
	}
}

func TestInspectionKeepsBareRateLimitOutOfAccountHealth(t *testing.T) {
	service, accountRepo, runs, route, credential, adapter := newInspectionTestService(t)
	adapter.set(credential.ID, http.StatusTooManyRequests, `{"error":{"code":8,"message":"rate limited"}}`)
	run, err := service.Start(context.Background(), StartInput{Provider: account.ProviderBuild, ModelRouteID: route.ID, Mode: inspectiondomain.RunModeFull, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.processAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, _, err := runs.ListInspectionResults(context.Background(), run.ID, 0, 10)
	if err != nil || len(results) != 1 || results[0].Classification != inspectiondomain.ClassificationProbeError || results[0].SuggestedAction != inspectiondomain.ActionKeep || results[0].Attempts != 2 || results[0].FailureScope != "provider" {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if results[0].AppliedAt != nil || results[0].ApplyStatus != inspectiondomain.ApplyStatusSkipped {
		t.Fatalf("ambiguous rate limit was automatically applied: %#v", results[0])
	}
	current, err := accountRepo.Get(context.Background(), credential.ID)
	if err != nil || current.CooldownUntil != nil || current.FailureCount != 0 || current.AuthStatus != account.AuthStatusActive {
		t.Fatalf("rate limit changed account health: %#v err=%v", current, err)
	}
}

func TestInspectionPersistsModelUnavailableInferenceHealth(t *testing.T) {
	service, accountRepo, runs, route, credential, adapter := newInspectionTestService(t)
	adapter.set(credential.ID, http.StatusNotFound, `{"error":{"code":"model_not_found","message":"model unavailable"}}`)
	run, err := service.Start(context.Background(), StartInput{
		Provider: account.ProviderBuild, ModelRouteID: route.ID, Mode: inspectiondomain.RunModeFull, Concurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.processAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, _, err := runs.ListInspectionResults(context.Background(), run.ID, 0, 10)
	if err != nil || len(results) != 1 || results[0].Classification != inspectiondomain.ClassificationModelUnavailable || results[0].ApplyStatus != inspectiondomain.ApplyStatusSkipped {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	candidates, err := accountRepo.ListRoutingCandidates(context.Background(), account.ProviderBuild, route.UpstreamModel, "")
	if err != nil || len(candidates) != 1 || candidates[0].InferenceHealth == nil || candidates[0].InferenceHealth.Status != account.InferenceHealthModelUnavailable {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
}

func TestInspectionDoesNotApplyReauthForRefreshableOAuth401(t *testing.T) {
	service, accountRepo, runs, route, credential, adapter := newInspectionTestService(t)
	updated, err := accountRepo.UpdateTokens(context.Background(), credential.ID, credential.EncryptedAccessToken, "encrypted-refresh", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	adapter.set(updated.ID, http.StatusUnauthorized, `{"error":{"code":"invalid_token","message":"token expired"}}`)
	run, err := service.Start(context.Background(), StartInput{Provider: account.ProviderBuild, ModelRouteID: route.ID, Mode: inspectiondomain.RunModeFull, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.processAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, _, err := runs.ListInspectionResults(context.Background(), run.ID, 0, 10)
	if err != nil || len(results) != 1 || results[0].Classification != inspectiondomain.ClassificationProbeError || results[0].SuggestedAction != inspectiondomain.ActionReview || results[0].Confidence != inspectiondomain.ConfidenceMedium {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if results[0].AppliedAt != nil || results[0].ApplyStatus != inspectiondomain.ApplyStatusSkipped {
		t.Fatalf("refreshable OAuth 401 was automatically applied: %#v", results[0])
	}
	current, err := accountRepo.Get(context.Background(), credential.ID)
	if err != nil || current.AuthStatus != account.AuthStatusActive {
		t.Fatalf("refreshable credential was marked invalid: %#v err=%v", current, err)
	}
}

func TestInspectionRejectsRouteWithNoSupportedAccounts(t *testing.T) {
	service, _, _, route, credential, _ := newInspectionTestService(t)
	if err := service.models.ReplaceAccountCapabilities(context.Background(), credential.ID, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	annotated, err := service.models.Get(context.Background(), route.ID)
	if err != nil || annotated.SyncedAccounts != 1 || annotated.SupportedAccounts != 0 {
		t.Fatalf("route=%#v err=%v", annotated, err)
	}
	_, err = service.Start(context.Background(), StartInput{
		Provider: account.ProviderBuild, ModelRouteID: route.ID, Mode: inspectiondomain.RunModeFull, Concurrency: 1,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
	var validation *InvalidInputError
	if !errors.As(err, &validation) || validation.Reason != "model_route_has_no_supported_accounts" {
		t.Fatalf("validation = %#v", err)
	}
}

func TestInspectionRenewsRunLeaseWhileAutomaticActionIsBlocked(t *testing.T) {
	service, _, runs, route, credential, adapter := newInspectionTestService(t)
	adapter.set(credential.ID, http.StatusUnauthorized, `{"error":{"code":"invalid_token","message":"token expired"}}`)
	blocker := newBlockingCredentialManager(false)
	service.credentials = blocker
	service.lease = 60 * time.Millisecond
	service.heartbeat = 15 * time.Millisecond
	service.watchPoll = 5 * time.Millisecond
	run, err := service.Start(context.Background(), StartInput{Provider: account.ProviderBuild, ModelRouteID: route.ID, Mode: inspectiondomain.RunModeFull, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- service.processAvailable(context.Background()) }()
	select {
	case <-blocker.started:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic action did not start")
	}
	time.Sleep(150 * time.Millisecond)
	if _, claimed, claimErr := runs.TryClaimInspectionRun(context.Background(), run.ID, "ffffffffffffffffffffffffffffffff", time.Now().UTC(), time.Now().UTC().Add(time.Minute)); claimErr != nil || claimed {
		t.Fatalf("second instance claimed live automatic action: claimed=%v err=%v", claimed, claimErr)
	}
	close(blocker.release)
	select {
	case processErr := <-done:
		if processErr != nil {
			t.Fatal(processErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inspection did not finish")
	}
	finished, err := runs.GetInspectionRun(context.Background(), run.ID)
	if err != nil || finished.Status != inspectiondomain.RunStatusCompleted {
		t.Fatalf("finished=%#v err=%v", finished, err)
	}
}

func TestInspectionCancellationInterruptsAutomaticAction(t *testing.T) {
	service, _, runs, route, credential, adapter := newInspectionTestService(t)
	adapter.set(credential.ID, http.StatusUnauthorized, `{"error":{"code":"invalid_token","message":"token expired"}}`)
	blocker := newBlockingCredentialManager(true)
	service.credentials = blocker
	service.watchPoll = 5 * time.Millisecond
	run, err := service.Start(context.Background(), StartInput{Provider: account.ProviderBuild, ModelRouteID: route.ID, Mode: inspectiondomain.RunModeFull, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- service.processAvailable(context.Background()) }()
	select {
	case <-blocker.started:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic action did not start")
	}
	if _, err := service.Cancel(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case processErr := <-done:
		if processErr != nil {
			t.Fatal(processErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled inspection did not stop")
	}
	finished, err := runs.GetInspectionRun(context.Background(), run.ID)
	if err != nil || finished.Status != inspectiondomain.RunStatusCancelled {
		t.Fatalf("finished=%#v err=%v", finished, err)
	}
	results, _, err := runs.ListInspectionResults(context.Background(), run.ID, 0, 10)
	if err != nil || len(results) != 1 || results[0].ApplyStatus != inspectiondomain.ApplyStatusFailed || results[0].AppliedAt != nil {
		t.Fatalf("results=%#v err=%v", results, err)
	}
}

type blockingCredentialManager struct {
	started       chan struct{}
	release       chan struct{}
	respectCancel bool
	once          sync.Once
}

func newBlockingCredentialManager(respectCancel bool) *blockingCredentialManager {
	return &blockingCredentialManager{started: make(chan struct{}), release: make(chan struct{}), respectCancel: respectCancel}
}

func (m *blockingCredentialManager) MarkReauthRequired(ctx context.Context, _ uint64, _ string) error {
	m.once.Do(func() { close(m.started) })
	if m.respectCancel {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.release:
			return nil
		}
	}
	<-m.release
	return nil
}

func (m *blockingCredentialManager) MarkInspectionHealthy(context.Context, uint64) (account.Credential, error) {
	return account.Credential{}, errors.New("unexpected healthy action")
}

type inspectionTestAdapter struct {
	mu        sync.Mutex
	responses map[uint64]struct {
		status int
		body   string
	}
}

func (a *inspectionTestAdapter) Provider() account.Provider { return account.ProviderBuild }

func (a *inspectionTestAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	value := a.responses[request.Credential.ID]
	a.mu.Unlock()
	if value.status == 0 {
		value.status = http.StatusOK
		value.body = `{"id":"probe"}`
	}
	return &provider.Response{StatusCode: value.status, Status: http.StatusText(value.status), Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(value.body))}, nil
}

func (a *inspectionTestAdapter) set(accountID uint64, status int, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.responses[accountID] = struct {
		status int
		body   string
	}{status: status, body: body}
}

func newInspectionTestService(t *testing.T) (*Service, *relational.AccountRepository, *relational.AccountInspectionRepository, modeldomain.Route, account.Credential, *inspectionTestAdapter) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "inspection.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	runs := relational.NewAccountInspectionRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "build", SourceKey: "build",
		EncryptedAccessToken: encrypted, ExpiresAt: time.Now().UTC().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{PublicID: "grok-inspection-test", Provider: account.ProviderBuild, UpstreamModel: "grok-inspection-test", Capability: modeldomain.CapabilityResponses, Origin: modeldomain.OriginManual, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-inspection-test"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderBuild, "grok-inspection-test")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &inspectionTestAdapter{responses: make(map[uint64]struct {
		status int
		body   string
	})}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	auditRepo := relational.NewAuditRepository(database)
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	selector := gateway.NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	return NewService(accountRepo, modelRepo, runs, registry, accountService, selector, nil), accountRepo, runs, route, credential, adapter
}
