package launcher

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// #1521: the optional acpid install was `dnf install || yum install`, and on
// AL2023 yum is dnf. On a t4g.nano the OOM killer took dnf, the same install
// ran again as yum, and that second run's OOM took spored and
// systemd-resolved. And the AL2023 AMI ships acpid, so that install was a
// no-op whose repo-metadata load was all it did. These run the whole inline
// bootstrap in the boot sandbox with recording package-manager stand-ins.

const (
	acpidNotInstalled = "acpid not installed; stop/terminate will not shut down gracefully"
	acpidSkipped      = "acpid skipped: MemTotal "
)

// acpidBin is an acpid on the sandbox's PATH, as the AL2023 AMI ships one.
const acpidBin = "#!/bin/bash\nexit 0\n"

// pm is a package-manager stand-in that records its arguments in
// $HOME/<name>-calls and exits code; exiting 0, it puts acpid on the PATH, as
// a real install does.
func pm(name string, code int) string {
	s := "#!/bin/bash\necho \"$*\" >> \"$HOME/" + name + "-calls\"\n"
	if code == 0 {
		s += "printf '#!/bin/bash\\nexit 0\\n' > \"$HOME/bin/acpid\"\nchmod +x \"$HOME/bin/acpid\"\n"
	}
	return s + "exit " + strconv.Itoa(code) + "\n"
}

func meminfo(kb int) string {
	return "MemTotal:       " + strconv.Itoa(kb) + " kB\nMemFree:          100000 kB\n"
}

func acpidEnabled(sb *bootSandbox) bool {
	return strings.Contains(sb.read("systemctl-calls"), "enable --now acpid")
}

// acpidBoot runs a full boot and fails the test unless the bootstrap
// succeeded and spored started: the acpid step is never fatal (#1274).
func acpidBoot(t *testing.T, o bootOpts) (*bootSandbox, string) {
	t.Helper()
	sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester", CustomUserData: "echo post > /tmp/POST_RAN\n"}, "", o)
	if err != nil {
		t.Fatalf("the bootstrap failed on the optional acpid step: %v\n%s", err, out)
	}
	if !sporedStarted(sb) || retired(sb) {
		t.Fatalf("spored=%v retired=%v\n%s", sporedStarted(sb), retired(sb), out)
	}
	if !fileExists(sb.dir + "/tmp/POST_RAN") {
		t.Fatalf("the custom user data did not run after the acpid step\n%s", out)
	}
	return sb, out
}

func TestAcpidInstallRunsExactlyOnePackageManager(t *testing.T) {
	const big = 1900000 // a 2 GiB type's MemTotal (t4g.small)

	// The live finding on the fix itself: every AL2023 type, the nano
	// included, already had acpid, so the install only cost memory.
	t.Run("acpid already present: no package manager runs and it is enabled", func(t *testing.T) {
		for name, kb := range map[string]int{"t4g.small": big, "t4g.nano": 422188} {
			sb, out := acpidBoot(t, bootOpts{meminfo: meminfo(kb), standIns: map[string]string{"acpid": acpidBin, "dnf": pm("dnf", 0), "yum": pm("yum", 0)}})
			if got := sb.read("dnf-calls") + sb.read("yum-calls"); got != "" {
				t.Errorf("%s: a package manager ran with acpid already installed: %q\n%s", name, got, out)
			}
			if !acpidEnabled(sb) {
				t.Errorf("%s: the shipped acpid was not enabled\n%s", name, out)
			}
			for _, msg := range []string{acpidNotInstalled, acpidSkipped, "acpid installed but not started"} {
				if strings.Contains(out, msg) {
					t.Errorf("%s: printed %q though acpid is installed and enabled\n%s", name, msg, out)
				}
			}
		}
	})

	t.Run("acpid present but the enable fails: says so", func(t *testing.T) {
		sb, out, err := fullBoot(t, BootstrapConfig{Username: "tester"}, "acpid", bootOpts{meminfo: meminfo(big), standIns: map[string]string{"acpid": acpidBin}})
		if err != nil || !sporedStarted(sb) {
			t.Fatalf("err=%v spored=%v\n%s", err, sporedStarted(sb), out)
		}
		if !strings.Contains(out, "acpid installed but not started; stop/terminate will not shut down gracefully") || strings.Contains(out, acpidNotInstalled) {
			t.Errorf("a failed enable of an installed acpid was misreported\n%s", out)
		}
	})

	t.Run("an install that exits 0 but leaves no acpid is not called installed", func(t *testing.T) {
		sb, out := acpidBoot(t, bootOpts{meminfo: meminfo(big), standIns: map[string]string{"dnf": "#!/bin/bash\nexit 0\n"}})
		if !strings.Contains(out, acpidNotInstalled) || acpidEnabled(sb) {
			t.Errorf("acpid is absent, so it must be reported not installed and not enabled\n%s", out)
		}
	})

	t.Run("dnf present: only dnf runs", func(t *testing.T) {
		sb, out := acpidBoot(t, bootOpts{meminfo: meminfo(big), standIns: map[string]string{"dnf": pm("dnf", 0), "yum": pm("yum", 0)}})
		if got := sb.read("dnf-calls"); got != "install -y acpid\n" {
			t.Errorf("dnf calls = %q, want one install\n%s", got, out)
		}
		if got := sb.read("yum-calls"); got != "" {
			t.Errorf("yum ran after dnf succeeded: %q", got)
		}
		if !acpidEnabled(sb) {
			t.Errorf("acpid was installed but not enabled\n%s", out)
		}
	})

	// The #1521 live finding: yum must not rerun the transaction dnf was
	// killed in.
	t.Run("dnf OOM-killed: yum is not run and the boot goes on", func(t *testing.T) {
		sb, out := acpidBoot(t, bootOpts{meminfo: meminfo(big), standIns: map[string]string{"dnf": pm("dnf", 137), "yum": pm("yum", 0)}})
		if got := sb.read("dnf-calls"); got != "install -y acpid\n" {
			t.Errorf("dnf calls = %q, want one install", got)
		}
		if got := sb.read("yum-calls"); got != "" {
			t.Errorf("yum reran the install dnf was killed in (#1521): %q\n%s", got, out)
		}
		if !strings.Contains(out, acpidNotInstalled) {
			t.Errorf("the failed install was not reported\n%s", out)
		}
		if acpidEnabled(sb) {
			t.Error("acpid was enabled after its install failed")
		}
	})

	t.Run("yum-only base: yum runs", func(t *testing.T) {
		sb, out := acpidBoot(t, bootOpts{meminfo: meminfo(big), standIns: map[string]string{"dnf": "", "yum": pm("yum", 0)}})
		if got := sb.read("yum-calls"); got != "install -y acpid\n" {
			t.Errorf("yum calls = %q, want one install\n%s", got, out)
		}
		if !acpidEnabled(sb) {
			t.Errorf("acpid was installed but not enabled\n%s", out)
		}
	})

	t.Run("neither present: skipped", func(t *testing.T) {
		sb, out := acpidBoot(t, bootOpts{meminfo: meminfo(big), standIns: map[string]string{"dnf": "", "yum": ""}})
		if !strings.Contains(out, acpidNotInstalled) {
			t.Errorf("the missing acpid was not reported\n%s", out)
		}
		if acpidEnabled(sb) {
			t.Error("acpid was enabled with no package manager to install it")
		}
	})

	t.Run("MemTotal unreadable: the install is attempted", func(t *testing.T) {
		sb, out := acpidBoot(t, bootOpts{standIns: map[string]string{"dnf": pm("dnf", 0), "yum": pm("yum", 0)}})
		if got := sb.read("dnf-calls"); got != "install -y acpid\n" {
			t.Errorf("dnf calls = %q, want one install\n%s", got, out)
		}
		if strings.Contains(out, acpidSkipped) {
			t.Errorf("an unknown memory size was treated as under the floor\n%s", out)
		}
	})
}

