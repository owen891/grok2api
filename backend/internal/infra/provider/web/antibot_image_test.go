package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestLooksLikeAntiBotCloudflareChallengeHTML(t *testing.T) {
	body := []byte(`<!DOCTYPE html><html><head><title>Just a moment...</title></head><body>
<script>window._cf_chl_opt={cType:'managed'};</script>
<script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1?ray=abc"></script>
</body></html>`)
	if !looksLikeAntiBot(body) {
		t.Fatal("expected cloudflare challenge html to be detected")
	}
	if !isAntiBotStatus(403) {
		t.Fatal("expected 403 to be anti-bot status")
	}
	resp := antiBotProviderResponseWithStatus(403)
	if resp.StatusCode != 403 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestIsAntiBotResponseDoesNotClassifyPlain403JSON(t *testing.T) {
	if isAntiBotResponse(http.StatusForbidden, []byte(`{"error":"account rejected"}`)) {
		t.Fatal("plain JSON 403 must remain an upstream account error")
	}
	if !isAntiBotResponse(http.StatusForbidden, []byte("<html>Just a moment...</html>")) {
		t.Fatal("Cloudflare challenge body was not detected")
	}
}

func TestExtractCapturedImageCandidatesFromUUID(t *testing.T) {
	payload := []byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUuid":"img_abc123","progress":100},"isSoftStop":true}}}`)
	candidates := extractCapturedImageCandidates(payload)
	if len(candidates) == 0 {
		t.Fatal("expected uuid-based candidates")
	}
	joined := strings.Join(candidates, "\n")
	if !strings.Contains(joined, "img_abc123") {
		t.Fatalf("candidates missing uuid: %v", candidates)
	}
}

func TestCollectCapturedImageURLsAcceptsFinalAsset(t *testing.T) {
	payload := map[string]any{
		"imageUrl": "users/user_1/generated/cat/final.jpg",
		"progress": float64(100),
	}
	results := make([]string, 0, 1)
	collectCapturedImageURLs(payload, &results)
	if len(results) != 1 {
		t.Fatalf("results=%v", results)
	}
	if !strings.Contains(results[0], "assets.grok.com") && !strings.Contains(results[0], "/generated/") {
		t.Fatalf("unexpected url %s", results[0])
	}
}

func TestCollectCapturedImageURLsRejectsPartFrames(t *testing.T) {
	payload := map[string]any{
		"imageUrl": "users/user_1/generated/cat/image-part-1.jpg",
		"progress": float64(40),
	}
	results := make([]string, 0, 1)
	collectCapturedImageURLs(payload, &results)
	if len(results) != 0 {
		t.Fatalf("part frame should be rejected: %v", results)
	}
}
