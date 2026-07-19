package registration

import "time"

type Stage string

const (
	StageQueued            Stage = "queued"
	StageSelectingProxy    Stage = "selecting_proxy"
	StagePendingClearance  Stage = "pending_clearance"
	StagePendingEmail      Stage = "pending_email"
	StagePendingSubmit     Stage = "pending_submit"
	StagePendingSSO        Stage = "pending_sso"
	StageSubmissionUnknown Stage = "submission_unknown"
	StageRetryable         Stage = "retryable"
	StageSucceeded         Stage = "succeeded"
	StageFailed            Stage = "failed"
)

type Attempt struct {
	JobID        string    `json:"job_id"`
	AttemptID    string    `json:"attempt_id"`
	EmailHash    string    `json:"email_hash"`
	ProxyNodeID  uint64    `json:"proxy_node_id,omitempty"`
	ProxyGroupID uint64    `json:"proxy_group_id,omitempty"`
	Stage        Stage     `json:"stage"`
	Result       string    `json:"result,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type ReplenishmentStatus string

const (
	ReplenishmentIdle      ReplenishmentStatus = "idle"
	ReplenishmentStarting  ReplenishmentStatus = "starting"
	ReplenishmentRunning   ReplenishmentStatus = "running"
	ReplenishmentVerifying ReplenishmentStatus = "verifying"
	ReplenishmentCooling   ReplenishmentStatus = "cooling"
	ReplenishmentFailed    ReplenishmentStatus = "failed"
)

type ReplenishmentState struct {
	Scope            string
	Status           ReplenishmentStatus
	ClaimToken       string
	LeaseUntil       *time.Time
	LastTriggerAt    *time.Time
	NextAttemptAt    *time.Time
	CounterDate      time.Time
	DailyStarts      int
	BaselineEligible int
	LastError        string
	UpdatedAt        time.Time
}

func (a Attempt) Terminal() bool {
	return a.Stage == StageSucceeded || a.Stage == StageFailed
}

// CanRetry prevents a network interruption after submit from being treated as
// a fresh account creation. Operators must reconcile submission_unknown first.
func (a Attempt) CanRetry() bool {
	return a.Stage == StageRetryable || a.Stage == StagePendingClearance || a.Stage == StagePendingEmail || a.Stage == StagePendingSSO
}
