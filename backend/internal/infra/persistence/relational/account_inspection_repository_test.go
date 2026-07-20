package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/domain/accountinspection"
	"github.com/owen891/grok2api/backend/internal/repository"
)

func TestAccountInspectionRepositoryClaimsRecoversAndCompletes(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountInspectionRepository(openTestDatabase(t))
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	run := accountinspection.Run{
		ID: "0123456789abcdef0123456789abcdef", Provider: account.ProviderBuild, ModelRouteID: 7, UpstreamModel: "grok-test",
		Mode: accountinspection.RunModeFull, Status: accountinspection.RunStatusQueued, Concurrency: 2, Total: 2, CreatedAt: now, UpdatedAt: now,
	}
	targets := []accountinspection.Result{
		{RunID: run.ID, AccountID: 1, Provider: account.ProviderBuild, AccountName: "one", AccountEnabled: true, AccountUpdatedAt: now, Model: run.UpstreamModel, Classification: accountinspection.ClassificationPending, SuggestedAction: accountinspection.ActionKeep, Confidence: accountinspection.ConfidenceLow, CreatedAt: now, UpdatedAt: now},
		{RunID: run.ID, AccountID: 2, Provider: account.ProviderBuild, AccountName: "two", AccountEnabled: true, AccountUpdatedAt: now, Model: run.UpstreamModel, Classification: accountinspection.ClassificationPending, SuggestedAction: accountinspection.ActionKeep, Confidence: accountinspection.ConfidenceLow, CreatedAt: now, UpdatedAt: now},
	}
	if err := repo.CreateInspectionRun(ctx, run, targets); err != nil {
		t.Fatal(err)
	}
	duplicate := run
	duplicate.ID = "fedcba9876543210fedcba9876543210"
	if err := repo.CreateInspectionRun(ctx, duplicate, targets); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("active provider conflict = %v", err)
	}
	claimToken := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	claimed, ok, err := repo.TryClaimInspectionRun(ctx, run.ID, claimToken, now, now.Add(time.Minute))
	if err != nil || !ok || claimed.Status != accountinspection.RunStatusRunning || claimed.ClaimToken != claimToken {
		t.Fatalf("claim=%#v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := repo.TryClaimInspectionRun(ctx, run.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", now.Add(time.Second), now.Add(2*time.Minute)); err != nil || ok {
		t.Fatalf("second claim ok=%v err=%v", ok, err)
	}
	completed := targets[0]
	completed.Classification = accountinspection.ClassificationHealthy
	completed.SuggestedAction = accountinspection.ActionClearHealth
	completed.Confidence = accountinspection.ConfidenceHigh
	completed.HTTPStatus = 200
	completed.Attempts = 1
	completed.UpdatedAt = now.Add(2 * time.Second)
	if updated, err := repo.CompleteInspectionResult(ctx, completed, "stale-stale-stale-stale", now.Add(2*time.Second)); err != nil || updated {
		t.Fatalf("stale complete updated=%v err=%v", updated, err)
	}
	if updated, err := repo.CompleteInspectionResult(ctx, completed, claimToken, now.Add(2*time.Second)); err != nil || !updated {
		t.Fatalf("owner complete updated=%v err=%v", updated, err)
	}
	if updated, err := repo.CompleteInspectionResult(ctx, completed, claimToken, now.Add(3*time.Second)); err != nil || updated {
		t.Fatalf("duplicate complete updated=%v err=%v", updated, err)
	}
	if _, err := repo.RequestInspectionCancellation(ctx, run.ID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.TryClaimInspectionResultApplication(ctx, run.ID, completed.AccountID, claimToken, "cccccccccccccccccccccccccccccccc", now.Add(4*time.Second), now.Add(time.Minute)); err != nil || !claimed {
		t.Fatalf("application claim before cancellation=%v err=%v", claimed, err)
	}
	if requested, err := repo.InspectionCancellationRequested(ctx, run.ID, claimToken); err != nil || !requested {
		t.Fatalf("cancel requested=%v err=%v", requested, err)
	}
	if cancelled, err := repo.CancelPendingInspectionResults(ctx, run.ID, claimToken, now.Add(5*time.Second)); err != nil || cancelled != 1 {
		t.Fatalf("cancelled=%d err=%v", cancelled, err)
	}
	values, _, err := repo.ListInspectionResults(ctx, run.ID, 0, 10)
	if err != nil || len(values) != 2 {
		t.Fatalf("cancelled results=%#v err=%v", values, err)
	}
	for _, value := range values {
		if value.ApplyStatus != accountinspection.ApplyStatusSkipped || value.ApplyError != "inspection_cancelled" || value.ApplyClaimToken != "" || value.ApplyLeaseUntil != nil {
			t.Fatalf("cancelled application state=%#v", value)
		}
	}
	if updated, err := repo.FinishInspectionRun(ctx, run.ID, claimToken, accountinspection.RunStatusCancelled, "", now.Add(6*time.Second)); err != nil || !updated {
		t.Fatalf("finish updated=%v err=%v", updated, err)
	}
	finished, err := repo.GetInspectionRun(ctx, run.ID)
	if err != nil || finished.Completed != 2 || finished.Status != accountinspection.RunStatusCancelled || finished.ClaimToken != "" {
		t.Fatalf("finished=%#v err=%v", finished, err)
	}
}

func TestAccountInspectionRepositoryReclaimsExpiredLease(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountInspectionRepository(openTestDatabase(t))
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	run := accountinspection.Run{
		ID: "11111111111111111111111111111111", Provider: account.ProviderWeb, ModelRouteID: 8, UpstreamModel: "grok-chat-fast",
		Mode: accountinspection.RunModeFull, Status: accountinspection.RunStatusQueued, Concurrency: 1, Total: 1, CreatedAt: now, UpdatedAt: now,
	}
	target := accountinspection.Result{RunID: run.ID, AccountID: 1, Provider: account.ProviderWeb, AccountName: "one", AccountEnabled: true, AccountUpdatedAt: now, Model: run.UpstreamModel, Classification: accountinspection.ClassificationPending, SuggestedAction: accountinspection.ActionKeep, Confidence: accountinspection.ConfidenceLow, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInspectionRun(ctx, run, []accountinspection.Result{target}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.TryClaimInspectionRun(ctx, run.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", now, now.Add(time.Minute)); err != nil || !ok {
		t.Fatal(err)
	}
	ids, err := repo.ListClaimableInspectionRunIDs(ctx, now.Add(2*time.Minute), 10)
	if err != nil || len(ids) != 1 || ids[0] != run.ID {
		t.Fatalf("claimable=%v err=%v", ids, err)
	}
	recovered, ok, err := repo.TryClaimInspectionRun(ctx, run.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil || !ok || recovered.ClaimToken != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("recovered=%#v ok=%v err=%v", recovered, ok, err)
	}
}

func TestAccountInspectionRepositoryApplicationClaimRecoversAfterCrash(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountInspectionRepository(openTestDatabase(t))
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)
	run := accountinspection.Run{
		ID: "22222222222222222222222222222222", Provider: account.ProviderBuild, ModelRouteID: 9, UpstreamModel: "grok-test",
		Mode: accountinspection.RunModeFull, Status: accountinspection.RunStatusQueued, Concurrency: 1, Total: 1, CreatedAt: now, UpdatedAt: now,
	}
	target := accountinspection.Result{RunID: run.ID, AccountID: 1, Provider: run.Provider, AccountName: "one", AccountEnabled: true, AccountUpdatedAt: now, Model: run.UpstreamModel, Classification: accountinspection.ClassificationPending, SuggestedAction: accountinspection.ActionKeep, Confidence: accountinspection.ConfidenceLow, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateInspectionRun(ctx, run, []accountinspection.Result{target}); err != nil {
		t.Fatal(err)
	}
	runToken := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, ok, err := repo.TryClaimInspectionRun(ctx, run.ID, runToken, now, now.Add(5*time.Minute)); err != nil || !ok {
		t.Fatalf("run claim ok=%v err=%v", ok, err)
	}
	target.Classification = accountinspection.ClassificationHealthy
	target.SuggestedAction = accountinspection.ActionClearHealth
	target.Confidence = accountinspection.ConfidenceHigh
	if updated, err := repo.CompleteInspectionResult(ctx, target, runToken, now.Add(time.Second)); err != nil || !updated {
		t.Fatalf("complete updated=%v err=%v", updated, err)
	}
	firstApplyToken := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if claimed, err := repo.TryClaimInspectionResultApplication(ctx, run.ID, target.AccountID, runToken, firstApplyToken, now.Add(2*time.Second), now.Add(time.Minute)); err != nil || !claimed {
		t.Fatalf("first application claim=%v err=%v", claimed, err)
	}
	if claimed, err := repo.TryClaimInspectionResultApplication(ctx, run.ID, target.AccountID, runToken, "cccccccccccccccccccccccccccccccc", now.Add(30*time.Second), now.Add(2*time.Minute)); err != nil || claimed {
		t.Fatalf("concurrent application claim=%v err=%v", claimed, err)
	}
	recoveredApplyToken := "dddddddddddddddddddddddddddddddd"
	if claimed, err := repo.TryClaimInspectionResultApplication(ctx, run.ID, target.AccountID, runToken, recoveredApplyToken, now.Add(2*time.Minute), now.Add(3*time.Minute)); err != nil || !claimed {
		t.Fatalf("recovered application claim=%v err=%v", claimed, err)
	}
	if updated, err := repo.FinishInspectionResultApplication(ctx, run.ID, target.AccountID, runToken, firstApplyToken, accountinspection.ApplyStatusApplied, string(target.SuggestedAction), "", now.Add(2*time.Minute)); err != nil || updated {
		t.Fatalf("stale application finish=%v err=%v", updated, err)
	}
	if updated, err := repo.FinishInspectionResultApplication(ctx, run.ID, target.AccountID, runToken, recoveredApplyToken, accountinspection.ApplyStatusApplied, string(target.SuggestedAction), "", now.Add(2*time.Minute)); err != nil || !updated {
		t.Fatalf("owner application finish=%v err=%v", updated, err)
	}
	values, _, err := repo.ListInspectionResults(ctx, run.ID, 0, 10)
	if err != nil || len(values) != 1 || values[0].ApplyStatus != accountinspection.ApplyStatusApplied || values[0].ApplyAttempts != 2 || values[0].AppliedAt == nil {
		t.Fatalf("application result=%#v err=%v", values, err)
	}
}

func TestListLatestInspectionResultsUsesLatestResultPerAccount(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountInspectionRepository(openTestDatabase(t))
	base := time.Date(2026, 7, 19, 3, 0, 0, 0, time.UTC)
	older := accountinspection.Run{ID: "33333333333333333333333333333333", Provider: account.ProviderBuild, ModelRouteID: 10, UpstreamModel: "grok-test", Mode: accountinspection.RunModeFull, Status: accountinspection.RunStatusCompleted, Concurrency: 1, Total: 2, Completed: 2, CreatedAt: base, UpdatedAt: base}
	olderResults := []accountinspection.Result{
		{RunID: older.ID, AccountID: 1, Provider: older.Provider, AccountName: "one", AccountEnabled: true, AccountUpdatedAt: base, Model: older.UpstreamModel, Classification: accountinspection.ClassificationQuotaExhausted, SuggestedAction: accountinspection.ActionUpdateQuota, Confidence: accountinspection.ConfidenceHigh, CreatedAt: base, UpdatedAt: base},
		{RunID: older.ID, AccountID: 2, Provider: older.Provider, AccountName: "two", AccountEnabled: true, AccountUpdatedAt: base, Model: older.UpstreamModel, Classification: accountinspection.ClassificationReauth, SuggestedAction: accountinspection.ActionRequireReauth, Confidence: accountinspection.ConfidenceHigh, CreatedAt: base, UpdatedAt: base},
	}
	if err := repo.CreateInspectionRun(ctx, older, olderResults); err != nil {
		t.Fatal(err)
	}
	newerAt := base.Add(time.Minute)
	newer := accountinspection.Run{ID: "44444444444444444444444444444444", Provider: older.Provider, ModelRouteID: older.ModelRouteID, UpstreamModel: older.UpstreamModel, Mode: accountinspection.RunModeSelected, Status: accountinspection.RunStatusCompleted, Concurrency: 1, Total: 1, Completed: 1, CreatedAt: newerAt, UpdatedAt: newerAt}
	newerResult := accountinspection.Result{RunID: newer.ID, AccountID: 1, Provider: newer.Provider, AccountName: "one", AccountEnabled: true, AccountUpdatedAt: newerAt, Model: newer.UpstreamModel, Classification: accountinspection.ClassificationHealthy, SuggestedAction: accountinspection.ActionClearHealth, Confidence: accountinspection.ConfidenceHigh, CreatedAt: newerAt, UpdatedAt: newerAt}
	if err := repo.CreateInspectionRun(ctx, newer, []accountinspection.Result{newerResult}); err != nil {
		t.Fatal(err)
	}
	cancelledAt := base.Add(2 * time.Minute)
	cancelledRun := accountinspection.Run{ID: "55555555555555555555555555555555", Provider: older.Provider, ModelRouteID: older.ModelRouteID, UpstreamModel: older.UpstreamModel, Mode: accountinspection.RunModeRecheck, Status: accountinspection.RunStatusCancelled, Concurrency: 1, Total: 1, Completed: 1, CreatedAt: cancelledAt, UpdatedAt: cancelledAt}
	cancelledResult := accountinspection.Result{RunID: cancelledRun.ID, AccountID: 2, Provider: cancelledRun.Provider, AccountName: "two", AccountEnabled: true, AccountUpdatedAt: cancelledAt, Model: cancelledRun.UpstreamModel, Classification: accountinspection.ClassificationCancelled, SuggestedAction: accountinspection.ActionKeep, Confidence: accountinspection.ConfidenceLow, CreatedAt: cancelledAt, UpdatedAt: cancelledAt}
	if err := repo.CreateInspectionRun(ctx, cancelledRun, []accountinspection.Result{cancelledResult}); err != nil {
		t.Fatal(err)
	}
	latest, err := repo.ListLatestInspectionResults(ctx, older.Provider, nil)
	if err != nil || len(latest) != 2 || latest[0].AccountID != 1 || latest[0].Classification != accountinspection.ClassificationHealthy || latest[1].AccountID != 2 || latest[1].Classification != accountinspection.ClassificationReauth {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	abnormal, err := repo.ListLatestInspectionResults(ctx, older.Provider, []accountinspection.Classification{accountinspection.ClassificationQuotaExhausted, accountinspection.ClassificationReauth})
	if err != nil || len(abnormal) != 1 || abnormal[0].AccountID != 2 {
		t.Fatalf("latest abnormal=%#v err=%v", abnormal, err)
	}
}

func TestInitializeSchemaBackfillsTerminalInspectionApplicationState(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountInspectionRepository(database)
	now := time.Date(2026, 7, 19, 4, 0, 0, 0, time.UTC)
	run := accountinspection.Run{ID: "55555555555555555555555555555555", Provider: account.ProviderBuild, ModelRouteID: 11, UpstreamModel: "grok-test", Mode: accountinspection.RunModeFull, Status: accountinspection.RunStatusCompleted, Concurrency: 1, Total: 2, Completed: 2, CreatedAt: now, UpdatedAt: now}
	appliedAt := now.Add(time.Minute)
	results := []accountinspection.Result{
		{RunID: run.ID, AccountID: 1, Provider: run.Provider, AccountName: "applied", AccountEnabled: true, AccountUpdatedAt: now, Model: run.UpstreamModel, Classification: accountinspection.ClassificationHealthy, SuggestedAction: accountinspection.ActionClearHealth, Confidence: accountinspection.ConfidenceHigh, AppliedAction: string(accountinspection.ActionClearHealth), AppliedAt: &appliedAt, CreatedAt: now, UpdatedAt: now},
		{RunID: run.ID, AccountID: 2, Provider: run.Provider, AccountName: "legacy", AccountEnabled: true, AccountUpdatedAt: now, Model: run.UpstreamModel, Classification: accountinspection.ClassificationProbeError, SuggestedAction: accountinspection.ActionKeep, Confidence: accountinspection.ConfidenceLow, CreatedAt: now, UpdatedAt: now},
	}
	if err := repo.CreateInspectionRun(ctx, run, results); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	values, _, err := repo.ListInspectionResults(ctx, run.ID, 0, 10)
	if err != nil || len(values) != 2 {
		t.Fatalf("values=%#v err=%v", values, err)
	}
	if values[0].ApplyStatus != accountinspection.ApplyStatusApplied || values[1].ApplyStatus != accountinspection.ApplyStatusSkipped || values[1].ApplyError != "terminal_result_not_auto_applied" {
		t.Fatalf("backfilled values=%#v", values)
	}
}
