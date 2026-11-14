//go:build oidc_trust_sync

package main

// Network-bound (raw.githubusercontent.com), so behind its own build tag like
// spored_events_sync_test.go; CI runs it as its own step.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"
)

const upstreamOIDCTrusts = "https://raw.githubusercontent.com/spore-host/spore-host/main/infra/iam/github-oidc/trusts.json"

// spore-host's inventory (spore-host#709) lists spawn's two roles too. Two
// copies of one expected trust are only worth having while they agree, so
// every spawn role must be there with the same ARN and the same trust
// (#1601). Change both in step.
func TestOIDCTrustsAgreeWithSporeHost(t *testing.T) {
	if os.Getenv("OIDC_TRUST_SYNC") != "1" {
		t.Skip("set OIDC_TRUST_SYNC=1 to compare against spore-host main (network)")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var body []byte
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 5 * time.Second)
		}
		resp, err := client.Get(upstreamOIDCTrusts)
		if err != nil {
			lastErr = err
			continue
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("GET %s: %s", upstreamOIDCTrusts, resp.Status)
		}
		if err != nil {
			lastErr = err
			continue
		}
		body, lastErr = b, nil
		break
	}
	if lastErr != nil {
		t.Fatal(lastErr)
	}
	var upstream struct{ Roles []oidcRole }
	if err := json.Unmarshal(body, &upstream); err != nil {
		t.Fatal(err)
	}
	theirs := map[string]oidcRole{}
	for _, r := range upstream.Roles {
		theirs[r.Role] = r
	}
	ours, _, _ := readOIDCInventory(t)
	for _, r := range ours {
		u, ok := theirs[r.Role]
		if !ok {
			t.Errorf("%s is in spawn's trusts.json but not in spore-host's (%s)", r.Role, upstreamOIDCTrusts)
			continue
		}
		var a, b any
		if err := json.Unmarshal(r.Trust, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(u.Trust, &b); err != nil {
			t.Fatal(err)
		}
		if r.RoleARN != u.RoleARN || !reflect.DeepEqual(a, b) {
			t.Errorf("%s: spawn's expected trust differs from spore-host's:\n spawn:      %s %s\n spore-host: %s %s", r.Role, r.RoleARN, r.Trust, u.RoleARN, u.Trust)
		}
	}
}
