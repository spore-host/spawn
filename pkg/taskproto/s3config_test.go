package taskproto

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// spawn#1256: stage-in and stage-out run the AWS CLI with a per-run config
// that selects the CRT transfer client and a target bandwidth from the
// instance's NIC class. These run the generated scripts under real bash with a
// fake aws that logs, for every call, the AWS_CONFIG_FILE it saw and that
// file's content.

// s3cfgFakeAWS answers --version with $AWSVER and logs every other call as
// "<AWS_CONFIG_FILE>|<its content, ; for newlines>|<args>" to $AWSLOG. A copy
// to a local path creates it, so stage-in succeeds.
const s3cfgFakeAWS = `#!/bin/bash
if [ "${1:-}" = --version ]; then printf '%s\n' "$AWSVER"; exit 0; fi
c=''; if [ -n "${AWS_CONFIG_FILE:-}" ] && [ -f "$AWS_CONFIG_FILE" ]; then c="$(tr '\n' ';' < "$AWS_CONFIG_FILE")"; fi
printf '%s|%s|%s\n' "${AWS_CONFIG_FILE:-}" "$c" "$*" >> "$AWSLOG"
dst="${@: -1}"
case "$dst" in s3://*) ;; */) mkdir -p "$dst" && : > "$dst/f" ;; *) : > "$dst" ;; esac
exit 0
`

type s3cfgRun struct {
	version, instanceType string
	memKiB                int // 0: no meminfo file
	env                   map[string]string
	homeConfig            bool // the user has ~/.aws/config
}

type awsCall struct{ cfgFile, cfg, args string }

