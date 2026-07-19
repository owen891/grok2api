package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/accountinspection"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type AccountInspectionRepository struct{ db *Database }

var latestRecheckClassifications = []accountinspection.Classification{
	accountinspection.ClassificationHealthy,
	accountinspection.ClassificationPermissionDenied,
	accountinspection.ClassificationQuotaExhausted,
	accountinspection.ClassificationReauth,
	accountinspection.ClassificationModelUnavailable,
	accountinspection.ClassificationProbeError,
}

func NewAccountInspectionRepository(db *Database) *AccountInspectionRepository {
	return &AccountInspectionRepository{db: db}
}

func (r *AccountInspectionRepository) CreateInspectionRun(ctx context.Context, run accountinspection.Run, targets []accountinspection.Result) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(inspectionRunFromDomain(run)).Error; err != nil {
			return mapError(err)
		}
		if len(targets) == 0 {
			return nil
		}
		rows := make([]accountInspectionResultModel, 0, len(targets))
		for _, value := range targets {
			rows = append(rows, *inspectionResultFromDomain(value))
		}
		return tx.CreateInBatches(rows, 200).Error
	})
}

func (r *AccountInspectionRepository) GetInspectionRun(ctx context.Context, id string) (accountinspection.Run, error) {
	var row accountInspectionRunModel
	if err := r.db.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		return accountinspection.Run{}, mapError(err)
	}
	return inspectionRunToDomain(row), nil
}

func (r *AccountInspectionRepository) GetLatestInspectionRun(ctx context.Context, providerValue account.Provider) (accountinspection.Run, error) {
	var row accountInspectionRunModel
	if err := r.db.db.WithContext(ctx).Where("provider = ?", providerValue).Order("created_at DESC, id DESC").First(&row).Error; err != nil {
		return accountinspection.Run{}, mapError(err)
	}
	return inspectionRunToDomain(row), nil
}

func (r *AccountInspectionRepository) ListInspectionRuns(ctx context.Context, providerValue account.Provider, limit int) ([]accountinspection.Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := r.db.db.WithContext(ctx).Model(&accountInspectionRunModel{})
	if providerValue != "" {
		query = query.Where("provider = ?", providerValue)
	}
	var rows []accountInspectionRunModel
	if err := query.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]accountinspection.Run, 0, len(rows))
	for _, row := range rows {
		values = append(values, inspectionRunToDomain(row))
	}
	return values, nil
}

func (r *AccountInspectionRepository) ListInspectionResults(ctx context.Context, runID string, offset, limit int) ([]accountinspection.Result, int64, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := r.db.db.WithContext(ctx).Model(&accountInspectionResultModel{}).Where("run_id = ?", runID)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []accountInspectionResultModel
	if err := query.Order("account_id ASC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := make([]accountinspection.Result, 0, len(rows))
	for _, row := range rows {
		values = append(values, inspectionResultToDomain(row))
	}
	return values, total, nil
}

func (r *AccountInspectionRepository) SummarizeInspectionResults(ctx context.Context, runID string) (map[accountinspection.Classification]int, error) {
	var rows []struct {
		Classification string
		Count          int
	}
	if err := r.db.db.WithContext(ctx).Model(&accountInspectionResultModel{}).
		Select("classification, COUNT(*) AS count").Where("run_id = ?", runID).Group("classification").Scan(&rows).Error; err != nil {
		return nil, err
	}
	result := make(map[accountinspection.Classification]int, len(rows))
	for _, row := range rows {
		result[accountinspection.Classification(row.Classification)] = row.Count
	}
	return result, nil
}

func (r *AccountInspectionRepository) ListLatestInspectionResults(ctx context.Context, providerValue account.Provider, classifications []accountinspection.Classification) ([]accountinspection.Result, error) {
	terminal := []accountinspection.RunStatus{
		accountinspection.RunStatusCompleted, accountinspection.RunStatusFailed, accountinspection.RunStatusCancelled,
	}
	query := r.db.db.WithContext(ctx).
		Table("account_inspection_results AS result").
		Select("result.*").
		Joins("JOIN account_inspection_runs AS run ON run.id = result.run_id").
		Where("run.provider = ? AND run.status IN ?", providerValue, terminal).
		Where("result.classification IN ?", latestRecheckClassifications).
		Where(`NOT EXISTS (
			SELECT 1
			FROM account_inspection_results AS newer_result
			JOIN account_inspection_runs AS newer_run ON newer_run.id = newer_result.run_id
			WHERE newer_result.account_id = result.account_id
				AND newer_run.provider = run.provider
				AND newer_run.status IN ?
				AND newer_result.classification IN ?
				AND (newer_run.created_at > run.created_at OR (newer_run.created_at = run.created_at AND newer_run.id > run.id))
		)`, terminal, latestRecheckClassifications)
	if len(classifications) > 0 {
		query = query.Where("result.classification IN ?", classifications)
	}
	var rows []accountInspectionResultModel
	if err := query.Order("result.account_id ASC").Scan(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]accountinspection.Result, 0, len(rows))
	for _, row := range rows {
		values = append(values, inspectionResultToDomain(row))
	}
	return values, nil
}

