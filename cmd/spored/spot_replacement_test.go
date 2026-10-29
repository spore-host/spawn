package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/launchconfig"
	"github.com/spore-host/spawn/pkg/spotrequest"
)

type fakeRetirer struct {
	verdicts []spotrequest.Detection // returned in order; the last repeats
	detects  int
	retired  []spotrequest.Detection
	outcome  spotrequest.RetireOutcome // returned by RetireSpotReplacement; 0 means RetiredTerminated
	err      error
}

func (f *fakeRetirer) DetectSpotReplacement(context.Context) spotrequest.Detection {
	d := f.verdicts[min(f.detects, len(f.verdicts)-1)]
	f.detects++
	return d
}

func (f *fakeRetirer) RetireSpotReplacement(_ context.Context, d spotrequest.Detection, _ func(context.Context) error) (spotrequest.RetireOutcome, error) {
	f.retired = append(f.retired, d)
	if f.outcome == 0 && f.err == nil {
		return spotrequest.RetiredTerminated, nil
	}
	return f.outcome, f.err
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
// request's, which carry the original's spawn:config, and the notification
// settings are in the launch config object that names (#1359), fetched with
// the replacement's role. Not from the request's tags: a tag with one of
// their names is not read.
func TestNotifyConfigFromRequestTags(t *testing.T) {
	sha := strings.Repeat("c", 64)
	var asked []string
	fetch := func(_ context.Context, s string) (map[string]string, error) {
		asked = append(asked, s)
		return map[string]string{launchconfig.NotifyURL: "https://bot.example", launchconfig.SlackWorkspaceID: "T1"}, nil
	}
	cfg := notifyConfigFromTags(context.Background(), map[string]string{
		"spawn:config": sha, "spawn:dns-name": "orig",
		"spawn:notify-url": "https://from-a-tag.example",
	}, fetch)
	if cfg.NotifyURL != "https://bot.example" || cfg.SlackWorkspaceID != "T1" || cfg.DNSName != "orig" {
		t.Fatalf("config = %+v", cfg)
	}
	if len(asked) != 1 || asked[0] != sha {
		t.Errorf("fetched %v, want the request's spawn:config %s once", asked, sha)
	}
	none := notifyConfigFromTags(context.Background(), map[string]string{"spawn:notify-url": "https://from-a-tag.example"}, fetch)
	if none.NotifyURL != "" {
		t.Errorf("read the notify URL from a tag: %q", none.NotifyURL)
	}
	failed := notifyConfigFromTags(context.Background(), map[string]string{"spawn:config": sha},
		func(context.Context, string) (map[string]string, error) { return nil, errors.New("NoSuchKey") })
	if failed.NotifyURL != "" {
		t.Errorf("an unreadable object gave notify URL %q", failed.NotifyURL)
	}
}

// When the self-terminate is refused (a role from before #1080), the instance is
// only stopped. spored must say that, with the exact command that ends it,
// naming this instance and region, rather than log that it retired.
func TestReplacementCheck_StoppedFallbackSaysSoWithTheCommand(t *testing.T) {
	repl := spotrequest.Detection{Verdict: spotrequest.VerdictReplacement, RequestID: "sir-1", OriginalInstanceID: "i-orig"}
	f := &fakeRetirer{verdicts: []spotrequest.Detection{repl}, outcome: spotrequest.RetiredShutdown,
		err: errors.New("terminating i-0repl was refused (UnauthorizedOperation), so the OS was shut down instead, which leaves it STOPPED")}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	replacementCheck{
		r: f, instanceID: "i-0repl", region: "eu-west-1",
		shutdown: func(context.Context) error { return nil },
		notify:   func(context.Context, spotrequest.Detection) {},
		sleep:    func(context.Context, time.Duration) bool { return true },
	}.run(context.Background())

	out := buf.String()
	for _, want := range []string{"STOPPED", "aws ec2 terminate-instances --region eu-west-1 --instance-ids i-0repl"} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "terminating itself") {
		t.Errorf("the stopped fallback was logged as a self-terminate:\n%s", out)
	}
}
