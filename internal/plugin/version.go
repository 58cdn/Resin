package plugin

import (
	"fmt"
	"strconv"
	"strings"
)

// parseVersion parses a dotted numeric version such as "1.2.3" or "v1.2".
// Pre-release and build suffixes ("-rc.1", "+abc") are ignored.
func parseVersion(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

// compareVersions compares two dotted numeric versions. ok is false when
// either side cannot be parsed.
func compareVersions(a, b string) (cmp int, ok bool) {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	for i := 0; i < max(len(va), len(vb)); i++ {
		var x, y int
		if i < len(va) {
			x = va[i]
		}
		if i < len(vb) {
			y = vb[i]
		}
		switch {
		case x < y:
			return -1, true
		case x > y:
			return 1, true
		}
	}
	return 0, true
}

// isNewerVersion reports whether candidate should be offered as an update to
// installed. Versions that cannot be compared are treated as newer when they
// differ.
func isNewerVersion(candidate, installed string) bool {
	if cmp, ok := compareVersions(candidate, installed); ok {
		return cmp > 0
	}
	return strings.TrimSpace(candidate) != strings.TrimSpace(installed)
}

// checkMinResinVersion fails when the running Resin version is known and
// older than minVersion. Development builds ("dev") are never rejected.
func checkMinResinVersion(minVersion, resinVersion string) error {
	if strings.TrimSpace(minVersion) == "" {
		return nil
	}
	if _, ok := parseVersion(minVersion); !ok {
		return fmt.Errorf("invalid min_resin_version %q", minVersion)
	}
	if cmp, ok := compareVersions(resinVersion, minVersion); ok && cmp < 0 {
		return fmt.Errorf("requires Resin %s or newer (running %s)", strings.TrimSpace(minVersion), resinVersion)
	}
	return nil
}
