package gateway

import (
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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := newHTTPUpstreamFailure(test.status, []byte(test.body), 42, "build")
			if failure.HTTPStatus != test.status || failure.Code != test.code || failure.AccountScoped != test.accountScoped || failure.PermanentAccountDenial != test.permanentAccountDenial || failure.QuotaExhausted != test.quotaExhausted || failure.FreeQuotaExhausted != test.freeQuotaExhausted || failure.ModelQuotaExhausted != test.modelQuotaExhausted || failure.UpstreamCode != test.upstreamCode {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
}
