package cmd

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestBuildLaunchConfig_HibernateLifecycleImpliesHibernation is #949. EC2
// hibernates only an instance launched configured for hibernation, and only
// --hibernate configured it: --on-idle hibernate, --hibernate-on-idle and
// --on-complete hibernate launched an instance whose idle or completion
// hibernate silently became a stop. Each must now configure hibernation, which
// is also what puts it through the hibernation pre-flight.
func TestBuildLaunchConfig_HibernateLifecycleImpliesHibernation(t *testing.T) {
	prev := struct {
		typ, onIdle, onComplete, idle string
		hib, hibOnIdle                bool
	}{instanceType, onIdle, onComplete, idleTimeout, hibernate, hibernateOnIdle}
	t.Cleanup(func() {
		instanceType, onIdle, onComplete, idleTimeout = prev.typ, prev.onIdle, prev.onComplete, prev.idle
		hibernate, hibernateOnIdle = prev.hib, prev.hibOnIdle
	})
	instanceType = "m7i.large"

	cases := []struct {
		name                  string
		onIdle, onComplete    string
		hibernateFlag, hibIdl bool
		want                  bool
	}{
		{"--on-idle hibernate", "hibernate", "", false, false, true},
		{"--hibernate-on-idle (deprecated)", "", "", false, true, true},
		{"--on-complete hibernate", "", "hibernate", false, false, true},
		{"--hibernate", "", "", true, false, true},
		{"--on-idle stop", "stop", "", false, false, false},
		{"--on-complete stop", "", "stop", false, false, false},
		{"--on-complete terminate", "", "terminate", false, false, false},
		{"nothing", "", "", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			onIdle, onComplete, hibernate, hibernateOnIdle = tc.onIdle, tc.onComplete, tc.hibernateFlag, tc.hibIdl
			idleTimeout = "15m"
			cfg, err := buildLaunchConfig(nil)
			if err != nil {
				t.Fatalf("buildLaunchConfig: %v", err)
			}
			if cfg.Hibernate != tc.want {
				t.Errorf("Hibernate = %v, want %v: an instance not configured for hibernation "+
					"cannot hibernate, and spored falls back to a plain stop", cfg.Hibernate, tc.want)
			}
		})
	}
}

// The pre-flight refusal must name the flag the user actually passed: since
// #949 --on-idle hibernate and --on-complete hibernate need a hibernation-capable
// type too, and an error naming only --hibernate points at a flag they never used.
func TestHibernationRequestedBy(t *testing.T) {
	prev := hibernate
	t.Cleanup(func() { hibernate = prev })

	hibernate = false
	if got := hibernationRequestedBy(&aws.LaunchConfig{OnComplete: "hibernate", Hibernate: true}); got != "--on-complete hibernate" {
		t.Errorf("on-complete: %q", got)
	}
	if got := hibernationRequestedBy(&aws.LaunchConfig{HibernateOnIdle: true, Hibernate: true}); got != "--on-idle hibernate" {
		t.Errorf("on-idle: %q", got)
	}
	hibernate = true
	if got := hibernationRequestedBy(&aws.LaunchConfig{Hibernate: true, HibernateOnIdle: true}); !strings.Contains(got, "--hibernate") || !strings.Contains(got, "--on-idle hibernate") {
		t.Errorf("both: %q", got)
	}
}
