package dns

import (
	"fmt"
	"math/big"
	"strings"
)

// EncodeAccountID converts AWS account ID (12 decimal digits) to base36 (7 chars)
// Example: "123456789012" -> "1s69p4h"
func EncodeAccountID(accountID string) string {
	// Parse decimal account ID
	n := new(big.Int)
	n.SetString(accountID, 10)

	// Convert to base36 (lowercase for DNS compatibility)
	return strings.ToLower(n.Text(36))
}

// DecodeAccountID converts base36 back to AWS account ID
// Example: "1s69p4h" -> "123456789012"
func DecodeAccountID(encoded string) string {
	n := new(big.Int)
	n.SetString(encoded, 36)
	return n.Text(10)
}

// GetAccountSubdomain returns the base36-encoded account subdomain
// Example: "123456789012" -> "1s69p4h.spore.host"
func GetAccountSubdomain(accountID, domain string) string {
	encoded := EncodeAccountID(accountID)
	return fmt.Sprintf("%s.%s", encoded, domain)
}

// GetFullDNSName returns the complete DNS name with account subdomain
// Example: ("my-instance", "123456789012", "spore.host") -> "my-instance.1s69p4h.spore.host"
func GetFullDNSName(recordName, accountID, domain string) string {
	encoded := EncodeAccountID(accountID)
	return fmt.Sprintf("%s.%s.%s", recordName, encoded, domain)
}

// ParseDNSName extracts the record name and account ID from a full DNS name
// Example: "my-instance.1s69p4h.spore.host" -> ("my-instance", "123456789012", nil)
func ParseDNSName(fullName, domain string) (recordName, accountID string, err error) {
	// Remove domain suffix
	suffix := "." + domain
	if !strings.HasSuffix(fullName, suffix) {
		return "", "", fmt.Errorf("invalid domain suffix")
	}
	withoutDomain := strings.TrimSuffix(fullName, suffix)

	// Split by dots
	parts := strings.Split(withoutDomain, ".")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid DNS name format")
	}

	recordName = parts[0]
	encodedAccount := parts[1]

	// Decode account ID
	accountID = DecodeAccountID(encodedAccount)

	return recordName, accountID, nil
}
