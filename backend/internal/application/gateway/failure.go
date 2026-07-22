package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

type FailureScope string

const (
	FailureScopeUnknown     FailureScope = "unknown"
	FailureScopeAccount     FailureScope = "account"
	FailureScopeModel       FailureScope = "model"
	FailureScopeQuota       FailureScope = "quota"
	FailureScopeEgress      FailureScope = "egress"
	FailureScopeProvider    FailureScope = "provider"
	FailureScopeProtocol    FailureScope = "protocol"
	FailureScopeNetwork     FailureScope = "network"
	FailureScopePostProcess FailureScope = "post_process"
)

type FailureAction string

const (
	FailureActionFail          FailureAction = "fail"
	FailureActionRotateAccount FailureAction = "rotate_account"
	FailureActionUpdateQuota   FailureAction = "update_quota"
	FailureActionRetryEgress   FailureAction = "retry_egress"
	FailureActionRefresh       FailureAction = "refresh_credential"
	FailureActionRequireReauth FailureAction = "require_reauth"
	FailureActionRetryProvider FailureAction = "retry_provider"
)

type FailureDecision struct {
	Scope           FailureScope
	Action          FailureAction
	PenalizeAccount bool
	Retryable       bool
}

// UpstreamFailure 保存可安全暴露给下游和审计的上游失败分类，不包含响应正文或凭据。
type UpstreamFailure struct {
	HTTPStatus             int
	Code                   string
	PublicMessage          string
	UpstreamCode           string
	AccountID              uint64
	AccountName            string
	AccountScoped          bool
	ModelUnavailable       bool
	Scope                  FailureScope
	PermanentAccountDenial bool
	QuotaExhausted         bool
	FreeQuotaExhausted     bool
	ModelQuotaExhausted    bool
	CredentialRejected     bool
	Fingerprint            string
	Cause                  error
}

// DecideFailure centralizes the routing action for future breaker/admission
// control while existing retry branches migrate incrementally.
func DecideFailure(failure *UpstreamFailure) FailureDecision {
	if failure == nil {
		return FailureDecision{Scope: FailureScopeUnknown, Action: FailureActionFail}
	}
	if failure.QuotaExhausted || failure.FreeQuotaExhausted || failure.ModelQuotaExhausted {
		return FailureDecision{Scope: FailureScopeQuota, Action: FailureActionUpdateQuota, Retryable: true}
	}
	if failure.ModelUnavailable {
		return FailureDecision{Scope: FailureScopeModel, Action: FailureActionRotateAccount, Retryable: true}
	}
	if failure.PermanentAccountDenial || failure.CredentialRejected {
		return FailureDecision{Scope: FailureScopeAccount, Action: FailureActionRequireReauth, PenalizeAccount: true, Retryable: true}
	}
	if failure.AccountScoped {
		return FailureDecision{Scope: FailureScopeAccount, Action: FailureActionRotateAccount, PenalizeAccount: true, Retryable: true}
	}
	if failure.Scope == FailureScopeProtocol {
		return FailureDecision{Scope: FailureScopeProtocol, Action: FailureActionRetryProvider, Retryable: true}
	}
	if failure.Scope == FailureScopeEgress {
		return FailureDecision{Scope: FailureScopeEgress, Action: FailureActionRetryEgress, Retryable: true}
	}
	if failure.Scope == FailureScopeNetwork {
		return FailureDecision{Scope: FailureScopeNetwork, Action: FailureActionRetryProvider, Retryable: true}
	}
	return FailureDecision{Scope: FailureScopeProvider, Action: FailureActionRetryProvider, Retryable: true}
}

func (e *UpstreamFailure) Error() string {
	if e == nil {
		return "上游请求失败"
	}
	if e.UpstreamCode != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.UpstreamCode)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Cause)
	}
	return e.Code
}

func (e *UpstreamFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *UpstreamFailure) AuditCode() string {
	if e == nil {
		return "upstream_error"
	}
	if suffix := normalizeFailureCode(e.UpstreamCode); suffix != "" {
		return truncateFailureCode(e.Code + "_" + suffix)
	}
	return truncateFailureCode(e.Code)
}

