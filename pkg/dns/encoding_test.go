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
