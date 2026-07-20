package operations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	operationsapp "github.com/owen891/grok2api/backend/internal/application/operations"
	"github.com/owen891/grok2api/backend/internal/domain/account"
	modeldomain "github.com/owen891/grok2api/backend/internal/domain/model"
)

func TestHandlerReturnsOperationalSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := operationsapp.NewService(modelStub{}, capacityStub{}, quotaStub{}, nil, operationsapp.ReplenishmentConfig{})
	router := gin.New()
	NewHandler(service).Register(router.Group("/api/admin/v1"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/operations", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerTriggersConfiguredReplenishment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := operationsapp.NewService(modelStub{}, capacityStub{}, quotaStub{}, nil, operationsapp.ReplenishmentConfig{Enabled: true})
	trigger := &triggerStub{}
	service.SetReplenishmentTrigger(trigger)
	router := gin.New()
	NewHandler(service).Register(router.Group("/api/admin/v1"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/v1/operations/replenishment/trigger", nil))
	if recorder.Code != http.StatusAccepted || trigger.calls != 1 || !strings.Contains(recorder.Body.String(), `"triggered":true`) {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, trigger.calls, recorder.Body.String())
	}
}

type modelStub struct{}

func (modelStub) ListConfiguredEnabled(context.Context) ([]modeldomain.Route, error) {
	return []modeldomain.Route{{ID: 1, Provider: account.ProviderBuild, PublicID: "Build/grok-4.5", UpstreamModel: "grok-4.5", Capability: modeldomain.CapabilityResponses}}, nil
}

type capacityStub struct{}

func (capacityStub) CapacitySnapshot(context.Context, account.Provider, string, string, time.Duration) (account.RoutingCapacity, error) {
	return account.RoutingCapacity{Total: 1, Eligible: 1}, nil
}

type quotaStub struct{}

func (quotaStub) QuotaMode(account.Provider, string) string { return "" }

type triggerStub struct{ calls int }

func (s *triggerStub) Trigger(context.Context) error {
	s.calls++
	return nil
}
