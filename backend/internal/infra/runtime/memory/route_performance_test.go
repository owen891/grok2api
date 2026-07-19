package memory

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRoutePerformanceStoreOpensCircuitAndResetsFailureWindow(t *testing.T) {
	ctx := context.Background()
	store := NewRoutePerformanceStore()
	key := repository.RoutePerformanceKey{AccountID: 7, UpstreamModel: "image-model"}
	policy := repository.RoutePerformancePolicy{Alpha: .25, TTL: 30 * time.Minute, CircuitThreshold: 3, CircuitWindow: time.Minute, CircuitOpenDuration: 2 * time.Minute}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	observe := func(at time.Time, success, circuitFailure bool) {
		t.Helper()
		if err := store.ObserveRoutePerformance(ctx, repository.RoutePerformanceObservation{Key: key, Latency: time.Second, Success: success, CircuitFailure: circuitFailure, ObservedAt: at}, policy); err != nil {
			t.Fatal(err)
		}
	}
	observe(now, false, true)
	observe(now.Add(2*time.Minute), false, true)
	observe(now.Add(2*time.Minute+time.Second), false, true)
	state, err := store.GetRoutePerformances(ctx, []repository.RoutePerformanceKey{key}, now.Add(2*time.Minute+time.Second))
	if err != nil || state[key].CircuitOpenUntil != nil || state[key].ConsecutiveFailures != 2 {
		t.Fatalf("window-reset state=%#v err=%v", state[key], err)
	}
	observe(now.Add(2*time.Minute+2*time.Second), false, true)
	state, err = store.GetRoutePerformances(ctx, []repository.RoutePerformanceKey{key}, now.Add(2*time.Minute+2*time.Second))
	if err != nil || state[key].CircuitOpenUntil == nil || !state[key].CircuitOpenUntil.After(now) {
		t.Fatalf("open-circuit state=%#v err=%v", state[key], err)
	}
	observe(now.Add(2*time.Minute+3*time.Second), true, false)
	state, err = store.GetRoutePerformances(ctx, []repository.RoutePerformanceKey{key}, now.Add(2*time.Minute+3*time.Second))
	if err != nil || state[key].CircuitOpenUntil != nil || state[key].ConsecutiveFailures != 0 {
		t.Fatalf("success-reset state=%#v err=%v", state[key], err)
	}
}
