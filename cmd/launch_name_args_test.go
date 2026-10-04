package cmd

import (
	"strings"
	"testing"
)

// TestLaunchNameArgs is spawn#499.
//
// `spawn launch` required the spore name POSITIONALLY, while its own --help
// described a --name flag as "required" and showed a "Direct with flags" example
// that omitted the positional entirely. Following either piece of the built-in
// documentation failed — and failed with "accepts 1 arg(s), received 0", which
// never mentions the name, so the error pointed at nothing.
//
// runLaunch already read both forms (--name wins, the positional fills in). Only
// cobra.ExactArgs(1) stood in the way, and it could not express this: it sees
// only the positional count, so --name alone was indistinguishable from passing
// nothing at all.
func TestLaunchNameArgs(t *testing.T) {
	prev := name
	t.Cleanup(func() { name = prev })

	tests := []struct {
		desc     string
		args     []string
		flagName string
		wantErr  string // substring; "" means it must be accepted
	}{{
		desc: "positional only — the form that always worked",
		args: []string{"my-spore"},
	}, {
		desc:     "--name only — the form the flag help calls required",
		flagName: "my-spore",
	}, {
		desc:     "both, agreeing",
		args:     []string{"my-spore"},
		flagName: "my-spore",
	}, {
		desc:    "neither — must name the missing thing, not the arg count",
		wantErr: "a spore name is required",
	}, {
		desc:     "both, disagreeing",
		args:     []string{"aaa"},
		flagName: "bbb",
		wantErr:  "conflicting names",
	}, {
		desc:    "too many positionals",
		args:    []string{"a", "b"},
		wantErr: "too many arguments",
	}}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if err := launchCmd.Flags().Set("name", tt.flagName); err != nil {
				t.Fatalf("set --name: %v", err)
			}
			t.Cleanup(func() { _ = launchCmd.Flags().Set("name", "") })

			err := launchNameArgs(launchCmd, tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected acceptance, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
			// Whatever the failure, it must not be the old message that named
			// nothing useful.
			if strings.Contains(err.Error(), "accepts 1 arg(s)") {
				t.Error("still reporting the arg count rather than the missing name (#499)")
			}
		})
	}
}

// TestLaunchUsageMentionsBothForms: the usage string is what a caller reads
// first, so if both forms are accepted it has to say so. A `Use` of
// "launch <name>" alone implies the positional is mandatory, which is what sent
// the reporter down the wrong path.
func TestLaunchUsageMentionsBothForms(t *testing.T) {
	if !strings.Contains(launchCmd.Use, "name") {
		t.Errorf("Use = %q, expected it to mention the name", launchCmd.Use)
	}
	f := launchCmd.Flags().Lookup("name")
	if f == nil {
		t.Fatal("--name flag is gone; #499's premise was that both forms exist")
	}
	// The flag help said "required" while the positional was the real
	// requirement — the contradiction at the heart of #499. Now that either form
	// satisfies, the help must SAY so.
	//
	// Asserted positively, on purpose. My first version of this check was
	// `Contains(usage,"required") && !Contains(usage,"or")`, which passed
	// vacuously because "spore" contains the substring "or" — a test that could
	// not fail is worse than no test, and it is the same mistake #684's
	// region-substitution test made.
	if !strings.Contains(f.Usage, "positional") {
		t.Errorf("--name help = %q\n\nIt must mention that the positional form is also "+
			"accepted. Describing this flag as the required one, while the positional was "+
			"the real requirement, is what made the built-in documentation "+
			"self-contradictory (#499).", f.Usage)
	}
}
