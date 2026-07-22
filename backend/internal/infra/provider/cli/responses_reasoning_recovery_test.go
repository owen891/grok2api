package cli

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/infra/provider"
	"github.com/owen891/grok2api/backend/internal/infra/security"
)

func TestStripReasoningEncryptedContentKeepsPortableHistory(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","id":"rs_1","status":"completed","summary":[],"encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`)
	cleaned, changed := stripReasoningEncryptedContent(body)
	if !changed || strings.Contains(string(cleaned), "encrypted_content") || !strings.Contains(string(cleaned), "continue") {
		t.Fatalf("cleaned=%s changed=%v", cleaned, changed)
	}
}

func TestForwardResponseRecoversReasoningDecodeAndPreservesRateLimit(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://build.test/v1"}, cipher)
	calls := 0
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		data, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			return nil, readErr
		}
		switch calls {
		case 1:
			if !strings.Contains(string(data), `"encrypted_content":"opaque"`) || request.Header.Get("Idempotency-Key") != "original" {
				t.Fatalf("initial request body=%s headers=%#v", data, request.Header)
			}
			return jsonResponse(http.StatusBadRequest, `{"error":"Could not decode the compaction blob"}`, request), nil
		case 2:
			if strings.Contains(string(data), "encrypted_content") || request.Header.Get("Idempotency-Key") == "original" {
				t.Fatalf("recovery request body=%s headers=%#v", data, request.Header)
			}
			response := jsonResponse(http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`, request)
			response.Header.Set("Retry-After", "17")
			return response, nil
		default:
			t.Fatalf("unexpected request count %d", calls)
			return nil, nil
		}
	})

	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1", IdempotencyID: "original",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls != 2 || response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "17" {
		t.Fatalf("calls=%d status=%d retry-after=%q", calls, response.StatusCode, response.Header.Get("Retry-After"))
	}
	if !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "reasoning_encrypted_content_downgraded") {
		t.Fatalf("warnings=%q", response.Header.Get("X-Grok2API-Compatibility-Warnings"))
	}
}

func TestForwardResponseResetsPromptCacheAfterPersistentReasoningDecode(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://build.test/v1"}, cipher)
	calls := 0
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		data, _ := io.ReadAll(request.Body)
		if calls == 1 {
			if request.Header.Get("x-grok-session-id") == "" || !strings.Contains(string(data), `"prompt_cache_key":"session-1"`) {
				t.Fatalf("initial session body=%s headers=%#v", data, request.Header)
			}
		} else if calls == 2 {
			if request.Header.Get("x-grok-session-id") != "" || strings.Contains(string(data), "prompt_cache_key") {
				t.Fatalf("stateless recovery body=%s headers=%#v", data, request.Header)
			}
			return jsonResponse(http.StatusOK, `{"id":"ok","status":"completed","output":[]}`, request), nil
		}
		return jsonResponse(http.StatusBadRequest, `{"error":"Could not decode the compaction blob"}`, request), nil
	})

	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "session-1",
		Body: []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"continue"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls != 2 || response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "reasoning_session_reset") {
		t.Fatalf("calls=%d status=%d warnings=%q", calls, response.StatusCode, response.Header.Get("X-Grok2API-Compatibility-Warnings"))
	}
}

func TestForwardResponseKeepsReasoningAndToolCompatibilityWarnings(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://build.test/v1"}, cipher)
	calls := 0
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return jsonResponse(http.StatusBadRequest, `{"error":"Could not decode the compaction blob"}`, request), nil
		}
		return jsonResponse(http.StatusOK, `{"id":"resp_1","object":"response","tools":[{"type":"function","name":"crm__lookup"}],"output":[]}`, request), nil
	})

	response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-4.5", NormalizeBody: true, Operation: "responses",
		Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"encrypted_content":"opaque"},{"role":"user","content":"continue"}],"tools":[{"type":"namespace","name":"crm","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	warnings := response.Header.Get("X-Grok2API-Compatibility-Warnings")
	if calls != 2 || !strings.Contains(warnings, "reasoning_encrypted_content_downgraded") || !strings.Contains(warnings, "namespace_flattened") {
		t.Fatalf("calls=%d warnings=%q", calls, warnings)
	}
}
