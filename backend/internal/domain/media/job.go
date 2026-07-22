package media

import "time"

// ImageJobRecoveryTimeout bounds how long a stopped image worker can hold a job.
// Active workers renew their lease before this interval elapses.
const ImageJobRecoveryTimeout = 2 * time.Minute

type Status string

type JobKind string

const (
	JobKindVideo JobKind = "video"
	JobKindImage JobKind = "image"

	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// Job 表示可跨进程重启恢复的异步视频任务。
type Job struct {
	ID               string
	Kind             JobKind
	RequestID        string
	ClientKeyID      uint64
	ClientKeyName    string
	AccountID        uint64
	AccountName      string
	EgressNodeID     *uint64
	EgressNodeName   string
	EgressScope      string
	EgressMode       string
	Provider         string
	Model            string
	ModelRouteID     uint64
	UpstreamModel    string
	Prompt           string
	Seconds          int
	Size             string
	Quality          string
	Status           Status
	Progress         int
	InputJSON        string
	OutputJSON       string
	UpstreamURL      string
	ContentType      string
	ErrorCode        string
	ErrorMessage     string
	RoutingTraceJSON string
	LeaseUntil       *time.Time
	ClaimToken       string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
	UsageRecordedAt  *time.Time
}
