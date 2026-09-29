package pluginsdk

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestValidPluginID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"a", true},
		{"0", true},
		{"ab", true},
		{"acme.rate-limit", true},
		{"a_b", true},
		{"a-b.c_d9", true},
		{"a..b", true},
		{strings.Repeat("a", 64), true},

		{"", false},
		{strings.Repeat("a", 65), false},
		{"A", false},
		{"Acme.demo", false},
		{"-a", false},
		{"a-", false},
		{".a", false},
		{"a.", false},
		{"_a", false},
		{"a_", false},
		{"..", false},
		{"a b", false},
		{"a/b", false},
		{`a\b`, false},
		{"a:b", false},
		{"é", false},
		{"a\n", false},
	}
	for _, tc := range tests {
		if got := ValidPluginID(tc.id); got != tc.want {
			t.Errorf("ValidPluginID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

const tmValidManifest = `{
	"schema_version": 1,
	"id": "acme.demo",
	"name": "Demo",
	"version": "1.2.3",
	"description": "demo plugin",
	"min_resin_version": "1.0.0",
	"capabilities": {"request_hook": true, "events": ["request.finished", "lease.*"]},
	"config_fields": [
		{"name": "greeting", "type": "string", "default": "hello"},
		{"name": "mode", "type": "enum", "enum_values": ["a", "b"], "default": "a"},
		{"name": "limit", "type": "integer", "default": 10},
		{"name": "token", "type": "secret"}
	],
	"runtimes": {
		"linux-amd64": {"command": ["bin/demo"]},
		"any": {"command": ["python3", "main.py"], "env": {"X": "1"}}
	}
}`

func TestParseManifestValid(t *testing.T) {
	m, err := ParseManifest([]byte(tmValidManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.ID != "acme.demo" || m.Name != "Demo" || m.Version != "1.2.3" || m.MinResinVersion != "1.0.0" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if !m.Capabilities.RequestHook || len(m.Capabilities.Events) != 2 {
		t.Fatalf("unexpected capabilities: %+v", m.Capabilities)
	}
	if len(m.ConfigFields) != 4 || len(m.Runtimes) != 2 {
		t.Fatalf("unexpected fields/runtimes: %+v / %+v", m.ConfigFields, m.Runtimes)
	}
}

// tmManifestJSON renders a manifest map, applying mutate to a fresh copy of a
// minimal valid manifest.
func tmManifestJSON(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	m := map[string]any{
		"schema_version": 1,
		"id":             "acme.demo",
		"name":           "Demo",
		"version":        "1.0.0",
		"capabilities":   map[string]any{},
	}
	if mutate != nil {
		mutate(m)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return data
}

func TestParseManifestRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"unknown top-level field", func(m map[string]any) { m["bogus"] = true }, "unknown field"},
		{"unknown capabilities field", func(m map[string]any) { m["capabilities"] = map[string]any{"hooks": true} }, "unknown field"},
		{"unknown config field key", func(m map[string]any) {
			m["config_fields"] = []any{map[string]any{"name": "a", "type": "string", "placeholder": "x"}}
		}, "unknown field"},
		{"schema_version missing", func(m map[string]any) { delete(m, "schema_version") }, "schema_version"},
		{"schema_version 2", func(m map[string]any) { m["schema_version"] = 2 }, "schema_version"},
		{"schema_version string", func(m map[string]any) { m["schema_version"] = "1" }, "invalid manifest"},
		{"id missing", func(m map[string]any) { delete(m, "id") }, "invalid id"},
		{"id uppercase", func(m map[string]any) { m["id"] = "Acme" }, "invalid id"},
		{"id path", func(m map[string]any) { m["id"] = "../x" }, "invalid id"},
		{"name missing", func(m map[string]any) { delete(m, "name") }, "name is required"},
		{"name blank", func(m map[string]any) { m["name"] = "   " }, "name is required"},
		{"version missing", func(m map[string]any) { delete(m, "version") }, "version is required"},
		{"version blank", func(m map[string]any) { m["version"] = " \t" }, "version is required"},
		{"version too long", func(m map[string]any) { m["version"] = strings.Repeat("1", 65) }, "version is required"},
		{"unknown event", func(m map[string]any) {
			m["capabilities"] = map[string]any{"events": []string{"request.started"}}
		}, "unknown event subscription"},
		{"unknown event wildcard prefix", func(m map[string]any) {
			m["capabilities"] = map[string]any{"events": []string{"bogus.*"}}
		}, "unknown event subscription"},
		{"partial prefix wildcard", func(m map[string]any) {
			m["capabilities"] = map[string]any{"events": []string{"lea.*"}}
		}, "unknown event subscription"},
		{"bare prefix", func(m map[string]any) {
			m["capabilities"] = map[string]any{"events": []string{"lease"}}
		}, "unknown event subscription"},
		{"runtime without command", func(m map[string]any) {
			m["runtimes"] = map[string]any{"any": map[string]any{"command": []string{}}}
		}, "command is required"},
		{"runtime blank command", func(m map[string]any) {
			m["runtimes"] = map[string]any{"any": map[string]any{"command": []string{"  "}}}
		}, "command is required"},
	}
	for _, raw := range []string{`[1,2,3]`, `"x"`, ``, `{"schema_version":1,`} {
		if _, err := ParseManifest([]byte(raw)); err == nil {
			t.Errorf("ParseManifest accepted %q", raw)
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := tmManifestJSON(t, tc.mutate)
			_, err := ParseManifest(data)
			if err == nil {
				t.Fatalf("ParseManifest accepted %s", data)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseManifestAcceptsEventPatterns(t *testing.T) {
	for _, events := range [][]string{
		{"*"},
		{"lease.*"},
		{"request.*"},
		{"request.finished", "lease.created", "lease.replaced", "lease.removed", "lease.expired"},
	} {
		data := tmManifestJSON(t, func(m map[string]any) {
			m["capabilities"] = map[string]any{"events": events}
		})
		if _, err := ParseManifest(data); err != nil {
			t.Errorf("events %v rejected: %v", events, err)
		}
	}
}

func TestManifestValidateConfigFields(t *testing.T) {
	tests := []struct {
		name    string
		fields  []any
		wantErr string // empty => valid
	}{
		{"all types valid", []any{
			map[string]any{"name": "s", "type": "string", "default": "x"},
			map[string]any{"name": "sec", "type": "secret"},
			map[string]any{"name": "txt", "type": "text", "default": "multi\nline"},
			map[string]any{"name": "n", "type": "number", "default": 1.5},
			map[string]any{"name": "i", "type": "integer", "default": 3},
			map[string]any{"name": "b", "type": "boolean", "default": false},
			map[string]any{"name": "e", "type": "enum", "enum_values": []string{"x", "y"}, "default": "y"},
			map[string]any{"name": "l", "type": "string_list", "default": []string{"a", "b"}},
			map[string]any{"name": "j", "type": "json", "default": map[string]any{"k": []int{1}}},
			map[string]any{"name": "nul", "type": "integer", "default": nil},
		}, ""},
		{"missing name", []any{map[string]any{"type": "string"}}, "name is required"},
		{"blank name", []any{map[string]any{"name": " ", "type": "string"}}, "name is required"},
		{"duplicate name", []any{
			map[string]any{"name": "a", "type": "string"},
			map[string]any{"name": "a", "type": "number"},
		}, "duplicate name"},
		{"unknown type", []any{map[string]any{"name": "a", "type": "float"}}, "unknown type"},
		{"missing type", []any{map[string]any{"name": "a"}}, "unknown type"},
		{"enum without values", []any{map[string]any{"name": "a", "type": "enum"}}, "enum_values is required"},
		{"string default not string", []any{map[string]any{"name": "a", "type": "string", "default": 5}}, "default"},
		{"integer default fractional", []any{map[string]any{"name": "a", "type": "integer", "default": 1.5}}, "must be an integer"},
		{"number default string", []any{map[string]any{"name": "a", "type": "number", "default": "1"}}, "must be a number"},
		{"boolean default string", []any{map[string]any{"name": "a", "type": "boolean", "default": "true"}}, "must be a boolean"},
		{"enum default outside values", []any{map[string]any{"name": "a", "type": "enum", "enum_values": []string{"x"}, "default": "z"}}, "must be one of"},
		{"string_list default mixed", []any{map[string]any{"name": "a", "type": "string_list", "default": []any{"a", 1}}}, "must be a string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := tmManifestJSON(t, func(m map[string]any) { m["config_fields"] = tc.fields })
			_, err := ParseManifest(data)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestManifestRuntimeFor(t *testing.T) {
	m := &Manifest{Runtimes: map[string]RuntimeSpec{
		"linux-amd64": {Command: []string{"bin/linux-amd64"}},
		"any":         {Command: []string{"python3", "main.py"}},
	}}
	if rt, ok := m.RuntimeFor("linux", "amd64"); !ok || rt.Command[0] != "bin/linux-amd64" {
		t.Fatalf("exact runtime: got %+v, %v", rt, ok)
	}
	if rt, ok := m.RuntimeFor("linux", "arm64"); !ok || rt.Command[0] != "python3" {
		t.Fatalf("fallback for linux-arm64: got %+v, %v", rt, ok)
	}
	if rt, ok := m.RuntimeFor("windows", "amd64"); !ok || rt.Command[0] != "python3" {
		t.Fatalf("fallback for windows-amd64: got %+v, %v", rt, ok)
	}

	exactOnly := &Manifest{Runtimes: map[string]RuntimeSpec{
		"linux-amd64": {Command: []string{"bin/x"}},
	}}
	if _, ok := exactOnly.RuntimeFor("linux", "arm64"); ok {
		t.Fatal("RuntimeFor matched a different arch without an any entry")
	}
	if _, ok := exactOnly.RuntimeFor("linux", ""); ok {
		t.Fatal("RuntimeFor matched an empty arch")
	}
	if _, ok := (&Manifest{}).RuntimeFor("linux", "amd64"); ok {
		t.Fatal("RuntimeFor matched with no runtimes")
	}
}

func TestManifestDefaultConfig(t *testing.T) {
	m := &Manifest{ConfigFields: []ConfigField{
		{Name: "greeting", Type: FieldString, Default: json.RawMessage(`"hello"`)},
		{Name: "limit", Type: FieldInteger, Default: json.RawMessage(`10`)},
		{Name: "list", Type: FieldStringList, Default: json.RawMessage(`["a","b"]`)},
		{Name: "token", Type: FieldSecret},
	}}
	var got map[string]any
	if err := json.Unmarshal(m.DefaultConfig(), &got); err != nil {
		t.Fatalf("DefaultConfig is not JSON: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("DefaultConfig = %v, want 3 keys", got)
	}
	if got["greeting"] != "hello" || got["limit"] != float64(10) {
		t.Fatalf("DefaultConfig = %v", got)
	}
	if _, ok := got["token"]; ok {
		t.Fatalf("field without default present in DefaultConfig: %v", got)
	}
	if list, ok := got["list"].([]any); !ok || len(list) != 2 {
		t.Fatalf("list default = %v", got["list"])
	}

	if got := string((&Manifest{}).DefaultConfig()); got != "{}" {
		t.Fatalf("DefaultConfig without fields = %s, want {}", got)
	}

	// Defaults from a parsed manifest round-trip and satisfy the manifest.
	parsed, err := ParseManifest([]byte(tmValidManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	def := parsed.DefaultConfig()
	if err := parsed.ValidateConfig(def); err != nil {
		t.Fatalf("default config %s does not validate: %v", def, err)
	}
}

func TestManifestApplyConfigDefaults(t *testing.T) {
	m := &Manifest{ConfigFields: []ConfigField{
		{Name: "url", Type: FieldString, Required: true, Default: json.RawMessage(`"http://default"`)},
		{Name: "limit", Type: FieldInteger, Default: json.RawMessage(`10`)},
		{Name: "token", Type: FieldSecret},
	}}
	tests := []struct {
		name   string
		config string
		want   map[string]any
	}{
		{"empty input", ``, map[string]any{"url": "http://default", "limit": float64(10)}},
		{"missing keys", `{}`, map[string]any{"url": "http://default", "limit": float64(10)}},
		{"null keys", `{"url":null,"limit":null}`, map[string]any{"url": "http://default", "limit": float64(10)}},
		{"set keys are kept", `{"url":"http://x","limit":0,"token":"t"}`, map[string]any{"url": "http://x", "limit": float64(0), "token": "t"}},
		{"undeclared keys are kept", `{"extra":[1]}`, map[string]any{"url": "http://default", "limit": float64(10), "extra": []any{float64(1)}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := m.ApplyConfigDefaults(json.RawMessage(tc.config))
			if err != nil {
				t.Fatalf("ApplyConfigDefaults: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("result %s is not a JSON object: %v", out, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ApplyConfigDefaults(%s) = %s, want %v", tc.config, out, tc.want)
			}
			// A required field with a default is satisfied once defaults apply.
			if err := m.ValidateConfig(out); err != nil {
				t.Fatalf("ValidateConfig(%s): %v", out, err)
			}
		})
	}

	for _, bad := range []string{`[1]`, `null`, `"x"`, `{"url":`} {
		if _, err := m.ApplyConfigDefaults(json.RawMessage(bad)); err == nil {
			t.Errorf("ApplyConfigDefaults(%s) accepted a non-object", bad)
		}
	}

	// Nothing to fill in: the input is returned as is.
	in := json.RawMessage(`{"url":"http://x", "limit":1}`)
	out, err := m.ApplyConfigDefaults(in)
	if err != nil || string(out) != string(in) {
		t.Fatalf("ApplyConfigDefaults(%s) = %s, %v; want input unchanged", in, out, err)
	}
}

func TestValidateConfigFields(t *testing.T) {
	fields := []ConfigField{
		{Name: "s", Type: FieldString},
		{Name: "sec", Type: FieldSecret},
		{Name: "txt", Type: FieldText},
		{Name: "n", Type: FieldNumber},
		{Name: "i", Type: FieldInteger},
		{Name: "b", Type: FieldBoolean},
		{Name: "e", Type: FieldEnum, EnumValues: []string{"red", "green"}},
		{Name: "l", Type: FieldStringList},
		{Name: "j", Type: FieldJSON},
	}
	tests := []struct {
		name    string
		config  string
		wantErr string // empty => valid
	}{
		{"empty object", `{}`, ""},
		{"empty input", ``, ""},
		{"whitespace input", "  \n", ""},
		{"all valid", `{"s":"x","sec":"p","txt":"t","n":1.5,"i":3,"b":true,"e":"green","l":["a"],"j":{"any":[1,"x"]}}`, ""},
		{"integer written as float", `{"i":2.0}`, ""},
		{"negative number", `{"n":-3e2}`, ""},
		{"empty string list", `{"l":[]}`, ""},
		{"null values are unset", `{"s":null,"i":null,"e":null}`, ""},
		{"json accepts scalar", `{"j":"str"}`, ""},
		{"json accepts null-like", `{"j":false}`, ""},
		{"undeclared keys pass through", `{"extra":{"nested":true},"other":[1,2]}`, ""},

		{"not an object", `[1,2]`, "JSON object"},
		{"null config", `null`, "JSON object"},
		{"string config", `"x"`, "JSON object"},
		{"invalid json", `{"s":`, "JSON object"},
		{"string gets number", `{"s":1}`, "config.s: must be a string"},
		{"secret gets bool", `{"sec":true}`, "config.sec: must be a string"},
		{"text gets array", `{"txt":["a"]}`, "config.txt: must be a string"},
		{"number gets string", `{"n":"1"}`, "config.n: must be a number"},
		{"integer gets fraction", `{"i":1.5}`, "config.i: must be an integer"},
		{"integer gets string", `{"i":"1"}`, "config.i: must be an integer"},
		{"boolean gets string", `{"b":"true"}`, "config.b: must be a boolean"},
		{"boolean gets number", `{"b":1}`, "config.b: must be a boolean"},
		{"enum not member", `{"e":"blue"}`, "config.e: must be one of red, green"},
		{"enum case sensitive", `{"e":"Red"}`, "must be one of"},
		{"enum gets number", `{"e":1}`, "config.e: must be a string"},
		{"string list gets string", `{"l":"a"}`, "config.l: must be an array of strings"},
		{"string list gets mixed", `{"l":["a",2]}`, "config.l: [1]: must be a string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfigFields(fields, json.RawMessage(tc.config))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateConfigFieldsRequired(t *testing.T) {
	fields := []ConfigField{
		{Name: "url", Type: FieldString, Required: true},
		{Name: "opt", Type: FieldString},
	}
	tests := []struct {
		config  string
		wantErr bool
	}{
		{`{"url":"http://x"}`, false},
		{`{"url":""}`, false}, // present, correct type
		{`{}`, true},
		{``, true},
		{`{"url":null}`, true},
		{`{"opt":"x"}`, true},
	}
	for _, tc := range tests {
		err := ValidateConfigFields(fields, json.RawMessage(tc.config))
		if (err != nil) != tc.wantErr {
			t.Errorf("config %q: err = %v, wantErr %v", tc.config, err, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "config.url: required") {
			t.Errorf("config %q: unexpected error %q", tc.config, err)
		}
	}

	m := &Manifest{ConfigFields: fields}
	if err := m.ValidateConfig(json.RawMessage(`{}`)); err == nil {
		t.Fatal("Manifest.ValidateConfig accepted a missing required field")
	}
}

func TestMatchEvent(t *testing.T) {
	tests := []struct {
		patterns  []string
		eventType string
		want      bool
	}{
		{[]string{"request.finished"}, "request.finished", true},
		{[]string{"request.finished"}, "lease.created", false},
		{[]string{"*"}, "lease.expired", true},
		{[]string{"*"}, "anything", true},
		{[]string{"lease.*"}, "lease.created", true},
		{[]string{"lease.*"}, "lease.expired", true},
		{[]string{"lease.*"}, "request.finished", false},
		{[]string{"lease.*"}, "lease", false},
		{[]string{"lease.*"}, "leasex.created", false},
		{[]string{"request.finished", "lease.*"}, "lease.removed", true},
		{[]string{"request.finished", "lease.created"}, "lease.removed", false},
		{nil, "request.finished", false},
		{[]string{}, "request.finished", false},
		{[]string{""}, "request.finished", false},
	}
	for _, tc := range tests {
		if got := MatchEvent(tc.patterns, tc.eventType); got != tc.want {
			t.Errorf("MatchEvent(%v, %q) = %v, want %v", tc.patterns, tc.eventType, got, tc.want)
		}
	}
}

func TestKnownEventTypesAreValidPatterns(t *testing.T) {
	for _, ev := range KnownEventTypes {
		if !ValidEventPattern(ev) {
			t.Errorf("known event type %q rejected by ValidEventPattern", ev)
		}
		if !MatchEvent([]string{ev}, ev) {
			t.Errorf("MatchEvent does not match %q exactly", ev)
		}
	}
}