func (r *AccountInspectionRepository) ListClaimableInspectionRunIDs(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	var ids []string
	err := r.db.db.WithContext(ctx).Model(&accountInspectionRunModel{}).
		Where("status = ? OR (status = ? AND (lease_until IS NULL OR lease_until <= ?))", accountinspection.RunStatusQueued, accountinspection.RunStatusRunning, now).
		Order("created_at ASC, id ASC").Limit(limit).Pluck("id", &ids).Error
	return ids, err
}

func (r *AccountInspectionRepository) TryClaimInspectionRun(ctx context.Context, id, claimToken string, now, leaseUntil time.Time) (accountinspection.Run, bool, error) {
	if claimToken == "" {
		return accountinspection.Run{}, false, repository.ErrConflict
	}
	result := r.db.db.WithContext(ctx).Model(&accountInspectionRunModel{}).
		Where("id = ? AND (status = ? OR (status = ? AND (lease_until IS NULL OR lease_until <= ?)))", id, accountinspection.RunStatusQueued, accountinspection.RunStatusRunning, now).
		Updates(map[string]any{
			"status": accountinspection.RunStatusRunning, "claim_token": claimToken, "lease_until": leaseUntil,
			"started_at": gorm.Expr("COALESCE(started_at, ?)", now), "updated_at": now,
		})
	if result.Error != nil {
		return accountinspection.Run{}, false, result.Error
	}
	if result.RowsAffected == 0 {
		return accountinspection.Run{}, false, nil
	}
	value, err := r.GetInspectionRun(ctx, id)
	return value, err == nil, err
}

func (r *AccountInspectionRepository) RenewInspectionRun(ctx context.Context, id, claimToken string, now, leaseUntil time.Time) (bool, error) {
	result := r.db.db.WithContext(ctx).Model(&accountInspectionRunModel{}).
		Where("id = ? AND claim_token = ? AND status = ?", id, claimToken, accountinspection.RunStatusRunning).
		Updates(map[string]any{"lease_until": leaseUntil, "updated_at": now})
	return result.RowsAffected == 1, result.Error
}

