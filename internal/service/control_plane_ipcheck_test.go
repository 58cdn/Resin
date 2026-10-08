package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/ipcheck"
)

func TestIPQualityServiceError(t *testing.T) {
	if err := ipQualityServiceError(nil); err != nil {
		t.Fatalf("nil error mapped to %v", err)
	}
	for _, tc := range []struct {
		name    string
		err     error
		code    string
		message string
	}{
		{"validation", &ipcheck.ValidationError{Message: "unknown source"}, "INVALID_ARGUMENT", "unknown source"},
		{"node absent", ipcheck.ErrNodeNotFound, "NOT_FOUND", "node not found"},
		{"not checked", ipcheck.ErrNotChecked, "NOT_FOUND", "not checked"},
		{"no egress", ipcheck.ErrNoEgressIP, "CONFLICT", "egress IP"},
		{"changed", ipcheck.ErrNodeChanged, "CONFLICT", "changed"},
		{"closed", ipcheck.ErrClosed, "SERVICE_UNAVAILABLE", "unavailable"},
		{"busy", ipcheck.ErrBusy, "SERVICE_UNAVAILABLE", "retry later"},
		{"deadline", context.DeadlineExceeded, "SERVICE_UNAVAILABLE", "timed out"},
		{"canceled", context.Canceled, "SERVICE_UNAVAILABLE", "canceled"},
		{"unexpected", errors.New("secret-key /private/runtime/cache.db"), "INTERNAL", "IP quality check failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ipQualityServiceError(fmt.Errorf("private runtime context: %w", tc.err))
			var svcErr *ServiceError
			if !errors.As(err, &svcErr) || svcErr.Code != tc.code || !strings.Contains(svcErr.Message, tc.message) {
				t.Fatalf("mapped error = %#v, want code %s, message containing %q", err, tc.code, tc.message)
			}
			if strings.Contains(svcErr.Message, "private") || strings.Contains(svcErr.Message, "secret-key") {
				t.Fatalf("internal error details exposed: %s", svcErr.Message)
			}
		})
	}
}

func TestIPQualityServiceAddressValidation(t *testing.T) {
	for _, value := range []string{"", "garbage", "1.1.1.1:443", "127.0.0.1", "192.168.0.1", "::1", "fe80::1%eth0", "224.0.0.1"} {
		if _, err := parseIPCheckAddress(value); err == nil {
			t.Errorf("accepted invalid/nonpublic IP %q", value)
		}
	}
	ip, err := parseIPCheckAddress("::ffff:8.8.8.8")
	if err != nil || ip.String() != "8.8.8.8" {
		t.Fatalf("mapped address = %s, error=%v", ip, err)
	}
}
