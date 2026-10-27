package cmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
)

// #1343: every Linux launch's user data runs `spored discard-inherited-state`,
// which strips the MPI cluster key root's authorized_keys inherited from the
// image (instancestate.StripClusterKeys, gated in pkg/instancestate and
// cmd/spored). For --mpi it must come before the MPI script's key handling,
// so the strip runs first and this cohort's key is added after it.

var discardInvocation = regexp.MustCompile(`(?m)^/usr/local/bin/spored discard-inherited-state \|\| true$`)

func TestEveryLinuxLaunchStripsInheritedClusterKeys(t *testing.T) {
	boot, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatal(err)
	}
	if !discardInvocation.MatchString(boot) {
		t.Fatal("a plain launch's bootstrap does not run spored discard-inherited-state")
	}
	if !discardInvocation.MatchString(launcher.BootstrapBody()) {
		t.Fatal("the published body (what a release's stub fetches) does not run spored discard-inherited-state")
	}

	base := &aws.LaunchConfig{Region: "us-east-1", InstanceType: "c5n.18xlarge", UserData: encodeUserData(boot)}
	mp := memberParams{name: "mpitest", size: 2, mpi: true}
	for _, index := range []int{0, 1} {
		cfg, err := buildJobArrayMemberConfig(base, mp, "mpitest-abc", index, nil)
		if err != nil {
			t.Fatal(err)
		}
		ud := mustDecodeUserData(t, cfg.UserData)
		loc := discardInvocation.FindStringIndex(ud)
		if loc == nil {
			t.Fatalf("rank %d: no spored discard-inherited-state in the --mpi user data", index)
		}
		keys := strings.Index(ud, "/root/.ssh/authorized_keys")
		if keys < 0 {
			t.Fatalf("rank %d: no MPI key handling found; the test no longer sees the MPI script", index)
		}
		if loc[0] > keys {
			t.Errorf("rank %d: the MPI script touches root's authorized_keys before the inherited cluster key is stripped", index)
		}
	}
}
