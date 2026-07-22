package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/domain/audit"
	"github.com/owen891/grok2api/backend/internal/infra/persistence/relational"
	"github.com/owen891/grok2api/backend/internal/infra/runtime/memory"
	"github.com/owen891/grok2api/backend/internal/repository"
)

func TestSelectorBlocksObservedFreeRollingUsageWithoutRecoveryRow(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-free-usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	audits := relational.NewAuditRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "over-limit", SourceKey: "over-limit", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1, ObservedModel: "grok-test-build-free",
	})
	if err != nil {
		t.Fatal(err)
	}
	accountID := value.ID
	if err := audits.Create(ctx, audit.Record{RequestID: "over-limit", ClientKeyID: 1, ModelRouteID: 1, AccountID: &accountID, StatusCode: 200, TotalTokens: account.EstimatedFreeTokenLimit + 17, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	_, err = selector.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionQuotaExhausted {
		t.Fatalf("error = %v, want quota exhausted", err)
	}
}

func TestInspectionHealthyRestoresOnlyTheVerifiedModel(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-inspection-healthy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	audits := relational.NewAuditRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "verified", SourceKey: "verified", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1, ObservedModel: "grok-test-build-free",
	})
	if err != nil {
		t.Fatal(err)
	}
	accountID := credential.ID
	if err := audits.Create(ctx, audit.Record{RequestID: "verified-over-limit", ClientKeyID: 1, ModelRouteID: 1, AccountID: &accountID, StatusCode: 200, TotalTokens: account.EstimatedFreeTokenLimit + 17, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Hour)
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{AccountID: credential.ID, UpstreamModel: "verified-model", Reason: "quota", CooldownUntil: until}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{AccountID: credential.ID, UpstreamModel: "other-model", Reason: "quota", CooldownUntil: until}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if err := selector.ApplyInferenceHealth(ctx, credential.ID, "verified-model", account.InferenceHealthHealthy, 200, ""); err != nil {
		t.Fatal(err)
	}
	if err := selector.ApplyInspectionHealthy(ctx, credential, "verified-model"); err != nil {
		t.Fatal(err)
	}
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "verified-model", "", "", nil, false)
	if err != nil {
		t.Fatalf("verified model was not restored: %v", err)
	}
	lease.Release()
	if _, err := selector.Acquire(ctx, account.ProviderBuild, "other-model", "", "", nil, false); err == nil {
		t.Fatal("inspection cleared another model's cooldown")
	}
}

func TestCapacitySnapshotUsesRoutingPolicyAndFutureRecoveryWindow(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "capacity-snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	now := time.Now().UTC()
	create := func(name string, remaining int, resetAt *time.Time) uint64 {
		value, _, createErr := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: name, SourceKey: name,
			EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if saveErr := accounts.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{{
			AccountID: value.ID, Mode: "fast", Remaining: remaining, ResetAt: resetAt, Source: account.QuotaSourceUpstream,
		}}); saveErr != nil {
			t.Fatal(saveErr)
		}
		return value.ID
	}
	past := now.Add(-time.Minute)
	soon := now.Add(5 * time.Minute)
	create("expired", 0, &past)
	create("soon", 0, &soon)
	readyID := create("ready", 1, nil)
	limiter := &batchConcurrencyLimiter{values: map[string]int{"account:" + fmt.Sprint(readyID): 1}}
	selector := NewSelector(accounts, limiter, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	snapshot, err := selector.CapacitySnapshot(ctx, account.ProviderWeb, "", "fast", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Total != 3 || snapshot.Eligible != 1 || snapshot.QuotaExhausted != 2 || snapshot.RecoveringSoon != 1 || snapshot.InFlight != 1 || snapshot.TotalSlots != 2 || snapshot.AvailableSlots != 1 || snapshot.EarliestRecovery == nil || !snapshot.EarliestRecovery.Equal(soon) {
		t.Fatalf("capacity snapshot = %#v", snapshot)
	}
	if limiter.batchCalls != 1 || limiter.currentCalls != 0 {
		t.Fatalf("concurrency reads batch=%d current=%d", limiter.batchCalls, limiter.currentCalls)
	}
}

func TestSelectorPrioritizesDueQuotaProbeOnce(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	probe, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "probe", SourceKey: "probe", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	active, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "active", SourceKey: "active", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 200, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	due := now.Add(-time.Minute)
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: probe.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		ConfirmedUsed: 1_065_387, ConfirmedLimit: 1_000_000,
		ExhaustedAt: &now, NextProbeAt: &due, LastConfirmedAt: &now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != probe.ID || !lease.QuotaProbe {
		t.Fatalf("lease = %#v, want due probe account %d", lease, probe.ID)
	}
	lease.Release()

	lease, err = selector.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", map[uint64]bool{probe.ID: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Credential.ID != active.ID || lease.QuotaProbe {
		t.Fatalf("lease = %#v, want active account %d", lease, active.ID)
	}
	lease.Release()

	selector.MarkSuccess(ctx, probe)
	if _, err := accounts.GetQuotaRecovery(ctx, probe.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("quota recovery should be cleared, err = %v", err)
	}
}

