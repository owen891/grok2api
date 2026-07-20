package system

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	updatecheckapp "github.com/owen891/grok2api/backend/internal/application/updatecheck"
	"github.com/gin-gonic/gin"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestHandlerReturnsOnlyPublicFrontendConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler("https://api.example.com/", nil).Register(router.Group("/api/admin/v1"))
	request := httptest.NewRequest(http.MethodGet, "/api/admin/v1/system", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var payload struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data["publicApiBaseURL"] != "https://api.example.com" {
		t.Fatalf("data = %#v", payload.Data)
	}
}

func TestHandlerVersionEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"latest":"v3.0.1","repositoryURL":"https://github.com/owen891/grok2api","releases":[{"version":"v3.0.1","entries":[{"type":"fix","zh":"修复说明","en":"Release notes"}]}]}`)),
		}, nil
	})}
	service := updatecheckapp.NewService("v3.0.0", client)
	handler := NewHandler("https://api.example.com/", service)
	router := gin.New()
	handler.Register(router.Group("/api/admin/v1"))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/system/version", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var snapshotPayload struct {
		Data struct {
			CurrentVersion string `json:"currentVersion"`
			LatestVersion  string `json:"latestVersion"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshotPayload); err != nil {
		t.Fatal(err)
	}
	if snapshotPayload.Data.CurrentVersion != "v3.0.0" || snapshotPayload.Data.LatestVersion != "" {
		t.Fatalf("payload = %#v", snapshotPayload.Data)
	}

	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/v1/system/update/check", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("check status = %d", recorder.Code)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshotPayload); err != nil {
		t.Fatal(err)
	}
	if snapshotPayload.Data.CurrentVersion != "v3.0.0" || snapshotPayload.Data.LatestVersion != "v3.0.1" {
		t.Fatalf("checked payload = %#v", snapshotPayload.Data)
	}
}
