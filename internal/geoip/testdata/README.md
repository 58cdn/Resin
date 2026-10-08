# Synthetic GeoIP fixtures

These tiny MMDB files contain hand-authored fictional data, not downloaded data.
Both use a single 24-bit IPv4 tree node whose branches point to the same record.

- `details.mmdb`: country US, Fixture City, documentation ASN 64500, Fixture Org.
- `country-only.mmdb`: registered country CA, with no city or ASN fields.

They exercise the real MMDB reader offline. They are not suitable for lookups
outside tests and have no third-party data attribution requirement.