func TestSelectorSkipsQuotaProbeBeforeDue(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "waiting", SourceKey: "waiting", EncryptedAccessToken: "encrypted", Enabled: true,
		AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: value.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		NextProbeAt: &next, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", map[uint64]bool{}, true); err == nil {
		t.Fatal("expected no account before next probe time")
	}
}

func TestSelectorSkipsWaitingResetAccountEvenWhenModeWindowHasRemaining(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-recovery-window.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "waiting-reset", SourceKey: "waiting-reset", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	next := now.Add(24 * time.Hour)
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: value.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		NextProbeAt: &next, ExhaustedAt: &now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{{
		AccountID: value.ID, Mode: "fast", Remaining: 30, Total: 30, ResetAt: &next, SyncedAt: &now,
	}}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderWeb, "grok-chat-fast", "fast", "", nil, false); err == nil {
		t.Fatal("waiting-reset account with a positive mode window must not be selected")
	}
}

func TestSelectorUsesPaidWeeklyPoolAsWebQuotaGate(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "weekly-web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "paid-web", SourceKey: "paid-web",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	resetAt := now.Add(7 * 24 * time.Hour)
	if err := accounts.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: value.ID, Mode: "weekly", Remaining: 0, Total: 10000, UsagePercent: 100, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
		{AccountID: value.ID, Mode: "fast", Remaining: 30, Total: 30, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderWeb, "", "fast", "", nil, false); err == nil {
		t.Fatal("exhausted weekly pool must take precedence over a stale fast quota window")
	}
	if err := accounts.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: value.ID, Mode: "weekly", Remaining: 8900, Total: 10000, UsagePercent: 11, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
		{AccountID: value.ID, Mode: "fast", Remaining: 0, Total: 30, ResetAt: &resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	selector.MarkQuotaStateChanged(account.ProviderWeb)
	lease, err := selector.Acquire(ctx, account.ProviderWeb, "", "fast", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.QuotaMode != "weekly" {
		t.Fatalf("quota mode = %q, want weekly", lease.QuotaMode)
	}
}

func TestSelectorClaimsPaidBillingProbeAfterPeriodEnd(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "paid-probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "paid", SourceKey: "paid", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	due := now.Add(-time.Minute)
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{AccountID: value.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted, NextProbeAt: &due, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if !lease.QuotaProbe || lease.QuotaProbeKind != account.QuotaRecoveryKindPaid {
		t.Fatalf("lease = %#v", lease)
	}
}

func TestSelectorPinnedInferenceRejectsRecoveryAccount(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "pinned-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "recovering", SourceKey: "recovering", EncryptedAccessToken: "encrypted",
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	due := now.Add(-time.Minute)
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: value.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted,
		NextProbeAt: &due, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	_, err = selector.AcquirePinned(ctx, account.ProviderBuild, value.ID, "grok-test", "", true)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionQuotaExhausted {
		t.Fatalf("error = %v, want quota exhausted", err)
	}
	recovery, err := accounts.GetQuotaRecovery(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Status != account.QuotaRecoveryStatusExhausted {
		t.Fatalf("recovery status = %s, want exhausted", recovery.Status)
	}
}

func TestSelectorOnlyUsesAccountsSupportingRequestedModel(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-model.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	unsupported, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "basic", SourceKey: "basic", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
		Priority: 500, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	supported, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "premium", SourceKey: "premium", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive,
		Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := models.ReplaceAccountCapabilities(ctx, unsupported.ID, []string{"grok-basic"}, now); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, supported.ID, []string{"grok-basic", "grok-premium"}, now); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "grok-premium", "", "", map[uint64]bool{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != supported.ID {
		t.Fatalf("selected account = %d, want %d", lease.Credential.ID, supported.ID)
	}
}

func TestSelectorKeepsWebQuotaModesIsolated(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-web-quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web", SourceKey: "web", EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	resetAt := now.Add(time.Hour)
	if err := accounts.SaveQuotaWindows(ctx, value.ID, account.WebTierSuper, now, []account.QuotaWindow{
		{AccountID: value.ID, Mode: "fast", Remaining: 0, Total: 20, ResetAt: &resetAt, Source: account.QuotaSourceUpstream},
		{AccountID: value.ID, Mode: "auto", Remaining: 5, Total: 10, ResetAt: &resetAt, Source: account.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderWeb, "grok-chat", "fast", "", nil, false); err == nil {
		t.Fatal("exhausted fast mode should not be selected")
	}
	lease, err := selector.Acquire(ctx, account.ProviderWeb, "grok-chat-auto", "auto", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != value.ID || lease.QuotaMode != "auto" {
		t.Fatalf("lease = %#v", lease)
	}
}

func TestSelectorHonorsWebTierPoolOrderBeforeAccountPriority(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-web-tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	for index, tier := range []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy} {
		if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: tier,
			Name: string(tier), SourceKey: string(tier), EncryptedAccessToken: "encrypted",
			AuthStatus: account.AuthStatusActive, Priority: 300 - index*100, MaxConcurrent: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), staticTierOrder{order: []account.WebTier{account.WebTierHeavy, account.WebTierSuper, account.WebTierBasic}}, time.Hour, time.Second, time.Minute)
	lease, err := selector.Acquire(ctx, account.ProviderWeb, "fast-prefer-best", "fast", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.WebTier != account.WebTierHeavy {
		t.Fatalf("selected tier = %s", lease.Credential.WebTier)
	}
}

func TestSelectorPropagatesConcurrencyStoreFailure(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-runtime-error.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "active", SourceKey: "active", EncryptedAccessToken: "encrypted",
		AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}

	runtimeErr := errors.New("runtime store unavailable")
	selector := NewSelector(accounts, failingConcurrencyLimiter{err: runtimeErr}, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderBuild, "", "", "", map[uint64]bool{}, true); !errors.Is(err, runtimeErr) {
		t.Fatalf("Acquire error = %v, want wrapped runtime error", err)
	}
}

func TestPromptCacheStickyKeyIsFixedLengthAndStable(t *testing.T) {
	first := promptCacheStickyKey("cache-key")
	if len(first) != 64 || first != promptCacheStickyKey("cache-key") {
		t.Fatalf("sticky key = %q", first)
	}
	if first == promptCacheStickyKey("another-key") {
		t.Fatal("different prompt cache keys produced the same sticky key")
	}
	if promptCacheStickyKey("") != "" {
		t.Fatal("empty prompt cache key should remain empty")
	}
}

func TestSelectorUsesBatchConcurrencySnapshot(t *testing.T) {
	limiter := &batchConcurrencyLimiter{values: map[string]int{"account:1": 2, "account:2": 1}}
	selector := &Selector{concurrency: limiter, lastSelectedAt: make(map[uint64]time.Time)}
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Priority: 1}},
		{Credential: account.Credential{ID: 2, Priority: 1}},
	}
	if err := selector.sortCandidates(context.Background(), values, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	if limiter.batchCalls != 1 || limiter.currentCalls != 0 || values[0].Credential.ID != 2 {
		t.Fatalf("batchCalls=%d currentCalls=%d values=%#v", limiter.batchCalls, limiter.currentCalls, values)
	}
}

func TestSelectorPrefersHealthyPeerWithEqualPriority(t *testing.T) {
	selector := &Selector{
		concurrency:    memory.NewConcurrencyLimiter(),
		lastSelectedAt: make(map[uint64]time.Time),
	}
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Priority: 10, FailureCount: 2}},
		{Credential: account.Credential{ID: 2, Priority: 10}},
	}
	if err := selector.sortCandidates(context.Background(), values, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	if values[0].Credential.ID != 2 {
		t.Fatalf("sorted candidates = %#v, want healthy account first", values)
	}
}

func TestSelectorPrefersFreshHigherWebQuotaAtEqualLoad(t *testing.T) {
	now := time.Now().UTC()
	selector := &Selector{
		concurrency:    memory.NewConcurrencyLimiter(),
		lastSelectedAt: make(map[uint64]time.Time),
	}
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Priority: 10}, QuotaWindow: &account.QuotaWindow{Remaining: 1, Total: 10, SyncedAt: &now}},
		{Credential: account.Credential{ID: 2, Priority: 10}, QuotaWindow: &account.QuotaWindow{Remaining: 8, Total: 10, SyncedAt: &now}},
	}
	if err := selector.sortCandidates(context.Background(), values, now, nil); err != nil {
		t.Fatal(err)
	}
	if values[0].Credential.ID != 2 {
		t.Fatalf("sorted candidates = %#v, want higher remaining quota first", values)
	}
}

