package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/accountinspection"
)

type AccountInspectionRepository interface {
	CreateInspectionRun(ctx context.Context, run accountinspection.Run, targets []accountinspection.Result) error
	GetInspectionRun(ctx context.Context, id string) (accountinspection.Run, error)
	GetLatestInspectionRun(ctx context.Context, provider account.Provider) (accountinspection.Run, error)
	ListInspectionRuns(ctx context.Context, provider account.Provider, limit int) ([]accountinspection.Run, error)
	ListInspectionResults(ctx context.Context, runID string, offset, limit int) ([]accountinspection.Result, int64, error)
	SummarizeInspectionResults(ctx context.Context, runID string) (map[accountinspection.Classification]int, error)
	ListLatestInspectionResults(ctx context.Context, provider account.Provider, classifications []accountinspection.Classification) ([]accountinspection.Result, error)
	ListClaimableInspectionRunIDs(ctx context.Context, now time.Time, limit int) ([]string, error)
	TryClaimInspectionRun(ctx context.Context, id, claimToken string, now, leaseUntil time.Time) (accountinspection.Run, bool, error)
	RenewInspectionRun(ctx context.Context, id, claimToken string, now, leaseUntil time.Time) (bool, error)
	ListPendingInspectionResults(ctx context.Context, runID string, limit int) ([]accountinspection.Result, error)
	CompleteInspectionResult(ctx context.Context, value accountinspection.Result, claimToken string, now time.Time) (bool, error)
	InspectionCancellationRequested(ctx context.Context, id, claimToken string) (bool, error)
	RequestInspectionCancellation(ctx context.Context, id string, now time.Time) (accountinspection.Run, error)
	CancelPendingInspectionResults(ctx context.Context, id, claimToken string, now time.Time) (int64, error)
	FinishInspectionRun(ctx context.Context, id, claimToken string, status accountinspection.RunStatus, message string, now time.Time) (bool, error)
	TryClaimInspectionResultApplication(ctx context.Context, runID string, accountID uint64, runClaimToken, applyClaimToken string, now, leaseUntil time.Time) (bool, error)
	FinishInspectionResultApplication(ctx context.Context, runID string, accountID uint64, runClaimToken, applyClaimToken string, status accountinspection.ApplyStatus, action, message string, now time.Time) (bool, error)
}
