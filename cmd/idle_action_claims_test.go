package cmd

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/spore-host/libs/i18n"

	"github.com/spore-host/spawn/pkg/aws"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
)

// Help text that says idle "terminates" must belong to a command whose
// instances really are terminated when idle (#1151, #1132, #1146).
//
// spored's idle action is a stop unless the launch config sets
// TerminateOnIdle, yet `spawn service --idle-timeout`, `spawn launch
// --idle-timeout`, `spawn burst --idle-timeout`, `spawn defaults` and `spawn
// app` all said idle terminates. A stopped instance keeps billing its EBS
// volume, so the wording was a cost claim, not a style nit.
//
// Discovered, not listed: every command in the cobra tree is walked and every
// clause of its Short, Long, Example and flag usages is checked. A clause that
// mentions idleness and termination is a claim; the command making it must
// appear in idleTerminateProofs, whose entry builds the command's REAL launch
// config (the function its RunE uses) — so the proof is the mechanism, and a
// command that loses TerminateOnIdle fails here even if its help is unchanged.
//
// Scope: the help as a user sees it, with i18n initialised and
// updateCommandDescriptions applied (done here explicitly, so the result does
// not depend on whether an earlier test ran the root command). The Short/Long
// strings that function takes from spore-host/libs i18n are excluded: several
// of them (spawn.root.long's "Auto-terminates on idle or TTL", among others)
// make the same false claim, but they live in another repository and are
// tracked there. Which strings are libs-owned is discovered from
// updateCommandDescriptions' source, not listed.

// idleClaim matches a clause that says idleness ends in termination.
var (
	idleWord      = regexp.MustCompile(`(?i)\b(idle\w*|inactivity)\b`)
	terminateWord = regexp.MustCompile(`(?i)terminat\w*`)
	clauseSplit   = regexp.MustCompile(`[;\n]|\.(\s|$)`)
)

// idleTerminateProofs maps a command path to the launch config its instances
// get. Adding a command here is only right if the config says TerminateOnIdle.
var idleTerminateProofs = map[string]func() aws.LaunchConfig{
	"spawn service": func() aws.LaunchConfig { return serviceLaunchConfig("svc", "us-east-1") },
	"spawn pool":    func() aws.LaunchConfig { return poolWorkerBaseConfig("us-east-1", "ami-x", "p", "") },
	"spawn pool create": func() aws.LaunchConfig {
		return poolWorkerBaseConfig("us-east-1", "ami-x", "p", "")
	},
	"spawn pool drain": func() aws.LaunchConfig {
		return poolWorkerBaseConfig("us-east-1", "ami-x", "p", "")
	},
	"spawn burst": func() aws.LaunchConfig {
		members, err := burstMemberConfigs(burstTestParams())
		if err != nil || len(members) == 0 {
			return aws.LaunchConfig{}
		}
		return members[0]
	},
}

func idleTerminateClaims(c *cobra.Command) []string {
	var texts []string
	texts = append(texts, c.Short, c.Long, c.Example)
	c.LocalFlags().VisitAll(func(f *pflag.Flag) { texts = append(texts, "--"+f.Name+": "+f.Usage) })
	var claims []string
	for _, text := range texts {
		for _, clause := range clauseSplit.Split(text, -1) {
			if idleWord.MatchString(clause) && terminateWord.MatchString(clause) {
				claims = append(claims, strings.TrimSpace(clause))
			}
		}
	}
	return claims
}

