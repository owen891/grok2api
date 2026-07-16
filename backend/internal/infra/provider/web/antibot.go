package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func isAntiBotStatus(status int) bool {
	switch status {
	case http.StatusForbidden,
		http.StatusUnavailableForLegalReasons,
		520, 521, 522, 523, 524, 525, 526, 530:
		return true
	default:
		return false
	}
}

func isAntiBotResponse(status int, body []byte) bool {
	if status == http.StatusForbidden {
		return looksLikeAntiBot(body)
	}
	return isAntiBotStatus(status) || looksLikeAntiBot(body)
}

func looksLikeAntiBot(body []byte) bool {
	content := strings.ToLower(string(body))
	if content == "" {
		return false
	}
	markers := []string{
		"just a moment",
		"cf-browser-verification",
		"cf-challenge",
		"challenge-platform",
		"cdn-cgi/challenge-platform",
		"attention required",
		"enable javascript and cookies to continue",
		"checking your browser",
		"cloudflare",
		"_cf_chl",
		"cf_chl_",
		"cf-ray",
		"turnstile",
		"access denied",
		"captcha",
		"window._cf_chl_opt",
		"challenges.cloudflare.com",
		"request rejected by anti-bot",
	}
	for _, marker := range markers {
		if strings.Contains(content, marker) {
			return true
		}
	}
	if strings.Contains(content, "<html") && (strings.Contains(content, "challenges.cloudflare.com") || strings.Contains(content, "window._cf_chl_opt") || strings.Contains(content, "cftype")) {
		return true
	}
	return false
}

func cloudflareChallengeMessage(status int) string {
	if status > 0 {
		return fmt.Sprintf("Cloudflare challenge blocked upstream (HTTP %d). Configure a grok_web egress node with valid cf_clearance; avoid direct egress", status)
	}
	return "Cloudflare challenge blocked upstream. Configure a grok_web egress node with valid cf_clearance; avoid direct egress"
}

func antiBotProviderResponse() *provider.Response {
	return antiBotProviderResponseWithStatus(0)
}

func antiBotProviderResponseWithStatus(status int) *provider.Response {
	return jsonProviderResponse(http.StatusForbidden, map[string]any{
		"error": map[string]any{
			"message": cloudflareChallengeMessage(status),
			"type":    "upstream_error",
			"code":    "cloudflare_challenge",
		},
	})
}
