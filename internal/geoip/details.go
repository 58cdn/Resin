package geoip

import (
	"net"
	"net/netip"
	"strings"
)

// Info contains only fields available in the installed database. Empty strings
// and a nil ASN mean unknown; a country-only database cannot supply city or ASN.
// Reading details never downloads a database or starts a new update lifecycle.
type Info struct {
	Country string
	City    string
	ASN     *uint32
	ASOrg   string
}

// DetailedGeoReader is an optional extension. Existing country-only readers
// remain compatible with GeoReader and Service.Lookup.
type DetailedGeoReader interface {
	LookupDetails(ip netip.Addr) (Info, bool)
}

// LookupDetails reads from the same reader and hot-reload lock as Lookup.
// The boolean is false when the database has no usable fields for the address.
func (s *Service) LookupDetails(ip netip.Addr) (Info, bool) {
	if !ip.IsValid() {
		return Info{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.reader == nil {
		return Info{}, false
	}
	if reader, ok := s.reader.(DetailedGeoReader); ok {
		info, found := reader.LookupDetails(ip.Unmap())
		// Detach pointer-valued data from readers that retain mutable records.
		if info.ASN != nil {
			asn := *info.ASN
			info.ASN = &asn
		}
		return info, found
	}
	country := s.reader.Lookup(ip.Unmap())
	return Info{Country: country}, country != ""
}

type mmdbDetailRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	ASN    uint32 `maxminddb:"autonomous_system_number"`
	ASOrg  string `maxminddb:"autonomous_system_organization"`
	Traits struct {
		ASN   uint32 `maxminddb:"autonomous_system_number"`
		ASOrg string `maxminddb:"autonomous_system_organization"`
	} `maxminddb:"traits"`
}

func (record mmdbDetailRecord) info() (Info, bool) {
	info := Info{
		Country: strings.ToLower(record.Country.ISOCode),
		City:    record.City.Names["en"],
		ASOrg:   record.ASOrg,
	}
	if info.Country == "" {
		info.Country = strings.ToLower(record.RegisteredCountry.ISOCode)
	}
	asn := record.ASN
	if asn == 0 {
		asn = record.Traits.ASN
	}
	if asn != 0 {
		info.ASN = &asn
	}
	if info.ASOrg == "" {
		info.ASOrg = record.Traits.ASOrg
	}
	return info, info.Country != "" || info.City != "" || info.ASN != nil || info.ASOrg != ""
}

func (m *mmdbReader) LookupDetails(ip netip.Addr) (Info, bool) {
	if m == nil || m.reader == nil || !ip.IsValid() {
		return Info{}, false
	}
	var record mmdbDetailRecord
	if err := m.reader.Lookup(net.IP(ip.Unmap().AsSlice()), &record); err != nil {
		return Info{}, false
	}
	return record.info()
}
