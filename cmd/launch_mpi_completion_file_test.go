package cmd

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestMPIMemberHonoursCompletionFile is the CLI half of the #1170 gate: the
// launch's --completion-file (LaunchConfig.CompletionFile, which spored watches)
// must reach the MPI script's completion writes on every member. pkg/userdata's
// tests prove the generator renders whatever it is given; this proves the CLI
// gives it.
func TestMPIMemberHonoursCompletionFile(t *testing.T) {
	prev := mpiCommand
	t.Cleanup(func() { mpiCommand = prev })
	mpiCommand = "hostname"

	base := &aws.LaunchConfig{
		Region:         "us-east-1",
		InstanceType:   "c5n.18xlarge",
		OnComplete:     "terminate",
		CompletionFile: "/data/done",
		UserData:       base64.StdEncoding.EncodeToString([]byte("#!/bin/bash\necho base\n")),
	}
	mp := memberParams{name: "mpitest", size: 2, mpi: true}
	for _, idx := range []int{0, 1} {
		cfg, err := buildJobArrayMemberConfig(base, mp, "mpitest-abc", idx, nil)
		if err != nil {
			t.Fatalf("index %d: %v", idx, err)
		}
		ud, err := decodeUserData(cfg.UserData)
		if err != nil {
			t.Fatalf("index %d: decode user-data: %v", idx, err)
		}
		// spawn_mpi_put, the one local write of every record (#1483).
		if !strings.Contains(ud, `echo "$rec" > '/data/done'`) {
			t.Errorf("member %d: the MPI script does not write the configured --completion-file /data/done; spored would never see completion (#1170)", idx)
		}
		if strings.Contains(ud, "/tmp/SPAWN_COMPLETE") {
			t.Errorf("member %d: the MPI script still writes /tmp/SPAWN_COMPLETE despite --completion-file /data/done", idx)
		}
	}
}