func TestSelectorUsesModelScopedRecentPerformance(t *testing.T) {
	now := time.Now().UTC()
	selector := &Selector{
		concurrency:    memory.NewConcurrencyLimiter(),
		lastSelectedAt: make(map[uint64]time.Time),
		performance:    make(map[routePerformanceKey]routePerformance),
	}
	selector.ObserveRouteResult(1, "image-model", 100*time.Millisecond, false)
	selector.ObserveRouteResult(2, "image-model", 2*time.Second, true)
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 1, Priority: 10}},
		{Credential: account.Credential{ID: 2, Priority: 10}},
	}
	if err := selector.sortCandidates(context.Background(), values, now, nil, "image-model"); err != nil {
		t.Fatal(err)
	}
	if values[0].Credential.ID != 2 {
		t.Fatalf("performance-ranked candidates = %#v, want successful account first", values)
	}
	if err := selector.sortCandidates(context.Background(), values, now, nil, "different-model"); err != nil {
		t.Fatal(err)
	}
	if values[0].Credential.ID != 1 {
		t.Fatalf("performance leaked across models: %#v", values)
	}
}

func TestSelectorSharesAccountModelCircuitAcrossInstances(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "shared-circuit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "circuit", SourceKey: "circuit", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	shared := memory.NewRoutePerformanceStore()
	first := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	second := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	first.SetRoutePerformanceRepository(shared)
	second.SetRoutePerformanceRepository(shared)
	decision := FailureDecision{Scope: FailureScopeAccount, Action: FailureActionRotateAccount, PenalizeAccount: true, Retryable: true}
	for range 3 {
		first.ObserveRouteFailure(value.ID, "grok-test", time.Second, decision)
	}
	_, err = second.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionModelCooling || unavailable.RetryAfter <= 0 {
		t.Fatalf("shared circuit error=%v", err)
	}
	_, err = second.AcquirePinned(ctx, account.ProviderBuild, value.ID, "grok-test", "", true)
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionModelCooling {
		t.Fatalf("pinned shared circuit error=%v", err)
	}
	lease, err := second.Acquire(ctx, account.ProviderBuild, "different-model", "", "", nil, false)
	if err != nil {
		t.Fatalf("circuit leaked across models: %v", err)
	}
	lease.Release()
}

