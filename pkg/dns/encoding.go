package dns

import (
	"fmt"
	"math/big"
	"strings"
)

// EncodeAccountID converts AWS account ID (12 decimal digits) to base36 (≤8 chars)
// Example: "123456789012" -> "1kpqzg2c"
func EncodeAccountID(accountID string) string {
	// Parse decimal account ID
	n := new(big.Int)
	n.SetString(accountID, 10)

	// Convert to base36 (lowercase for DNS compatibility)
	return strings.ToLower(n.Text(36))
}

// accountLabelLen is the base36 width of every 12-digit AWS account ID (see
// DecodeAccountID). minAccountID/maxAccountID bound the 12-digit decimal range.
const accountLabelLen = 8

var (
	minAccountID = big.NewInt(100000000000)
	maxAccountID = big.NewInt(999999999999)
)

// DecodeAccountID is the inverse of EncodeAccountID: it recovers the AWS account
// ID from a base36 subdomain label, and reports whether the label is an account
// subdomain at all.
//
// The discrimination matters more than the arithmetic. Callers use this to walk a
// hosted zone asking "which subdomains belong to accounts?", so a false positive
// misreads ordinary DNS (www, api, docs) as an account. Two properties make the
// test exact: AWS account IDs are always 12 decimal digits, and 36^7 < 10^11 while
// 36^8 > 10^12 — so every 12-digit ID encodes to EXACTLY 8 base36 characters, and
// an 8-character base36 label decoding into the 12-digit range is a valid account
// ID. "www" and "api" are legal base36 but land nowhere near the account range.
//
// The range check alone is sufficient — any 7-character base36 value is below
// 10^11 and any 9-character one above 10^12, so the width test cannot change the
// answer. It is kept as a cheap early-out that avoids running a big.Int parse over
// an arbitrarily long DNS label.
func DecodeAccountID(label string) (string, bool) {
	if len(label) != accountLabelLen {
		return "", false
	}
	// base36 is case-insensitive and DNS labels compare case-insensitively, but
	// EncodeAccountID always lowercases — accept either and normalize.
	n, ok := new(big.Int).SetString(strings.ToLower(label), 36)
	if !ok {
		return "", false
	}
	if n.Cmp(minAccountID) < 0 || n.Cmp(maxAccountID) > 0 {
		return "", false
	}
	return n.String(), true
}

// GetFullDNSName returns the complete DNS name with account subdomain
// Example: ("my-instance", "123456789012", "spore.host") -> "my-instance.1kpqzg2c.spore.host"
func GetFullDNSName(recordName, accountID, domain string) string {
	encoded := EncodeAccountID(accountID)
	return fmt.Sprintf("%s.%s.%s", recordName, encoded, domain)
}
