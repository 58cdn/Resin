package ipcheck

import (
	"math"
	"net/netip"
	"sort"
)

func aggregate(ip netip.Addr, results []SourceResult, priority []string) *Result {
	ranks := map[string]int{}
	for i, name := range priority {
		ranks[name] = i
	}
	rank := func(name string) int {
		if value, ok := ranks[name]; ok {
			return value
		}
		return len(priority)
	}
	sort.Slice(results, func(i, j int) bool {
		a, b := rank(results[i].Source), rank(results[j].Source)
		if a != b {
			return a < b
		}
		return results[i].Source < results[j].Source
	})
	summary := Summary{Country: "unknown", City: "unknown", ASOrg: "unknown", IPType: "unknown", IPOrigin: "unknown", AbuseFlags: []string{}, CFChallenge: "unknown", EgressIPsBySite: map[string]string{}, Services: map[string]string{"claude": "unknown", "chatgpt": "unknown"}, RiskLevel: "unknown"}
	result := &Result{EgressIP: ip.String(), Summary: summary, Sources: results}
	flags := map[string]bool{}
	for _, source := range results {
		if source.CheckedAt.After(result.CheckedAt) {
			result.CheckedAt = source.CheckedAt
		}
		data := source.Data
		// Preserve partial evidence, while unknown targets cannot imply success.
		for site, observed := range data.EgressIPsBySite {
			if _, exists := result.Summary.EgressIPsBySite[site]; !exists {
				result.Summary.EgressIPsBySite[site] = observed
			}
		}
		if result.Summary.EgressConsistent == nil && data.EgressConsistent != nil {
			value := *data.EgressConsistent
			result.Summary.EgressConsistent = &value
		}
		if allowed(data.CFChallenge, "none", "challenge", "blocked") {
			firstKnown(&result.Summary.CFChallenge, data.CFChallenge)
		}
		for service, value := range data.Services {
			if _, exists := result.Summary.Services[service]; exists && allowed(value, "available", "region_blocked", "risky") && result.Summary.Services[service] == "unknown" {
				result.Summary.Services[service] = value
			}
		}
		if !source.OK {
			continue
		}
		firstKnown(&result.Summary.Country, data.Country)
		firstKnown(&result.Summary.City, data.City)
		firstKnown(&result.Summary.ASOrg, data.ASOrg)
		if result.Summary.ASN == nil && data.ASN != nil {
			value := *data.ASN
			result.Summary.ASN = &value
		}
		if allowed(data.IPType, "residential", "datacenter", "mobile") {
			firstKnown(&result.Summary.IPType, data.IPType)
		}
		if allowed(data.IPOrigin, "native", "broadcast") {
			firstKnown(&result.Summary.IPOrigin, data.IPOrigin)
		}
		if data.FraudScore != nil && !math.IsNaN(*data.FraudScore) && !math.IsInf(*data.FraudScore, 0) && *data.FraudScore >= 0 && *data.FraudScore <= 100 {
			if result.Summary.FraudScore == nil || *data.FraudScore > *result.Summary.FraudScore {
				value := *data.FraudScore
				result.Summary.FraudScore = &value
			}
		}
		for _, flag := range data.AbuseFlags {
			if flag != "" {
				flags[flag] = true
			}
		}

	}
	for flag := range flags {
		result.Summary.AbuseFlags = append(result.Summary.AbuseFlags, flag)
	}
	sort.Strings(result.Summary.AbuseFlags)
	return result
}

func firstKnown(target *string, value string) {
	if *target == "unknown" && value != "" && value != "unknown" {
		*target = value
	}
}
func allowed(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
func cloneSource(source *SourceResult) *SourceResult {
	if source == nil {
		return nil
	}
	clone := *source
	if source.Data.ASN != nil {
		value := *source.Data.ASN
		clone.Data.ASN = &value
	}
	if source.Data.FraudScore != nil {
		value := *source.Data.FraudScore
		clone.Data.FraudScore = &value
	}
	if source.Data.EgressConsistent != nil {
		value := *source.Data.EgressConsistent
		clone.Data.EgressConsistent = &value
	}
	clone.Data.AbuseFlags = append([]string(nil), source.Data.AbuseFlags...)
	clone.Data.EgressIPsBySite = cloneMap(source.Data.EgressIPsBySite)
	clone.Data.TargetErrors = cloneMap(source.Data.TargetErrors)
	clone.Data.Services = cloneMap(source.Data.Services)
	return &clone
}
func cloneMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}
