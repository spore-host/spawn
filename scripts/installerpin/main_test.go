package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// copies of the three real files into a temp dir, so set edits copies.
func fixture(t *testing.T) paths {
	t.Helper()
	dir := t.TempDir()
	cp := func(src, name string) string {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, name)
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	return paths{
		app:       cp("../../cmd/app.go", "app.go"),
		pins:      cp("../../pkg/sporedinstall/installer_pins.go", "installer_pins.go"),
		bootstrap: cp("../../pkg/launcher/bootstrap.go", "bootstrap.go"),
		changelog: cp("../../CHANGELOG.md", "CHANGELOG.md"),
	}
}

func (p paths) flags() []string {
	return []string{"--app", p.app, "--pins", p.pins, "--bootstrap", p.bootstrap, "--changelog", p.changelog}
}

var (
	shaA = strings.Repeat("a", 64)
	shaB = strings.Repeat("b", 64)
	fpr1 = strings.Repeat("1", 40)
	fprF = strings.Repeat("F", 40)
)

// The real files carry every constant the workflow moves.
func TestShowReadsTheRealFiles(t *testing.T) {
	var out bytes.Buffer
	real := paths{app: "../../cmd/app.go", pins: "../../pkg/sporedinstall/installer_pins.go", bootstrap: "../../pkg/launcher/bootstrap.go", changelog: "../../CHANGELOG.md"}
	if err := run(append([]string{"show"}, real.flags()...), nil, &out); err != nil {
		t.Fatal(err)
	}
	for _, n := range append(append(append([]string(nil), appConsts...), pinConsts...), bootstrapConsts...) {
		if !strings.Contains(out.String(), n+"=") {
			t.Errorf("show printed no %s", n)
		}
	}
}

func TestSetMovesEachGroupAndOnlyIt(t *testing.T) {
	p := fixture(t)
	dcv := readConst(t, p, "dcvVersion")
	sum := filepath.Join(t.TempDir(), "summary.md")
	var out bytes.Buffer
	args := append([]string{"set", "--summary-out", sum,
		"--msi-version", "2.40.1", "--msi-sha256", shaA,
		"--dcv-version", dcv, "--dcv-sha256-x86_64", shaA, "--dcv-sha256-aarch64", shaB, "--dcv-key-fingerprints", fprF + " " + fpr1,
	}, p.flags()...)
	if err := run(args, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "changed=true") {
		t.Fatalf("set reported %q", out.String())
	}
	for n, want := range map[string]string{
		"windowsAWSCLIMSIVersion": "2.40.1", "windowsAWSCLIMSISHA256": shaA,
		"windowsSSMAgentVersion": "", "windowsSSMAgentSHA256": "",
		"dcvPinVersion": dcv, "dcvTGZSHA256x86_64": shaA, "dcvTGZSHA256aarch64": shaB,
		"dcvKeyFingerprints": fpr1 + " " + fprF, // sorted
		"dcvVersion":         dcv,
	} {
		if got := readConst(t, p, n); got != want {
			t.Errorf("%s = %q, want %q", n, got, want)
		}
	}
	cl, _ := os.ReadFile(p.changelog)
	if !strings.Contains(string(cl), "**Installer pins moved** (#1620)") {
		t.Error("no CHANGELOG entry")
	}
	s, _ := os.ReadFile(sum)
	if !strings.Contains(string(s), "AWSCLIV2-2.40.1.msi") || !strings.Contains(string(s), "Amazon DCV "+dcv) {
		t.Errorf("summary: %s", s)
	}
	// The same values again move nothing and write nothing.
	before, _ := os.ReadFile(p.changelog)
	out.Reset()
	if err := run(args, nil, &out); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p.changelog)
	if !strings.Contains(out.String(), "changed=false") || !bytes.Equal(before, after) {
		t.Errorf("a no-op set reported %q or rewrote the CHANGELOG", out.String())
	}
	// A key change is called out.
	out.Reset()
	args2 := append([]string{"set", "--summary-out", sum,
		"--dcv-version", dcv, "--dcv-sha256-x86_64", shaA, "--dcv-sha256-aarch64", shaB, "--dcv-key-fingerprints", fprF,
	}, p.flags()...)
	if err := run(args2, nil, &out); err != nil {
		t.Fatal(err)
	}
	if s, _ := os.ReadFile(sum); !strings.Contains(string(s), "The DCV signing key changed") {
		t.Errorf("a changed DCV key was not called out: %s", s)
	}
}

func TestSetRefuses(t *testing.T) {
	p := fixture(t)
	dcv := readConst(t, p, "dcvVersion")
	for name, args := range map[string][]string{
		"half a group":        {"--msi-version", "2.40.1"},
		"a bad sha256":        {"--ssm-version", "3.3.1.0", "--ssm-sha256", "ABC"},
		"a v1 CLI":            {"--msi-version", "1.2.3", "--msi-sha256", shaA},
		"another DCV version": {"--dcv-version", "2099.1-1", "--dcv-sha256-x86_64", shaA, "--dcv-sha256-aarch64", shaB, "--dcv-key-fingerprints", fpr1},
		"a short fingerprint": {"--dcv-version", dcv, "--dcv-sha256-x86_64", shaA, "--dcv-sha256-aarch64", shaB, "--dcv-key-fingerprints", "ABCD"},
	} {
		if err := run(append(append([]string{"set"}, args...), p.flags()...), nil, &bytes.Buffer{}); err == nil {
			t.Errorf("set accepted %s", name)
		}
	}
}

func readConst(t *testing.T, p paths, name string) string {
	t.Helper()
	cur, err := readAll(p, false)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := cur[name]
	if !ok {
		t.Fatalf("no %s", name)
	}
	return v
}

// The edited files still compile: set must not break the Go it rewrites.
func TestSetOutputIsGo(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("no gofmt")
	}
	p := fixture(t)
	args := append([]string{"set", "--ssm-version", "3.3.2299.0", "--ssm-sha256", shaA}, p.flags()...)
	if err := run(args, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	if out, err := exec.Command("gofmt", "-l", p.pins, p.app).CombinedOutput(); err != nil || len(out) > 0 { //nolint:gosec // test fixture paths
		t.Errorf("gofmt -l: %v %s", err, out)
	}
}

func TestIssueBody(t *testing.T) {
	sum := filepath.Join(t.TempDir(), "s.md")
	if err := os.WriteFile(sum, []byte("- a line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"issue-body", "--summary", sum, "--compare", "https://github.com/spore-host/spawn/compare/main...bot/x"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "- a line") || !strings.Contains(out.String(), "compare/main...bot/x") {
		t.Errorf("issue body: %s", out.String())
	}
	if err := run([]string{"issue-body", "--summary", sum, "--compare", "http://evil"}, nil, &out); err == nil {
		t.Error("a non-github compare URL was accepted")
	}
	if err := run([]string{"pr-refused"}, strings.NewReader("pull request create failed: GraphQL: something else"), &out); err == nil {
		t.Error("an unrelated gh failure counted as GitHub's refusal")
	}
}
