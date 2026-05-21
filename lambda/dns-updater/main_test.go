package main

import (
	"math/big"
	"strings"
	"testing"
	"unicode"
)

func TestEncodeAccountID(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		id := "123456789012"
		got1 := encodeAccountID(id)
		got2 := encodeAccountID(id)
		if got1 != got2 {
			t.Errorf("encodeAccountID(%q) not deterministic: %q vs %q", id, got1, got2)
		}
	})

	t.Run("different inputs produce different outputs", func(t *testing.T) {
		ids := []string{"123456789012", "966362334030", "435415984226", "000000000001"}
		seen := make(map[string]string)
		for _, id := range ids {
			enc := encodeAccountID(id)
			if prev, ok := seen[enc]; ok {
				t.Errorf("collision: encodeAccountID(%q) == encodeAccountID(%q) == %q", id, prev, enc)
			}
			seen[enc] = id
		}
	})

	t.Run("result is lowercase alphanumeric", func(t *testing.T) {
		ids := []string{"123456789012", "966362334030", "000000000000", "999999999999"}
		for _, id := range ids {
			enc := encodeAccountID(id)
			for _, ch := range enc {
				if !unicode.IsLower(ch) && !unicode.IsDigit(ch) {
					t.Errorf("encodeAccountID(%q) = %q contains non-lowercase-alphanumeric char %q", id, enc, ch)
				}
			}
		}
	})

	t.Run("matches expected base36 algorithm", func(t *testing.T) {
		cases := []string{"123456789012", "966362334030", "435415984226"}
		for _, id := range cases {
			got := encodeAccountID(id)

			// Compute expected using the same algorithm
			n := new(big.Int)
			n.SetString(id, 10)
			want := strings.ToLower(n.Text(36))

			if got != want {
				t.Errorf("encodeAccountID(%q) = %q, want %q", id, got, want)
			}
		}
	})

	t.Run("zero account ID", func(t *testing.T) {
		got := encodeAccountID("000000000000")
		if got != "0" {
			t.Errorf("encodeAccountID(%q) = %q, want %q", "000000000000", got, "0")
		}
	})
}

func TestGetFullDNSName(t *testing.T) {
	tests := []struct {
		name       string
		recordName string
		accountID  string
		domain     string
		wantSuffix string
	}{
		{
			name:       "basic name",
			recordName: "my-instance",
			accountID:  "123456789012",
			domain:     "spore.host",
			wantSuffix: ".spore.host",
		},
		{
			name:       "short name",
			recordName: "a",
			accountID:  "966362334030",
			domain:     "spore.host",
			wantSuffix: ".spore.host",
		},
		{
			name:       "alternate domain",
			recordName: "gpu-node-01",
			accountID:  "435415984226",
			domain:     "prismcloud.host",
			wantSuffix: ".prismcloud.host",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getFullDNSName(tc.recordName, tc.accountID, tc.domain)

			if !strings.HasSuffix(got, tc.wantSuffix) {
				t.Errorf("getFullDNSName(%q, %q, %q) = %q, want suffix %q", tc.recordName, tc.accountID, tc.domain, got, tc.wantSuffix)
			}

			if !strings.HasPrefix(got, tc.recordName+".") {
				t.Errorf("getFullDNSName(%q, %q, %q) = %q, want prefix %q.", tc.recordName, tc.accountID, tc.domain, got, tc.recordName)
			}

			encoded := encodeAccountID(tc.accountID)
			if !strings.Contains(got, "."+encoded+".") {
				t.Errorf("getFullDNSName(%q, %q, %q) = %q, want to contain .%s.", tc.recordName, tc.accountID, tc.domain, got, encoded)
			}
		})
	}

	t.Run("format is name.base36.domain", func(t *testing.T) {
		recordName := "my-instance"
		accountID := "123456789012"
		encoded := encodeAccountID(accountID)
		want := recordName + "." + encoded + ".spore.host"
		got := getFullDNSName(recordName, accountID, "spore.host")
		if got != want {
			t.Errorf("getFullDNSName(%q, %q, %q) = %q, want %q", recordName, accountID, "spore.host", got, want)
		}
	})

	t.Run("different accounts produce different fqdns", func(t *testing.T) {
		name := "worker"
		fqdn1 := getFullDNSName(name, "123456789012", "spore.host")
		fqdn2 := getFullDNSName(name, "966362334030", "spore.host")
		if fqdn1 == fqdn2 {
			t.Errorf("same fqdn for different accounts: %q", fqdn1)
		}
	})

	t.Run("different domains produce different fqdns", func(t *testing.T) {
		name := "worker"
		accountID := "123456789012"
		fqdn1 := getFullDNSName(name, accountID, "spore.host")
		fqdn2 := getFullDNSName(name, accountID, "prismcloud.host")
		if fqdn1 == fqdn2 {
			t.Errorf("same fqdn for different domains: %q", fqdn1)
		}
	})
}

func TestErrorResponseStructure(t *testing.T) {
	resp, err := errorResponse(400, "test error")
	if err != nil {
		t.Fatalf("errorResponse returned unexpected error: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("got status %d, want 400", resp.StatusCode)
	}
	if resp.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type header = %q, want %q", resp.Headers["Content-Type"], "application/json")
	}
	if resp.Headers["Access-Control-Allow-Origin"] != "*" {
		t.Errorf("CORS header = %q, want *", resp.Headers["Access-Control-Allow-Origin"])
	}
	if !strings.Contains(resp.Body, "test error") {
		t.Errorf("body %q does not contain error message", resp.Body)
	}
	if !strings.Contains(resp.Body, `"success":false`) {
		t.Errorf("body %q does not contain success:false", resp.Body)
	}
}
