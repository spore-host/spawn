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

func TestDecodeAccountID(t *testing.T) {
	tests := []struct {
		encoded  string
		expected string
	}{
		{"1kpqzg2c", "123456789012"},
		{"1", "1"},
		{"cre66i9r", "999999999999"},
		{"9lir3wux", "752123829273"},
		{"c0zxr0ao", "942542972736"},
	}

	for _, tt := range tests {
		t.Run(tt.encoded, func(t *testing.T) {
			result := DecodeAccountID(tt.encoded)
			if result != tt.expected {
				t.Errorf("DecodeAccountID(%s) = %s, want %s", tt.encoded, result, tt.expected)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	accountIDs := []string{
		"123456789012",
		"752123829273",
		"942542972736",
		"000000000001",
		"999999999999",
	}

	for _, accountID := range accountIDs {
		t.Run(accountID, func(t *testing.T) {
			encoded := EncodeAccountID(accountID)
			decoded := DecodeAccountID(encoded)

			// Pad decoded with leading zeros if needed
			for len(decoded) < len(accountID) {
				decoded = "0" + decoded
			}

			if decoded != accountID {
				t.Errorf("Round trip failed: %s -> %s -> %s", accountID, encoded, decoded)
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

func TestParseDNSName(t *testing.T) {
	tests := []struct {
		fullName        string
		domain          string
		expectedRecord  string
		expectedAccount string
		expectError     bool
	}{
		{"my-instance.1kpqzg2c.spore.host", "spore.host", "my-instance", "123456789012", false},
		{"dev.9lir3wux.spore.host", "spore.host", "dev", "752123829273", false},
		{"invalid.spore.host", "spore.host", "", "", true},              // Wrong format
		{"my-instance.1kpqzg2c.wrong.host", "spore.host", "", "", true}, // Wrong domain
	}

	for _, tt := range tests {
		t.Run(tt.fullName, func(t *testing.T) {
			recordName, accountID, err := ParseDNSName(tt.fullName, tt.domain)

			if tt.expectError {
				if err == nil {
					t.Errorf("ParseDNSName(%s, %s) expected error, got nil", tt.fullName, tt.domain)
				}
				return
			}

			if err != nil {
				t.Errorf("ParseDNSName(%s, %s) unexpected error: %v", tt.fullName, tt.domain, err)
				return
			}

			if recordName != tt.expectedRecord {
				t.Errorf("ParseDNSName(%s, %s) recordName = %s, want %s",
					tt.fullName, tt.domain, recordName, tt.expectedRecord)
			}

			if accountID != tt.expectedAccount {
				t.Errorf("ParseDNSName(%s, %s) accountID = %s, want %s",
					tt.fullName, tt.domain, accountID, tt.expectedAccount)
			}
		})
	}
}