// libsOwnedHelp returns the rendered text of every i18n key
// updateCommandDescriptions assigns to a command's Short or Long.
func libsOwnedHelp(t *testing.T) map[string]string {
	t.Helper()
	src, err := os.ReadFile("root.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func updateCommandDescriptions()")
	if start < 0 {
		t.Fatal("updateCommandDescriptions not found in root.go — the libs-owned discovery is broken")
	}
	body = body[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	owned := map[string]string{}
	for _, m := range regexp.MustCompile(`\.(?:Short|Long) = i18n\.T\("([^"]+)"\)`).FindAllStringSubmatch(body, -1) {
		owned[i18n.T(m[1])] = m[1]
	}
	if len(owned) == 0 {
		t.Fatal("found no i18n-assigned help in updateCommandDescriptions — the discovery is broken")
	}
	return owned
}

func TestIdleTerminateClaimsAreTrue(t *testing.T) {
	ensureI18nInitialized()
	updateCommandDescriptions()
	libsOwned := libsOwnedHelp(t)

	commands := 0
	claimed := map[string][]string{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		commands++
		view := *c // a copy, so blanking a libs-owned field touches nothing shared
		for _, f := range []*string{&view.Short, &view.Long} {
			if key, ok := libsOwned[*f]; ok {
				if cl := idleTerminateClaims(&cobra.Command{Long: *f}); len(cl) > 0 {
					t.Logf("not judged here (spore-host/libs i18n %s): %q", key, cl)
				}
				*f = ""
			}
		}
		if cl := idleTerminateClaims(&view); len(cl) > 0 {
			claimed[c.CommandPath()] = cl
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
	if commands < 20 {
		t.Fatalf("walked only %d commands — the discovery is broken, not the help clean", commands)
	}
	if len(claimed) == 0 {
		t.Fatal("no command claims an idle terminate, not even `spawn service` — the clause matcher is broken")
	}

	paths := make([]string, 0, len(claimed))
	for p := range claimed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		proof, ok := idleTerminateProofs[path]
		if !ok {
			t.Errorf("%s says idle terminates, but its instances are stopped when idle:\n    %s\n"+
				"  Say \"stop\", or set TerminateOnIdle on its launch config and add it to idleTerminateProofs.",
				path, strings.Join(claimed[path], "\n    "))
			continue
		}
		if cfg := proof(); !cfg.TerminateOnIdle {
			t.Errorf("%s says idle terminates, but its launch config has TerminateOnIdle=false:\n    %s",
				path, strings.Join(claimed[path], "\n    "))
		}
	}
	// A proof for a command that no longer exists is a stale entry.
	for path := range idleTerminateProofs {
		args := strings.Fields(strings.TrimPrefix(path, "spawn"))
		if c, _, err := rootCmd.Find(args); err != nil || c.CommandPath() != path {
			t.Errorf("idleTerminateProofs names %q, which is not a command", path)
		}
	}
}

// The matcher must catch the shapes the bug shipped in, and must not count a
// clause that only names the TTL's terminate next to an idle default.
func TestIdleTerminateClaimMatcher(t *testing.T) {
	claim := []string{
		"Terminate the instance after this much idleness (e.g. 30m)",
		"Auto-terminate if idle, e.g. 30m",
		"hibernate-on-idle  Hibernate instead of terminating on idle (true/false)",
		"Workers self-terminate on idle-timeout (scale to zero).",
	}
	for _, s := range claim {
		if len(idleTerminateClaims(&cobra.Command{Long: s})) == 0 {
			t.Errorf("not recognised as an idle-terminate claim: %q", s)
		}
	}
	notClaim := []string{
		"Auto-terminate after duration (e.g., 8h; defaults to 1h idle if not set)",
		"Terminate the instance after this long (e.g. 2h). With no --ttl, a 1h idle timeout is applied",
		"Stop the instance after this much idleness",
	}
	for _, s := range notClaim {
		if got := idleTerminateClaims(&cobra.Command{Long: s}); len(got) > 0 {
			t.Errorf("wrongly counted as an idle-terminate claim: %q (%q)", s, got)
		}
	}
}

// `spawn service`'s idle action is terminate (#1151), and so the dry run and
// the auto-applied idle default say so; a `spawn launch` instance's is a stop,
// and its notice must not say terminate.
func TestIdleDefaultNoticeNamesTheRealAction(t *testing.T) {
	resetServiceFlags(t)
	serviceInstanceType = "m7i.large"
	svc := serviceLaunchConfig("svc", "us-east-1")
	if !svc.TerminateOnIdle {
		t.Fatal("spawn service's launch config does not terminate on idle (#1151)")
	}
	launch := aws.LaunchConfig{InstanceType: "m7i.large"}
	hib := aws.LaunchConfig{InstanceType: "m7i.large", HibernateOnIdle: true}

	cases := []struct {
		name    string
		cfg     *aws.LaunchConfig
		want    string
		mustNot string
	}{
		{"service", &svc, "will terminate after 1 hour", "will stop"},
		{"launch", &launch, "will stop after 1 hour", "terminate after"},
		{"launch --on-idle hibernate", &hib, "will hibernate after 1 hour", "terminate after"},
	}
	for _, tc := range cases {
		out := captureStderr(t, func() {
			if err := guardZombieInstance(tc.cfg); err != nil {
				t.Fatalf("%s: guardZombieInstance: %v", tc.name, err)
			}
		})
		if !strings.Contains(out, tc.want) || strings.Contains(out, tc.mustNot) {
			t.Errorf("%s: idle-default notice\n%s\nwant it to contain %q and not %q", tc.name, out, tc.want, tc.mustNot)
		}
	}
}

// `spawn app launch` always sets a TTL (#1150). An app's idle action is a stop
// so the session can be restarted; the TTL is the only thing that terminates an
// abandoned one. The default is the flag's, so the launch config (TTL:
// appLaunchTTL) and the help both carry it, and an explicitly empty --ttl is
// refused before anything is launched.
func TestAppLaunchAlwaysHasATTL(t *testing.T) {
	f := appLaunchCmd.Flags().Lookup("ttl")
	if f == nil {
		t.Fatal("spawn app launch has no --ttl flag")
	}
	if f.DefValue != "8h" {
		t.Errorf("--ttl default = %q, want \"8h\" (the owner's decision on #1150)", f.DefValue)
	}
	if d, err := ttlpkg.ParseTTL(f.DefValue); err != nil || d <= 0 {
		t.Errorf("--ttl default %q is not a positive TTL: %v", f.DefValue, err)
	}

	// Through runAppLaunch, for every catalog app: an empty TTL must stop the
	// launch with the TTL error, before any AWS client exists. The AWS
	// environment is pointed nowhere so a regression fails fast instead of
	// calling a real account.
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_REGION", "us-east-1")
	saved := appLaunchTTL
	t.Cleanup(func() { appLaunchTTL = saved })
	for _, v := range []string{"", "  "} {
		appLaunchTTL = v
		err := runAppLaunch(appLaunchCmd, []string{"paraview"})
		if err != errAppTTLRequired {
			t.Errorf("--ttl %q: runAppLaunch returned %v, want the TTL-required refusal", v, err)
		}
	}
}