func newHTTPUpstreamFailure(status int, body []byte, accountID uint64, accountName string) *UpstreamFailure {
	upstreamCode, upstreamType, upstreamMessage := extractUpstreamErrorMetadata(body)
	failure := &UpstreamFailure{
		HTTPStatus: status, Code: "upstream_error", PublicMessage: "上游服务返回错误",
		UpstreamCode: upstreamCode, AccountID: accountID, AccountName: accountName,
		Scope: FailureScopeProvider,
	}
	if status < 400 || status > 599 {
		failure.HTTPStatus = http.StatusBadGateway
	}
	metadataText := strings.ToLower(strings.Join([]string{upstreamCode, upstreamType, upstreamMessage}, " "))
	switch status {
	case http.StatusUnauthorized:
		failure.Code = "upstream_unauthorized"
		failure.PublicMessage = "上游账号认证失败"
		failure.AccountScoped = true
		failure.Scope = FailureScopeAccount
		failure.CredentialRejected = true
	case http.StatusPaymentRequired:
		failure.Code = "upstream_payment_required"
		failure.PublicMessage = "上游账号额度不足"
		failure.AccountScoped = true
		failure.Scope = FailureScopeQuota
		failure.QuotaExhausted = true
	case http.StatusForbidden:
		failure.Code = "upstream_forbidden"
		failure.PublicMessage = "上游拒绝了该请求"
		failure.PermanentAccountDenial = isPermanentAccountDenial(metadataText)
		failure.ModelQuotaExhausted = isModelQuotaExhaustion(metadataText)
		failure.FreeQuotaExhausted = failure.ModelQuotaExhausted || isFreeQuotaExhaustion(metadataText)
		failure.QuotaExhausted = failure.FreeQuotaExhausted || isPaidQuotaExhaustion(metadataText)
		failure.CredentialRejected = !failure.QuotaExhausted && containsAny(metadataText, "authentication", "unauthorized", "invalid token", "token expired")
		failure.AccountScoped = failure.PermanentAccountDenial || failure.QuotaExhausted || failure.CredentialRejected || isAccountScopedForbidden(metadataText)
		if failure.AccountScoped {
			failure.Scope = FailureScopeAccount
		}
		switch {
		case failure.PermanentAccountDenial:
			failure.Code = "upstream_account_permission_denied"
			failure.PublicMessage = "上游账号无权访问当前接口"
		case failure.CredentialRejected:
			failure.Code = "upstream_credential_rejected"
			failure.PublicMessage = "上游账号凭据被拒绝"
		case failure.QuotaExhausted:
			failure.Code = "upstream_quota_exhausted"
			failure.PublicMessage = "上游账号额度不足或等待恢复"
		}
	case http.StatusTooManyRequests:
		failure.Code = "upstream_rate_limited"
		failure.PublicMessage = "上游请求频率受限"
		failure.ModelQuotaExhausted = isModelQuotaExhaustion(metadataText)
		failure.FreeQuotaExhausted = failure.ModelQuotaExhausted || isFreeQuotaExhaustion(metadataText)
		failure.QuotaExhausted = failure.FreeQuotaExhausted || isPaidQuotaExhaustion(metadataText)
		if failure.QuotaExhausted {
			failure.AccountScoped = true
			failure.Scope = FailureScopeQuota
			failure.Code = "upstream_quota_exhausted"
			failure.PublicMessage = "上游账号额度不足或等待恢复"
		}
	default:
		failure.Code = "upstream_server_error"
		failure.PublicMessage = "上游服务暂时异常"
	}
	if isModelUnavailable(status, metadataText) {
		failure.Code = "upstream_model_unavailable"
		failure.PublicMessage = "上游模型当前不可用"
		failure.ModelUnavailable = true
		failure.AccountScoped = false
		failure.Scope = FailureScopeModel
	}
	fingerprintPart := normalizeFailureCode(firstNonEmptyFailure(upstreamCode, upstreamType, upstreamMessage))
	if fingerprintPart == "" {
		fingerprintPart = "unknown"
	}
	failure.Fingerprint = fmt.Sprintf("%d:%s", status, fingerprintPart)
	return failure
}

func isModelUnavailable(status int, metadataText string) bool {
	if containsAny(metadataText,
		"model_not_found", "model-not-found", "model not found",
		"model_unavailable", "model-unavailable", "model unavailable",
		"model_not_available", "model-not-available", "model not available",
		"unknown_model", "unknown-model", "unknown model",
		"unsupported_model", "unsupported-model", "unsupported model",
	) {
		return true
	}
	return status == http.StatusNotFound && strings.Contains(metadataText, "model")
}

