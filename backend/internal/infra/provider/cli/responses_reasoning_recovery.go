package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/owen891/grok2api/backend/internal/infra/provider"
	"github.com/owen891/grok2api/backend/internal/infra/security"
)

var reasoningDecodeFailureMarkers = [][]byte{
	[]byte("could not decode the compaction blob"),
	[]byte("could not decrypt the provided encrypted_content"),
}

type reasoningRecoveryOutcome struct {
	encryptedContentDowngraded bool
	sessionReset               bool
	failed                     bool
}

func (o reasoningRecoveryOutcome) merge(other reasoningRecoveryOutcome) reasoningRecoveryOutcome {
	return reasoningRecoveryOutcome{
		encryptedContentDowngraded: o.encryptedContentDowngraded || other.encryptedContentDowngraded,
		sessionReset:               o.sessionReset || other.sessionReset,
		failed:                     o.failed || other.failed,
	}
}

func (o reasoningRecoveryOutcome) appendWarnings(header http.Header) {
	if o.encryptedContentDowngraded {
		appendCompatibilityWarning(header, "reasoning_encrypted_content_downgraded")
	}
	if o.sessionReset {
		appendCompatibilityWarning(header, "reasoning_session_reset")
	}
	if o.failed {
		appendCompatibilityWarning(header, "reasoning_recovery_failed")
	}
}

// recoverReasoningDecodeFailure handles only the upstream's explicit
// pre-generation opaque-reasoning rejection. Recovery stays on the same
// account and API plane; an unsuccessful retry returns the original 400.
func (a *Adapter) recoverReasoningDecodeFailure(
	ctx context.Context,
	request provider.ResponseResourceRequest,
	accessToken string,
	body []byte,
	base string,
	response *http.Response,
	requestURL string,
) (*http.Response, string, reasoningRecoveryOutcome) {
	if response == nil || response.StatusCode != http.StatusBadRequest {
		return response, requestURL, reasoningRecoveryOutcome{}
	}
	errorBody, truncated, err := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return cloneBufferedResponse(response, errorBody, truncated), requestURL, reasoningRecoveryOutcome{}
	}
	original := cloneBufferedResponse(response, errorBody, truncated)
	if truncated || !isReasoningDecodeFailure(errorBody) {
		return original, requestURL, reasoningRecoveryOutcome{}
	}

	portableBody, encryptedChanged := stripReasoningEncryptedContent(body)
	if encryptedChanged {
		retry, retryURL, retryErr := a.retryReasoningRecovery(ctx, request, accessToken, portableBody, base, false)
		if retryErr != nil {
			return original, requestURL, reasoningRecoveryOutcome{failed: true}
		}
		if err := normalizeGzipResponse(retry); err != nil {
			_ = retry.Body.Close()
			return original, requestURL, reasoningRecoveryOutcome{failed: true}
		}
		if isHTTPSuccess(retry.StatusCode) {
			_ = original.Body.Close()
			return retry, retryURL, reasoningRecoveryOutcome{encryptedContentDowngraded: true}
		}
		buffered, sameDecodeFailure, inspectErr := bufferReasoningResponse(retry)
		if inspectErr != nil {
			return original, requestURL, reasoningRecoveryOutcome{failed: true}
		}
		if !sameDecodeFailure {
			_ = original.Body.Close()
			return buffered, retryURL, reasoningRecoveryOutcome{encryptedContentDowngraded: true}
		}
		_ = buffered.Body.Close()
	}

	if !canResetReasoningSession(request, portableBody) {
		return original, requestURL, reasoningRecoveryOutcome{failed: true}
	}
	statelessBody := removePromptCacheKey(portableBody)
	retry, retryURL, retryErr := a.retryReasoningRecovery(ctx, request, accessToken, statelessBody, base, true)
	if retryErr != nil {
		return original, requestURL, reasoningRecoveryOutcome{failed: true}
	}
	if err := normalizeGzipResponse(retry); err != nil {
		_ = retry.Body.Close()
		return original, requestURL, reasoningRecoveryOutcome{failed: true}
	}
	if !isHTTPSuccess(retry.StatusCode) {
		buffered, sameDecodeFailure, inspectErr := bufferReasoningResponse(retry)
		if inspectErr != nil {
			return original, requestURL, reasoningRecoveryOutcome{failed: true}
		}
		if sameDecodeFailure {
			_ = buffered.Body.Close()
			return original, requestURL, reasoningRecoveryOutcome{failed: true}
		}
		_ = original.Body.Close()
		return buffered, retryURL, reasoningRecoveryOutcome{
			encryptedContentDowngraded: encryptedChanged,
			sessionReset:               true,
		}
	}
	_ = original.Body.Close()
	return retry, retryURL, reasoningRecoveryOutcome{
		encryptedContentDowngraded: encryptedChanged,
		sessionReset:               true,
	}
}

