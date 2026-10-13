package ipcheck

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	cloudflareConfigurationReason = "unavailable: configure cloudflare_challenge_urls to use this optional source"
	aiRegionConfigurationReason   = "unavailable: configure ai_region_targets to use this optional source"
)

// AIRegionTargetConfig describes one explicitly selected observation endpoint.
// There are no built-in provider endpoints or maintained country lists. A
// supported country is not proof that login, inference, or an account works.
type AIRegionTargetConfig struct {
	URL                string   `json:"url"`
	SupportedCountries []string `json:"supported_countries,omitempty"`
}

func validateOptionalTargets(challengeURLs []string, regionTargets map[string]AIRegionTargetConfig) error {
	if len(challengeURLs) > MaxTargets {
		return invalid("cloudflare_challenge_urls must contain at most 8 URLs")
	}
	seen := map[string]bool{}
	for _, target := range challengeURLs {
		if err := ValidateTarget(target); err != nil {
			return err
		}
		label := optionalTargetLabel(target)
		if seen[label] {
			return invalid("duplicate cloudflare challenge target")
		}
		seen[label] = true
	}
	for service, target := range regionTargets {
		if service != "claude" && service != "chatgpt" {
			return invalid("ai_region_targets supports only claude and chatgpt")
		}
		if err := ValidateTarget(target.URL); err != nil {
			return err
		}
		if target.SupportedCountries != nil && (len(target.SupportedCountries) == 0 || len(target.SupportedCountries) > 249) {
			return invalid("supported_countries must contain between 1 and 249 country codes when provided")
		}
		countries := map[string]bool{}
		for _, country := range target.SupportedCountries {
			if !countryCode(country) {
				return invalid("supported_countries requires uppercase two-letter country codes")
			}
			if countries[country] {
				return invalid("duplicate supported country")
			}
			countries[country] = true
		}
	}
	return nil
}

func cloneAIRegionTargets(targets map[string]AIRegionTargetConfig) map[string]AIRegionTargetConfig {
	cloned := make(map[string]AIRegionTargetConfig, len(targets))
	for service, target := range targets {
		target.SupportedCountries = append([]string(nil), target.SupportedCountries...)
		cloned[service] = target
	}
	return cloned
}

type cloudflareChallengeSource struct{ targets []string }

func (*cloudflareChallengeSource) Name() string      { return "cloudflare-challenge" }
func (*cloudflareChallengeSource) Kind() SourceKind  { return SourceKindViaNode }
func (*cloudflareChallengeSource) RequiresKey() bool { return false }
func (s *cloudflareChallengeSource) Check(ctx context.Context, req CheckRequest) (*SourceResult, error) {
	if len(s.targets) == 0 {
		return &SourceResult{Status: "unavailable", Error: cloudflareConfigurationReason}, nil
	}
	result := &SourceResult{OK: true, Status: "ok", Data: Data{CFChallenge: "unknown", TargetErrors: map[string]string{}}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	allClear, challenged, blocked := true, false, false
	for _, target := range s.targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			observation, err := observeOptionalTarget(ctx, req.HTTP, target)
			classification := "unknown"
			if err == nil {
				classification = ClassifyCloudflareChallenge(observation.status, observation.headers, observation.body)
				if classification == "unknown" && !successfulStatus(observation.status) {
					err = errBadStatus
				}
			} else if strings.EqualFold(strings.TrimSpace(observation.headers.Get("cf-mitigated")), "challenge") {
				// A complete response header remains evidence even when reading
				// its body times out or exceeds the cap. Never classify partial HTML.
				classification = "challenge"
			}
			mu.Lock()
			defer mu.Unlock()
			allClear = allClear && classification == "none" && err == nil
			challenged = challenged || classification == "challenge"
			blocked = blocked || classification == "blocked"
			if err != nil {
				result.Data.TargetErrors[optionalTargetLabel(target)] = publicSourceError(err)
			}
		}(target)
	}
	wg.Wait()
	switch {
	case blocked:
		result.Data.CFChallenge = "blocked"
	case challenged:
		result.Data.CFChallenge = "challenge"
	case allClear:
		result.Data.CFChallenge = "none"
	}
	markOptionalFailures(result, "one or more challenge targets failed")
	return result, nil
}

