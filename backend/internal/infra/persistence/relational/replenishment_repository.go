package relational

import (
	"context"
	"strings"
	"time"

	registrationdomain "github.com/owen891/grok2api/backend/internal/domain/registration"
	"github.com/owen891/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ReplenishmentRepository struct{ db *Database }

func NewReplenishmentRepository(db *Database) *ReplenishmentRepository {
	return &ReplenishmentRepository{db: db}
}

func (r *ReplenishmentRepository) GetReplenishmentState(ctx context.Context, scope string) (registrationdomain.ReplenishmentState, error) {
	var row registrationReplenishmentModel
	if err := r.db.db.WithContext(ctx).Where("scope = ?", strings.TrimSpace(scope)).First(&row).Error; err != nil {
		return registrationdomain.ReplenishmentState{}, mapError(err)
	}
	return toReplenishmentState(row), nil
}

func (r *ReplenishmentRepository) ClaimReplenishment(ctx context.Context, value repository.ReplenishmentClaim) (registrationdomain.ReplenishmentState, bool, error) {
	value.Scope = strings.TrimSpace(value.Scope)
	value.ClaimToken = strings.TrimSpace(value.ClaimToken)
	if value.Scope == "" || len(value.ClaimToken) < 16 || len(value.ClaimToken) > 64 || value.DailyLimit <= 0 || value.Now.IsZero() || !value.LeaseUntil.After(value.Now) || value.NextAt.Before(value.Now) {
		return registrationdomain.ReplenishmentState{}, false, repository.ErrInvalid
	}
	now := value.Now.UTC()
	day := now.Truncate(24 * time.Hour)
	initial := registrationReplenishmentModel{
		Scope: value.Scope, Status: string(registrationdomain.ReplenishmentIdle), CounterDate: day, UpdatedAt: now,
	}
	if err := r.db.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; err != nil {
		return registrationdomain.ReplenishmentState{}, false, err
	}
	result := r.db.db.WithContext(ctx).Model(&registrationReplenishmentModel{}).
		Where("scope = ?", value.Scope).
		Where("lease_until IS NULL OR lease_until <= ?", now).
		Where("next_attempt_at IS NULL OR next_attempt_at <= ?", now).
		Where("counter_date <> ? OR daily_starts < ?", day, value.DailyLimit).
		Updates(map[string]any{
			"status":            string(registrationdomain.ReplenishmentStarting),
			"claim_token":       value.ClaimToken,
			"lease_until":       value.LeaseUntil.UTC(),
			"last_trigger_at":   now,
			"next_attempt_at":   value.NextAt.UTC(),
			"counter_date":      day,
			"daily_starts":      gorm.Expr("CASE WHEN counter_date = ? THEN daily_starts + 1 ELSE 1 END", day),
			"baseline_eligible": value.BaselineEligible,
			"last_error":        "",
			"updated_at":        now,
		})
	if result.Error != nil {
		return registrationdomain.ReplenishmentState{}, false, result.Error
	}
	state, err := r.GetReplenishmentState(ctx, value.Scope)
	return state, result.RowsAffected == 1, err
}

func (r *ReplenishmentRepository) FinishReplenishment(ctx context.Context, scope, claimToken string, status registrationdomain.ReplenishmentStatus, nextAt time.Time, lastError string, release bool, now time.Time) (bool, error) {
	var next *time.Time
	if !nextAt.IsZero() {
		value := nextAt.UTC()
		next = &value
	}
	updates := map[string]any{
		"status":          string(status),
		"next_attempt_at": next,
		"last_error":      truncate(strings.TrimSpace(lastError), 512),
		"updated_at":      now.UTC(),
	}
	if release {
		updates["lease_until"] = nil
		updates["claim_token"] = ""
	}
	result := r.db.db.WithContext(ctx).Model(&registrationReplenishmentModel{}).
		Where("scope = ? AND claim_token = ?", strings.TrimSpace(scope), strings.TrimSpace(claimToken)).
		Updates(updates)
	return result.RowsAffected == 1, result.Error
}

func (r *ReplenishmentRepository) RenewReplenishment(ctx context.Context, scope, claimToken string, leaseUntil, now time.Time) (bool, error) {
	if strings.TrimSpace(scope) == "" || strings.TrimSpace(claimToken) == "" || now.IsZero() || !leaseUntil.After(now) {
		return false, repository.ErrInvalid
	}
	result := r.db.db.WithContext(ctx).Model(&registrationReplenishmentModel{}).
		Where("scope = ? AND claim_token = ?", strings.TrimSpace(scope), strings.TrimSpace(claimToken)).
		Where("status IN ?", []string{string(registrationdomain.ReplenishmentStarting), string(registrationdomain.ReplenishmentRunning), string(registrationdomain.ReplenishmentVerifying)}).
		UpdateColumn("lease_until", leaseUntil.UTC())
	return result.RowsAffected == 1, result.Error
}

func (r *ReplenishmentRepository) ExpireReplenishment(ctx context.Context, scope, claimToken string, now, nextAt time.Time, lastError string) (bool, error) {
	if strings.TrimSpace(scope) == "" || strings.TrimSpace(claimToken) == "" || now.IsZero() || nextAt.Before(now) {
		return false, repository.ErrInvalid
	}
	result := r.db.db.WithContext(ctx).Model(&registrationReplenishmentModel{}).
		Where("scope = ? AND claim_token = ?", strings.TrimSpace(scope), strings.TrimSpace(claimToken)).
		Where("status IN ?", []string{string(registrationdomain.ReplenishmentStarting), string(registrationdomain.ReplenishmentRunning), string(registrationdomain.ReplenishmentVerifying)}).
		Where("lease_until IS NULL OR lease_until <= ?", now.UTC()).
		Updates(map[string]any{
			"status": string(registrationdomain.ReplenishmentFailed), "claim_token": "", "lease_until": nil,
			"next_attempt_at": nextAt.UTC(), "last_error": truncate(strings.TrimSpace(lastError), 512), "updated_at": now.UTC(),
		})
	return result.RowsAffected == 1, result.Error
}

func toReplenishmentState(row registrationReplenishmentModel) registrationdomain.ReplenishmentState {
	return registrationdomain.ReplenishmentState{
		Scope: row.Scope, Status: registrationdomain.ReplenishmentStatus(row.Status), ClaimToken: row.ClaimToken, LeaseUntil: row.LeaseUntil,
		LastTriggerAt: row.LastTriggerAt, NextAttemptAt: row.NextAttemptAt, CounterDate: row.CounterDate.UTC(),
		DailyStarts: row.DailyStarts, BaselineEligible: row.BaselineEligible, LastError: row.LastError, UpdatedAt: row.UpdatedAt.UTC(),
	}
}
