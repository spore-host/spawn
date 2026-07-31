package dns

import (
	"testing"
)

func TestEncodeAccountID(t *testing.T) {
	tests := []struct {
		accountID string
		expected  string
	}{
		{"123456789012", "1kpqzg2c"},
		{"000000000001", "1"},
		{"999999999999", "cre66i9r"},
		{"752123829273", "9lir3wux"},
		{"942542972736", "c0zxr0ao"},
	}

	for _, tt := range tests {
		t.Run(tt.accountID, func(t *testing.T) {
			result := EncodeAccountID(tt.accountID)
			if result != tt.expected {
				t.Errorf("EncodeAccountID(%s) = %s, want %s", tt.accountID, result, tt.expected)
			}
		})
	}
}

func TestGetFullDNSName(t *testing.T) {
	tests := []struct {
		recordName string
		accountID  string
		domain     string
		expected   string
	}{
		{"my-instance", "123456789012", "spore.host", "my-instance.1kpqzg2c.spore.host"},
		{"dev", "752123829273", "spore.host", "dev.9lir3wux.spore.host"},
		{"i-0abc123def", "942542972736", "spore.host", "i-0abc123def.c0zxr0ao.spore.host"},
	}

	for _, tt := range tests {
		t.Run(tt.recordName, func(t *testing.T) {
			result := GetFullDNSName(tt.recordName, tt.accountID, tt.domain)
			if result != tt.expected {
				t.Errorf("GetFullDNSName(%s, %s, %s) = %s, want %s",
					tt.recordName, tt.accountID, tt.domain, result, tt.expected)
			}
		})
	}
}

func TestDecodeAccountID(t *testing.T) {
	tests := []struct {
		name  string
		label string
		want  string
		ok    bool
	}{
		// Real subdomains observed in the spore.host zone.
		{"dev account", "5k0zfnmq", "435415984226", true},
		{"infra account", "cbxv6l3i", "966362334030", true},
		{"stale account (#457)", "4zlw3a1t", "390967728545", true},

		// Range edges: every 12-digit ID is exactly 8 base36 chars.
		{"lowest 12-digit id", "19xtf1ts", "100000000000", true},
		{"highest 12-digit id", "cre66i9r", "999999999999", true},

		// Ordinary DNS labels must NOT be mistaken for accounts — a false
		// positive here reports live DNS as an abandoned account.
		{"www is base36 but out of range", "www", "", false},
		{"api is base36 but out of range", "api", "", false},
		{"friendly subdomain is not base36", "mycelium-development", "", false},
		{"hyphen is not a base36 digit", "abcd-fgh", "", false},

		// Boundary: 8 chars but decoding outside the 12-digit range.
		{"8 chars below the range", "00000001", "", false},
		{"8 chars above the range", "zzzzzzzz", "", false},

		// Wrong widths are rejected before decoding.
		{"empty", "", "", false},
		{"7 chars", "1kpqzg2", "", false},
		{"9 chars", "1kpqzg2cx", "", false},

		// Uppercase must round-trip: DNS labels compare case-insensitively.
		{"uppercase accepted", "5K0ZFNMQ", "435415984226", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DecodeAccountID(tt.label)
			if ok != tt.ok {
				t.Fatalf("DecodeAccountID(%q) ok = %v, want %v", tt.label, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("DecodeAccountID(%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}

// TestAccountIDRoundTrip is the property the unmanaged-subdomain report depends on:
// encode-then-decode must be the identity for every valid account ID, or the report
// would attribute a subdomain to the wrong account.
func TestAccountIDRoundTrip(t *testing.T) {
	ids := []string{
		"100000000000", "999999999999", // range edges
		"435415984226", "966362334030", "390967728545", "752123829273",
		"123456789012", "942542972736", "570220934149", "472876920125",
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			label := EncodeAccountID(id)
			if len(label) != accountLabelLen {
				t.Errorf("EncodeAccountID(%s) = %q, want %d chars", id, label, accountLabelLen)
			}
			got, ok := DecodeAccountID(label)
			if !ok {
				t.Fatalf("DecodeAccountID(%q) rejected a label produced by EncodeAccountID(%s)", label, id)
			}
			if got != id {
				t.Errorf("round-trip %s -> %q -> %s, want %s", id, label, got, id)
			}
		})
	}
}
