package cmd

import (
	"encoding/base64"
	"testing"

	"github.com/spore-host/spawn/internal/shellcheckgate"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/userdata"
)

// shellcheck on the scripts cmd renders itself (#689 part C): the app
// templates, the pool worker loop, `spawn upgrade-spored`, the terminate-time
// flush and the SSM key injection, plus the job-array member user-data as a
// launch composes it (bootstrap + storage + MPI).
func TestCmdScripts_Shellcheck(t *testing.T) {
	dec := func(t *testing.T, enc string) string {
		t.Helper()
		b, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("user-data is not base64: %v", err)
		}
		return string(b)
	}
	const privImage = "111111111111.dkr.ecr.us-east-1.amazonaws.com/paraview:5.13.2"

	t.Run("app container DCV", func(t *testing.T) {
		shellcheckgate.Check(t, "cmd.containerRunWrapper", containerRunWrapper(privImage, true))
		shellcheckgate.Check(t, "cmd.buildDCVUserDataWithInit",
			dec(t, buildContainerDCVUserData(privImage, true, true, "us-east-1", "console")))
	})
	t.Run("app DCV command", func(t *testing.T) {
		shellcheckgate.Check(t, "cmd.buildDCVUserDataWithInit",
			dec(t, buildDCVUserData("/opt/paraview/bin/paraview --data '/data/my set'", "console")))
	})
	t.Run("app desktop", func(t *testing.T) {
		shellcheckgate.Check(t, "cmd.buildDesktopDCVUserData", dec(t, buildDesktopDCVUserData("console")))
	})
	t.Run("app web", func(t *testing.T) {
		shellcheckgate.Check(t, "cmd.buildWebUserData",
			dec(t, buildWebUserData(privImage, 8080, true, true, "us-east-1", []string{"--bind-addr", "0.0.0.0:8080", "--auth", "none"})))
		shellcheckgate.Check(t, "cmd.buildWebUserData",
			dec(t, buildWebUserData("public.ecr.aws/x/jupyter:1", 8888, false, false, "us-east-1", nil)))
	})
	t.Run("pool worker", func(t *testing.T) {
		shellcheckgate.Check(t, "cmd.buildPoolWorkerCommand",
			"#!/bin/bash\n"+buildPoolWorkerCommand("run-1", "spec-bucket", "staging", "5m0s", "/tmp/SPAWN_COMPLETE"))
		boot, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
			Username:       "ec2-user",
			CustomUserData: buildPoolWorkerCommand("run-1", "spec-bucket", "staging", "5m0s", "/tmp/SPAWN_COMPLETE"),
		})
		if err != nil {
			t.Fatal(err)
		}
		shellcheckgate.Check(t, "cmd.buildPoolWorkerCommand", boot)
	})
	t.Run("upgrade spored", func(t *testing.T) {
		shellcheckgate.Check(t, "cmd.buildSporedUpgradeScript", "#!/bin/bash\n"+buildSporedUpgradeScript("0.138.0"))
	})
	t.Run("terminate flush", func(t *testing.T) {
		shellcheckgate.Check(t, "cmd.taskFlushCommand", "#!/bin/bash\n"+taskFlushCommand())
	})
	t.Run("ssm key injection", func(t *testing.T) {
		s, err := authorizedKeyInjectionScript("ec2-user", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample spawn")
		if err != nil {
			t.Fatal(err)
		}
		shellcheckgate.Check(t, "cmd.authorizedKeyInjectionScript", "#!/bin/bash\n"+s)
	})
	t.Run("burst", func(t *testing.T) {
		s, err := buildBurstUserData()
		if err != nil {
			t.Fatal(err)
		}
		shellcheckgate.Check(t, "cmd.buildBurstUserData", s)
	})
	t.Run("job array member (bootstrap + storage + MPI)", func(t *testing.T) {
		storage, err := userdata.GenerateStorageUserData(userdata.StorageConfig{
			EFSEnabled: true, EFSFilesystemDNS: "fs-0456.efs.us-east-1.amazonaws.com", EFSMountPoint: "/efs",
		})
		if err != nil {
			t.Fatal(err)
		}
		boot, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
			Username:         "ec2-user",
			StorageScript:    storage,
			ReadyMountPoints: []string{"/efs"},
			ReadyGates:       readyGatesForLaunch(true),
			Command:          "mpirun ./sim",
		})
		if err != nil {
			t.Fatal(err)
		}
		mpi, err := userdata.GenerateMPIUserData(userdata.MPIConfig{
			Region: "us-east-1", JobArrayID: "ja-1", JobArraySize: 2, MPIProcessesPerNode: 4,
			MPICommand: "./sim", ReadyGate: launcher.MPIReadyGate,
		})
		if err != nil {
			t.Fatal(err)
		}
		shellcheckgate.Check(t, "pkg/userdata.GenerateMPIUserData", boot+"\n"+mpi)
	})
}
