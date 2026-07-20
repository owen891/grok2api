package web

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	egressdomain "github.com/owen891/grok2api/backend/internal/domain/egress"
	infraegress "github.com/owen891/grok2api/backend/internal/infra/egress"
	"github.com/owen891/grok2api/backend/internal/infra/security"
	"github.com/owen891/grok2api/backend/internal/repository"
)

func TestParseCapturedWeeklyCreditsResponse(t *testing.T) {
	body, err := hex.DecodeString("00000000630a610d0000304112001a00220c089abbccd2061080f2d1fc012a0c089ab0f1d2061080f2d1fc013a07080515000020413a070804150000803f3a020802421e0802120c089abbccd2061080f2d1fc011a0c089ab0f1d2061080f2d1fc01580162006801800000000f677270632d7374617475733a300d0a")
	if err != nil {
		t.Fatal(err)
	}
	syncedAt := time.Date(2026, 7, 12, 14, 0, 0, 0, time.UTC)
	window, err := parseWeeklyCreditsResponse(body, 42, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if window.AccountID != 42 || window.Mode != weeklyQuotaMode || window.Total != 10000 || window.Remaining != 8900 || window.WindowSeconds != 7*24*60*60 {
		t.Fatalf("window = %#v", window)
	}
	if math.Abs(window.UsagePercent-11) > 0.001 || window.ResetAt == nil || window.ResetAt.Unix() != 1784436762 {
		t.Fatalf("usage/reset = %#v", window)
	}
	if len(window.Breakdown) != 3 || window.Breakdown[0].ProductCode != account.QuotaProductImagine || window.Breakdown[0].UsagePercent != 10 || window.Breakdown[1].ProductCode != account.QuotaProductChat || window.Breakdown[1].UsagePercent != 1 || window.Breakdown[2].ProductCode != account.QuotaProductBuild || window.Breakdown[2].UsagePercent != 0 {
		t.Fatalf("breakdown = %#v", window.Breakdown)
	}
}

func TestInferWebTierFromUpstreamQuota(t *testing.T) {
	tests := []struct {
		name    string
		windows []account.QuotaWindow
		want    account.WebTier
		known   bool
	}{
		{name: "current basic", windows: []account.QuotaWindow{{Mode: "auto", Total: 7}, {Mode: "fast", Total: 30}}, want: account.WebTierBasic, known: true},
		{name: "legacy basic", windows: []account.QuotaWindow{{Mode: "auto", Total: 20}}, want: account.WebTierBasic, known: true},
		{name: "super", windows: []account.QuotaWindow{{Mode: "auto", Total: 50}, {Mode: "fast", Total: 140}}, want: account.WebTierSuper, known: true},
		{name: "heavy", windows: []account.QuotaWindow{{Mode: "auto", Total: 150}, {Mode: "fast", Total: 400}}, want: account.WebTierHeavy, known: true},
		{name: "heavy mode", windows: []account.QuotaWindow{{Mode: "heavy", Total: 20}}, want: account.WebTierHeavy, known: true},
		{name: "conflicting signal uses lower tier", windows: []account.QuotaWindow{{Mode: "auto", Total: 50}, {Mode: "fast", Total: 30}}, want: account.WebTierBasic, known: true},
		{name: "unknown", windows: []account.QuotaWindow{{Mode: "auto", Total: 9}, {Mode: "fast", Total: 31}}, want: account.WebTierAuto, known: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, known := inferWebTierFromQuota(test.windows)
			if got != test.want || known != test.known {
				t.Fatalf("tier = %q, known = %v, want %q/%v", got, known, test.want, test.known)
			}
		})
	}
}

