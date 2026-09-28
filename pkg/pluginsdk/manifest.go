package pluginsdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// ManifestFileName is the manifest file expected at the root of a plugin package.
const ManifestFileName = "plugin.json"

// RuntimeAny is the runtimes key used when a command works on every platform
// (for example a script run by an interpreter found in PATH).
const RuntimeAny = "any"

var pluginIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

// ValidPluginID reports whether id is a valid plugin identifier: 1-64
// characters of lowercase letters, digits, '.', '_' or '-', starting and
// ending with a letter or digit.
func ValidPluginID(id string) bool { return pluginIDPattern.MatchString(id) }

// Config field types understood by the Resin WebUI form generator.
const (
	FieldString     = "string"
	FieldSecret     = "secret"
	FieldText       = "text"
	FieldNumber     = "number"
	FieldInteger    = "integer"
	FieldBoolean    = "boolean"
	FieldEnum       = "enum"
	FieldStringList = "string_list"
	FieldJSON       = "json"
)

var validFieldTypes = map[string]bool{
	FieldString: true, FieldSecret: true, FieldText: true, FieldNumber: true,
	FieldInteger: true, FieldBoolean: true, FieldEnum: true, FieldStringList: true,
	FieldJSON: true,
}

// ConfigField describes one top-level key of the plugin config object.
type ConfigField struct {
	Name        string          `json:"name"`
	Label       string          `json:"label,omitempty"`
	Type        string          `json:"type"`
	Description string          `json:"description,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	EnumValues  []string        `json:"enum_values,omitempty"`
}

// RuntimeSpec describes how to start the plugin process on one platform.
type RuntimeSpec struct {
	// Command is argv. A first element containing a path separator (for
	// example "bin/plugin" or "./run.sh") is resolved inside the package
	// directory; a bare name (for example "python3") is looked up in PATH.
	Command []string `json:"command"`
	// Env adds environment variables to the plugin process.
	Env map[string]string `json:"env,omitempty"`
}

// Manifest is the content of plugin.json.
type Manifest struct {
	SchemaVersion   int                    `json:"schema_version"`
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	Version         string                 `json:"version"`
	Description     string                 `json:"description,omitempty"`
	Author          string                 `json:"author,omitempty"`
	Homepage        string                 `json:"homepage,omitempty"`
	License         string                 `json:"license,omitempty"`
	MinResinVersion string                 `json:"min_resin_version,omitempty"`
	Capabilities    Capabilities           `json:"capabilities"`
	ConfigFields    []ConfigField          `json:"config_fields,omitempty"`
	Runtimes        map[string]RuntimeSpec `json:"runtimes,omitempty"`
}

// ParseManifest decodes and validates a manifest.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the static structure of the manifest. It does not require
// a runtime entry; use RuntimeFor for that.
func (m *Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("manifest: unsupported schema_version %d (want %d)", m.SchemaVersion, SchemaVersion)
	}
	if !ValidPluginID(m.ID) {
		return fmt.Errorf("manifest: invalid id %q", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("manifest: name is required")
	}
	if strings.TrimSpace(m.Version) == "" || len(m.Version) > 64 {
		return errors.New("manifest: version is required (max 64 chars)")
	}
	for _, ev := range m.Capabilities.Events {
		if !validEventPattern(ev) {
			return fmt.Errorf("manifest: unknown event subscription %q", ev)
		}
	}
	seen := make(map[string]bool, len(m.ConfigFields))
	for i, f := range m.ConfigFields {
		if strings.TrimSpace(f.Name) == "" {
			return fmt.Errorf("manifest: config_fields[%d]: name is required", i)
		}
		if seen[f.Name] {
			return fmt.Errorf("manifest: config_fields[%d]: duplicate name %q", i, f.Name)
		}
		seen[f.Name] = true
		if !validFieldTypes[f.Type] {
			return fmt.Errorf("manifest: config_fields[%d]: unknown type %q", i, f.Type)
		}
		if f.Type == FieldEnum && len(f.EnumValues) == 0 {
			return fmt.Errorf("manifest: config_fields[%d]: enum_values is required for enum", i)
		}
		if len(f.Default) > 0 && !bytes.Equal(bytes.TrimSpace(f.Default), []byte("null")) {
			if err := checkFieldValue(f, f.Default); err != nil {
				return fmt.Errorf("manifest: config_fields[%d]: default: %w", i, err)
			}
		}
	}
	for key, rt := range m.Runtimes {
		if len(rt.Command) == 0 || strings.TrimSpace(rt.Command[0]) == "" {
			return fmt.Errorf("manifest: runtimes[%q]: command is required", key)
		}
	}
	return nil
}

// RuntimeFor returns the runtime for goos/goarch, falling back to "any".
func (m *Manifest) RuntimeFor(goos, goarch string) (RuntimeSpec, bool) {
	if rt, ok := m.Runtimes[goos+"-"+goarch]; ok {
		return rt, true
	}
	if rt, ok := m.Runtimes[RuntimeAny]; ok {
		return rt, true
	}
	return RuntimeSpec{}, false
}

// DefaultConfig builds a config object from config_fields defaults.
func (m *Manifest) DefaultConfig() json.RawMessage {
	obj := make(map[string]json.RawMessage)
	for _, f := range m.ConfigFields {
		if len(f.Default) > 0 {
			obj[f.Name] = f.Default
		}
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

// ValidateConfig checks a config object against config_fields. Keys that are
// not declared are allowed and passed through to the plugin unchanged.
func (m *Manifest) ValidateConfig(raw json.RawMessage) error {
	return ValidateConfigFields(m.ConfigFields, raw)
}

// ValidateConfigFields checks a config object against a list of fields.
func ValidateConfigFields(fields []ConfigField, raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return errors.New("config must be a JSON object")
	}
	for _, f := range fields {
		value, ok := obj[f.Name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if f.Required {
				return fmt.Errorf("config.%s: required", f.Name)
			}
			continue
		}
		if err := checkFieldValue(f, value); err != nil {
			return fmt.Errorf("config.%s: %w", f.Name, err)
		}
	}
	return nil
}

func checkFieldValue(f ConfigField, value json.RawMessage) error {
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		return errors.New("invalid JSON value")
	}
	switch f.Type {
	case FieldString, FieldSecret, FieldText:
		if _, ok := v.(string); !ok {
			return errors.New("must be a string")
		}
	case FieldNumber:
		if _, ok := v.(float64); !ok {
			return errors.New("must be a number")
		}
	case FieldInteger:
		n, ok := v.(float64)
		if !ok || math.Trunc(n) != n {
			return errors.New("must be an integer")
		}
	case FieldBoolean:
		if _, ok := v.(bool); !ok {
			return errors.New("must be a boolean")
		}
	case FieldEnum:
		s, ok := v.(string)
		if !ok {
			return errors.New("must be a string")
		}
		for _, allowed := range f.EnumValues {
			if s == allowed {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", strings.Join(f.EnumValues, ", "))
	case FieldStringList:
		arr, ok := v.([]any)
		if !ok {
			return errors.New("must be an array of strings")
		}
		for i, item := range arr {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("[%d]: must be a string", i)
			}
		}
	case FieldJSON:
		// Any JSON value.
	}
	return nil
}

func validEventPattern(pattern string) bool {
	if pattern == "*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok {
		for _, known := range KnownEventTypes {
			if strings.HasPrefix(known, prefix+".") {
				return true
			}
		}
		return false
	}
	for _, known := range KnownEventTypes {
		if pattern == known {
			return true
		}
	}
	return false
}

// MatchEvent reports whether eventType is covered by any of the patterns.
func MatchEvent(patterns []string, eventType string) bool {
	for _, p := range patterns {
		if p == "*" || p == eventType {
			return true
		}
		if prefix, ok := strings.CutSuffix(p, ".*"); ok && strings.HasPrefix(eventType, prefix+".") {
			return true
		}
	}
	return false
}
