package gateway

import (
	"context"
	"net/http"
	"testing"
)

func TestHTTPUpstreamFailureClassifiesBuildForbiddenBodies(t *testing.T) {
	tests := []struct {
		name                   string
		status                 int
		body                   string
		accountScoped          bool
		permanentAccountDenial bool
		quotaExhausted         bool
		freeQuotaExhausted     bool
		modelQuotaExhausted    bool
		modelUnavailable       bool
		code                   string
		upstreamCode           string
	}{
		{
			name: "top-level permanent chat denial", status: http.StatusForbidden, body: `{"status_code":403,"error":"Access to the chat endpoint is denied. Please update the permissions."}`,
			accountScoped: true, permanentAccountDenial: true, code: "upstream_account_permission_denied",
		},
		{
			name: "nested permission code", status: http.StatusForbidden, body: `{"error":{"code":"permission_denied","message":"Endpoint access denied"}}`,
			accountScoped: true, permanentAccountDenial: true, code: "upstream_account_permission_denied", upstreamCode: "permission_denied",
		},
		{
			name: "spending limit", status: http.StatusForbidden, body: `{"code":"personal-team-blocked:spending-limit","error":"quota exhausted"}`,
			accountScoped: true, quotaExhausted: true, code: "upstream_quota_exhausted", upstreamCode: "personal-team-blocked:spending-limit",
		},
		{
			name: "generic paid quota", status: http.StatusForbidden, body: `{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`,
			accountScoped: true, quotaExhausted: true, code: "upstream_quota_exhausted", upstreamCode: "insufficient_quota",
		},
		{
			name: "unknown policy rejection", status: http.StatusForbidden, body: `{"error":"upstream policy rejected request"}`,
			code: "upstream_forbidden",
		},
		{
			name: "free model quota", status: http.StatusForbidden, body: `{"error":"You've used all the included free usage for model grok-build"}`,
			accountScoped: true, quotaExhausted: true, freeQuotaExhausted: true, modelQuotaExhausted: true, code: "upstream_quota_exhausted",
		},
		{
			name: "image usage limit", status: http.StatusTooManyRequests, body: `{"error":{"code":"usage_limit_reached","message":"You've reached your usage limit. Please try again later."}}`,
			accountScoped: true, quotaExhausted: true, freeQuotaExhausted: true, code: "upstream_quota_exhausted", upstreamCode: "usage_limit_reached",
		},
		{
			name: "ambiguous numeric rate limit", status: http.StatusTooManyRequests, body: `{"error":{"code":8,"message":"rate limited"}}`,
			code: "upstream_rate_limited",
		},
		{
			name: "model unavailable", status: http.StatusNotFound, body: `{"error":{"code":"model_not_found","message":"model unavailable"}}`,
			modelUnavailable: true, code: "upstream_model_unavailable", upstreamCode: "model_not_found",
		},
		{
			name: "unrelated not found", status: http.StatusNotFound, body: `{"error":{"code":"route_not_found","message":"endpoint unavailable"}}`,
			code: "upstream_server_error", upstreamCode: "route_not_found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := newHTTPUpstreamFailure(test.status, []byte(test.body), 42, "build")
			if failure.HTTPStatus != test.status || failure.Code != test.code || failure.AccountScoped != test.accountScoped || failure.PermanentAccountDenial != test.permanentAccountDenial || failure.QuotaExhausted != test.quotaExhausted || failure.FreeQuotaExhausted != test.freeQuotaExhausted || failure.ModelQuotaExhausted != test.modelQuotaExhausted || failure.ModelUnavailable != test.modelUnavailable || failure.UpstreamCode != test.upstreamCode {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
}

func TestModelUnavailableRotatesWithoutPenalizingAccount(t *testing.T) {
	failure := ClassifyHTTPFailure(http.StatusNotFound, []byte(`{"error":{"code":"model_not_found","message":"model unavailable"}}`), 42, "account")
	decision := DecideFailure(failure)
	if failure.AccountScoped || failure.Scope != FailureScopeModel || !failure.ModelUnavailable || decision.Scope != FailureScopeModel || decision.Action != FailureActionRotateAccount || decision.PenalizeAccount {
		t.Fatalf("failure=%#v decision=%#v", failure, decision)
	}
}

func TestBareRateLimitDoesNotPenalizeAccount(t *testing.T) {
	failure := ClassifyHTTPFailure(http.StatusTooManyRequests, []byte(`{"error":{"code":8,"message":"rate limited"}}`), 42, "account")
	decision := DecideFailure(failure)
	if failure.AccountScoped || failure.Scope != FailureScopeProvider || decision.PenalizeAccount || decision.Action != FailureActionRetryProvider {
		t.Fatalf("failure=%#v decision=%#v", failure, decision)
	}
}

func TestDecideFailureSeparatesQuotaAndNetworkActions(t *testing.T) {
	quota := newHTTPUpstreamFailure(429, []byte(`{"error":{"code":"usage_limit_reached"}}`), 42, "account")
	quotaDecision := DecideFailure(quota)
	if quota.Scope != FailureScopeQuota || quotaDecision.Scope != FailureScopeQuota || quotaDecision.Action != FailureActionUpdateQuota || quotaDecision.PenalizeAccount {
		t.Fatalf("quota failure = %#v decision = %#v", quota, quotaDecision)
	}
	network := newTransportUpstreamFailure(context.DeadlineExceeded, 42, "account")
	networkDecision := DecideFailure(network)
	if network.Scope != FailureScopeNetwork || networkDecision.Scope != FailureScopeNetwork || networkDecision.Action != FailureActionRetryProvider || networkDecision.PenalizeAccount {
		t.Fatalf("network failure = %#v decision = %#v", network, networkDecision)
	}
}
