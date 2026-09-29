package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tpPluginEnvs returns requiredEnvs with every plugin env var explicitly
// cleared so the host environment cannot leak into the tests.
func tpPluginEnvs() map[string]string {
	envs := requiredEnvs()
	envs["RESIN_PLUGIN_DIR"] = ""
	envs["RESIN_EXTERNAL_PLUGINS_ENABLED"] = ""
	envs["RESIN_PLUGIN_MARKETPLACE_URLS"] = ""
	return envs
}

func tpLoadPluginEnv(t *testing.T, overrides map[string]string) (*EnvConfig, error) {
	t.Helper()
	envs := tpPluginEnvs()
	for k, v := range overrides {
		envs[k] = v
	}
	setEnvs(t, envs)
	return LoadEnvConfig()
}

func TestEnvPlugin_Defaults(t *testing.T) {
	cfg, err := tpLoadPluginEnv(t, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEqual(t, "PluginDir", cfg.PluginDir, filepath.Join(cfg.StateDir, "plugins"))
	assertEqual(t, "ExternalPluginsEnabled", cfg.ExternalPluginsEnabled, false)
	if cfg.PluginMarketplaceURLs == nil || len(cfg.PluginMarketplaceURLs) != 0 {
		t.Fatalf("PluginMarketplaceURLs = %#v, want empty non-nil slice", cfg.PluginMarketplaceURLs)
	}
}

func TestEnvPlugin_PluginDirFollowsStateDir(t *testing.T) {
	cfg, err := tpLoadPluginEnv(t, map[string]string{"RESIN_STATE_DIR": "/srv/resin-state"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEqual(t, "PluginDir", cfg.PluginDir, filepath.Join("/srv/resin-state", "plugins"))
}

func TestEnvPlugin_PluginDirOverride(t *testing.T) {
	cfg, err := tpLoadPluginEnv(t, map[string]string{"RESIN_PLUGIN_DIR": "  /opt/resin/plugins  "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEqual(t, "PluginDir", cfg.PluginDir, "/opt/resin/plugins")

	// Whitespace-only falls back to the default.
	cfg, err = tpLoadPluginEnv(t, map[string]string{"RESIN_PLUGIN_DIR": "   "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertEqual(t, "PluginDir", cfg.PluginDir, filepath.Join(cfg.StateDir, "plugins"))
}

func TestEnvPlugin_ExternalPluginsEnabled(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{" true ", true},
		{"t", true},
		{"false", false},
		{"0", false},
		{"F", false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := tpLoadPluginEnv(t, map[string]string{"RESIN_EXTERNAL_PLUGINS_ENABLED": tc.raw})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertEqual(t, "ExternalPluginsEnabled", cfg.ExternalPluginsEnabled, tc.want)
		})
	}
}

func TestEnvPlugin_ExternalPluginsEnabledInvalid(t *testing.T) {
	for _, raw := range []string{"yes", "on", "enabled", "2"} {
		t.Run(raw, func(t *testing.T) {
			_, err := tpLoadPluginEnv(t, map[string]string{"RESIN_EXTERNAL_PLUGINS_ENABLED": raw})
			if err == nil {
				t.Fatalf("expected error for RESIN_EXTERNAL_PLUGINS_ENABLED=%q", raw)
			}
			assertContains(t, err.Error(), "RESIN_EXTERNAL_PLUGINS_ENABLED")
			assertContains(t, err.Error(), "invalid boolean")
		})
	}
}

func TestEnvPlugin_MarketplaceURLsSplitting(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"single", "https://a.example.com/index.json", []string{"https://a.example.com/index.json"}},
		{"semicolon", "https://a.example.com/i.json;https://b.example.com/i.json", []string{"https://a.example.com/i.json", "https://b.example.com/i.json"}},
		{"comma", "https://a.example.com/i.json,http://b.example.com/i.json", []string{"https://a.example.com/i.json", "http://b.example.com/i.json"}},
		{"newline", "https://a.example.com/i.json\nhttps://b.example.com/i.json", []string{"https://a.example.com/i.json", "https://b.example.com/i.json"}},
		{"crlf", "https://a.example.com/i.json\r\nhttps://b.example.com/i.json\r\n", []string{"https://a.example.com/i.json", "https://b.example.com/i.json"}},
		{"mixed with whitespace and empties", "  https://a.example.com/i.json ; ;,\n\n , https://b.example.com/i.json  ;https://c.example.com/i.json,", []string{"https://a.example.com/i.json", "https://b.example.com/i.json", "https://c.example.com/i.json"}},
		{"only separators", " ; , \n ", []string{}},
		{"keeps order and duplicates", "https://b.example.com/i.json;https://a.example.com/i.json;https://b.example.com/i.json", []string{"https://b.example.com/i.json", "https://a.example.com/i.json", "https://b.example.com/i.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := tpLoadPluginEnv(t, map[string]string{"RESIN_PLUGIN_MARKETPLACE_URLS": tc.raw})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(cfg.PluginMarketplaceURLs, tc.want) {
				t.Fatalf("PluginMarketplaceURLs = %#v, want %#v", cfg.PluginMarketplaceURLs, tc.want)
			}
		})
	}
}

func TestEnvPlugin_MarketplaceURLsWithCredentialsAccepted(t *testing.T) {
	raw := "https://user:pass@a.example.com/i.json?token=abc"
	cfg, err := tpLoadPluginEnv(t, map[string]string{"RESIN_PLUGIN_MARKETPLACE_URLS": raw})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(cfg.PluginMarketplaceURLs, []string{raw}) {
		t.Fatalf("PluginMarketplaceURLs = %#v", cfg.PluginMarketplaceURLs)
	}
}

func TestEnvPlugin_MarketplaceURLsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantIdx []string
	}{
		{"ftp scheme", "ftp://a.example.com/i.json", []string{"RESIN_PLUGIN_MARKETPLACE_URLS[0]"}},
		{"relative path", "/index.json", []string{"RESIN_PLUGIN_MARKETPLACE_URLS[0]"}},
		{"no scheme", "a.example.com/i.json", []string{"RESIN_PLUGIN_MARKETPLACE_URLS[0]"}},
		{"scheme relative", "//a.example.com/i.json", []string{"RESIN_PLUGIN_MARKETPLACE_URLS[0]"}},
		{"no host", "https:///i.json", []string{"RESIN_PLUGIN_MARKETPLACE_URLS[0]"}},
		{"unparseable", "http://[::1", []string{"RESIN_PLUGIN_MARKETPLACE_URLS[0]"}},
		{"second invalid", "https://ok.example.com/i.json;file:///etc/passwd", []string{"RESIN_PLUGIN_MARKETPLACE_URLS[1]"}},
		{"multiple invalid", "bad-one;https://ok.example.com/i.json,also bad", []string{"RESIN_PLUGIN_MARKETPLACE_URLS[0]", "RESIN_PLUGIN_MARKETPLACE_URLS[2]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tpLoadPluginEnv(t, map[string]string{"RESIN_PLUGIN_MARKETPLACE_URLS": tc.raw})
			if err == nil {
				t.Fatalf("expected error for %q", tc.raw)
			}
			for _, want := range tc.wantIdx {
				assertContains(t, err.Error(), want)
			}
			assertContains(t, err.Error(), "must be an absolute http(s) URL")
			if strings.Contains(err.Error(), "RESIN_PLUGIN_MARKETPLACE_URLS[1]") && tc.name == "multiple invalid" {
				t.Fatalf("valid entry [1] should not be reported: %v", err)
			}
		})
	}
}

func TestEnvPlugin_MarketplaceURLErrorDoesNotLeakSecrets(t *testing.T) {
	_, err := tpLoadPluginEnv(t, map[string]string{"RESIN_PLUGIN_MARKETPLACE_URLS": "ftp://user:s3cr3t@a.example.com/i.json?token=t0ken"})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, secret := range []string{"s3cr3t", "t0ken"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("validation error leaks %q: %v", secret, err)
		}
	}
}

func TestEnvPlugin_SplitDelimitedStringSlice(t *testing.T) {
	cases := map[string][]string{
		"":                  {},
		"a":                 {"a"},
		"a;b,c\nd\re":       {"a", "b", "c", "d", "e"},
		" a ;; b ,, c ":     {"a", "b", "c"},
		";;;":               {},
		"a b;c":             {"a b", "c"},
		"\t a \t,\t b \t\n": {"a", "b"},
	}
	for raw, want := range cases {
		got := splitDelimitedStringSlice(raw)
		if got == nil {
			t.Fatalf("splitDelimitedStringSlice(%q) returned nil", raw)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("splitDelimitedStringSlice(%q) = %#v, want %#v", raw, got, want)
		}
	}
}
