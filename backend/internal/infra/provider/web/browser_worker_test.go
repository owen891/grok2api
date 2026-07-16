package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestCallBrowserWorkerPropagatesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(writer).Encode(browserWorkerResponse{Error: "browser unavailable"})
	}))
	defer server.Close()

	_, err := callBrowserWorker(context.Background(), server.URL, browserWorkerRequest{})
	if err == nil || err.Error() != "browser unavailable" {
		t.Fatalf("error = %v", err)
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
