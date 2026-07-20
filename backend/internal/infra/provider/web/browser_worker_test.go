package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	infraegress "github.com/owen891/grok2api/backend/internal/infra/egress"
	"github.com/owen891/grok2api/backend/internal/infra/provider"
	"github.com/owen891/grok2api/backend/internal/infra/security"
)

func TestCallBrowserWorker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/grok/fast-image" || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var value browserWorkerRequest
		if err := json.NewDecoder(request.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
		if value.SSOToken != "secret" || value.Payload["message"] != "Drawing: test" {
			t.Fatalf("worker request = %#v", value)
		}
		_ = json.NewEncoder(writer).Encode(browserWorkerResponse{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Headers:    map[string]string{"content-type": "text/event-stream"},
			BodyBase64: base64.StdEncoding.EncodeToString([]byte("data: done\n\n")),
		})
	}))
	defer server.Close()

	result, err := callBrowserWorker(context.Background(), server.URL, browserWorkerRequest{
		SSOToken: "secret", Payload: map[string]any{"message": "Drawing: test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK || result.Headers["content-type"] != "text/event-stream" {
		t.Fatalf("worker response = %#v", result)
	}
}

func TestBrowserWorkerRequestMarshalsImageMode(t *testing.T) {
	data, err := json.Marshal(browserWorkerRequest{ImageMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"imageMode":true`)) {
		t.Fatalf("request JSON = %s", data)
	}
}

func TestCallBrowserWorkerPropagatesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(writer).Encode(browserWorkerResponse{Error: "browser unavailable", Code: "proxy_unavailable"})
	}))
	defer server.Close()

	_, err := callBrowserWorker(context.Background(), server.URL, browserWorkerRequest{})
	if err == nil || err.Error() != "browser unavailable" {
		t.Fatalf("error = %v", err)
	}
	var failure *browserWorkerFailure
	if !errors.As(err, &failure) || failure.Code != "proxy_unavailable" {
		t.Fatalf("classified error = %#v", failure)
	}
}

func TestCallBrowserWorkerRetriesTransientFailure(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(writer).Encode(browserWorkerResponse{Error: "browser restarting", Code: "browser_unavailable"})
			return
		}
		_ = json.NewEncoder(writer).Encode(browserWorkerResponse{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			BodyBase64: base64.StdEncoding.EncodeToString([]byte("data: done\n\n")),
		})
	}))
	defer server.Close()

	result, err := callBrowserWorker(context.Background(), server.URL, browserWorkerRequest{})
	if err != nil || result.StatusCode != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("result=%#v err=%v calls=%d", result, err, calls.Load())
	}
}

func TestBrowserWorkerAntiBotErrorIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(writer).Encode(browserWorkerResponse{Error: "Cloudflare challenge did not clear in Chromium"})
	}))
	defer server.Close()

	_, err := callBrowserWorker(context.Background(), server.URL, browserWorkerRequest{})
	if err == nil || !looksLikeAntiBot([]byte(err.Error())) {
		t.Fatalf("error = %v", err)
	}
}

func TestCallBrowserWorkerWarm(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/grok/warm" || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		_ = json.NewEncoder(writer).Encode(browserWorkerWarmResponse{OK: true})
	}))
	defer server.Close()

	if err := callBrowserWorkerWarm(context.Background(), server.URL, browserWorkerRequest{SSOToken: "secret", Payload: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
}

func TestCallBrowserWorkerWarmStateReturnsRefreshedSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(browserWorkerWarmResponse{OK: true, CloudflareCookie: "cf_clearance=fresh", UserAgent: "Chrome/Fresh"})
	}))
	defer server.Close()

	state, err := callBrowserWorkerWarmState(context.Background(), server.URL, browserWorkerRequest{})
	if err != nil || state.CloudflareCookie != "cf_clearance=fresh" || state.UserAgent != "Chrome/Fresh" {
		t.Fatalf("state=%#v err=%v", state, err)
	}
}

func TestCallBrowserWorkerQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/grok/quota" || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		_ = json.NewEncoder(writer).Encode(browserWorkerResponse{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Headers:    map[string]string{"content-type": "application/json"},
			BodyBase64: base64.StdEncoding.EncodeToString([]byte(`{"remainingQueries":29,"totalQueries":30,"windowSizeSeconds":7200}`)),
		})
	}))
	defer server.Close()

	result, err := callBrowserWorkerQuota(context.Background(), server.URL, browserWorkerRequest{SSOToken: "secret"})
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}

func TestBrowserWorkerTimeoutReservesClientResponseBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	timeout := browserWorkerTimeoutSeconds(ctx, 120)
	if timeout < 7 || timeout > 8 {
		t.Fatalf("timeout = %d, want close to 8 seconds", timeout)
	}
}

func TestForwardNonStreamingChatUsesBrowserWorker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/grok/chat" || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var value browserWorkerRequest
		if err := json.NewDecoder(request.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
		if value.Endpoint != "https://grok.com/rest/app-chat/conversations/new" || value.SSOToken != "test-sso" {
			t.Fatalf("worker request = %#v", value)
		}
		fixture := "data: {\"result\":{\"conversation\":{\"conversationId\":\"conv_1\"}}}\n" +
			"data: {\"result\":{\"response\":{\"userResponse\":{\"responseId\":\"parent_1\"}}}}\n" +
			"data: {\"result\":{\"response\":{\"token\":\"pong\",\"isThinking\":false,\"messageTag\":\"final\"}}}\n" +
			"data: [DONE]\n"
		_ = json.NewEncoder(writer).Encode(browserWorkerResponse{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Headers:    map[string]string{"content-type": "text/event-stream"},
			BodyBase64: base64.StdEncoding.EncodeToString([]byte(fixture)),
		})
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://grok.com", BrowserWorkerURL: server.URL}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	body, _ := json.Marshal(map[string]any{
		"model": "grok-chat-auto", "messages": []map[string]string{{"role": "user", "content": "ping"}}, "stream": false,
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, EncryptedAccessToken: encrypted}, Method: http.MethodPost,
		Path: "/chat/completions", Body: body, Model: "grok-chat-auto", Operation: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Contains(result, []byte("pong")) {
		t.Fatalf("status=%d body=%s err=%v", response.StatusCode, result, err)
	}
}
