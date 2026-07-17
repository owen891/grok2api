package registration

import (
	"os"
	"strings"
)

func resolveRegistrationProxy(raw string) (string, bool, string) {
	return resolveRegistrationProxyWithSystem(raw, systemProxyURL)
}

func resolveRegistrationProxyWithSystem(raw string, lookup func() string) (string, bool, string) {
	raw = strings.TrimSpace(raw)
	if !strings.EqualFold(raw, "system") {
		ok, detail := proxyReady(raw)
		return raw, ok, detail
	}
	effective := strings.TrimSpace(lookup())
	if effective == "" {
		return "", false, "system proxy is not configured"
	}
	ok, detail := proxyReady(effective)
	return effective, ok, "system -> " + detail
}

func systemProxyURL() string {
	for _, key := range []string{
		"REGISTRATION_HTTPS_PROXY", "REGISTRATION_HTTP_PROXY", "REGISTRATION_ALL_PROXY",
		"REGISTRATION_SOLVER_PROXY",
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy",
	} {
		if value := normalizeSystemProxySetting(os.Getenv(key)); value != "" {
			return value
		}
	}
	return normalizeSystemProxySetting(platformSystemProxy())
}

func normalizeSystemProxySetting(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "=") {
		values := make(map[string]string)
		for _, entry := range strings.Split(raw, ";") {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				values[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
			}
		}
		for _, key := range []string{"https", "http", "socks"} {
			if value := values[key]; value != "" {
				if strings.Contains(value, "://") {
					return value
				}
				if key == "socks" {
					return "socks5://" + value
				}
				return "http://" + value
			}
		}
		return ""
	}
	if strings.Contains(raw, "://") {
		return raw
	}
	return "http://" + raw
}
