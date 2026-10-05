package cmd

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

type fakeMemoryLookup struct {
	mib  int64
	ok   bool
	seen string // instance type asked about
}

func (f *fakeMemoryLookup) InstanceMemoryMiB(_ context.Context, _, instanceType string) (int64, bool) {
	f.seen = instanceType
	return f.mib, f.ok
}

// TestSporedMemoryFloorWarningNamesTheConsequence is the spawn#682 gate.
//
// The warning exists because the failure is invisible: a 0.5 GiB instance wedged
// during a package install and ran to TWICE its 6-minute TTL with no completion
// record and no signal, until a human noticed. From outside it is
// indistinguishable from a long-running job.
//
// So the test asserts the message explains that nothing will stop the instance —
// not merely that the instance is small, which the user already knows from the
// type name they typed.
func TestSporedMemoryFloorWarningNamesTheConsequence(t *testing.T) {
	msg := renderSporedMemoryFloorWarning("t4g.nano", 512, "6m", true)
	if msg == "" {
		t.Fatal("no warning for a 512 MiB instance, which is the exact case in #682")
	}

	for _, want := range []string{
		"t4g.nano",     // which instance
		"0.5 GiB",      // how small
		"inside",       // WHY it cannot self-enforce: enforcement is in-instance
		"--ttl 6m",     // the specific promise that may not hold
		"#682",         // where the evidence is
		"spawn doctor", // how to find out if a backstop exists
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning does not mention %q:\n%s", want, msg)
		}
	}

	// The point is the consequence, not the size. A message that only said the
	// box was small would be useless — the user chose the type.
	if !regexp.MustCompile(`(?i)too starved to stop itself|not be enforced`).MatchString(msg) {
		t.Errorf("warning never says the instance may not stop itself, which is the whole "+
			"point — it reads as a sizing nit instead:\n%s", msg)
	}
}

// TestSporedMemoryFloorIsSilentAtAndAboveTheFloor: a warning on a normal launch
// is noise, and noise trains the reader to skip the one that matters.
func TestSporedMemoryFloorIsSilentAtAndAboveTheFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  string
		mib  int64
	}{
		{"exactly at the floor", "t4g.micro", aws.SporedMemoryFloorMiB},
		{"the size that worked in #682", "t4g.medium", 4096},
		{"a large instance", "c8g.48xlarge", 196608},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if msg := renderSporedMemoryFloorWarning(tc.typ, tc.mib, "1h", true); msg != "" {
				t.Errorf("warned about %s (%d MiB):\n%s", tc.typ, tc.mib, msg)
			}
		})
	}
}

// TestSporedMemoryFloorStaysSilentOnAnUnknownType: when the lookup cannot answer,
// say nothing. Warning on every unresolvable type would guess, and guessing here
// means crying wolf on instances that are fine.
func TestSporedMemoryFloorStaysSilentOnAnUnknownType(t *testing.T) {
	var b bytes.Buffer
	f := &fakeMemoryLookup{mib: 0, ok: false}
	warnIfBelowSporedMemoryFloor(context.Background(), f, &b, "us-east-1", "zz9.plural", "1h")
	if b.Len() != 0 {
		t.Errorf("warned despite an unresolved memory lookup: %s", b.String())
	}
	if f.seen != "zz9.plural" {
		t.Errorf("looked up %q, want the requested type", f.seen)
	}
	// And a zero memory value must not be treated as "below the floor".
	if msg := renderSporedMemoryFloorWarning("zz9.plural", 0, "1h", true); msg != "" {
		t.Errorf("zero memory produced a warning instead of silence:\n%s", msg)
	}
}

// TestSporedMemoryFloorWarningWithoutATTL: the TTL line is the sharpest part of
// the message, but a launch with no TTL still deserves the warning — spored also
// enforces idle and cost limits, and those are equally starved.
func TestSporedMemoryFloorWarningWithoutATTL(t *testing.T) {
	msg := renderSporedMemoryFloorWarning("t4g.nano", 512, "", false)
	if msg == "" {
		t.Fatal("no warning for a 512 MiB instance just because no --ttl was set")
	}
	if strings.Contains(msg, "--ttl") {
		t.Errorf("message references --ttl when none was given:\n%s", msg)
	}
	if !strings.Contains(msg, "idle and cost") {
		t.Errorf("message should still name the other limits spored enforces:\n%s", msg)
	}
}

// TestLaunchWarnsBeforeSpending asserts the warning is emitted ahead of the
// first AWS mutation. A warning printed after the IAM role and security group
// exist is advice about a decision already paid for — the same ordering mistake
// as #685.
func TestLaunchWarnsBeforeSpending(t *testing.T) {
	b, err := os.ReadFile("launch_single.go")
	if err != nil {
		t.Fatalf("read launch_single.go: %v", err)
	}
	src := string(b)

	idxWarn := strings.Index(src, "warnIfBelowSporedMemoryFloor(ctx")
	if idxWarn < 0 {
		t.Fatal("launch does not call warnIfBelowSporedMemoryFloor; the #682 warning is dead code")
	}
	for _, mutation := range []string{"ensureIAMProfile(ctx", "ensureSecurityGroup(ctx"} {
		idx := strings.Index(src, mutation)
		if idx < 0 {
			t.Fatalf("could not find %s; this gate would pass vacuously", mutation)
		}
		if idxWarn > idx {
			t.Errorf("the memory warning (offset %d) is printed after %s (offset %d) — the user "+
				"should hear it before spending, not after", idxWarn, mutation, idx)
		}
	}
}
