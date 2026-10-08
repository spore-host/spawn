package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

func inst(name, id, state, reason string, age time.Duration) aws.InstanceInfo {
	return aws.InstanceInfo{
		Name: name, InstanceID: id, State: state,
		StateTransitionReason: reason,
		LaunchTime:            time.Now().Add(-age),
	}
}

// The reported case: `spawn status chem-arm` right after it self-terminated
// answered `no instance found with name: chem-arm`. Accurate, and the wrong
// answer to the question being asked, which was why it vanished (#736).
func TestFormatRecentlyGone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		all      []aws.InstanceInfo
		id       string
		wantHas  []string
		wantNone []string
	}{
		{
			name: "a just-terminated instance is named, with EC2's reason",
			all: []aws.InstanceInfo{
				inst("chem-arm", "i-aaa", "terminated", "Client.InstanceInitiatedShutdown", time.Hour),
			},
			id:      "chem-arm",
			wantHas: []string{"chem-arm", "i-aaa", "terminated", "Client.InstanceInitiatedShutdown"},
		},
		{
			// The default state filter excludes shutting-down too, which is how
			// `terminate`'s "already shutting down" branch became unreachable.
			name: "a shutting-down instance is reported as such",
			all: []aws.InstanceInfo{
				inst("web", "i-bbb", "shutting-down", "User initiated", time.Minute),
			},
			id:      "web",
			wantHas: []string{"i-bbb", "shutting-down", "User initiated"},
		},
		{
			name: "shutting-down wins over terminated for the same name",
			all: []aws.InstanceInfo{
				inst("web", "i-old", "terminated", "User initiated", 2*time.Hour),
				inst("web", "i-new", "shutting-down", "User initiated", time.Minute),
			},
			id:       "web",
			wantHas:  []string{"i-new", "shutting-down"},
			wantNone: []string{"i-old"},
		},
		{
			name: "among same-state matches the most recent wins",
			all: []aws.InstanceInfo{
				inst("web", "i-older", "terminated", "", 5*time.Hour),
				inst("web", "i-newer", "terminated", "", time.Minute),
			},
			id:       "web",
			wantHas:  []string{"i-newer"},
			wantNone: []string{"i-older"},
		},
		{
			name:    "lookup by instance ID works too",
			all:     []aws.InstanceInfo{inst("", "i-ccc", "terminated", "Server.SpotInstanceTermination", time.Minute)},
			id:      "i-ccc",
			wantHas: []string{"i-ccc", "Server.SpotInstanceTermination"},
		},
		{
			// A RUNNING instance must not produce a hint: it would have been found
			// by the normal lookup, so saying "it is running" on a not-found path
			// would be contradictory nonsense.
			name:     "a running instance produces nothing",
			all:      []aws.InstanceInfo{inst("web", "i-ddd", "running", "", time.Minute)},
			id:       "web",
			wantNone: []string{"i-ddd", "running"},
		},
		{
			name:     "a stopped instance produces nothing",
			all:      []aws.InstanceInfo{inst("web", "i-eee", "stopped", "User initiated", time.Minute)},
			id:       "web",
			wantNone: []string{"i-eee"},
		},
		{
			name:     "a genuinely unknown name produces nothing",
			all:      []aws.InstanceInfo{inst("other", "i-fff", "terminated", "", time.Minute)},
			id:       "web",
			wantNone: []string{"i-fff", "other"},
		},
		{
			name:     "no instances at all produces nothing",
			all:      nil,
			id:       "web",
			wantNone: []string{"—"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatRecentlyGone(tc.all, tc.id)
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("hint %q does not contain %q", got, want)
				}
			}
			for _, unwanted := range tc.wantNone {
				if strings.Contains(got, unwanted) {
					t.Errorf("hint %q should not contain %q", got, unwanted)
				}
			}
			if len(tc.wantHas) == 0 && got != "" {
				t.Errorf("hint = %q, want empty", got)
			}
		})
	}
}

// A missing transition reason must not produce a dangling separator. EC2 leaves
// it empty for some states, so the message has to read correctly without it.
func TestFormatRecentlyGone_NoReasonReadsCleanly(t *testing.T) {
	got := formatRecentlyGone([]aws.InstanceInfo{inst("web", "i-aaa", "terminated", "", time.Minute)}, "web")
	if strings.HasSuffix(got, ":") || strings.Contains(got, ": \n") {
		t.Errorf("hint %q has a dangling reason separator", got)
	}
	if !strings.Contains(got, "terminated") {
		t.Errorf("hint %q lost the state", got)
	}
}