func TestAcpidInstallIsSkippedBelowTheMemoryFloor(t *testing.T) {
	cases := []struct {
		name    string
		kb      int
		install bool
	}{
		{"t4g.nano, 0.5 GiB", 446000, false},
		{"just under the floor", 786431, false},
		{"at the floor", 786432, true},
		{"t4g.micro, 1 GiB", 948000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb, out := acpidBoot(t, bootOpts{meminfo: meminfo(tc.kb), standIns: map[string]string{"dnf": pm("dnf", 0), "yum": pm("yum", 0)}})
			ran := sb.read("dnf-calls") + sb.read("yum-calls")
			if tc.install {
				if ran != "install -y acpid\n" || strings.Contains(out, acpidSkipped) {
					t.Errorf("at %d kB the install should run once: calls=%q\n%s", tc.kb, ran, out)
				}
				return
			}
			if ran != "" {
				t.Errorf("a package manager ran at %d kB, under the floor (#1521): %q", tc.kb, ran)
			}
			want := acpidSkipped + strconv.Itoa(tc.kb) + " kB < 768 MiB"
			if !strings.Contains(out, want) || !strings.Contains(out, acpidNotInstalled) {
				t.Errorf("the skip was not reported (want %q)\n%s", want, out)
			}
			if acpidEnabled(sb) {
				t.Error("acpid was enabled though it was never installed")
			}
		})
	}
}

// TestAcpidMemoryFloorIsBelowTheLaunchWarningFloor: the body compares MemTotal
// (what the kernel leaves, about 0.9 GiB on a 1 GiB type) against a literal,
// so it must sit below the 1 GiB instance-memory floor the launch warns at
// (aws.SporedMemoryFloorMiB), or the t4g.micro, where dnf survives (#1515),
// loses acpid; and above 0.5 GiB, where dnf was killed (#1521).
func TestAcpidMemoryFloorIsBelowTheLaunchWarningFloor(t *testing.T) {
	m := regexp.MustCompile(`SPAWN_MEM_KB:-(\d+)\}" -lt (\d+) \]`).FindStringSubmatch(BootstrapBody())
	if m == nil {
		t.Fatal("the body's acpid MemTotal floor was not found")
	}
	kb, _ := strconv.ParseInt(m[2], 10, 64)
	if m[1] != m[2] {
		t.Errorf("an unreadable MemTotal defaults to %s kB, not the %s kB floor, so it is not treated as at the floor", m[1], m[2])
	}
	if kb >= aws.SporedMemoryFloorMiB*1024*7/8 {
		t.Errorf("acpid floor %d kB is within 1/8 of the %d MiB instance floor; a 1 GiB type's MemTotal is under it", kb, aws.SporedMemoryFloorMiB)
	}
	if kb <= 512*1024 {
		t.Errorf("acpid floor %d kB is at or under a 0.5 GiB type, where dnf was OOM-killed (#1521)", kb)
	}
}
