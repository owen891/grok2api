package account

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/infra/persistence/relational"
	memoryruntime "github.com/owen891/grok2api/backend/internal/infra/runtime/memory"
	"github.com/owen891/grok2api/backend/internal/repository"
)

func TestAutoCleanReauthIsOptInAndSkipsDisabledOrActiveAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auto-clean.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	concurrency := memoryruntime.NewConcurrencyLimiter()
	service := NewService(repo, nil, nil, memoryruntime.NewStickyStore(), nil, nil, nil)
	service.SetConcurrency(concurrency)

	create := func(name string, status accountdomain.AuthStatus, enabled bool) accountdomain.Credential {
		t.Helper()
		value, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: accountdomain.ProviderBuild, Name: name, SourceKey: "auto-clean-" + name,
			EncryptedAccessToken: "token", Enabled: enabled, AuthStatus: status,
		})
		if err != nil {
			t.Fatal(err)
		}
		if value.Enabled != enabled || value.AuthStatus != status {
			value.Enabled = enabled
			value.AuthStatus = status
			value, err = repo.Update(ctx, value)
			if err != nil {
				t.Fatal(err)
			}
		}
		return value
	}

	enabledReauth := create("enabled-reauth", accountdomain.AuthStatusReauthRequired, true)
	disabledReauth := create("disabled-reauth", accountdomain.AuthStatusReauthRequired, false)
	active := create("active", accountdomain.AuthStatusActive, true)

	service.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }
	_, revision := service.autoCleanSnapshot()
	if err := service.runAutoCleanReauth(ctx, AutoCleanConfig{}, revision); err != nil {
		t.Fatal(err)
	}
	assertAccountPresent(t, repo, enabledReauth.ID)

	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: time.Minute, MinAge: time.Hour})
	_, revision = service.autoCleanSnapshot()
	if err := service.runAutoCleanReauth(ctx, AutoCleanConfig{Enabled: true, Interval: time.Minute, MinAge: time.Hour}, revision); err != nil {
		t.Fatal(err)
	}
	assertAccountMissing(t, repo, enabledReauth.ID)
	assertAccountPresent(t, repo, disabledReauth.ID)
	assertAccountPresent(t, repo, active.ID)

	service.UpdateAutoCleanConfig(AutoCleanConfig{Enabled: true, Interval: time.Minute, MinAge: time.Hour, IncludeDisabled: true})
	release, acquired, err := concurrency.Acquire(ctx, repository.AccountConcurrencyKey(disabledReauth.ID), 1)
	if err != nil || !acquired {
		t.Fatalf("acquire active account lease: acquired=%v err=%v", acquired, err)
	}
	current, revision := service.autoCleanSnapshot()
	if err := service.runAutoCleanReauth(ctx, current, revision); err != nil {
		t.Fatal(err)
	}
	assertAccountPresent(t, repo, disabledReauth.ID)
	release()

	current, revision = service.autoCleanSnapshot()
	if err := service.runAutoCleanReauth(ctx, current, revision); err != nil {
		t.Fatal(err)
	}
	assertAccountMissing(t, repo, disabledReauth.ID)
}

func assertAccountMissing(t *testing.T, repo *relational.AccountRepository, id uint64) {
	t.Helper()
	if _, err := repo.Get(context.Background(), id); err == nil {
		t.Fatalf("account %d still exists", id)
	}
}

func assertAccountPresent(t *testing.T, repo *relational.AccountRepository, id uint64) {
	t.Helper()
	if _, err := repo.Get(context.Background(), id); err != nil {
		t.Fatalf("account %d missing: %v", id, err)
	}
}
