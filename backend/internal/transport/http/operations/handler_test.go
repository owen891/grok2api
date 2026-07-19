package operations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	operationsapp "github.com/chenyme/grok2api/backend/internal/application/operations"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
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
