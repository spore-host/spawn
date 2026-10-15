package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// #924: burst never installed spored. Its script waited for
// /usr/local/bin/spored and gave up, so spored ran only on an AMI that already
// had it, and without spored a member has no idle termination, no on-complete,
// no registry registration and no DNS. A member must install spored the way
// `spawn launch` does: download, verify (fail closed), start under systemd.

var _ burstAMIResolver = (*spawnaws.Client)(nil)

func decodedBurstUserData(t *testing.T, userData string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(userData)
	if err != nil {
		t.Fatalf("UserData is not base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("UserData is not gzip: %v", err)
	}
	script, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip user-data: %v", err)
	}
	return string(script)
}

func TestBurstMembersInstallAndVerifySpored(t *testing.T) {
	// Invocations, anchored at line start, so a comment or an echo naming the
	// step cannot satisfy them (CLAUDE.md gate rule 3).
	install := regexp.MustCompile(`(?m)^if ! spore_install_verified_spored "\$BINARY"`)
	abort := regexp.MustCompile(`(?m)^\s+exit 1$`)
	start := regexp.MustCompile(`(?m)^systemctl start spored$`)
	waitOnly := regexp.MustCompile(`(?m)^while \[ ! -f /usr/local/bin/spored \]`)

	for _, m := range launchedBurst(t, burstTestParams()) {
		script := decodedBurstUserData(t, m.UserData)
		if !strings.Contains(script, sporedinstall.VerifiedInstallFunc()) {
			t.Error("burst user-data does not define the shared verified spored install (pkg/sporedinstall)")
		}
		if !install.MatchString(script) {
			t.Error("burst user-data never runs the verified spored install")
		}
		if !abort.MatchString(script) {
			t.Error("burst user-data has no fail-closed exit for a failed spored install")
		}
		if !start.MatchString(script) {
			t.Error("burst user-data never starts spored")
		}
		if waitOnly.MatchString(script) {
			t.Error("burst user-data still only waits for a preinstalled spored (#924)")
		}
		if i, j := strings.Index(script, "spore_install_verified_spored \"$BINARY\""), strings.Index(script, "systemctl start spored"); i < 0 || j < i {
			t.Error("spored must be installed and verified before it is started")
		}
	}
}

func TestBurstUserDataIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := buildBurstUserData()
	if err != nil {
		t.Fatalf("buildBurstUserData: %v", err)
	}
	cmd := exec.Command("bash", "-n") //nolint:gosec // nosemgrep
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("burst user-data is not valid bash: %v\n%s", err, out)
	}
}

// TestBurstMembersRecordTheBootstrapUser: the bootstrap configures
// burstUsername, and spawn connect logs in as the spawn:local-username tag that
// LaunchConfig.Username becomes.
func TestBurstMembersRecordTheBootstrapUser(t *testing.T) {
	for _, m := range launchedBurst(t, burstTestParams()) {
		if m.Username != "ec2-user" {
			t.Errorf("member %d Username = %q, want ec2-user (the user the bootstrap configures)", m.JobArrayIndex, m.Username)
		}
	}
}

// fakeAMIResolver answers burst's AMI lookups and records which were made.
type fakeAMIResolver struct {
	own, rec       string
	ownErr, recErr error
	windows        map[string]bool
	calls          []string
}

func (f *fakeAMIResolver) GetSelfOwnedSpawnAMI(_ context.Context, _, _ string) (string, error) {
	f.calls = append(f.calls, "self")
	return f.own, f.ownErr
}

func (f *fakeAMIResolver) GetRecommendedAMI(_ context.Context, _, _ string) (string, error) {
	f.calls = append(f.calls, "recommended")
	return f.rec, f.recErr
}

func (f *fakeAMIResolver) IsWindowsAMI(_ context.Context, _, ami string) bool {
	f.calls = append(f.calls, "windows?")
	return f.windows[ami]
}

func TestResolveBurstAMI(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		ami     string
		r       *fakeAMIResolver
		want    string
		wantErr bool
	}{
		{"explicit Linux AMI is used as given", "ami-mine", &fakeAMIResolver{own: "ami-own", rec: "ami-al2023"}, "ami-mine", false},
		{"explicit Windows AMI is refused", "ami-win", &fakeAMIResolver{windows: map[string]bool{"ami-win": true}}, "", true},
		{"auto prefers a fitting self-owned spawn AMI", "", &fakeAMIResolver{own: "ami-own", rec: "ami-al2023"}, "ami-own", false},
		{"\"auto\" is auto, as for spawn launch", "auto", &fakeAMIResolver{own: "ami-own", rec: "ami-al2023"}, "ami-own", false},
		{"no fitting self-owned AMI falls back to AL2023", "", &fakeAMIResolver{rec: "ami-al2023"}, "ami-al2023", false},
		{"a failed self-owned lookup falls back to AL2023", "", &fakeAMIResolver{ownErr: errors.New("UnauthorizedOperation"), own: "ami-own", rec: "ami-al2023"}, "ami-al2023", false},
		{"no AMI at all is an error", "", &fakeAMIResolver{recErr: errors.New("ParameterNotFound")}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBurstAMI(ctx, tc.r, "us-east-1", tc.ami, "t3.micro")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("AMI = %q, want %q (lookups %v)", got, tc.want, tc.r.calls)
			}
		})
	}
}
