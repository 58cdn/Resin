package plugin

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in     string
		want   []int
		wantOK bool
	}{
		{"1.2.3", []int{1, 2, 3}, true},
		{"v1.2.3", []int{1, 2, 3}, true},
		{"  v1.2  ", []int{1, 2}, true},
		{"1", []int{1}, true},
		{"0.0.0", []int{0, 0, 0}, true},
		{"1.10.0", []int{1, 10, 0}, true},
		{"1.2.3-rc.1", []int{1, 2, 3}, true},
		{"1.2.3+build.5", []int{1, 2, 3}, true},
		{"v2.0.0-beta+meta", []int{2, 0, 0}, true},
		{"1.02", []int{1, 2}, true},

		{"", nil, false},
		{"v", nil, false},
		{"dev", nil, false},
		{"-rc.1", nil, false},
		{"1..2", nil, false},
		{"1.2.", nil, false},
		{".1", nil, false},
		{"1.x", nil, false},
		{"1.2.3a", nil, false},
		{"1. 2", nil, false},
		{"V1.2", nil, false},
		{"vv1.2", nil, false},
	}
	for _, tc := range tests {
		got, ok := parseVersion(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseVersion(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseVersion(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b   string
		want   int
		wantOK bool
	}{
		{"1.2.3", "1.2.3", 0, true},
		{"1.2", "1.2.0", 0, true},
		{"1.2.0.0", "1.2", 0, true},
		{"v1.2.3", "1.2.3", 0, true},
		{"1.2.3-rc.1", "1.2.3", 0, true},
		{"1.2.3+abc", "1.2.3+def", 0, true},
		{"1.2.3", "1.2.4", -1, true},
		{"1.2.4", "1.2.3", 1, true},
		{"1.10.0", "1.9.0", 1, true},
		{"1.9.0", "1.10.0", -1, true},
		{"2", "1.99.99", 1, true},
		{"1.2", "1.2.1", -1, true},
		{"1.2.1", "1.2", 1, true},
		{"0.9", "1", -1, true},

		{"dev", "1.0.0", 0, false},
		{"1.0.0", "dev", 0, false},
		{"", "1.0.0", 0, false},
		{"1.0.0", "garbage", 0, false},
	}
	for _, tc := range tests {
		got, ok := compareVersions(tc.a, tc.b)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("compareVersions(%q, %q) = (%d, %v), want (%d, %v)", tc.a, tc.b, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestIsNewerVersion(t *testing.T) {
	tests := []struct {
		candidate, installed string
		want                 bool
	}{
		{"1.1.0", "1.0.0", true},
		{"1.10.0", "1.9.0", true},
		{"1.9.0", "1.10.0", false},
		{"1.0.0", "1.0.0", false},
		{"1.0", "1.0.0", false},
		{"v1.0.0", "1.0.0", false},
		{"1.0.0-rc.2", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"2", "1.9.9", true},
		// Incomparable versions: newer only when they differ.
		{"nightly", "1.0.0", true},
		{"1.0.0", "?", true},
		{"nightly", "nightly", false},
		{" nightly ", "nightly", false},
		{"nightly-2", "nightly-1", true},
	}
	for _, tc := range tests {
		if got := isNewerVersion(tc.candidate, tc.installed); got != tc.want {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", tc.candidate, tc.installed, got, tc.want)
		}
	}
}

func TestCheckMinResinVersion(t *testing.T) {
	tests := []struct {
		name         string
		minVersion   string
		resinVersion string
		wantErr      []string // substrings; nil => no error
	}{
		{"no requirement", "", "1.0.0", nil},
		{"blank requirement", "   ", "1.0.0", nil},
		{"equal", "1.0.0", "1.0.0", nil},
		{"equal different length", "1.2", "1.2.0", nil},
		{"newer resin", "1.0.0", "1.2.0", nil},
		{"newer resin numeric", "1.9.0", "1.10.0", nil},
		{"v prefix", "v1.0.0", "1.0.0", nil},
		{"resin v prefix", "1.0.0", "v1.0.0", nil},
		{"resin pre-release suffix ignored", "1.2.0", "1.2.0-rc.1", nil},
		{"min build suffix ignored", "1.2.0+meta", "1.2.0", nil},
		{"dev build skips check", "9.0.0", "dev", nil},
		{"empty resin version skips check", "9.0.0", "", nil},
		{"unparseable resin version skips check", "9.0.0", "custom-build", nil},

		{"older resin", "9.0.0", "1.0.0", []string{"9.0.0", "1.0.0", "requires Resin"}},
		{"older resin patch", "1.0.1", "1.0", []string{"1.0.1"}},
		{"older resin numeric", "1.10.0", "1.9.9", []string{"1.10.0", "1.9.9"}},
		{"older resin with v prefix", " v2.0 ", "1.5.0", []string{"v2.0"}},
		{"invalid min version", "latest", "1.0.0", []string{"invalid min_resin_version", "latest"}},
		{"invalid min version with dev", "1.x", "dev", []string{"invalid min_resin_version"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkMinResinVersion(tc.minVersion, tc.resinVersion)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkMinResinVersion(%q, %q) = nil, want error", tc.minVersion, tc.resinVersion)
			}
			for _, sub := range tc.wantErr {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("error %q does not contain %q", err, sub)
				}
			}
		})
	}
}
