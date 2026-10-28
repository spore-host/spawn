package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spore-host/spawn/pkg/aws"
)

// #1375: a nested-spawn role named directly is held to --nested-spawn's rule,
// an instance that can launch other instances must have a TTL.

// isNestedRefusal reports whether err is one of the two refusals of a
// nested-spawn role: no TTL, or not usable on this path at all.
func isNestedRefusal(err error) bool {
	return errors.Is(err, aws.ErrNestedSpawnNeedsTTL) || errors.Is(err, aws.ErrNestedSpawnRoleReserved)
}

// keepCommandFlags puts every flag of c back as it was when the test ends.
func keepCommandFlags(t *testing.T, c *cobra.Command) {
	t.Helper()
	type saved struct {
		value   string
		slice   []string
		changed bool
	}
	flags := map[string]saved{}
	c.Flags().VisitAll(func(f *pflag.Flag) {
		s := saved{value: f.Value.String(), changed: f.Changed}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			s.slice = sv.GetSlice()
		}
		flags[f.Name] = s
	})
	t.Cleanup(func() {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			s := flags[f.Name]
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(s.slice)
			} else {
				_ = f.Value.Set(s.value)
			}
			f.Changed = s.changed
		})
	})
}

func setFlag(t *testing.T, c *cobra.Command, name, value string) {
	t.Helper()
	if err := c.Flags().Set(name, value); err != nil {
		t.Fatalf("%s --%s %q: %v", c.CommandPath(), name, value, err)
	}
}

// Gate: `spawn launch --instance-profile <nested>` with no --ttl is refused
// before any AWS call, with the --nested-spawn refusal's reason.
func TestNestedInstanceProfileWithoutTTLRefusedBeforeAnyAWSCall(t *testing.T) {
	keepLaunchGlobals(t)
	prevYes := autoYes
	t.Cleanup(func() { autoYes = prevYes })
	autoYes = true
	nested := aws.NestedSpawnRoleName()
	setFlag(t, launchCmd, "instance-type", "m7i.large")
	setFlag(t, launchCmd, "region", "us-east-1")
	setFlag(t, launchCmd, "name", "head")
	setFlag(t, launchCmd, "instance-profile", nested)
	setFlag(t, launchCmd, "ttl", "")

	calls := 0
	t.Setenv("AWS_ENDPOINT_URL", countingAWSEndpoint(t, &calls))
	err := runLaunch(launchCmd, nil)
	if !errors.Is(err, aws.ErrNestedSpawnNeedsTTL) {
		t.Fatalf("launch --instance-profile %s with no --ttl: err = %v, want the nested-spawn TTL refusal", nested, err)
	}
	if calls != 0 {
		t.Errorf("the refused launch made %d AWS call(s) first", calls)
	}
	// Same reason as `--nested-spawn` without --ttl, and it names the flag.
	flagPath := aws.NestedSpawnTTLError().Error()
	reason := flagPath[strings.Index(flagPath, "requires --ttl"):]
	if !strings.HasSuffix(err.Error(), reason) || !strings.Contains(err.Error(), "--instance-profile") {
		t.Errorf("refusal %q does not end with the --nested-spawn wording %q and name --instance-profile", err, reason)
	}
}

// Gate: the same launch with --ttl is not refused and goes on to AWS.
func TestNestedInstanceProfileWithTTLProceeds(t *testing.T) {
	keepLaunchGlobals(t)
	prevYes := autoYes
	t.Cleanup(func() { autoYes = prevYes })
	autoYes = true
	nested := aws.NestedSpawnRoleName()
	setFlag(t, launchCmd, "instance-type", "m7i.large")
	setFlag(t, launchCmd, "region", "us-east-1")
	setFlag(t, launchCmd, "name", "head")
	setFlag(t, launchCmd, "instance-profile", nested)
	setFlag(t, launchCmd, "ttl", "8h")

	calls := 0
	t.Setenv("AWS_ENDPOINT_URL", countingAWSEndpoint(t, &calls))
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	err := runLaunch(launchCmd, nil)
	if isNestedRefusal(err) {
		t.Fatalf("launch --instance-profile %s --ttl 8h was refused: %v", nested, err)
	}
	if calls == 0 {
		t.Errorf("launch --instance-profile %s --ttl 8h made no AWS call (err = %v); it should have gone on to launch", nested, err)
	}
	// And the resolved-config check agrees.
	if err := requireNestedSpawnTTL(&aws.LaunchConfig{TTL: "8h"}); err != nil {
		t.Errorf("requireNestedSpawnTTL with --instance-profile %s and a TTL: %v", nested, err)
	}
	if err := requireNestedSpawnTTL(&aws.LaunchConfig{IdleTimeout: "1h"}); !errors.Is(err, aws.ErrNestedSpawnNeedsTTL) {
		t.Errorf("requireNestedSpawnTTL with --instance-profile %s and only an idle timeout: err = %v, want the TTL refusal", nested, err)
	}
}

