package ipcheck

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ClassifyCloudflareChallenge classifies captured responses. It does not accept
// a challenge or launch browser automation.
func ClassifyCloudflareChallenge(status int, headers http.Header, body []byte) string {
	if strings.EqualFold(strings.TrimSpace(headers.Get("cf-mitigated")), "challenge") {
		return "challenge"
	}
	if len(body) > int(MaxResponseBytes) {
		return "unknown"
	}
	text := strings.ToLower(string(body))
	challenge := strings.Contains(text, "just a moment") || strings.Contains(text, "challenge-platform") || strings.Contains(text, "cf-turnstile")
	if challenge && (status == http.StatusForbidden || status == http.StatusServiceUnavailable) {
		return "challenge"
	}
	if status == http.StatusForbidden {
		return "blocked"
	}
	if status >= 200 && status < 300 {
		return "none"
	}
	return "unknown"
}

// ClassifyAIRegion is a conservative parser for captured evidence. A
// generic successful webpage is not evidence that inference or login works.
// Supported-country membership alone never claims service availability.
func ClassifyAIRegion(service string, status int, headers http.Header, body []byte, country string, supportedCountries []string) string {
	if service != "claude" && service != "chatgpt" {
		return "unknown"
	}
	if len(body) > int(MaxResponseBytes) {
		return "unknown"
	}
	text := strings.ToLower(string(body))
	for _, marker := range []string{"unsupported_country_region_territory", "unsupported_country", "not available in your region", "not available in your country", "country is not supported"} {
		if strings.Contains(text, marker) {
			return "region_blocked"
		}
	}
	country = strings.ToUpper(strings.TrimSpace(country))
	if supportedCountries != nil && countryCode(country) {
		supported := false
		for _, value := range supportedCountries {
			if strings.EqualFold(country, value) {
				supported = true
				break
			}
		}
		if !supported {
			return "region_blocked"
		}
	}
	challenge := ClassifyCloudflareChallenge(status, headers, body)
	if challenge == "challenge" || status == http.StatusTooManyRequests {
		return "risky"
	}
	// Availability requires structured, explicit provider evidence, not HTML.
	if status >= 200 && status < 300 && strings.HasPrefix(strings.ToLower(headers.Get("Content-Type")), "application/json") {
		// The accepted JSON shape is deliberately local and provider-neutral; no
		// undocumented live endpoint is assumed to have this contract.
		if explicitAvailability(body, service) {
			return "available"
		}
	}
	return "unknown"
}

func explicitAvailability(body []byte, service string) bool {
	var value struct {
		Service   string `json:"service"`
		Available *bool  `json:"available"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return false
	}
	return value.Service == service && value.Available != nil && *value.Available
}
