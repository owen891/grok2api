package accountinspection

import (
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
)

type RunMode string

const (
	RunModeFull        RunMode = "full"
	RunModeIncremental RunMode = "incremental"
	RunModeSelected    RunMode = "selected"
	RunModeRecheck     RunMode = "recheck"
)

type RunStatus string

const (
	RunStatusQueued    RunStatus = "queued"
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCancelled RunStatus = "cancelled"
)

type Classification string

const (
	ClassificationPending          Classification = "pending"
	ClassificationHealthy          Classification = "healthy"
	ClassificationPermissionDenied Classification = "permission_denied"
	ClassificationQuotaExhausted   Classification = "quota_exhausted"
	ClassificationReauth           Classification = "reauth"
	ClassificationModelUnavailable Classification = "model_unavailable"
	ClassificationProbeError       Classification = "probe_error"
	ClassificationCancelled        Classification = "cancelled"
)

type SuggestedAction string

const (
	ActionKeep          SuggestedAction = "keep"
	ActionClearHealth   SuggestedAction = "clear_health"
	ActionRequireReauth SuggestedAction = "require_reauth"
	ActionUpdateQuota   SuggestedAction = "update_quota"
	ActionReview        SuggestedAction = "review"
)

type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

type ApplyStatus string

const (
	ApplyStatusPending  ApplyStatus = "pending"
	ApplyStatusApplying ApplyStatus = "applying"
	ApplyStatusApplied  ApplyStatus = "applied"
	ApplyStatusSkipped  ApplyStatus = "skipped"
	ApplyStatusFailed   ApplyStatus = "failed"
)

type Run struct {
	ID              string
	Provider        account.Provider
	ModelRouteID    uint64
	UpstreamModel   string
	Mode            RunMode
	Status          RunStatus
	IncludeDisabled bool
	Concurrency     int
	Total           int
	Completed       int
	CancelRequested bool
	ClaimToken      string
	LeaseUntil      *time.Time
	ErrorMessage    string
	StartedAt       *time.Time
	FinishedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Result struct {
	RunID                  string
	AccountID              uint64
	Provider               account.Provider
	AccountName            string
	AccountEmail           string
	AccountEnabled         bool
	AccountUpdatedAt       time.Time
	Model                  string
	Classification         Classification
	SuggestedAction        SuggestedAction
	Confidence             Confidence
	FailureScope           string
	FailureAction          string
	HTTPStatus             int
	ErrorCode              string
	ErrorMessage           string
	Attempts               int
	Latency                time.Duration
	QuotaExhausted         bool
	FreeQuotaExhausted     bool
	ModelQuotaExhausted    bool
	CredentialRejected     bool
	PermanentAccountDenial bool
	ApplyStatus            ApplyStatus
	ApplyClaimToken        string
	ApplyLeaseUntil        *time.Time
	ApplyAttempts          int
	ApplyError             string
	AppliedAction          string
	AppliedAt              *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

func (status RunStatus) Terminal() bool {
	switch status {
	case RunStatusCompleted, RunStatusFailed, RunStatusCancelled:
		return true
	default:
		return false
	}
}

func (classification Classification) ValidRecheckTarget() bool {
	switch classification {
	case ClassificationHealthy, ClassificationPermissionDenied, ClassificationQuotaExhausted,
		ClassificationReauth, ClassificationModelUnavailable, ClassificationProbeError:
		return true
	default:
		return false
	}
}
