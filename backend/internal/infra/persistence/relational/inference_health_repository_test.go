package relational

import (
	"context"
	"testing"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/domain/accountinspection"
)

func TestListRoutingCandidatesIgnoresExpiredInferenceHealth(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "stale-health", SourceKey: "stale-health",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	const upstreamModel = "grok-stale-health"
	if err := accounts.SetInferenceHealth(ctx, credential.ID, upstreamModel, account.InferenceHealthPermissionDenied, nil, 403, "upstream_account_permission_denied"); err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().UTC().Add(-account.InferenceHealthMaxAge - time.Minute)
	if err := database.db.WithContext(ctx).Model(&accountModelInferenceHealthModel{}).
		Where("account_id = ? AND upstream_model = ?", credential.ID, upstreamModel).
		Update("updated_at", staleAt).Error; err != nil {
		t.Fatal(err)
	}

	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, upstreamModel, "")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
	if candidates[0].InferenceHealth != nil {
		t.Fatalf("expired inference health still loaded: %#v", candidates[0].InferenceHealth)
	}
}

func TestInitializeSchemaBackfillsOnlyNewerRecentInferenceEvidence(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	inspections := NewAccountInspectionRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "backfill", SourceKey: "backfill",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	const upstreamModel = "grok-backfill"
	if err := accounts.SetInferenceHealth(ctx, credential.ID, upstreamModel, account.InferenceHealthPermissionDenied, nil, 403, "old_denial"); err != nil {
		t.Fatal(err)
	}
	oldAt := time.Now().UTC().Add(-time.Hour)
	if err := database.db.WithContext(ctx).Model(&accountModelInferenceHealthModel{}).
		Where("account_id = ? AND upstream_model = ?", credential.ID, upstreamModel).
		Update("updated_at", oldAt).Error; err != nil {
		t.Fatal(err)
	}
	evidenceAt := time.Now().UTC().Add(-time.Minute)
	run := accountinspection.Run{
		ID: "77777777777777777777777777777777", Provider: account.ProviderBuild, ModelRouteID: 1,
		UpstreamModel: upstreamModel, Mode: accountinspection.RunModeFull, Status: accountinspection.RunStatusCompleted,
		Concurrency: 1, Total: 1, Completed: 1, CreatedAt: evidenceAt, UpdatedAt: evidenceAt,
	}
	result := accountinspection.Result{
		RunID: run.ID, AccountID: credential.ID, Provider: account.ProviderBuild, AccountName: credential.Name,
		AccountEnabled: true, AccountUpdatedAt: credential.UpdatedAt, Model: upstreamModel,
		Classification: accountinspection.ClassificationHealthy, SuggestedAction: accountinspection.ActionClearHealth,
		Confidence: accountinspection.ConfidenceHigh, HTTPStatus: 200, CreatedAt: evidenceAt, UpdatedAt: evidenceAt,
	}
	if err := inspections.CreateInspectionRun(ctx, run, []accountinspection.Result{result}); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var health accountModelInferenceHealthModel
	if err := database.db.WithContext(ctx).First(&health, "account_id = ? AND upstream_model = ?", credential.ID, upstreamModel).Error; err != nil {
		t.Fatal(err)
	}
	if health.Status != account.InferenceHealthHealthy || health.HTTPStatus != 200 {
		t.Fatalf("backfilled health=%#v", health)
	}

	if err := accounts.SetInferenceHealth(ctx, credential.ID, upstreamModel, account.InferenceHealthPermissionDenied, nil, 403, "new_denial"); err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).First(&health, "account_id = ? AND upstream_model = ?", credential.ID, upstreamModel).Error; err != nil {
		t.Fatal(err)
	}
	if health.Status != account.InferenceHealthPermissionDenied || health.ErrorCode != "new_denial" {
		t.Fatalf("newer live health was overwritten: %#v", health)
	}
}
