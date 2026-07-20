package repository

import (
	"context"
	"time"

	registrationdomain "github.com/owen891/grok2api/backend/internal/domain/registration"
)

type ReplenishmentClaim struct {
	Scope            string
	ClaimToken       string
	Now              time.Time
	LeaseUntil       time.Time
	NextAt           time.Time
	DailyLimit       int
	BaselineEligible int
}

type ReplenishmentRepository interface {
	GetReplenishmentState(ctx context.Context, scope string) (registrationdomain.ReplenishmentState, error)
	ClaimReplenishment(ctx context.Context, value ReplenishmentClaim) (registrationdomain.ReplenishmentState, bool, error)
	RenewReplenishment(ctx context.Context, scope, claimToken string, leaseUntil, now time.Time) (bool, error)
	ExpireReplenishment(ctx context.Context, scope, claimToken string, now, nextAt time.Time, lastError string) (bool, error)
	FinishReplenishment(ctx context.Context, scope, claimToken string, status registrationdomain.ReplenishmentStatus, nextAt time.Time, lastError string, release bool, now time.Time) (bool, error)
}