func (r *AccountInspectionRepository) ListPendingInspectionResults(ctx context.Context, runID string, limit int) ([]accountinspection.Result, error) {
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	var rows []accountInspectionResultModel
	if err := r.db.db.WithContext(ctx).Where("run_id = ? AND classification = ?", runID, accountinspection.ClassificationPending).
		Order("account_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]accountinspection.Result, 0, len(rows))
	for _, row := range rows {
		values = append(values, inspectionResultToDomain(row))
	}
	return values, nil
}

func (r *AccountInspectionRepository) CompleteInspectionResult(ctx context.Context, value accountinspection.Result, claimToken string, now time.Time) (bool, error) {
	updated := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := inspectionResultFromDomain(value)
		result := tx.Model(&accountInspectionResultModel{}).
			Where("run_id = ? AND account_id = ? AND classification = ?", value.RunID, value.AccountID, accountinspection.ClassificationPending).
			Where("EXISTS (SELECT 1 FROM account_inspection_runs WHERE id = ? AND claim_token = ? AND status = ?)", value.RunID, claimToken, accountinspection.RunStatusRunning).
			Select("classification", "suggested_action", "confidence", "failure_scope", "failure_action", "http_status", "error_code", "error_message", "attempts", "latency_milliseconds", "quota_exhausted", "free_quota_exhausted", "model_quota_exhausted", "credential_rejected", "permanent_account_denial", "updated_at").
			Updates(row)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		runResult := tx.Model(&accountInspectionRunModel{}).
			Where("id = ? AND claim_token = ? AND status = ?", value.RunID, claimToken, accountinspection.RunStatusRunning).
			Updates(map[string]any{"completed": gorm.Expr("completed + 1"), "updated_at": now})
		if runResult.Error != nil {
			return runResult.Error
		}
		updated = runResult.RowsAffected == 1
		return nil
	})
	return updated, err
}

func (r *AccountInspectionRepository) InspectionCancellationRequested(ctx context.Context, id, claimToken string) (bool, error) {
	var value bool
	result := r.db.db.WithContext(ctx).Model(&accountInspectionRunModel{}).
		Select("cancel_requested").Where("id = ? AND claim_token = ? AND status = ?", id, claimToken, accountinspection.RunStatusRunning).Scan(&value)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, repository.ErrNotFound
	}
	return value, nil
}

func (r *AccountInspectionRepository) RequestInspectionCancellation(ctx context.Context, id string, now time.Time) (accountinspection.Run, error) {
	result := r.db.db.WithContext(ctx).Model(&accountInspectionRunModel{}).
		Where("id = ? AND status IN ?", id, []accountinspection.RunStatus{accountinspection.RunStatusQueued, accountinspection.RunStatusRunning}).
		Updates(map[string]any{"cancel_requested": true, "updated_at": now})
	if result.Error != nil {
		return accountinspection.Run{}, result.Error
	}
	return r.GetInspectionRun(ctx, id)
}

func (r *AccountInspectionRepository) CancelPendingInspectionResults(ctx context.Context, id, claimToken string, now time.Time) (int64, error) {
	var cancelled int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&accountInspectionResultModel{}).
			Where("run_id = ? AND classification = ?", id, accountinspection.ClassificationPending).
			Where("EXISTS (SELECT 1 FROM account_inspection_runs WHERE id = ? AND claim_token = ? AND status = ?)", id, claimToken, accountinspection.RunStatusRunning).
			Updates(map[string]any{
				"classification": accountinspection.ClassificationCancelled, "suggested_action": accountinspection.ActionKeep,
				"confidence": accountinspection.ConfidenceLow, "error_message": "inspection cancelled before probe", "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		cancelled = result.RowsAffected
		// A cancellation can race with automatic application after probing. Any
		// result that was not durably applied must be terminalized as skipped,
		// including an expired application claim left by a crashed worker.
		applicationResult := tx.Model(&accountInspectionResultModel{}).
			Where("run_id = ? AND applied_at IS NULL AND apply_status IN ?", id, []accountinspection.ApplyStatus{
				accountinspection.ApplyStatusPending, accountinspection.ApplyStatusApplying,
			}).
			Where("EXISTS (SELECT 1 FROM account_inspection_runs WHERE id = ? AND claim_token = ? AND status = ?)", id, claimToken, accountinspection.RunStatusRunning).
			Updates(map[string]any{
				"apply_status": accountinspection.ApplyStatusSkipped, "apply_claim_token": "", "apply_lease_until": nil,
				"apply_error": "inspection_cancelled", "updated_at": now,
			})
		if applicationResult.Error != nil {
			return applicationResult.Error
		}
		if cancelled == 0 {
			return nil
		}
		return tx.Model(&accountInspectionRunModel{}).
			Where("id = ? AND claim_token = ? AND status = ?", id, claimToken, accountinspection.RunStatusRunning).
			Updates(map[string]any{"completed": gorm.Expr("completed + ?", cancelled), "updated_at": now}).Error
	})
	return cancelled, err
}

