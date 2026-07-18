package registration

import "testing"

func TestAttemptSubmissionUnknownIsNotAutomaticallyRetryable(t *testing.T) {
	a := Attempt{Stage: StageSubmissionUnknown}
	if a.CanRetry() || a.Terminal() {
		t.Fatal("submission_unknown must require reconciliation")
	}
}
