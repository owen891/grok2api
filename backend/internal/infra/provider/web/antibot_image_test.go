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
	if candidates[0] != "https://imagine-public.x.ai/imagine-public/images/img_abc123.jpg" {
		t.Fatalf("preferred UUID candidate = %q", candidates[0])
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

func TestInspectLiteCaptureDetectsModeratedFinalImage(t *testing.T) {
	payload := []byte(`{"result":{"response":{"cardAttachment":{"jsonData":"{\"image_chunk\":{\"imageUuid\":\"img_blocked\",\"imageUrl\":\"users/test/generated/blocked/image.jpg\",\"progress\":100,\"moderated\":true}}"}}}}`)
	diagnostics := inspectLiteCapture(payload)
	if !diagnostics.Moderated || diagnostics.MaxProgress != 100 || diagnostics.ImageURLs != 1 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestLiteImagePolicyRefusalRequiresTerminalTextOnlyResponse(t *testing.T) {
	terminal := liteCaptureDiagnostics{SoftStop: true}
	for _, text := range []string{
		"I can't assist with that request because it violates the content policy.",
		"抱歉，无法生成这张图片。",
	} {
		if !isLiteImagePolicyRefusal(terminal, text) {
			t.Fatalf("expected policy refusal for %q", text)
		}
	}
	if isLiteImagePolicyRefusal(liteCaptureDiagnostics{}, "I can't assist with that request") {
		t.Fatal("non-terminal response must remain retryable")
	}
	if isLiteImagePolicyRefusal(liteCaptureDiagnostics{SoftStop: true, ImageChunks: 1}, "content policy") {
		t.Fatal("response containing image chunks must use image moderation metadata")
	}
	if isLiteImagePolicyRefusal(terminal, "The image service returned no artifact") {
		t.Fatal("generic incomplete response must remain retryable")
	}
}
