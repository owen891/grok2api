package gateway

import (
	"errors"
	"testing"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/infra/provider"
)

func TestImageJobAttemptProgress(t *testing.T) {
	for _, test := range []struct {
		attempts int
		attempt  int
		want     int
	}{
		{attempts: 1, attempt: 1, want: 50},
		{attempts: 3, attempt: 1, want: 10},
		{attempts: 3, attempt: 2, want: 50},
		{attempts: 3, attempt: 3, want: 90},
	} {
		if got := imageJobAttemptProgress(test.attempt, test.attempts); got != test.want {
			t.Fatalf("imageJobAttemptProgress(%d, %d) = %d, want %d", test.attempt, test.attempts, got, test.want)
		}
	}
}

func TestImageProtocolFailureReportsIncompleteGeneration(t *testing.T) {
	err := provider.NewMediaProtocolError("image_generation_incomplete", errors.New("missing usable image"))
	failure, ok := imageProtocolFailure(err, account.Credential{})
	if !ok || failure.Code != "image_generation_incomplete" || failure.AccountScoped || failure.PermanentAccountDenial || failure.Scope != FailureScopeProtocol {
		t.Fatalf("failure = %#v, ok = %t", failure, ok)
	}
}

func TestImageProtocolFailureRotatesSubscriptionlessAccount(t *testing.T) {
	err := provider.NewMediaProtocolError("image_subscription_required", errors.New("subscription page"))
	failure, ok := imageProtocolFailure(err, account.Credential{ID: 287, Name: "basic-account"})
	if !ok || failure.Code != "image_subscription_required" || !failure.AccountScoped || !failure.PermanentAccountDenial || failure.Scope != FailureScopeAccount {
		t.Fatalf("failure = %#v, ok = %t", failure, ok)
	}
}
