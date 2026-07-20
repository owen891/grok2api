package relational

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/domain/accountinspection"
	registrationdomain "github.com/owen891/grok2api/backend/internal/domain/registration"
	"github.com/owen891/grok2api/backend/internal/repository"
)

func TestPostgresRepositoriesIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAccountRepository(database)
	created, wasCreated, err := repository.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "postgres", SourceKey: "postgres-integration-" + time.Now().UTC().Format("150405.000000"),
		EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
	})
	if err != nil || !wasCreated || created.ID == 0 {
		t.Fatalf("account = %#v, created = %v, err = %v", created, wasCreated, err)
	}
	loaded, err := repository.Get(ctx, created.ID)
	if err != nil || loaded.SourceKey != created.SourceKey {
		t.Fatalf("loaded = %#v, err = %v", loaded, err)
	}
	if err := repository.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresInspectionClaimIsAtomicAcrossInstances(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	firstDB, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer firstDB.Close()
	secondDB, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	if err := firstDB.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runID := "inspection-" + now.Format("20060102150405000000")
	run := accountinspection.Run{
		ID: runID, Provider: account.ProviderConsole, ModelRouteID: 1, UpstreamModel: "grok-test",
		Mode: accountinspection.RunModeFull, Status: accountinspection.RunStatusQueued, Concurrency: 1, Total: 1, CreatedAt: now, UpdatedAt: now,
	}
	target := accountinspection.Result{
		RunID: runID, AccountID: 999999, Provider: account.ProviderConsole, AccountName: "integration", AccountEnabled: true,
		AccountUpdatedAt: now, Model: run.UpstreamModel, Classification: accountinspection.ClassificationPending,
		SuggestedAction: accountinspection.ActionKeep, Confidence: accountinspection.ConfidenceLow, CreatedAt: now, UpdatedAt: now,
	}
	first := NewAccountInspectionRepository(firstDB)
	second := NewAccountInspectionRepository(secondDB)
	defer firstDB.db.WithContext(ctx).Where("id = ?", runID).Delete(&accountInspectionRunModel{})
	if err := first.CreateInspectionRun(ctx, run, []accountinspection.Result{target}); err != nil {
		t.Fatal(err)
	}
	repositories := []*AccountInspectionRepository{first, second}
	start := make(chan struct{})
	claimed := make(chan bool, len(repositories))
	errorsCh := make(chan error, len(repositories))
	var workers sync.WaitGroup
	for index, value := range repositories {
		workers.Add(1)
		go func(index int, value *AccountInspectionRepository) {
			defer workers.Done()
			<-start
			_, ok, claimErr := value.TryClaimInspectionRun(ctx, runID, fmt.Sprintf("%032d", index+1), now, now.Add(time.Minute))
			claimed <- ok
			errorsCh <- claimErr
		}(index, value)
	}
	close(start)
	workers.Wait()
	close(claimed)
	close(errorsCh)
	claimCount := 0
	for value := range claimed {
		if value {
			claimCount++
		}
	}
	for claimErr := range errorsCh {
		if claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	if claimCount != 1 {
		t.Fatalf("inspection claims = %d, want 1", claimCount)
	}
}

func TestPostgresInspectionApplicationClaimIsAtomicAcrossInstances(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	firstDB, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer firstDB.Close()
	secondDB, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	if err := firstDB.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runID := "inspection-apply-" + now.Format("20060102150405000000")
	run := accountinspection.Run{ID: runID, Provider: account.ProviderConsole, ModelRouteID: 1, UpstreamModel: "grok-test", Mode: accountinspection.RunModeFull, Status: accountinspection.RunStatusQueued, Concurrency: 1, Total: 1, CreatedAt: now, UpdatedAt: now}
	target := accountinspection.Result{RunID: runID, AccountID: 999998, Provider: run.Provider, AccountName: "integration-apply", AccountEnabled: true, AccountUpdatedAt: now, Model: run.UpstreamModel, Classification: accountinspection.ClassificationPending, SuggestedAction: accountinspection.ActionKeep, Confidence: accountinspection.ConfidenceLow, CreatedAt: now, UpdatedAt: now}
	first := NewAccountInspectionRepository(firstDB)
	second := NewAccountInspectionRepository(secondDB)
	defer firstDB.db.WithContext(ctx).Where("id = ?", runID).Delete(&accountInspectionRunModel{})
	if err := first.CreateInspectionRun(ctx, run, []accountinspection.Result{target}); err != nil {
		t.Fatal(err)
	}
	runToken := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, ok, err := first.TryClaimInspectionRun(ctx, runID, runToken, now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("run claim ok=%v err=%v", ok, err)
	}
	target.Classification = accountinspection.ClassificationHealthy
	target.SuggestedAction = accountinspection.ActionClearHealth
	target.Confidence = accountinspection.ConfidenceHigh
	if updated, err := first.CompleteInspectionResult(ctx, target, runToken, now.Add(time.Second)); err != nil || !updated {
		t.Fatalf("complete updated=%v err=%v", updated, err)
	}
	repositories := []*AccountInspectionRepository{first, second}
	start := make(chan struct{})
	claimed := make(chan bool, len(repositories))
	errorsCh := make(chan error, len(repositories))
	var workers sync.WaitGroup
	for index, value := range repositories {
		workers.Add(1)
		go func(index int, value *AccountInspectionRepository) {
			defer workers.Done()
			<-start
			ok, claimErr := value.TryClaimInspectionResultApplication(ctx, runID, target.AccountID, runToken, fmt.Sprintf("%032d", index+11), now.Add(2*time.Second), now.Add(time.Minute))
			claimed <- ok
			errorsCh <- claimErr
		}(index, value)
	}
	close(start)
	workers.Wait()
	close(claimed)
	close(errorsCh)
	claimCount := 0
	for value := range claimed {
		if value {
			claimCount++
		}
	}
	for claimErr := range errorsCh {
		if claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	if claimCount != 1 {
		t.Fatalf("inspection application claims = %d, want 1", claimCount)
	}
}

func TestPostgresReplenishmentClaimIsAtomicAcrossInstances(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	firstDB, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer firstDB.Close()
	secondDB, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	if err := firstDB.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	scope := "integration:" + time.Now().UTC().Format("20060102150405.000000000")
	defer firstDB.db.WithContext(ctx).Where("scope = ?", scope).Delete(&registrationReplenishmentModel{})
	repositories := []*ReplenishmentRepository{NewReplenishmentRepository(firstDB), NewReplenishmentRepository(secondDB)}
	now := time.Now().UTC()
	start := make(chan struct{})
	claimed := make(chan bool, len(repositories))
	errorsCh := make(chan error, len(repositories))
	var workers sync.WaitGroup
	for index, value := range repositories {
		workers.Add(1)
		go func(index int, value *ReplenishmentRepository) {
			defer workers.Done()
			<-start
			_, ok, claimErr := value.ClaimReplenishment(ctx, repository.ReplenishmentClaim{
				Scope: scope, ClaimToken: fmt.Sprintf("%032d", index+1), Now: now,
				LeaseUntil: now.Add(time.Minute), NextAt: now.Add(30 * time.Minute), DailyLimit: 3,
			})
			claimed <- ok
			errorsCh <- claimErr
		}(index, value)
	}
	close(start)
	workers.Wait()
	close(claimed)
	close(errorsCh)
	claimCount := 0
	for value := range claimed {
		if value {
			claimCount++
		}
	}
	for claimErr := range errorsCh {
		if claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	state, err := repositories[0].GetReplenishmentState(ctx, scope)
	if err != nil || claimCount != 1 || state.Status != registrationdomain.ReplenishmentStarting || state.DailyStarts != 1 {
		t.Fatalf("claims=%d state=%#v err=%v", claimCount, state, err)
	}
}
