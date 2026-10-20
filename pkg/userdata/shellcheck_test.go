package userdata

import (
	"testing"

	"github.com/spore-host/spawn/internal/shellcheckgate"
)

// shellcheck on the rendered MPI, storage, container and queue-runner user-data
// (#689 part C). MPI, storage and container are fragments appended to the
// spored bootstrap; each is checked on its own, so a finding names the
// generator that produced it.
func TestUserData_Shellcheck(t *testing.T) {
	t.Run("mpi", func(t *testing.T) {
		for name, cfg := range map[string]MPIConfig{
			"install": {Region: "us-east-1", JobArrayID: "ja-1", JobArrayIndex: 0, JobArraySize: 4,
				MPIProcessesPerNode: 8, MPICommand: `mpirun -np 32 ./sim --label "run a"`, ReadyGate: "/run/spawn/mpi-ready"},
			"efa, peer": {Region: "us-east-1", JobArrayID: "ja-1", JobArrayIndex: 2, JobArraySize: 4,
				MPIProcessesPerNode: 8, MPICommand: "./sim", SkipInstall: true, EFAEnabled: true,
				CompletionFile: "/var/run/my complete.json"},
		} {
			t.Run(name, func(t *testing.T) {
				script, err := GenerateMPIUserData(cfg)
				if err != nil {
					t.Fatal(err)
				}
				shellcheckgate.Check(t, "pkg/userdata.GenerateMPIUserData", script)
			})
		}
	})
	t.Run("storage", func(t *testing.T) {
		script, err := GenerateStorageUserData(StorageConfig{
			FSxLustreEnabled: true, FSxFilesystemDNS: "fs-0123.fsx.us-east-1.amazonaws.com", FSxMountName: "abcdef", FSxMountPoint: "/fsx",
			EFSEnabled: true, EFSFilesystemDNS: "fs-0456.efs.us-east-1.amazonaws.com", EFSMountTargetIP: "10.0.0.12",
			EFSMountPoint: "/efs", EFSMountOptions: "nfsvers=4.1,rsize=1048576",
			AttachedVolumes: []AttachedVolume{{DeviceName: "/dev/sdf", MountPoint: "/data"}, {DeviceName: "/dev/sdg", MountPoint: "/ref", ReadOnly: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		shellcheckgate.Check(t, "pkg/userdata.GenerateStorageUserData", script)
	})
	t.Run("container", func(t *testing.T) {
		for name, cfg := range map[string]ContainerConfig{
			"private ECR, gpu, argv": {Image: "111111111111.dkr.ecr.us-east-1.amazonaws.com/tool:1", Region: "us-east-1",
				InstanceType: "g5.xlarge", Command: []string{"python3", "-c", "print('a b')"}},
			"public": {Image: "public.ecr.aws/docker/library/nginx:latest"},
		} {
			t.Run(name, func(t *testing.T) {
				script, err := GenerateContainerUserData(cfg)
				if err != nil {
					t.Fatal(err)
				}
				shellcheckgate.Check(t, "pkg/userdata.GenerateContainerUserData", script)
			})
		}
	})
	t.Run("queue runner", func(t *testing.T) {
		shellcheckgate.Check(t, "pkg/userdata.GenerateQueueRunnerUserData",
			GenerateQueueRunnerUserData("s3://bucket/queues/q.json", "q-1"))
	})
}
