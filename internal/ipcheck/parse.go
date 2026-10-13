package ipcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// decodeObservationObject rejects ambiguous top-level evidence before the
// standard decoder can silently replace an earlier field (including aliases).
// Extra provider metadata remains allowed; callers select only typed fields.
func decodeObservationObject(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("observation must be a JSON object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		key = observationFieldKey(key)
		if !ok || seen[key] {
			return errors.New("duplicate observation field")
		}
		seen[key] = true
		var field json.RawMessage
		if err := decoder.Decode(&field); err != nil {
			return err
		}
	}
	// Unmarshal also verifies the closing brace and rejects trailing payloads.
	return json.Unmarshal(body, value)
}

// The typed fields are ASCII. Normalize both ways to also catch Unicode
// aliases encoding/json accepts for ASCII letters, such as long s and Kelvin K.
func observationFieldKey(key string) string {
	return strings.ToUpper(strings.ToLower(key))
}

// traceValue accepts a complete key=value trace, never an embedded line in
// HTML or an error page. A duplicate requested field is ambiguous even if its
// first value was empty.
func traceValue(body []byte, field string) string {
	var result string
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsAny(key, " <>\t\r") {
			return ""
		}
		if key == field {
			if found {
				return ""
			}
			found = true
			result = strings.TrimSpace(value)
		}
	}
	return result
}
