package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clientkeyapp "github.com/owen891/grok2api/backend/internal/application/clientkey"
	"github.com/gin-gonic/gin"
)

func TestClientRuntimeStoreFailureUsesServiceUnavailable(t *testing.T) {
	err := errors.Join(clientkeyapp.ErrRuntimeUnavailable, errors.New("redis unavailable"))
	if status := clientErrorStatus(err); status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", status)
	}
	if code := clientErrorCode(err); code != "runtime_store_unavailable" {
		t.Fatalf("code = %q", code)
	}
	if message := clientErrorMessage(err); message == err.Error() {
		t.Fatal("runtime implementation detail leaked to client")
	}
}

func TestClientAuthErrorIncludesRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	context.Set(RequestIDKey, "req-client-auth")

	writeOpenAIError(context, http.StatusUnauthorized, "invalid_api_key", "invalid key")

	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), `"request_id":"req-client-auth"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBearerTokenAcceptsCaseInsensitiveSchemeAndWhitespace(t *testing.T) {
	token, ok := bearerToken("  bearer\tsecret-token  ")
	if !ok || token != "secret-token" {
		t.Fatalf("token = %q, ok = %v", token, ok)
	}
	for _, value := range []string{"", "Bearer", "Basic token", "Bearer token extra"} {
		if _, ok := bearerToken(value); ok {
			t.Fatalf("header %q unexpectedly accepted", value)
		}
	}
}

func TestImageJobPollingUsesReadOnlyAuthenticationPath(t *testing.T) {
	for _, test := range []struct {
		method string
		path   string
		want   bool
	}{
		{method: http.MethodGet, path: "/v1/images/image_opaque", want: true},
		{method: http.MethodPost, path: "/v1/images/image_opaque", want: false},
		{method: http.MethodGet, path: "/v1/images/not-an-image-job", want: false},
		{method: http.MethodGet, path: "/v1/models", want: false},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		if got := isImageJobPoll(request); got != test.want {
			t.Fatalf("%s %s = %v, want %v", test.method, test.path, got, test.want)
		}
	}
}
