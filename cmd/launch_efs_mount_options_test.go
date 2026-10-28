package cmd

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// --efs-mount-options on every path that renders the storage script (#1390):
// the single launch and both sweep paths share launchStorageScript (#1366), and
// each must mount with what the user asked for — an option the parser does not
// model (nconnect) included, an unsafe one refused, the modelled ones as before.

const (
	efsNconnectOpts = "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2,noresvport,nconnect=4,_netdev"
	efsTimeoOpts    = "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,timeo=150,retrans=2,noresvport,_netdev"
)

func setEFSMountOptions(t *testing.T, v string) {
	t.Helper()
	prev := efsMountOptions
	t.Cleanup(func() { efsMountOptions = prev })
	efsMountOptions = v
}

// efsMountOptionsIn is the -o argument of every `mount -t nfs4` in script and
// the options field of its EFS fstab line, as the shell will see them.
func efsMountOptionsIn(t *testing.T, script string) (mounts []string, fstab string) {
	t.Helper()
	for _, m := range regexp.MustCompile(`(?m)^\s*if mount -t nfs4 -o '([^']*)' `).FindAllStringSubmatch(script, -1) {
		mounts = append(mounts, m[1])
	}
	if len(mounts) != 2 {
		t.Fatalf("want the mount-target-IP and DNS mount commands, found %d:\n%s", len(mounts), script)
	}
	m := regexp.MustCompile(`(?m)^echo '\S+:/ \S+ nfs4 (\S+),_netdev 0 0' >> /etc/fstab$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("no EFS fstab line:\n%s", script)
	}
	return mounts, m[1]
}

func checkEFSMountOptions(t *testing.T, what, script, want string) {
	t.Helper()
	mounts, fstab := efsMountOptionsIn(t, script)
	for _, got := range mounts {
		if got != want {
			t.Errorf("%s: mount -t nfs4 -o %q, want %q", what, got, want)
		}
	}
	if fstab != want {
		t.Errorf("%s: fstab options %q, want %q", what, fstab, want)
	}
}

var efsRenderPaths = map[string]func(t *testing.T) ([]string, error){
	"single launch": func(t *testing.T) ([]string, error) {
		cfg := &aws.LaunchConfig{Region: "us-east-1", EFSID: "fs-0123456789abcdef0", EFSMountPoint: "/shared", TargetOS: "linux"}
		s, _, err := launchStorageScript(context.Background(), &fakeStorageLookups{}, cfg, nil, io.Discard)
		return []string{s}, err
	},
	"foreground sweep": func(t *testing.T) ([]string, error) {
		base := aws.LaunchConfig{InstanceType: "m7i.large", Region: "us-east-1", EFSID: "fs-0123456789abcdef0", EFSMountPoint: "/shared"}
		cfgs, err := foregroundMembers(t, testSweepPlat(t), base, twoRows(), &fakeStorageLookups{})
		var out []string
		for _, c := range cfgs {
			out = append(out, memberScript(t, c))
		}
		return out, err
	},
	"detached sweep": func(t *testing.T) ([]string, error) {
		pf := &ParamFileFormat{
			Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0", "region": "us-east-1", "ttl": "2h"},
			Params:   []map[string]interface{}{{"trial": "a"}, {"trial": "b"}},
		}
		base := aws.LaunchConfig{InstanceType: "t3.micro", EFSID: "fs-0123456789abcdef0", EFSMountPoint: "/shared"}
		if err := seedDetachedBootstrap(pf, base, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
			return nil, err
		}
		var out []string
		for i := range pf.Params {
			out = append(out, decodedScript(t, memberUserData(pf, i)))
		}
		return out, nil
	},
}

func TestEFSMountOptionsReachTheMountOnEveryPath(t *testing.T) {
	for name, render := range efsRenderPaths {
		for opts, want := range map[string]string{"nconnect=4": efsNconnectOpts, "timeo=150": efsTimeoOpts} {
			t.Run(name+"/"+opts, func(t *testing.T) {
				setEFSMountOptions(t, opts)
				scripts, err := render(t)
				if err != nil {
					t.Fatalf("--efs-mount-options %s: %v", opts, err)
				}
				if len(scripts) == 0 {
					t.Fatal("rendered no member")
				}
				for i, s := range scripts {
					checkEFSMountOptions(t, fmt.Sprintf("%s member %d", name, i), s, want)
				}
			})
		}
	}
}

func TestEFSMountOptionsWithMetacharactersAreRefusedOnEveryPath(t *testing.T) {
	for name, render := range efsRenderPaths {
		for _, opts := range []string{"nconnect=4;reboot", "nconnect=$(id)", "noac' x"} {
			t.Run(name+"/"+opts, func(t *testing.T) {
				setEFSMountOptions(t, opts)
				if _, err := render(t); err == nil || !strings.Contains(err.Error(), "--efs-mount-options") {
					t.Errorf("--efs-mount-options %q: want a refusal naming the flag, got %v", opts, err)
				}
			})
		}
	}
}

// A single launch creates its key pair, role, security group and any FSx
// before it renders the storage script, so a bad --efs-mount-options is
// refused by runLaunch's up-front flag checks, before the first AWS call.
func TestValidateEFSMountOptionsFlag(t *testing.T) {
	setEFSMountOptions(t, "nconnect=4;reboot")
	if err := validateEFSMountOptionsFlag(); err == nil {
		t.Error("an unsafe --efs-mount-options passed the up-front check")
	}
	setEFSMountOptions(t, "nconnect=4,timeo=150")
	if err := validateEFSMountOptionsFlag(); err != nil {
		t.Errorf("a valid --efs-mount-options failed the up-front check: %v", err)
	}
	setEFSMountOptions(t, "")
	if err := validateEFSMountOptionsFlag(); err != nil {
		t.Errorf("no --efs-mount-options failed the up-front check: %v", err)
	}
	src := readCmdSource(t, "launch_single.go")
	body := src[strings.Index(src, "func runLaunch("):]
	body = body[:strings.Index(body, "\n}\n")]
	call := strings.Index(body, "validateEFSMountOptionsFlag()")
	first := strings.Index(body, "aws.NewClient(")
	if first < 0 {
		t.Fatal("runLaunch no longer calls aws.NewClient; re-anchor this check on its first AWS call")
	}
	if call < 0 || call > first {
		t.Error("runLaunch does not call validateEFSMountOptionsFlag() before its first aws.NewClient")
	}
}
