//go:build spore_bot_sync

package main

// Network-bound, so it is behind its own build tag rather than in the default
// build: the hermetic test wrapper (#968) fails any default-build test that
// reaches the network, and build-tagged runs are the sanctioned exception (as
// the e2e tiers are). CI runs it as its own step with -tags=spore_bot_sync.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestSporeBotEventsVendoredCopyIsCurrent compares the vendored list with
// spore-bot's on spore-host main. Network-bound, so it runs only when
// SPORE_BOT_EVENTS_SYNC=1 (the CI sync step), never in a plain `go test`.
func TestSporeBotEventsVendoredCopyIsCurrent(t *testing.T) {
	if os.Getenv("SPORE_BOT_EVENTS_SYNC") != "1" {
		t.Skip("set SPORE_BOT_EVENTS_SYNC=1 to compare against spore-host main (network)")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var body []byte
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 5 * time.Second)
		}
		resp, err := client.Get(upstreamSporeBotEvents)
		if err != nil {
			lastErr = err
			continue
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("GET %s: %s", upstreamSporeBotEvents, resp.Status)
			continue
		}
		body, lastErr = b, nil
		break
	}
	if lastErr != nil {
		t.Fatal(lastErr)
	}
	upstream := parseSporeBotEvents(string(body))
	if len(upstream) == 0 {
		t.Fatalf("upstream list at %s parsed to no events", upstreamSporeBotEvents)
	}
	data, err := os.ReadFile(vendoredSporeBotEvents)
	if err != nil {
		t.Fatal(err)
	}
	vendored := parseSporeBotEvents(string(data))

	var diff []string
	for ev := range upstream {
		if !vendored[ev] {
			diff = append(diff, "+ "+ev+"  (upstream only)")
		}
	}
	for ev := range vendored {
		if !upstream[ev] {
			diff = append(diff, "- "+ev+"  (vendored only)")
		}
	}
	sort.Strings(diff)
	if len(diff) > 0 {
		t.Errorf("%s has drifted from spore-host main:\n  %s\n\nRe-vendor it:\n  curl -fsSL %s -o %s",
			vendoredSporeBotEvents, strings.Join(diff, "\n  "), upstreamSporeBotEvents, vendoredSporeBotEvents)
	}
}
