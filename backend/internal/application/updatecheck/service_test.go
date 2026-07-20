package updatecheck

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestCheckFindsLatestRelease(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != latestManifestURL || request.Header.Get("User-Agent") != "grok2api/v3.0.0" {
			t.Fatalf("request = %#v", request)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"latest":"v3.0.1","repositoryURL":"https://github.com/owen891/grok2api","releases":[{"version":"v3.0.1","entries":[{"type":"fix","zh":"修复说明","en":"Release notes"}]}]}`)), Header: make(http.Header)}, nil
	})}
	service := NewService("v3.0.0", client)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	snapshot := service.Check(context.Background())
	if snapshot.Status != StatusUpdateAvailable || !snapshot.UpdateAvailable || snapshot.LatestVersion != "v3.0.1" || snapshot.CheckedAt == nil || !snapshot.CheckedAt.Equal(now) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.ReleaseURL != "https://github.com/owen891/grok2api/tree/v3.0.1" || snapshot.ReleaseNotes != "- 修复说明" {
		t.Fatalf("release = %#v", snapshot)
	}
}

func TestCheckFailureKeepsLastSuccessfulRelease(t *testing.T) {
	fail := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if fail {
			return nil, errors.New("network down")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"latest":"v3.0.0","repositoryURL":"https://github.com/owen891/grok2api","releases":[{"version":"v3.0.0","entries":[{"type":"ops","zh":"稳定版","en":"Stable"}]}]}`)), Header: make(http.Header)}, nil
	})}
	service := NewService("v3.0.0", client)
	first := service.Check(context.Background())
	fail = true
	second := service.Check(context.Background())
	if first.Status != StatusUpToDate || second.Status != StatusCheckFailed || second.LatestVersion != "v3.0.0" || second.CheckedAt == nil || second.Error == "" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
}

func TestCheckIgnoresCallerCancellationForSharedRefresh(t *testing.T) {
	requests := 0
	var mu sync.Mutex
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Context().Err() != nil {
			t.Fatalf("request context should not be cancelled")
		}
		mu.Lock()
		requests++
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"latest":"v3.0.1","repositoryURL":"https://github.com/owen891/grok2api","releases":[{"version":"v3.0.1","entries":[{"type":"fix","zh":"修复说明","en":"Release notes"}]}]}`)), Header: make(http.Header)}, nil
	})}
	service := NewService("v3.0.0", client)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot := service.Check(cancelled)
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if snapshot.Status != StatusUpdateAvailable || snapshot.LatestVersion != "v3.0.1" || !snapshot.UpdateAvailable {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestSemanticVersionComparison(t *testing.T) {
	stable, ok := parseSemanticVersion("v3.0.1")
	if !ok {
		t.Fatal("stable version was rejected")
	}
	older, _ := parseSemanticVersion("3.0.0")
	prerelease, _ := parseSemanticVersion("v3.0.1-rc.1")
	if compareSemanticVersion(stable, older) <= 0 || compareSemanticVersion(prerelease, stable) >= 0 {
		t.Fatal("semantic version ordering is invalid")
	}
	if _, ok := parseSemanticVersion("dev"); ok {
		t.Fatal("development version was accepted as semver")
	}
}
