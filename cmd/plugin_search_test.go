package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

func TestHumanizeAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"zero is unknown, not 1970", time.Time{}, "at an unknown time"},
		{"seconds", now.Add(-30 * time.Second), "just now"},
		{"minutes", now.Add(-90 * time.Minute), "1h ago"},
		{"hours", now.Add(-5 * time.Hour), "5h ago"},
		{"days", now.Add(-72 * time.Hour), "3d ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanizeAge(tc.in); got != tc.want {
				t.Errorf("humanizeAge(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// A future stamp (clock skew, or an index generated on a machine ahead of
	// this one) must render as the absolute timestamp, not as a negative age
	// like "-2h ago".
	got := humanizeAge(now.Add(2 * time.Hour))
	if strings.HasSuffix(got, "ago") {
		t.Errorf("humanizeAge(future) = %q, want an absolute timestamp rather than an age", got)
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("humanizeAge(future) = %q, want a parseable RFC3339 timestamp: %v", got, err)
	}
}

func TestIndexProvenance_NamesSourceAndAge(t *testing.T) {
	idx := &plugin.Index{Source: "spore-host/spore-plugins", GeneratedAt: time.Now().Add(-3 * time.Hour)}
	got := indexProvenance(idx)
	for _, want := range []string{"spore-host/spore-plugins", "3h ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("indexProvenance() = %q, want it to contain %q", got, want)
		}
	}

	// An index with no source recorded still names the registry it must have
	// come from, rather than printing "index: , generated ...".
	bare := indexProvenance(&plugin.Index{GeneratedAt: time.Now()})
	if !strings.Contains(bare, "spore-host/spore-plugins") {
		t.Errorf("indexProvenance(no source) = %q, want a fallback source", bare)
	}
}
