package registration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	registrationapp "github.com/chenyme/grok2api/backend/internal/application/registration"
	"github.com/gin-gonic/gin"
)

type controllerStub struct {
	status registrationapp.Status
	err    error
}

func (s *controllerStub) Status() (registrationapp.Status, error) { return s.status, s.err }
func (s *controllerStub) Start(context.Context, registrationapp.StartInput) (registrationapp.Status, error) {
	return s.status, s.err
}
func (s *controllerStub) Stop(context.Context) (registrationapp.Status, error) {
	return s.status, s.err
}
func (s *controllerStub) Logs(int) (registrationapp.LogResult, error) {
	return registrationapp.LogResult{Items: []registrationapp.LogEntry{{ID: 2, Text: "latest"}}, NextLogID: 2}, s.err
}
func (s *controllerStub) Settings() (registrationapp.WorkerSettings, error) {
	return registrationapp.WorkerSettings{EmailProvider: "tempmail_lol"}, s.err
}
func (s *controllerStub) UpdateSettings(registrationapp.WorkerSettingsPatch) (registrationapp.WorkerSettings, error) {
	return s.Settings()
}
func (s *controllerStub) Preflight(context.Context) registrationapp.PreflightResult {
	return registrationapp.PreflightResult{OK: true}
}

func TestHandlerRegistersTaskEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/api/admin/v1")
	NewHandler(&controllerStub{status: registrationapp.Status{Configured: true, Running: true, PID: 42}}).Register(group)

	for _, request := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/api/admin/v1/registration", "", http.StatusOK},
		{http.MethodGet, "/api/admin/v1/registration/logs", "", http.StatusOK},
		{http.MethodGet, "/api/admin/v1/registration/config", "", http.StatusOK},
		{http.MethodPut, "/api/admin/v1/registration/config", `{}`, http.StatusOK},
		{http.MethodGet, "/api/admin/v1/registration/preflight", "", http.StatusOK},
		{http.MethodPost, "/api/admin/v1/registration/start", `{"count":1,"threads":1,"fast":true}`, http.StatusAccepted},
		{http.MethodPost, "/api/admin/v1/registration/stop", `{}`, http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		httpRequest := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
		httpRequest.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, httpRequest)
		if recorder.Code != request.status {
			t.Fatalf("%s %s status = %d, body = %s", request.method, request.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestHandlerMapsRunningConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(&controllerStub{err: registrationapp.ErrRunning}).Register(router.Group("/api/admin/v1"))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/registration/start", strings.NewReader(`{"count":1,"threads":1}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "registrationRunning") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerMapsUnexpectedFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(&controllerStub{err: errors.New("boom")}).Register(router.Group("/api/admin/v1"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/registration", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestHandlerReturnsUnconfiguredStatusWhenRegistrationIsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(&controllerStub{status: registrationapp.Status{Configured: false}}).Register(router.Group("/api/admin/v1"))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/registration", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"configured":false`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}