func TestResolveWebTierUsesFreshWebQuotaOverStoredTier(t *testing.T) {
	basicWindows := []account.QuotaWindow{{Mode: "auto", Total: 7}, {Mode: "fast", Total: 30}}
	for _, stored := range []account.WebTier{account.WebTierAuto, account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy} {
		tier, useWeekly := resolveWebTierFromQuota(stored, basicWindows, true)
		if tier != account.WebTierBasic || useWeekly {
			t.Fatalf("stored %q resolved to %q, weekly=%v", stored, tier, useWeekly)
		}
	}

	tier, useWeekly := resolveWebTierFromQuota(account.WebTierBasic, []account.QuotaWindow{{Mode: "auto", Total: 50}}, true)
	if tier != account.WebTierSuper || !useWeekly {
		t.Fatalf("super snapshot resolved to %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierHeavy, nil, true)
	if tier != account.WebTierHeavy || !useWeekly {
		t.Fatalf("heavy weekly fallback resolved to %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierSuper, []account.QuotaWindow{{Mode: "auto", Total: 9}}, true)
	if tier != account.WebTierAuto || useWeekly {
		t.Fatalf("unknown snapshot resolved to %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierSuper, nil, true)
	if tier != account.WebTierSuper || !useWeekly {
		t.Fatalf("super weekly fallback resolved to %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierBasic, nil, true)
	if tier != account.WebTierBasic || useWeekly {
		t.Fatalf("basic should not be promoted when modes unavailable: got %q, weekly=%v", tier, useWeekly)
	}

	tier, useWeekly = resolveWebTierFromQuota(account.WebTierAuto, nil, true)
	if tier != account.WebTierAuto || useWeekly {
		t.Fatalf("auto should not be promoted when modes unavailable: got %q, weekly=%v", tier, useWeekly)
	}
}

func TestSyncQuotaCorrectsStoredSuperFromFreshWebQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig":
			http.Error(writer, "not available", http.StatusNotFound)
		case "/rest/rate-limits":
			var payload struct {
				ModelName string `json:"modelName"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("quota payload: %v", err)
			}
			total := 0
			switch payload.ModelName {
			case "auto":
				total = 7
			case "fast":
				total = 30
			default:
				http.Error(writer, "unsupported mode", http.StatusBadRequest)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"windowSizeSeconds": 7200, "remainingQueries": total, "totalQueries": total,
			})
		default:
			http.NotFound(writer, request)
		}
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
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test-signature",
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	snapshot, err := adapter.SyncQuota(context.Background(), account.Credential{
		ID: 1, WebTier: account.WebTierSuper, EncryptedAccessToken: encrypted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Tier != account.WebTierBasic || len(snapshot.Windows) != 2 || snapshot.Windows[0].Mode != "auto" || snapshot.Windows[0].Total != 7 || snapshot.Windows[1].Mode != "fast" || snapshot.Windows[1].Total != 30 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestQuotaForbiddenDoesNotPoisonWorkingImageEgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "forbidden", http.StatusForbidden)
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
	repository := &quotaEgressRepository{node: egressdomain.Node{ID: 1, Name: "image-egress", Scope: egressdomain.ScopeWeb, Enabled: true, Health: 1}}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test-signature",
	}, infraegress.NewManager(repository, cipher), cipher, nil, nil)

	_, err = adapter.SyncQuotaMode(context.Background(), account.Credential{ID: 1, EncryptedAccessToken: encrypted}, "fast")
	if err == nil {
		t.Fatal("forbidden quota response unexpectedly succeeded")
	}
	if repository.updates != 0 || repository.node.Health != 1 || repository.node.LastError != "" {
		t.Fatalf("quota 403 poisoned egress: updates=%d node=%#v", repository.updates, repository.node)
	}
}

func TestSyncQuotaModeUsesBrowserWorkerWhenConfigured(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/grok/quota" {
			t.Fatalf("worker path = %s", request.URL.Path)
		}
		var value browserWorkerRequest
		if err := json.NewDecoder(request.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
		if value.Endpoint != "https://grok.com/rest/rate-limits" || value.Payload["modelName"] != "fast" || value.SSOToken != "test-sso" {
			t.Fatalf("worker request = %#v", value)
		}
		body := []byte(`{"windowSizeSeconds":7200,"remainingQueries":29,"totalQueries":30}`)
		_ = json.NewEncoder(writer).Encode(browserWorkerResponse{
			StatusCode: http.StatusOK, Status: "200 OK",
			Headers: map[string]string{"content-type": "application/json"}, BodyBase64: base64.StdEncoding.EncodeToString(body),
		})
	}))
	defer worker.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: "https://grok.com", BrowserWorkerURL: worker.URL}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	window, err := adapter.SyncQuotaMode(context.Background(), account.Credential{ID: 42, EncryptedAccessToken: encrypted}, "fast")
	if err != nil {
		t.Fatal(err)
	}
	if window.AccountID != 42 || window.Mode != "fast" || window.Remaining != 29 || window.Total != 30 {
		t.Fatalf("window = %#v", window)
	}
}

type quotaEgressRepository struct {
	node    egressdomain.Node
	updates int
}

func (r *quotaEgressRepository) ListEgressNodes(_ context.Context, scope egressdomain.Scope, _ repository.SortQuery) ([]egressdomain.Node, error) {
	if scope != "" && scope != r.node.Scope {
		return nil, nil
	}
	return []egressdomain.Node{r.node}, nil
}

func (r *quotaEgressRepository) GetEgressNode(context.Context, uint64) (egressdomain.Node, error) {
	return r.node, nil
}

func (r *quotaEgressRepository) CreateEgressNode(_ context.Context, value egressdomain.Node) (egressdomain.Node, error) {
	r.node = value
	return value, nil
}

func (r *quotaEgressRepository) UpdateEgressNode(_ context.Context, value egressdomain.Node) (egressdomain.Node, error) {
	r.node = value
	r.updates++
	return value, nil
}

func (r *quotaEgressRepository) DeleteEgressNode(context.Context, uint64) error {
	return nil
}