// The single-launch flag combinations --nested-spawn refuses are refused for the
// named role too, and --iam-role may never name one.
func TestNestedSpawnRoleFlagValidation(t *testing.T) {
	nested := aws.NestedSpawnRoleName()
	cases := []struct {
		name string
		set  func()
		want error // nil: accepted
	}{
		{"profile without ttl", func() { instanceProfile = nested }, aws.ErrNestedSpawnNeedsTTL},
		{"profile with ttl", func() { instanceProfile, ttl = nested, "2h" }, nil},
		{"profile with ttl and --no-timeout", func() { instanceProfile, ttl, noTimeout = nested, "2h", true }, aws.ErrNestedSpawnNeedsTTL},
		{"profile on a batch queue", func() { instanceProfile, ttl, batchQueueFile = nested, "2h", "q.json" }, aws.ErrNestedSpawnRoleReserved},
		{"profile on a sweep (checked per row)", func() { instanceProfile, paramFile = nested, "p.yaml" }, nil},
		{"iam-role, even with ttl", func() { iamRole, ttl = nested, "2h" }, aws.ErrNestedSpawnRoleReserved},
		{"another profile", func() { instanceProfile = "my-profile" }, nil},
		{"a spawn-instance role", func() { instanceProfile = "spawn-instance-0123abcd" }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keepLaunchGlobals(t)
			instanceProfile, iamRole, ttl, noTimeout, batchQueueFile, queueTemplate, paramFile, params = "", "", "", false, "", "", "", ""
			tc.set()
			err := validateNestedSpawnRoleFlags(false)
			if tc.want == nil && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// How a sweep row's profile and TTL are resolved for the rule: the row's own
// value, else --instance-profile / the folded --ttl, else defaults:.
func TestNestedSweepRowResolution(t *testing.T) {
	nested := aws.NestedSpawnRoleName()
	row := func(kv ...string) map[string]interface{} {
		m := map[string]interface{}{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name       string
		defaults   map[string]interface{}
		rows       []map[string]interface{}
		cliProfile string
		wantRow    string // "" = accepted
	}{
		{"row profile, no ttl", row(), []map[string]interface{}{row("iam_role", nested)}, "", "param set 0"},
		{"row profile, idle timeout only", row("idle_timeout", "1h"), []map[string]interface{}{row("iam_role", nested)}, "", "param set 0"},
		{"row profile, row ttl", row(), []map[string]interface{}{row("iam_role", nested, "ttl", "1h")}, "", ""},
		{"row profile, defaults ttl", row("ttl", "1h"), []map[string]interface{}{row("iam_role", nested)}, "", ""},
		{"defaults profile, one row unbounded", row("iam_role", nested), []map[string]interface{}{row("ttl", "1h"), row()}, "", "param set 1"},
		{"cli profile, no ttl", row(), []map[string]interface{}{row("ttl", "1h"), row()}, nested, "param set 1"},
		{"cli profile beats defaults profile", row("iam_role", "plain"), []map[string]interface{}{row()}, nested, "param set 0"},
		{"row profile beats cli profile", row(), []map[string]interface{}{row("iam_role", "plain")}, nested, ""},
		{"no nested profile anywhere", row(), []map[string]interface{}{row()}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseNestedSweepRowsWithoutTTL(&ParamFileFormat{Defaults: tc.defaults, Params: tc.rows}, tc.cliProfile)
			if tc.wantRow == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if !errors.Is(err, aws.ErrNestedSpawnNeedsTTL) || !strings.Contains(err.Error(), tc.wantRow) {
				t.Fatalf("err = %v, want the TTL refusal naming %s", err, tc.wantRow)
			}
		})
	}
}

// Gate: a foreground sweep with a row naming a nested profile and no TTL is
// refused, before the sweep makes any AWS call.
func TestForegroundSweepRowWithNestedProfileAndNoTTLRefused(t *testing.T) {
	keepLaunchGlobals(t)
	nested := aws.NestedSpawnRoleName()
	p := filepath.Join(t.TempDir(), "sweep.json")
	body := `{"defaults": {"instance_type": "t3.micro", "region": "us-east-1", "idle_timeout": "1h"},
	          "params": [{"ttl": "1h"}, {"iam_role": "` + nested + `"}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	setFlag(t, launchCmd, "param-file", p)
	setFlag(t, launchCmd, "no-detach", "true")
	setFlag(t, launchCmd, "ttl", "")

	calls := 0
	t.Setenv("AWS_ENDPOINT_URL", countingAWSEndpoint(t, &calls))
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	base := &aws.LaunchConfig{InstanceType: "t3.micro", Region: "us-east-1"}
	err := launchParameterSweep(context.Background(), base, nil, nil)
	if !errors.Is(err, aws.ErrNestedSpawnNeedsTTL) || !strings.Contains(err.Error(), "param set 1") {
		t.Fatalf("--no-detach sweep whose row 1 names %s with no ttl: err = %v, want the TTL refusal naming param set 1", nested, err)
	}
	if calls != 0 {
		t.Errorf("the refused sweep made %d AWS call(s) first", calls)
	}
}

// A sweep left to auto-detach gets the detached refusal (#1372), not an
// invitation to add a --ttl the detached path would refuse anyway.
func TestDetachedSweepWithNestedRowGetsTheDetachedRefusal(t *testing.T) {
	keepLaunchGlobals(t)
	p := filepath.Join(t.TempDir(), "sweep.json")
	body := `{"defaults": {"instance_type": "t3.micro", "region": "us-east-1", "idle_timeout": "1h"},
	          "params": [{"iam_role": "` + aws.NestedSpawnRoleName() + `"}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	setFlag(t, launchCmd, "param-file", p)
	setFlag(t, launchCmd, "ttl", "")

	calls := 0
	t.Setenv("AWS_ENDPOINT_URL", countingAWSEndpoint(t, &calls))
	base := &aws.LaunchConfig{InstanceType: "t3.micro", Region: "us-east-1"}
	err := launchParameterSweep(context.Background(), base, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "a detached sweep cannot use") || errors.Is(err, aws.ErrNestedSpawnNeedsTTL) {
		t.Fatalf("auto-detached sweep naming a nested role: err = %v, want the detached-sweep refusal", err)
	}
	if calls != 0 {
		t.Errorf("the refused sweep made %d AWS call(s) first", calls)
	}
}

// `spawn resume` rebuilds the rows from the param file and applies the same rule.
func TestResumeRefusesANestedRowWithoutTTL(t *testing.T) {
	calls := 0
	t.Setenv("AWS_ENDPOINT_URL", countingAWSEndpoint(t, &calls)) // sets HOME
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, "sweep.json")
	body := `{"defaults": {"instance_type": "t3.micro", "region": "us-east-1"},
	          "params": [{"iam_role": "` + aws.NestedSpawnRoleName() + `"}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".spawn", "sweeps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(SweepState{SweepID: "sw-1375", ParamFile: p, TotalParams: 1, Pending: 1})
	if err := os.WriteFile(filepath.Join(dir, "sw-1375.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	prevID, prevDetach := resumeSweepID, resumeDetach
	t.Cleanup(func() { resumeSweepID, resumeDetach = prevID, prevDetach })
	resumeSweepID, resumeDetach = "sw-1375", false

	err := runResume(resumeCmd, nil)
	if !errors.Is(err, aws.ErrNestedSpawnNeedsTTL) {
		t.Fatalf("resume of a row naming a nested role with no ttl: err = %v, want the TTL refusal", err)
	}
	if calls != 0 {
		t.Errorf("the refused resume made %d AWS call(s) first", calls)
	}
}

// Gate (discovery): every command that accepts --instance-profile or --iam-role,
// found by walking the command tree, refuses a nested-spawn role with no TTL
// before any AWS call. A new command taking either flag fails here until it
// routes the value through aws.CheckNestedSpawnTTL (or refuses the role with
// aws.ErrNestedSpawnRoleReserved).
func TestEveryProfileFlagRoutesThroughTheNestedSpawnCheck(t *testing.T) {
	var found []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, flag := range []string{"instance-profile", "iam-role"} {
			if c.Flags().Lookup(flag) == nil {
				continue
			}
			found = append(found, c.CommandPath()+" --"+flag)
			t.Run(c.CommandPath()+" --"+flag, func(t *testing.T) {
				if c.RunE == nil {
					t.Fatalf("%s takes --%s but has no RunE to check", c.CommandPath(), flag)
				}
				keepCommandFlags(t, c)
				keepLaunchGlobals(t)
				setFlag(t, c, flag, aws.NestedSpawnRoleName())
				if c.Flags().Lookup("ttl") != nil {
					setFlag(t, c, "ttl", "")
				}
				// An instance type keeps `spawn launch` out of its wizard, which
				// reads stdin and may supply the TTL itself (requireNestedSpawnTTL
				// checks that case on the resolved config).
				if c.Flags().Lookup("instance-type") != nil {
					setFlag(t, c, "instance-type", "m7i.large")
				}
				calls := 0
				t.Setenv("AWS_ENDPOINT_URL", countingAWSEndpoint(t, &calls))
				t.Setenv("AWS_MAX_ATTEMPTS", "1")
				err := c.RunE(c, nil)
				if !isNestedRefusal(err) {
					t.Errorf("%s --%s %s with no --ttl: err = %v, want a nested-spawn refusal", c.CommandPath(), flag, aws.NestedSpawnRoleName(), err)
				}
				if calls != 0 {
					t.Errorf("%s --%s: %d AWS call(s) before the refusal", c.CommandPath(), flag, calls)
				}
			})
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
	if len(found) == 0 {
		t.Fatal("found no command taking --instance-profile or --iam-role: the discovery is broken")
	}
	// The ones known today must be among them, so a broken walk can't pass by
	// finding only one.
	for _, want := range []string{"spawn launch --instance-profile", "spawn launch --iam-role", "spawn burst --instance-profile"} {
		found := strings.Join(found, "\n")
		if !strings.Contains(found, want) {
			t.Errorf("discovery did not find %q among:\n%s", want, found)
		}
	}
}
