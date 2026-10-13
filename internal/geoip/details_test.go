package geoip

import (
	"net/netip"
	"testing"
)

type detailedMockReader struct {
	mockReader
	info Info
}

func (r *detailedMockReader) LookupDetails(_ netip.Addr) (Info, bool) {
	return r.info, true
}

func TestLookupDetailsCountryOnlyCompatibility(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.1")
	s := &Service{reader: &mockReader{country: "jp"}}
	info, ok := s.LookupDetails(ip)
	if !ok || info.Country != "jp" || info.City != "" || info.ASN != nil || info.ASOrg != "" {
		t.Fatalf("country-only result must preserve unknown fields: %+v, %v", info, ok)
	}
	if got := s.Lookup(ip); got != "jp" {
		t.Fatalf("existing lookup changed: %q", got)
	}
}

func TestLookupDetailsUnavailable(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.1")
	for _, s := range []*Service{{}, {reader: noOpReader{}}} {
		if info, ok := s.LookupDetails(ip); ok || info.ASN != nil || info.Country != "" {
			t.Fatalf("unexpected unavailable result: %+v, %v", info, ok)
		}
	}
	if _, ok := (&Service{reader: &mockReader{country: "us"}}).LookupDetails(netip.Addr{}); ok {
		t.Fatal("invalid IP must not produce data")
	}
	if _, ok := (*mmdbReader)(nil).LookupDetails(ip); ok {
		t.Fatal("nil MMDB reader must not produce data")
	}
}

func TestLookupDetailsSharesReloadLifecycle(t *testing.T) {
	asn := uint32(64500)
	old := &detailedMockReader{info: Info{Country: "us", City: "Test City", ASN: &asn, ASOrg: "Fixture"}}
	s := &Service{reader: old}
	ip := netip.MustParseAddr("203.0.113.1")
	info, ok := s.LookupDetails(ip)
	if !ok || info.City != "Test City" || info.ASN == nil || *info.ASN != asn {
		t.Fatalf("unexpected detailed result: %+v, %v", info, ok)
	}
	*info.ASN = 1
	if asn != 64500 {
		t.Fatal("lookup exposed mutable reader data")
	}
	s.openDB = func(string) (GeoReader, error) { return &mockReader{country: "jp"}, nil }
	if err := s.reloadReader("fixture"); err != nil {
		t.Fatal(err)
	}
	if !old.isClosed() {
		t.Fatal("reload did not close old detailed reader")
	}
	info, ok = s.LookupDetails(ip)
	if !ok || info.Country != "jp" || info.ASN != nil {
		t.Fatalf("stale detailed result after reload: %+v", info)
	}
	s.Stop()
	if _, ok := s.LookupDetails(ip); ok {
		t.Fatal("lookup remains available after stop")
	}
}

func TestMMDBDetailedRecordMapping(t *testing.T) {
	var record mmdbDetailRecord
	if info, ok := record.info(); ok || info.ASN != nil {
		t.Fatalf("empty record invented data: %+v", info)
	}
	record.RegisteredCountry.ISOCode = "CA"
	record.City.Names = map[string]string{"en": "Fixture City"}
	record.Traits.ASN = 64500
	record.Traits.ASOrg = "Fixture Org"
	info, ok := record.info()
	if !ok || info.Country != "ca" || info.City != "Fixture City" || info.ASN == nil || *info.ASN != 64500 || info.ASOrg != "Fixture Org" {
		t.Fatalf("incorrect detailed mapping: %+v", info)
	}
	record.Country.ISOCode = "US"
	record.ASN = 64501
	record.ASOrg = "Root Org"
	info, ok = record.info()
	if !ok || info.Country != "us" || *info.ASN != 64501 || info.ASOrg != "Root Org" {
		t.Fatalf("incorrect field priority: %+v", info)
	}
}

func TestLookupDetailsMMDBFixtures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		country string
		city    string
		asn     uint32
		org     string
	}{
		{"details.mmdb", "us", "Fixture City", 64500, "Fixture Org"},
		{"country-only.mmdb", "ca", "", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, err := MMDBOpen("testdata/" + tc.name)
			if err != nil {
				t.Fatal(err)
			}
			s := &Service{reader: reader}
			defer s.Stop()
			ip := netip.MustParseAddr("203.0.113.10")
			if got := s.Lookup(ip); got != tc.country {
				t.Fatalf("country lookup = %q, want %q", got, tc.country)
			}
			info, ok := s.LookupDetails(ip)
			if !ok || info.Country != tc.country || info.City != tc.city || info.ASOrg != tc.org {
				t.Fatalf("detailed fixture mismatch: %+v, %v", info, ok)
			}
			if tc.asn == 0 {
				if info.ASN != nil {
					t.Fatal("country-only fixture invented ASN")
				}
			} else if info.ASN == nil || *info.ASN != tc.asn {
				t.Fatalf("ASN = %v, want %d", info.ASN, tc.asn)
			}
		})
	}
}
