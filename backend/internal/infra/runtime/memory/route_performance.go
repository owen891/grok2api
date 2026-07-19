package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

type RoutePerformanceStore struct {
	mu     sync.Mutex
	values map[repository.RoutePerformanceKey]repository.RoutePerformance
}

func NewRoutePerformanceStore() *RoutePerformanceStore {
	return &RoutePerformanceStore{values: make(map[repository.RoutePerformanceKey]repository.RoutePerformance)}
}

func (s *RoutePerformanceStore) ObserveRoutePerformance(_ context.Context, observation repository.RoutePerformanceObservation, policy repository.RoutePerformancePolicy) error {
	key, ok := normalizeRoutePerformanceKey(observation.Key)
	if !ok {
		return repository.ErrInvalid
	}
	now := observation.ObservedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	policy = normalizeRoutePerformancePolicy(policy)
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.values[key]
	if !exists || now.Sub(value.UpdatedAt) > policy.TTL {
		value = repository.RoutePerformance{}
	}
	sample := 0.0
	if observation.Success {
		sample = 1
	}
	latency := max(time.Duration(0), observation.Latency)
	if value.Samples == 0 {
		value.SuccessEWMA, value.LatencyEWMA = sample, latency
	} else {
		value.SuccessEWMA = policy.Alpha*sample + (1-policy.Alpha)*value.SuccessEWMA
		if latency > 0 {
			if value.LatencyEWMA <= 0 {
				value.LatencyEWMA = latency
			} else {
				value.LatencyEWMA = time.Duration(policy.Alpha*float64(latency) + (1-policy.Alpha)*float64(value.LatencyEWMA))
			}
		}
	}
	value.Samples++
	if observation.Success {
		value.ConsecutiveFailures = 0
		value.LastCircuitFailureAt = nil
		value.CircuitOpenUntil = nil
	} else if observation.CircuitFailure {
		if value.LastCircuitFailureAt == nil || now.Sub(*value.LastCircuitFailureAt) > policy.CircuitWindow {
			value.ConsecutiveFailures = 0
		}
		value.ConsecutiveFailures++
		value.LastCircuitFailureAt = &now
		if value.ConsecutiveFailures >= policy.CircuitThreshold {
			until := now.Add(policy.CircuitOpenDuration)
			value.CircuitOpenUntil = &until
			value.ConsecutiveFailures = 0
		}
	}
	value.UpdatedAt = now
	s.values[key] = value
	if len(s.values) > maxEntries {
		for candidateKey, candidate := range s.values {
			if now.Sub(candidate.UpdatedAt) > policy.TTL {
				delete(s.values, candidateKey)
			}
		}
		for len(s.values) > maxEntries {
			var oldestKey repository.RoutePerformanceKey
			var oldestAt time.Time
			for candidateKey, candidate := range s.values {
				if oldestAt.IsZero() || candidate.UpdatedAt.Before(oldestAt) {
					oldestKey, oldestAt = candidateKey, candidate.UpdatedAt
				}
			}
			delete(s.values, oldestKey)
		}
	}
	return nil
}

func (s *RoutePerformanceStore) GetRoutePerformances(_ context.Context, keys []repository.RoutePerformanceKey, now time.Time) (map[repository.RoutePerformanceKey]repository.RoutePerformance, error) {
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := make(map[repository.RoutePerformanceKey]repository.RoutePerformance, len(keys))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rawKey := range keys {
		key, ok := normalizeRoutePerformanceKey(rawKey)
		if !ok {
			continue
		}
		value, exists := s.values[key]
		if !exists || now.Sub(value.UpdatedAt) > routePerformanceDefaultTTL {
			delete(s.values, key)
			continue
		}
		if value.CircuitOpenUntil != nil && !now.Before(*value.CircuitOpenUntil) {
			value.CircuitOpenUntil = nil
			s.values[key] = value
		}
		result[key] = value
	}
	return result, nil
}

const routePerformanceDefaultTTL = 30 * time.Minute

func normalizeRoutePerformanceKey(value repository.RoutePerformanceKey) (repository.RoutePerformanceKey, bool) {
	value.UpstreamModel = strings.TrimSpace(value.UpstreamModel)
	return value, value.AccountID > 0 && value.UpstreamModel != "" && len(value.UpstreamModel) <= 255
}

func normalizeRoutePerformancePolicy(value repository.RoutePerformancePolicy) repository.RoutePerformancePolicy {
	if value.Alpha <= 0 || value.Alpha > 1 {
		value.Alpha = .25
	}
	if value.TTL <= 0 {
		value.TTL = routePerformanceDefaultTTL
	}
	if value.CircuitThreshold <= 0 {
		value.CircuitThreshold = 3
	}
	if value.CircuitWindow <= 0 {
		value.CircuitWindow = 2 * time.Minute
	}
	if value.CircuitOpenDuration <= 0 {
		value.CircuitOpenDuration = 2 * time.Minute
	}
	return value
}
