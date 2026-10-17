package main

import (
	"context"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/spotrequest"
)

type fakeRetirer struct {
	verdicts []spotrequest.Detection // returned in order; the last repeats
	detects  int
	retired  []spotrequest.Detection
}

func (f *fakeRetirer) DetectSpotReplacement(context.Context) spotrequest.Detection {
	d := f.verdicts[min(f.detects, len(f.verdicts)-1)]
	f.detects++
	return d
}

func (f *fakeRetirer) RetireSpotReplacement(_ context.Context, d spotrequest.Detection, _ func(context.Context) error) error {
	f.retired = append(f.retired, d)
	return nil
}

func runCheck(f *fakeRetirer) (notified int) {
	replacementCheck{
		r:        f,
		shutdown: func(context.Context) error { return nil },
		notify:   func(context.Context, spotrequest.Detection) { notified++ },
		sleep:    func(context.Context, time.Duration) bool { return true },
	}.run(context.Background())
	return notified
}

func TestReplacementCheck_RetiresOnlyAReplacement(t *testing.T) {
	repl := spotrequest.Detection{Verdict: spotrequest.VerdictReplacement, RequestID: "sir-1", OriginalInstanceID: "i-orig"}
	cases := map[string]struct {
		verdicts    []spotrequest.Detection
		wantRetire  bool
		wantDetects int
	}{
		"original":                   {[]spotrequest.Detection{{Verdict: spotrequest.VerdictNotReplacement}}, false, 1},
		"replacement":                {[]spotrequest.Detection{repl}, true, 1},
		"unknown throughout":         {[]spotrequest.Detection{{Verdict: spotrequest.VerdictUnknown}}, false, replacementCheckAttempts},
		"unknown, then replacement":  {[]spotrequest.Detection{{Verdict: spotrequest.VerdictUnknown}, repl}, true, 2},
		"unknown, then not replaced": {[]spotrequest.Detection{{Verdict: spotrequest.VerdictUnknown}, {Verdict: spotrequest.VerdictNotReplacement}}, false, 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeRetirer{verdicts: tc.verdicts}
			notified := runCheck(f)
			if got := len(f.retired) == 1; got != tc.wantRetire {
				t.Fatalf("retired %d times, want retire=%v", len(f.retired), tc.wantRetire)
			}
			if (notified == 1) != tc.wantRetire {
				t.Fatalf("notified %d times, want once iff retired", notified)
			}
			if f.detects != tc.wantDetects {
				t.Fatalf("detected %d times, want %d", f.detects, tc.wantDetects)
			}
		})
	}
}

// The replacement has no tags of its own; the notification must use the
// request's, which carry the original's notification settings.
func TestNotifyConfigFromRequestTags(t *testing.T) {
	cfg := notifyConfigFromTags(map[string]string{
		"spawn:notify-url": "https://bot.example", "spawn:slack-workspace-id": "T1",
	})
	if cfg.NotifyURL != "https://bot.example" || cfg.SlackWorkspaceID != "T1" {
		t.Fatalf("config = %+v", cfg)
	}
}
