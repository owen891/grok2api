package relational

import (
	"context"
	"testing"
	"time"

	registrationdomain "github.com/owen891/grok2api/backend/internal/domain/registration"
	"github.com/owen891/grok2api/backend/internal/repository"
)

func TestReplenishmentRepositoryPersistsDailyLimitAndResetsNextDay(t *testing.T) {
	ctx := context.Background()
	repo := NewReplenishmentRepository(openTestDatabase(t))
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	claim := repository.ReplenishmentClaim{
		Scope: "grok_web:grok-imagine-image:fast", ClaimToken: "0123456789abcdef0123456789abcdef", Now: now,
		LeaseUntil: now.Add(2 * time.Minute), NextAt: now.Add(30 * time.Minute), DailyLimit: 1,
	}
	state, claimed, err := repo.ClaimReplenishment(ctx, claim)
	if err != nil || !claimed || state.DailyStarts != 1 || state.Status != registrationdomain.ReplenishmentStarting {
		t.Fatalf("first claim state=%#v claimed=%v err=%v", state, claimed, err)
	}
	originalUpdatedAt := state.UpdatedAt
	if updated, renewErr := repo.RenewReplenishment(ctx, claim.Scope, "fedcba9876543210fedcba9876543210", now.Add(3*time.Minute), now.Add(time.Minute)); renewErr != nil || updated {
		t.Fatalf("stale renew updated=%v err=%v", updated, renewErr)
	}
	if updated, renewErr := repo.RenewReplenishment(ctx, claim.Scope, claim.ClaimToken, now.Add(3*time.Minute), now.Add(time.Minute)); renewErr != nil || !updated {
		t.Fatalf("owner renew updated=%v err=%v", updated, renewErr)
	}
	if renewed, loadErr := repo.GetReplenishmentState(ctx, claim.Scope); loadErr != nil || renewed.LeaseUntil == nil || !renewed.LeaseUntil.Equal(now.Add(3*time.Minute)) || !renewed.UpdatedAt.Equal(originalUpdatedAt) {
		t.Fatalf("renewed state=%#v err=%v", renewed, loadErr)
	}
	if expired, expireErr := repo.ExpireReplenishment(ctx, claim.Scope, claim.ClaimToken, now.Add(2*time.Minute), now.Add(12*time.Minute), "stale read"); expireErr != nil || expired {
		t.Fatalf("renewed claim expired=%v err=%v", expired, expireErr)
	}
	if updated, err := repo.FinishReplenishment(ctx, claim.Scope, "fedcba9876543210fedcba9876543210", registrationdomain.ReplenishmentFailed, now, "stale", true, now); err != nil || updated {
		t.Fatalf("stale finish updated=%v err=%v", updated, err)
	}
	if state, err = repo.GetReplenishmentState(ctx, claim.Scope); err != nil || state.ClaimToken != claim.ClaimToken || state.Status != registrationdomain.ReplenishmentStarting {
		t.Fatalf("state after stale finish=%#v err=%v", state, err)
	}
	if updated, err := repo.FinishReplenishment(ctx, claim.Scope, claim.ClaimToken, registrationdomain.ReplenishmentCooling, now, "", true, now); err != nil || !updated {
		t.Fatal(err)
	}
	if state, claimed, err = repo.ClaimReplenishment(ctx, claim); err != nil || claimed || state.DailyStarts != 1 {
		t.Fatalf("same-day claim state=%#v claimed=%v err=%v", state, claimed, err)
	}
	nextDay := now.Add(24 * time.Hour)
	claim.Now = nextDay
	claim.LeaseUntil = nextDay.Add(2 * time.Minute)
	claim.NextAt = nextDay.Add(30 * time.Minute)
	if state, claimed, err = repo.ClaimReplenishment(ctx, claim); err != nil || !claimed || state.DailyStarts != 1 || !state.CounterDate.Equal(nextDay.Truncate(24*time.Hour)) {
		t.Fatalf("next-day claim state=%#v claimed=%v err=%v", state, claimed, err)
	}
}