type aiRegionSource struct {
	targets map[string]AIRegionTargetConfig
}

func (*aiRegionSource) Name() string      { return "ai-region" }
func (*aiRegionSource) Kind() SourceKind  { return SourceKindViaNode }
func (*aiRegionSource) RequiresKey() bool { return false }
func (s *aiRegionSource) Check(ctx context.Context, req CheckRequest) (*SourceResult, error) {
	if len(s.targets) == 0 {
		return &SourceResult{Status: "unavailable", Error: aiRegionConfigurationReason}, nil
	}
	result := &SourceResult{OK: true, Status: "ok", Data: Data{Services: map[string]string{"claude": "unknown", "chatgpt": "unknown"}, TargetErrors: map[string]string{}}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for service, target := range s.targets {
		wg.Add(1)
		go func(service string, target AIRegionTargetConfig) {
			defer wg.Done()
			observation, err := observeOptionalTarget(ctx, req.HTTP, target.URL)
			classification := "unknown"
			if err == nil {
				country := ""
				if successfulStatus(observation.status) {
					country = traceCountry(observation.body)
				}
				classification = ClassifyAIRegion(service, observation.status, observation.headers, observation.body, country, target.SupportedCountries)
				if classification == "unknown" && !successfulStatus(observation.status) {
					err = errBadStatus
				}
			} else if observation.status == http.StatusTooManyRequests || strings.EqualFold(strings.TrimSpace(observation.headers.Get("cf-mitigated")), "challenge") {
				classification = "risky"
			}
			mu.Lock()
			defer mu.Unlock()
			result.Data.Services[service] = classification
			if err != nil {
				result.Data.TargetErrors[service] = publicSourceError(err)
			}
		}(service, target)
	}
	wg.Wait()
	markOptionalFailures(result, "one or more AI region targets failed")
	return result, nil
}

type optionalObservation struct {
	status  int
	headers http.Header
	body    []byte
}

// Only the engine's controlled HTTP client is used: no default client, direct
// fallback, credential, challenge submission, or additional endpoint discovery.
func observeOptionalTarget(ctx context.Context, client *http.Client, target string) (optionalObservation, error) {
	observation := optionalObservation{}
	if client == nil {
		return observation, errors.New("HTTP unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return observation, err
	}
	request.Header.Set("Accept", "text/plain, application/json, text/html")
	response, err := client.Do(request)
	if err != nil {
		return observation, err
	}
	observation.status, observation.headers = response.StatusCode, response.Header.Clone()
	if response.Body == nil {
		return observation, errors.New("empty response body")
	}
	defer response.Body.Close()
	if response.ContentLength > MaxResponseBytes {
		return observation, errResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return observation, err
	}
	if int64(len(body)) > MaxResponseBytes {
		return observation, errResponseTooLarge
	}
	observation.body = body
	return observation, nil
}

func successfulStatus(status int) bool { return status >= 200 && status < 300 }

func optionalTargetLabel(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return "configured target"
	}
	u.RawQuery, u.ForceQuery = "", false
	return u.String()
}

func markOptionalFailures(result *SourceResult, message string) {
	if len(result.Data.TargetErrors) > 0 {
		result.OK, result.Status, result.Error = false, "error", message
	}
}

// Parse only a complete key=value trace, never a substring in an HTML page or
// an error payload. Missing, duplicate, malformed and unknown loc stay unknown.
func traceCountry(body []byte) string {
	country := strings.ToUpper(traceValue(body, "loc"))
	if !countryCode(country) {
		return ""
	}
	return country
}

func countryCode(country string) bool {
	return len(country) == 2 && country[0] >= 'A' && country[0] <= 'Z' && country[1] >= 'A' && country[1] <= 'Z' && country != "XX" && country != "ZZ"
}
