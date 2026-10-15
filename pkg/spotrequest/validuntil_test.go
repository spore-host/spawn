package spotrequest

import (
	"strings"
	"testing"
	"time"
)

func TestValidUntil(t *testing.T) {
	launch := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	if got := ValidUntil(launch.Add(3*time.Hour), launch); !got.Equal(launch.Add(3 * time.Hour)) {
		t.Errorf("with a deadline: %s, want the deadline", got)
	}
	if got := ValidUntil(time.Time{}, launch); !got.Equal(launch.Add(DefaultMaxLifetime)) {
		t.Errorf("without a deadline: %s, want launch + %s", got, DefaultMaxLifetime)
	}
}

// #950: `spawn extend` cannot move a Spot request's ValidUntil (EC2 has no API
// for it), so extending past it must say what stops working.
func TestExtendWarning(t *testing.T) {
	vu := time.Date(2026, 10, 15, 15, 0, 0, 0, time.UTC)
	persistent := map[string]string{TypeTag: TypePersistent, ValidUntilTag: vu.Format(time.RFC3339)}

	w := ExtendWarning(persistent, vu.Add(time.Hour))
	for _, want := range []string{"2026-10-15 15:00 UTC", "expires", "restart"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning should mention %q: %q", want, w)
		}
	}
	if w := ExtendWarning(persistent, vu); w != "" {
		t.Errorf("a deadline AT valid-until is still covered: %q", w)
	}
	for name, tags := range map[string]map[string]string{
		"one-time":       {TypeTag: TypeOneTime, ValidUntilTag: vu.Format(time.RFC3339)},
		"on-demand":      {},
		"pre-#950":       {TypeTag: TypePersistent},
		"garbage tag":    {TypeTag: TypePersistent, ValidUntilTag: "soon"},
		"untyped w/ tag": {ValidUntilTag: vu.Format(time.RFC3339)},
	} {
		if w := ExtendWarning(tags, vu.Add(24*time.Hour)); w != "" {
			t.Errorf("%s: unexpected warning %q", name, w)
		}
	}
}

func TestStartFailureNote(t *testing.T) {
	vu := time.Date(2026, 10, 15, 15, 0, 0, 0, time.UTC)
	tags := map[string]string{TypeTag: TypePersistent, ValidUntilTag: vu.Format(time.RFC3339)}
	if n := StartFailureNote(tags, vu.Add(time.Minute)); !strings.Contains(n, "expired at 2026-10-15 15:00 UTC") {
		t.Errorf("after expiry: %q", n)
	}
	if n := StartFailureNote(tags, vu.Add(-time.Minute)); n != "" {
		t.Errorf("before expiry the request is not the cause: %q", n)
	}
}