func TestSelectorFailsOpenWhenSharedPerformanceStoreIsUnavailable(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "performance-fail-open.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "healthy", SourceKey: "healthy", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	selector.SetRoutePerformanceRepository(failingRoutePerformanceStore{})
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", nil, false)
	if err != nil || lease == nil || lease.Credential.ID != value.ID {
		t.Fatalf("fail-open lease=%#v err=%v", lease, err)
	}
	lease.Release()
}

func TestSelectorRoutesToVerifiedHealthyPoolBeforeDeniedAndPendingAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "verified-pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	const upstreamModel = "grok-4.5"
	create := func(index int, status string) account.Credential {
		t.Helper()
		name := fmt.Sprintf("account-%03d", index)
		value, _, createErr := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted",
			Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		now := time.Now().UTC()
		if writeErr := accounts.SetInferenceHealth(ctx, value.ID, upstreamModel, status, &now, 200, ""); writeErr != nil {
			t.Fatal(writeErr)
		}
		return value
	}
	for index := range 100 {
		value := create(index, account.InferenceHealthPermissionDenied)
		if err := accounts.SetInferenceHealth(ctx, value.ID, upstreamModel, account.InferenceHealthPermissionDenied, nil, 403, "upstream_account_permission_denied"); err != nil {
			t.Fatal(err)
		}
	}
	for index := 100; index < 120; index++ {
		create(index, account.InferenceHealthPending)
	}
	healthy := make(map[uint64]bool)
	for index := 120; index < 125; index++ {
		healthy[create(index, account.InferenceHealthHealthy).ID] = true
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	for attempt := range 10 {
		lease, acquireErr := selector.Acquire(ctx, account.ProviderBuild, upstreamModel, "", "", nil, false)
		if acquireErr != nil {
			t.Fatalf("acquire %d: %v", attempt, acquireErr)
		}
		if !healthy[lease.Credential.ID] {
			t.Fatalf("acquire %d selected unverified account %d", attempt, lease.Credential.ID)
		}
		lease.Release()
	}
}