// ClassifyHTTPFailure exposes the shared routing taxonomy to active account
// inspection without duplicating provider error parsing.
func ClassifyHTTPFailure(status int, body []byte, accountID uint64, accountName string) *UpstreamFailure {
	return newHTTPUpstreamFailure(status, body, accountID, accountName)
}

// ClassifyTransportFailure maps a probe transport failure into the same stable
// scope and action vocabulary used by normal routing.
func ClassifyTransportFailure(err error, accountID uint64, accountName string) *UpstreamFailure {
	return newTransportUpstreamFailure(err, accountID, accountName)
}

func newTransportUpstreamFailure(err error, accountID uint64, accountName string) *UpstreamFailure {
	code, message := "upstream_network_error", "连接上游服务失败"
	if errors.Is(err, context.DeadlineExceeded) {
		code, message = "upstream_timeout", "上游服务响应超时"
	}
	return &UpstreamFailure{
		HTTPStatus: http.StatusBadGateway, Code: code, PublicMessage: message,
		AccountID: accountID, AccountName: accountName, Fingerprint: code, Scope: FailureScopeNetwork, Cause: err,
	}
}

func newCredentialUpstreamFailure(err error, accountID uint64, accountName string) *UpstreamFailure {
	return &UpstreamFailure{
		HTTPStatus: http.StatusBadGateway, Code: "upstream_credential_unavailable", PublicMessage: "上游账号凭据不可用",
		AccountID: accountID, AccountName: accountName, AccountScoped: true, Scope: FailureScopeAccount, Cause: err,
	}
}

func extractUpstreamErrorMetadata(body []byte) (string, string, string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return "", "", strings.TrimSpace(string(body))
	}
	root, ok := payload.(map[string]any)
	if !ok {
		return "", "", ""
	}
	if nested, ok := root["error"].(map[string]any); ok {
		code := firstNonEmptyFailure(firstStringValue(nested, "code", "error_code"), firstStringValue(root, "code", "error_code"))
		errorType := firstNonEmptyFailure(firstStringValue(nested, "type", "error_type"), firstStringValue(root, "type", "error_type"))
		message := firstNonEmptyFailure(firstStringValue(nested, "message", "error"), firstStringValue(root, "message"))
		return code, errorType, message
	}
	message := firstNonEmptyFailure(firstStringValue(root, "error"), firstStringValue(root, "message"))
	return firstStringValue(root, "code", "error_code"), firstStringValue(root, "type", "error_type"), message
}

func isAccountScopedForbidden(text string) bool {
	return containsAny(text, "quota", "billing", "subscription", "entitlement", "permission", "unauthorized", "authentication", "token", "usage-exhausted", "insufficient", "spending-limit")
}

func isPermanentAccountDenial(text string) bool {
	if strings.Contains(text, "permission_denied") {
		return true
	}
	if strings.Contains(text, "access to the chat endpoint is denied") {
		return true
	}
	return strings.Trim(strings.TrimSpace(text), " .!\t\r\n") == "access denied"
}

func isPaidQuotaExhaustion(text string) bool {
	return containsAny(
		text,
		"personal-team-blocked:spending-limit",
		"insufficient_quota",
		"insufficient quota",
		"quota exhausted",
		"quota_exhausted",
		"billing limit",
		"billing_limit",
		"spending limit",
		"spending_limit",
		"credits exhausted",
		"credits_exhausted",
	)
}

func isFreeQuotaExhaustion(text string) bool {
	return containsAny(
		text,
		"subscription:free-usage-exhausted",
		"used all the included free usage for model",
		"usage_limit_reached",
		"usage limit reached",
		"reached your usage limit",
		"图片额度已用完",
	)
}

func isModelQuotaExhaustion(text string) bool {
	return strings.Contains(text, "used all the included free usage for model")
}

func containsAny(text string, signals ...string) bool {
	for _, signal := range signals {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmptyFailure(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func normalizeFailureCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, current := range value {
		switch {
		case unicode.IsLetter(current), unicode.IsDigit(current):
			builder.WriteRune(current)
		case current == '-', current == '_', current == '.', current == ':':
			builder.WriteByte('_')
		}
		if builder.Len() >= 48 {
			break
		}
	}
	return strings.Trim(builder.String(), "_")
}

func truncateFailureCode(value string) string {
	if len(value) <= 100 {
		return value
	}
	return value[:100]
}