// runS3cfgWrapper runs the host wrapper for a spec with two inputs and one
// output and returns its aws calls (other than --version), its output, and
// the dir its paths live under.
func runS3cfgWrapper(t *testing.T, r s3cfgRun) ([]awsCall, string) {
	t.Helper()
	skipOnWindows(t, "runs the generated Linux wrapper under bash")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	restore := SetLocalRecordDirForTest(t.TempDir())
	t.Cleanup(restore)
	dir := t.TempDir()
	mem := filepath.Join(dir, "meminfo")
	if r.memKiB > 0 {
		if err := os.WriteFile(mem, []byte(fmt.Sprintf("MemTotal:       %d kB\nMemFree: 1 kB\n", r.memKiB)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prev := meminfoPath
	meminfoPath = mem
	t.Cleanup(func() { meminfoPath = prev })

	spec := &TaskSpec{
		TaskID:  "s3cfg",
		Command: []string{"bash", "-c", `echo "UCFG=${AWS_CONFIG_FILE:-unset}"`},
		Env:     r.env,
		Inputs: []Manifest{
			{Source: "s3://in/a.bin", Destination: filepath.Join(dir, "a.bin")},
			{Source: "s3://in/tree/", Destination: filepath.Join(dir, "tree") + "/"},
		},
		Outputs:   []Manifest{{Source: filepath.Join(dir, "out.txt"), Destination: "s3://out/out.txt"}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeExe(t, filepath.Join(bin, "aws"), s3cfgFakeAWS)
	writeFakeExe(t, filepath.Join(bin, "curl"), fakeIMDSCurl(r.instanceType, "on-demand"))
	writeFakeSudo(t, bin)
	home := filepath.Join(dir, "home")
	if r.homeConfig {
		if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte("[default]\nregion = us-west-2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(script, []byte(w), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "aws.log")
	cmd := exec.Command("bash", script) //nolint:gosec // nosemgrep: the test's own generated script
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + home, "AWSLOG=" + log,
		"AWSVER=" + r.version, "SPAWN_SPORED_BIN=" + filepath.Join(dir, "no-spored"),
	}
	out, _ := cmd.CombinedOutput()
	var calls []awsCall
	for _, l := range readLines(log) {
		f := strings.SplitN(l, "|", 3)
		if len(f) != 3 {
			t.Fatalf("unparsable aws log line %q", l)
		}
		calls = append(calls, awsCall{f[0], f[1], f[2]})
	}
	if len(calls) == 0 {
		t.Fatalf("the wrapper made no aws calls; the harness is broken\noutput:\n%s", out)
	}
	return calls, string(out)
}

// stagingCalls are the stage-in and stage-out copies among calls, which must
// all be found or the discovery is broken.
func stagingCalls(t *testing.T, calls []awsCall) []awsCall {
	t.Helper()
	var got []awsCall
	var in, out int
	for _, c := range calls {
		switch {
		case strings.Contains(c.args, "s3://in/"):
			in++
		case strings.Contains(c.args, "s3://out/"):
			out++
		default:
			continue
		}
		got = append(got, c)
	}
	if in != 2 || out != 1 {
		t.Fatalf("found %d stage-in and %d stage-out copies, want 2 and 1: %+v", in, out, calls)
	}
	return got
}

func TestWrapperStagesThroughThePerRunCRTConfig(t *testing.T) {
	calls, out := runS3cfgWrapper(t, s3cfgRun{
		version: "aws-cli/2.31.4 Python/3.13.7 Linux/6.1 exe/aarch64.amzn.2023", instanceType: "r8g.16xlarge", memKiB: 512 * 1024 * 1024,
	})
	staged := stagingCalls(t, calls)
	// Every S3 copy, not only staging: the record and log go through spawn_aws too.
	for _, c := range calls {
		if c.cfgFile == "" {
			t.Errorf("aws call without the per-run AWS_CONFIG_FILE: %s", c.args)
			continue
		}
		for _, want := range []string{"[default]", "s3 =", "preferred_transfer_client = crt", "target_bandwidth = 30Gb/s", "multipart_chunksize = 16MB"} {
			if !strings.Contains(c.cfg, want) {
				t.Errorf("config for %q lacks %q: %s", c.args, want, c.cfg)
			}
		}
		if strings.Contains(c.cfg, "max_concurrent_requests") {
			t.Errorf("crt config caps concurrency (CLI 2.36.48+ applies it to crt): %s", c.cfg)
		}
	}
	if _, err := os.Stat(staged[0].cfgFile); !os.IsNotExist(err) {
		t.Errorf("per-run config %s left behind after the run (err=%v)", staged[0].cfgFile, err)
	}
	// The user's command must not inherit it: its own `aws --profile x` or
	// `aws configure set` would hit a file that has no profiles and is deleted.
	if !strings.Contains(out, "UCFG=unset") {
		t.Errorf("user command saw AWS_CONFIG_FILE; want it unset\noutput:\n%s", out)
	}
}

// A config the user supplied wins, untouched: AWS_CONFIG_FILE or a profile
// from the spec's env, or an existing ~/.aws/config.
func TestWrapperRespectsTheUsersAWSConfig(t *testing.T) {
	crt := s3cfgRun{version: "aws-cli/2.31.4 Python/3.13.7", instanceType: "r8g.16xlarge", memKiB: 512 * 1024 * 1024}
	for name, tc := range map[string]struct {
		mod     func(*s3cfgRun)
		wantCfg string
	}{
		"spec AWS_CONFIG_FILE": {func(r *s3cfgRun) { r.env = map[string]string{"AWS_CONFIG_FILE": "/opt/team/aws.cfg"} }, "/opt/team/aws.cfg"},
		"spec AWS_PROFILE":     {func(r *s3cfgRun) { r.env = map[string]string{"AWS_PROFILE": "research"} }, ""},
		"~/.aws/config":        {func(r *s3cfgRun) { r.homeConfig = true }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := crt
			tc.mod(&r)
			calls, out := runS3cfgWrapper(t, r)
			for _, c := range stagingCalls(t, calls) {
				if c.cfgFile != tc.wantCfg {
					t.Errorf("%s ran with AWS_CONFIG_FILE=%q, want %q", c.args, c.cfgFile, tc.wantCfg)
				}
			}
			if !strings.Contains(out, "S3 transfer tuning skipped") {
				t.Errorf("no note that the tuning was skipped\noutput:\n%s", out)
			}
		})
	}
}

// The client choice by CLI version and memory. `classic` is not a valid
// preferred_transfer_client before 2.14.0 (it was named `default`), and an
// invalid choice fails every copy, so older or unreadable versions get no
// client line at all, only the classic tuning.
func TestWrapperS3ClientByCLIVersionAndMemory(t *testing.T) {
	big, small := 64*1024*1024, 4*1024*1024
	for _, tc := range []struct {
		version    string
		memKiB     int
		wantClient string // "" means no preferred_transfer_client line
	}{
		{"aws-cli/2.14.0 Python/3.11.6", big, "crt"},
		{"aws-cli/2.38.1 Python/3.13.7", big, "crt"},
		{"aws-cli/2.14.0 Python/3.11.6", small, "classic"},
		{"aws-cli/2.31.4 Python/3.13.7", 0, "classic"}, // meminfo unreadable
		{"aws-cli/2.13.38 Python/3.11.6", big, ""},
		{"aws-cli/2.9.19 Python/3.9.11", big, ""},
		{"aws-cli/1.29.0 Python/3.9.16", big, ""},
		{"", big, ""},
		{"bash: aws: command not found", big, ""},
	} {
		t.Run(tc.version+"/"+strconv.Itoa(tc.memKiB), func(t *testing.T) {
			calls, _ := runS3cfgWrapper(t, s3cfgRun{version: tc.version, instanceType: "r8g.4xlarge", memKiB: tc.memKiB})
			for _, c := range stagingCalls(t, calls) {
				if c.cfgFile == "" {
					t.Fatalf("%s ran without the per-run config", c.args)
				}
				hasLine := strings.Contains(c.cfg, "preferred_transfer_client")
				switch {
				case tc.wantClient == "" && hasLine:
					t.Errorf("old/unknown CLI got a client line: %s", c.cfg)
				case tc.wantClient != "" && !strings.Contains(c.cfg, "preferred_transfer_client = "+tc.wantClient+";"):
					t.Errorf("want client %s: %s", tc.wantClient, c.cfg)
				}
				if tc.wantClient != "crt" && !strings.Contains(c.cfg, "max_concurrent_requests = 32") {
					t.Errorf("classic path lacks the raised concurrency: %s", c.cfg)
				}
				if tc.wantClient != "crt" && strings.Contains(c.cfg, "target_bandwidth") {
					t.Errorf("target_bandwidth is crt-only: %s", c.cfg)
				}
			}
		})
	}
}

// nicPeaks are documented peak network bandwidths, Gb/s, from the EC2
// instance-type specification tables
// (https://docs.aws.amazon.com/ec2/latest/instancetypes/mo.html, read
// 2026-10-26): burst for burstable sizes, else the sustained figure.
var nicPeaks = map[string]float64{
	"r8g.medium": 12.5, "r8g.4xlarge": 15, "r8g.8xlarge": 15, "r8g.12xlarge": 22.5,
	"r8g.16xlarge": 30, "r8g.24xlarge": 40, "r8g.48xlarge": 50,
	"r8g.metal-24xl": 40, "r8g.metal-48xl": 50,
	"r7i.large": 12.5, "r7i.8xlarge": 12.5, "r7i.12xlarge": 18.75, "r7i.16xlarge": 25,
	"r7i.24xlarge": 37.5, "r7i.48xlarge": 50,
	"r6in.large": 25, "r6in.4xlarge": 50, "r6in.8xlarge": 50, "r6in.12xlarge": 75,
	"r6in.16xlarge": 100, "r6in.24xlarge": 150, "r6in.32xlarge": 200, "r6in.metal": 200,
}

// nicTarget runs the emitted NIC-class shell for instanceType and returns G.
func nicTarget(t *testing.T, instanceType string) string {
	t.Helper()
	var b strings.Builder
	writeNICTarget(func(f string, a ...interface{}) { fmt.Fprintf(&b, f, a...) })
	// nosemgrep
	out, err := exec.Command("bash", "-c", "set -u; IT="+shQuote(instanceType)+"\n"+b.String()+"printf '%s' \"$G\"").CombinedOutput()
	if err != nil {
		t.Fatalf("NIC shell failed for %q: %v\n%s", instanceType, err, out)
	}
	return string(out)
}

// The target is never below what the NIC can do (that would cap the copy) and
// not absurdly above it (which only reserves buffer memory). Every row of
// nicSizeClasses, in both columns, must be covered by a documented type, so a
// row cannot be added from memory.
func TestNICClassTargetsMatchDocumentedPeaks(t *testing.T) {
	skipOnWindows(t, "runs the emitted shell under bash")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	covered := map[string]bool{}
	for it, peak := range nicPeaks {
		g, err := strconv.Atoi(nicTarget(t, it))
		if err != nil {
			t.Errorf("%s: no numeric target (%v)", it, err)
			continue
		}
		if float64(g) < peak || float64(g) > 4*peak {
			t.Errorf("%s: target %d Gb/s, documented peak %.4g; want within [peak, 4*peak]", it, g, peak)
		}
		_, size, _ := strings.Cut(it, ".")
		for i, c := range nicSizeClasses {
			for _, s := range c.Sizes {
				if ok, _ := filepath.Match(s, size); ok {
					covered[fmt.Sprintf("%d/%v", i, strings.Contains(it, "n."))] = true
				}
			}
		}
	}
	for i := range nicSizeClasses {
		for _, netOpt := range []bool{false, true} {
			if !covered[fmt.Sprintf("%d/%v", i, netOpt)] {
				t.Errorf("nicSizeClasses row %d (%v, netOpt=%v) has no documented type in nicPeaks", i, nicSizeClasses[i].Sizes, netOpt)
			}
		}
	}
	// Left to the CLI: the types it detects itself, no type, an unknown size.
	for _, it := range append([]string{"", "r8g", "u7i-12tb.224xlarge"}, crtOptimizedTypes...) {
		if g := nicTarget(t, it); g != "" {
			t.Errorf("%q: target %q, want none (the CLI chooses)", it, g)
		}
	}
}

// The flush hook writes its own config, root's, with the classic client: the
// CRT client ignores --cli-connect-timeout/--cli-read-timeout, which keep the
// hook inside its 45 s deadline. It never reads the wrapper's file.
func TestFlushHookUsesItsOwnClassicConfig(t *testing.T) {
	skipOnWindows(t, "runs the Linux flush hook under bash")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	restore := SetLocalRecordDirForTest(t.TempDir())
	t.Cleanup(restore)
	// A box big enough for crt, so only the hook's own choice keeps it classic.
	mem := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(mem, []byte("MemTotal: 536870912 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := meminfoPath
	meminfoPath = mem
	t.Cleanup(func() { meminfoPath = prev })
	script := genFlush(flushSpec(), "results-bucket", "us-east-1", testRunID)
	for name, tc := range map[string]struct {
		env     []string
		wantCfg string // "" = the hook's own file
	}{
		"own config":           {nil, ""},
		"user AWS_CONFIG_FILE": {[]string{"AWS_CONFIG_FILE=/root/team.cfg"}, "/root/team.cfg"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFakeExe(t, filepath.Join(bin, "aws"), s3cfgFakeAWS)
			path := filepath.Join(dir, "task-flush.sh")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "aws.log")
			cmd := exec.Command("bash", path, string(ExitTTLExpired)) //nolint:gosec // nosemgrep: the test's own generated script
			cmd.Env = append([]string{
				"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + filepath.Join(dir, "home"),
				"AWSLOG=" + log, "AWSVER=aws-cli/2.38.1 Python/3.13.7",
			}, tc.env...)
			out, _ := cmd.CombinedOutput()
			var n int
			for _, l := range readLines(log) {
				f := strings.SplitN(l, "|", 3)
				if len(f) != 3 || !strings.Contains(f[2], " s3 cp ") {
					continue
				}
				n++
				switch {
				case tc.wantCfg != "" && f[0] != tc.wantCfg:
					t.Errorf("%s: AWS_CONFIG_FILE=%q, want the user's %q", f[2], f[0], tc.wantCfg)
				case tc.wantCfg == "" && (!strings.Contains(f[1], "preferred_transfer_client = classic;") || strings.Contains(f[1], "crt")):
					t.Errorf("%s: config %q, want the classic client pinned", f[2], f[1])
				}
				if tc.wantCfg == "" {
					if _, err := os.Stat(f[0]); !os.IsNotExist(err) {
						t.Errorf("hook config %s left behind (err=%v)", f[0], err)
					}
				}
			}
			if n == 0 {
				t.Fatalf("flush hook made no s3 cp call; harness broken\noutput:\n%s", out)
			}
		})
	}
}