func TestSelectorReportsInferenceDeniedWhenAllAccountsAreIsolated(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "inference-denied.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "denied", SourceKey: "denied", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.SetInferenceHealth(ctx, credential.ID, "grok-denied", account.InferenceHealthPermissionDenied, nil, 403, "upstream_account_permission_denied"); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	_, err = selector.Acquire(ctx, account.ProviderBuild, "grok-denied", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionInferenceDenied {
		t.Fatalf("error=%v", err)
	}
}

func TestInferenceSuccessImmediatelyClearsARecentDenial(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "inference-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "recovered", SourceKey: "recovered", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	const upstreamModel = "grok-recovered"
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	selector.MarkInferenceHealthy(ctx, credential.ID, upstreamModel)
	selector.MarkInferenceDenied(ctx, credential.ID, upstreamModel, http.StatusForbidden, "permission_denied")
	selector.MarkInferenceHealthy(ctx, credential.ID, upstreamModel)
	candidates, err := accounts.ListRoutingCandidates(ctx, account.ProviderBuild, upstreamModel, "")
	if err != nil || len(candidates) != 1 || candidates[0].InferenceHealth == nil || candidates[0].InferenceHealth.Status != account.InferenceHealthHealthy {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
}

func TestSelectorDoesNotReportAllAccountsIsolatedForMixedQuotaAndHealthBlocks(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "mixed-inference-blocks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	create := func(name string) account.Credential {
		value, _, createErr := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted",
			Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return value
	}
	const upstreamModel = "grok-mixed-blocks"
	denied := create("denied")
	quota := create("quota")
	if err := accounts.SetInferenceHealth(ctx, denied.ID, upstreamModel, account.InferenceHealthPermissionDenied, nil, http.StatusForbidden, "permission_denied"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	if err := accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: quota.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted,
		ExhaustedAt: &now, NextProbeAt: &next, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	_, err = selector.Acquire(ctx, account.ProviderBuild, upstreamModel, "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionQuotaExhausted {
		t.Fatalf("error=%v", err)
	}
}

type failingRoutePerformanceStore struct{}

func (failingRoutePerformanceStore) ObserveRoutePerformance(context.Context, repository.RoutePerformanceObservation, repository.RoutePerformancePolicy) error {
	return errors.New("runtime store unavailable")
}

func (failingRoutePerformanceStore) GetRoutePerformances(context.Context, []repository.RoutePerformanceKey, time.Time) (map[repository.RoutePerformanceKey]repository.RoutePerformance, error) {
	return nil, errors.New("runtime store unavailable")
}

func TestSelectorRoundRobinsOnlyHealthyAccountsInIDOrder(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "round-robin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	create := func(name string, authStatus account.AuthStatus, priority int) account.Credential {
		t.Helper()
		value, _, createErr := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted",
			Enabled: true, AuthStatus: authStatus, Priority: priority, MaxConcurrent: 2,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return value
	}
	first := create("first", account.AuthStatusActive, 1)
	second := create("second", account.AuthStatusActive, 1000)
	third := create("third", account.AuthStatusActive, 10)
	disabled := create("disabled", account.AuthStatusActive, 2000)
	disabled.Enabled = false
	if _, err := accounts.Update(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	create("reauth", account.AuthStatusReauthRequired, 2000)
	cooling := create("cooling", account.AuthStatusActive, 2000)
	until := time.Now().UTC().Add(time.Hour)
	if err := accounts.UpdateHealth(ctx, cooling.ID, 1, &until, "cooling", false); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	want := []uint64{first.ID, second.ID, third.ID, first.ID, second.ID, third.ID}
	for index, expected := range want {
		lease, acquireErr := selector.Acquire(ctx, account.ProviderBuild, "grok-test", "", "", nil, false)
		if acquireErr != nil {
			t.Fatalf("acquire %d: %v", index, acquireErr)
		}
		if lease.Credential.ID != expected {
			t.Fatalf("acquire %d selected account %d, want %d", index, lease.Credential.ID, expected)
		}
		lease.Release()
	}
}

func TestSelectorReportsReauthWhenNoActiveAccountRemains(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-reauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "reauth-only", SourceKey: "reauth-only",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusReauthRequired,
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	_, err = selector.Acquire(ctx, account.ProviderBuild, "grok-reauth-only", "", "", nil, false)
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != SelectionReauthRequired {
		t.Fatalf("account %d selection error = %v", value.ID, err)
	}
}

func TestSelectorSuccessClearsPersistedFailureHealth(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "recovering", SourceKey: "recovering", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, FailureCount: 3, LastError: "upstream status 502",
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	selector.MarkSuccess(ctx, value)
	updated, err := accounts.Get(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FailureCount != 0 || updated.LastError != "" {
		t.Fatalf("updated health = %#v", updated)
	}
}

func TestSelectorConsumesOnlyMatchingQuotaSnapshot(t *testing.T) {
	key := candidateCacheKey{provider: account.ProviderWeb, upstreamModel: "chat", quotaMode: "fast"}
	selector := &Selector{candidates: map[candidateCacheKey]candidateSnapshot{
		key: {values: []account.RoutingCandidate{{
			Credential: account.Credential{ID: 7}, QuotaWindow: &account.QuotaWindow{AccountID: 7, Mode: "fast", Remaining: 10},
		}}},
	}}
	selector.ConsumeQuota(account.ProviderWeb, 7, "fast", 3)
	window := selector.candidates[key].values[0].QuotaWindow
	if window == nil || window.Remaining != 7 {
		t.Fatalf("quota window = %#v", window)
	}
}

func TestSelectorWaitsBrieflyForAccountCapacity(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "capacity-wait.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	if _, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "capacity", SourceKey: "capacity", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute, 300*time.Millisecond)
	first, err := selector.Acquire(ctx, account.ProviderBuild, "model", "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease *accountLease
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		lease, acquireErr := selector.Acquire(ctx, account.ProviderBuild, "model", "", "", nil, false)
		resultCh <- result{lease: lease, err: acquireErr}
	}()
	select {
	case value := <-resultCh:
		t.Fatalf("second acquire returned before capacity release: %v", value.err)
	case <-time.After(30 * time.Millisecond):
	}
	first.Release()
	select {
	case value := <-resultCh:
		if value.err != nil || value.lease == nil {
			t.Fatalf("second acquire lease=%v err=%v", value.lease, value.err)
		}
		value.lease.Release()
	case <-time.After(time.Second):
		t.Fatal("second acquire did not wake after capacity release")
	}
}

func TestSelectorAppliesPersistedCooldownOnlyToMatchingModel(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-cooldown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, Name: "model-cooling", SourceKey: "model-cooling", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Hour)
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{AccountID: credential.ID, UpstreamModel: "limited-model", Reason: "test", CooldownUntil: until}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{AccountID: credential.ID, UpstreamModel: "limited-model", Reason: "shorter", CooldownUntil: time.Now().UTC().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	if _, err := selector.Acquire(ctx, account.ProviderBuild, "limited-model", "", "", nil, false); err == nil {
		t.Fatal("matching model cooldown was ignored")
	} else {
		var unavailable *SelectionUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != SelectionModelCooling || unavailable.RetryAfter < 30*time.Minute {
			t.Fatalf("error = %v", err)
		}
	}
	lease, err := selector.Acquire(ctx, account.ProviderBuild, "other-model", "", "", nil, false)
	if err != nil {
		t.Fatalf("other model was blocked: %v", err)
	}
	lease.Release()
}

type failingConcurrencyLimiter struct{ err error }

type batchConcurrencyLimiter struct {
	values       map[string]int
	batchCalls   int
	currentCalls int
}

func (l *batchConcurrencyLimiter) Acquire(context.Context, string, int) (func(), bool, error) {
	return func() {}, true, nil
}

func (l *batchConcurrencyLimiter) Current(context.Context, string) (int, error) {
	l.currentCalls++
	return 0, nil
}

func (l *batchConcurrencyLimiter) CurrentMany(_ context.Context, keys []string) (map[string]int, error) {
	l.batchCalls++
	values := make(map[string]int, len(keys))
	for _, key := range keys {
		values[key] = l.values[key]
	}
	return values, nil
}

type staticTierOrder struct{ order []account.WebTier }

func (value staticTierOrder) TierOrder(account.Provider, string) []account.WebTier {
	return value.order
}

func (f failingConcurrencyLimiter) Acquire(context.Context, string, int) (func(), bool, error) {
	return nil, false, f.err
}

func (f failingConcurrencyLimiter) Current(context.Context, string) (int, error) {
	return 0, nil
}
