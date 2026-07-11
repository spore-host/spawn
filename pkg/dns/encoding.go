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

// GetFullDNSName returns the complete DNS name with account subdomain
// Example: ("my-instance", "123456789012", "spore.host") -> "my-instance.1s69p4h.spore.host"
func GetFullDNSName(recordName, accountID, domain string) string {
	encoded := EncodeAccountID(accountID)
	return fmt.Sprintf("%s.%s.%s", recordName, encoded, domain)
}