func (a *Adapter) retryReasoningRecovery(ctx context.Context, request provider.ResponseResourceRequest, accessToken string, body []byte, base string, resetSession bool) (*http.Response, string, error) {
	retryRequest := request
	idempotencyID, err := security.NewOpaqueToken(18)
	if err != nil {
		return nil, "", err
	}
	retryRequest.IdempotencyID = idempotencyID
	if resetSession {
		retryRequest.PromptCacheKey = ""
		retryRequest.GrokTurnIndex = ""
	}
	return a.doResponseRequest(ctx, retryRequest, accessToken, body, base)
}

func bufferReasoningResponse(response *http.Response) (*http.Response, bool, error) {
	if response == nil {
		return nil, false, nil
	}
	if response.StatusCode != http.StatusBadRequest {
		body, truncated, err := provider.ReadDiagnosticBody(response.Body)
		_ = response.Body.Close()
		if err != nil {
			return nil, false, err
		}
		return cloneBufferedResponse(response, body, truncated), false, nil
	}
	body, truncated, err := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, false, err
	}
	return cloneBufferedResponse(response, body, truncated), !truncated && isReasoningDecodeFailure(body), nil
}

func canResetReasoningSession(request provider.ResponseResourceRequest, body []byte) bool {
	if request.Method != http.MethodPost || strings.TrimSpace(request.PromptCacheKey) == "" {
		return false
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	previousResponseID, _ := payload["previous_response_id"].(string)
	return strings.TrimSpace(previousResponseID) == ""
}

func removePromptCacheKey(body []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	delete(payload, "prompt_cache_key")
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func isReasoningDecodeFailure(body []byte) bool {
	lower := bytes.ToLower(body)
	for _, marker := range reasoningDecodeFailureMarkers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// stripReasoningEncryptedContent removes opaque reasoning state while
// preserving readable summaries and all non-reasoning input items.
func stripReasoningEncryptedContent(body []byte) ([]byte, bool) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body, false
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) == 0 {
		return body, false
	}
	changed := false
	rebuilt := make([]any, 0, len(input))
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || stringField(item, "type") != "reasoning" {
			rebuilt = append(rebuilt, raw)
			continue
		}
		encrypted, ok := item["encrypted_content"].(string)
		if !ok || strings.TrimSpace(encrypted) == "" {
			rebuilt = append(rebuilt, raw)
			continue
		}
		cleaned := cloneJSONObject(item)
		delete(cleaned, "encrypted_content")
		delete(cleaned, "id")
		delete(cleaned, "status")
		changed = true
		if hasReadableReasoningContent(cleaned) {
			rebuilt = append(rebuilt, cleaned)
		}
	}
	if !changed {
		return body, false
	}
	payload["input"] = rebuilt
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return encoded, true
}

func hasReadableReasoningContent(item map[string]any) bool {
	for _, field := range []string{"summary", "content"} {
		parts, _ := item[field].([]any)
		for _, raw := range parts {
			part, _ := raw.(map[string]any)
			if strings.TrimSpace(stringField(part, "text")) != "" {
				return true
			}
		}
	}
	return false
}

func appendCompatibilityWarning(header http.Header, warning string) {
	if header == nil || strings.TrimSpace(warning) == "" {
		return
	}
	existing := strings.TrimSpace(header.Get("X-Grok2API-Compatibility-Warnings"))
	if existing == "" {
		header.Set("X-Grok2API-Compatibility-Warnings", warning)
		return
	}
	for _, value := range strings.Split(existing, ",") {
		if strings.TrimSpace(value) == warning {
			return
		}
	}
	header.Set("X-Grok2API-Compatibility-Warnings", existing+","+warning)
}