func (r *AccountInspectionRepository) FinishInspectionRun(ctx context.Context, id, claimToken string, status accountinspection.RunStatus, message string, now time.Time) (bool, error) {
	if len(message) > 512 {
		message = message[:512]
	}
	result := r.db.db.WithContext(ctx).Model(&accountInspectionRunModel{}).
		Where("id = ? AND claim_token = ? AND status = ?", id, claimToken, accountinspection.RunStatusRunning).
		Updates(map[string]any{
			"status": status, "claim_token": "", "lease_until": nil, "error_message": message,
			"finished_at": now, "updated_at": now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *AccountInspectionRepository) TryClaimInspectionResultApplication(ctx context.Context, runID string, accountID uint64, runClaimToken, applyClaimToken string, now, leaseUntil time.Time) (bool, error) {
	if runClaimToken == "" || applyClaimToken == "" {
		return false, repository.ErrConflict
	}
	result := r.db.db.WithContext(ctx).Model(&accountInspectionResultModel{}).
		Where("run_id = ? AND account_id = ? AND applied_at IS NULL AND classification <> ?", runID, accountID, accountinspection.ClassificationPending).
		Where("apply_status IN ? OR (apply_status = ? AND (apply_lease_until IS NULL OR apply_lease_until <= ?))", []accountinspection.ApplyStatus{
			accountinspection.ApplyStatusPending, accountinspection.ApplyStatusFailed,
		}, accountinspection.ApplyStatusApplying, now).
		Where("EXISTS (SELECT 1 FROM account_inspection_runs WHERE id = ? AND claim_token = ? AND status = ?)", runID, runClaimToken, accountinspection.RunStatusRunning).
		Updates(map[string]any{
			"apply_status": accountinspection.ApplyStatusApplying, "apply_claim_token": applyClaimToken,
			"apply_lease_until": leaseUntil, "apply_attempts": gorm.Expr("apply_attempts + 1"), "apply_error": "", "updated_at": now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *AccountInspectionRepository) FinishInspectionResultApplication(ctx context.Context, runID string, accountID uint64, runClaimToken, applyClaimToken string, status accountinspection.ApplyStatus, action, message string, now time.Time) (bool, error) {
	switch status {
	case accountinspection.ApplyStatusApplied, accountinspection.ApplyStatusSkipped, accountinspection.ApplyStatusFailed:
	default:
		return false, repository.ErrConflict
	}
	if len(message) > 512 {
		message = message[:512]
	}
	updates := map[string]any{
		"apply_status": status, "apply_claim_token": "", "apply_lease_until": nil,
		"apply_error": message, "applied_action": action, "updated_at": now,
	}
	if status == accountinspection.ApplyStatusApplied {
		updates["applied_at"] = now
	} else {
		updates["applied_at"] = nil
	}
	result := r.db.db.WithContext(ctx).Model(&accountInspectionResultModel{}).
		Where("run_id = ? AND account_id = ? AND apply_status = ? AND apply_claim_token = ?", runID, accountID, accountinspection.ApplyStatusApplying, applyClaimToken).
		Where("EXISTS (SELECT 1 FROM account_inspection_runs WHERE id = ? AND claim_token = ? AND status = ?)", runID, runClaimToken, accountinspection.RunStatusRunning).
		Updates(updates)
	return result.RowsAffected == 1, result.Error
}

func inspectionRunFromDomain(value accountinspection.Run) *accountInspectionRunModel {
	return &accountInspectionRunModel{
		ID: value.ID, Provider: string(value.Provider), ModelRouteID: value.ModelRouteID, UpstreamModel: value.UpstreamModel,
		Mode: string(value.Mode), Status: string(value.Status), IncludeDisabled: value.IncludeDisabled, Concurrency: value.Concurrency,
		Total: value.Total, Completed: value.Completed, CancelRequested: value.CancelRequested, ClaimToken: value.ClaimToken,
		LeaseUntil: value.LeaseUntil, ErrorMessage: value.ErrorMessage, StartedAt: value.StartedAt, FinishedAt: value.FinishedAt,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func inspectionRunToDomain(row accountInspectionRunModel) accountinspection.Run {
	return accountinspection.Run{
		ID: row.ID, Provider: account.Provider(row.Provider), ModelRouteID: row.ModelRouteID, UpstreamModel: row.UpstreamModel,
		Mode: accountinspection.RunMode(row.Mode), Status: accountinspection.RunStatus(row.Status), IncludeDisabled: row.IncludeDisabled,
		Concurrency: row.Concurrency, Total: row.Total, Completed: row.Completed, CancelRequested: row.CancelRequested,
		ClaimToken: row.ClaimToken, LeaseUntil: row.LeaseUntil, ErrorMessage: row.ErrorMessage, StartedAt: row.StartedAt,
		FinishedAt: row.FinishedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func inspectionResultFromDomain(value accountinspection.Result) *accountInspectionResultModel {
	applyStatus := value.ApplyStatus
	if applyStatus == "" {
		applyStatus = accountinspection.ApplyStatusPending
	}
	return &accountInspectionResultModel{
		RunID: value.RunID, AccountID: value.AccountID, Provider: string(value.Provider), AccountName: value.AccountName,
		AccountEmail: value.AccountEmail, AccountEnabled: value.AccountEnabled, AccountUpdatedAt: value.AccountUpdatedAt, Model: value.Model,
		Classification: string(value.Classification), SuggestedAction: string(value.SuggestedAction), Confidence: string(value.Confidence),
		FailureScope: value.FailureScope, FailureAction: value.FailureAction, HTTPStatus: value.HTTPStatus,
		ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage, Attempts: value.Attempts,
		LatencyMilliseconds: value.Latency.Milliseconds(), QuotaExhausted: value.QuotaExhausted,
		FreeQuotaExhausted: value.FreeQuotaExhausted, ModelQuotaExhausted: value.ModelQuotaExhausted,
		CredentialRejected: value.CredentialRejected, PermanentAccountDenial: value.PermanentAccountDenial,
		ApplyStatus: string(applyStatus), ApplyClaimToken: value.ApplyClaimToken, ApplyLeaseUntil: value.ApplyLeaseUntil,
		ApplyAttempts: value.ApplyAttempts, ApplyError: value.ApplyError,
		AppliedAction: value.AppliedAction, AppliedAt: value.AppliedAt, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func inspectionResultToDomain(row accountInspectionResultModel) accountinspection.Result {
	applyStatus := accountinspection.ApplyStatus(row.ApplyStatus)
	if applyStatus == "" {
		applyStatus = accountinspection.ApplyStatusPending
	}
	return accountinspection.Result{
		RunID: row.RunID, AccountID: row.AccountID, Provider: account.Provider(row.Provider), AccountName: row.AccountName,
		AccountEmail: row.AccountEmail, AccountEnabled: row.AccountEnabled, AccountUpdatedAt: row.AccountUpdatedAt, Model: row.Model,
		Classification: accountinspection.Classification(row.Classification), SuggestedAction: accountinspection.SuggestedAction(row.SuggestedAction),
		Confidence: accountinspection.Confidence(row.Confidence), FailureScope: row.FailureScope, FailureAction: row.FailureAction,
		HTTPStatus: row.HTTPStatus, ErrorCode: row.ErrorCode, ErrorMessage: row.ErrorMessage, Attempts: row.Attempts,
		Latency: time.Duration(row.LatencyMilliseconds) * time.Millisecond, QuotaExhausted: row.QuotaExhausted,
		FreeQuotaExhausted: row.FreeQuotaExhausted, ModelQuotaExhausted: row.ModelQuotaExhausted,
		CredentialRejected: row.CredentialRejected, PermanentAccountDenial: row.PermanentAccountDenial,
		ApplyStatus: applyStatus, ApplyClaimToken: row.ApplyClaimToken, ApplyLeaseUntil: row.ApplyLeaseUntil,
		ApplyAttempts: row.ApplyAttempts, ApplyError: row.ApplyError,
		AppliedAction: row.AppliedAction, AppliedAt: row.AppliedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
