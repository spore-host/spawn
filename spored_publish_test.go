package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// spored is published to S3 by the release workflow and by nothing else (#876).
//
// The boot installer refuses a spored whose .sha256 does not match or whose .sig
// does not verify against the key compiled into spawn — correctly, it is the only
// thing standing between a writable bucket and root on every new spore. The .sig
// is made by a KMS key reachable only through the release workflow's OIDC role,
// so NO script can produce a valid one. Any other path that writes a spored binary
// therefore leaves a new binary beside the OLD checksum and signature, and every
// instance launched afterwards refuses to install spored and boots with no
// lifecycle at all: no TTL, no idle stop, no cost limit.
//
// scripts/deploy-spored.sh was exactly that path. It uploaded bin/spored-* to every
// regional bucket with no .sha256 and no .sig, and was deleted rather than fixed,
// because it cannot be made safe without the signing key.
//
// So the rule is not "an upload must include its .sha256 and .sig" — a script could
// satisfy that and still publish an invalid signature. It is that the one place
// signing happens is the one place allowed to publish. Naming release.yaml is the
// rule, not an exemption from it.
//
// Discovered, not listed (rule 4): every shell script, Makefile and workflow in the
// repo is scanned, and the scan fails if it finds nothing to scan.
// writesSporedToS3 reports whether one LOGICAL command uploads a spored binary.
//
// Positional, not a regex over the line. The first version matched per physical
// line and passed with the deleted deploy-spored.sh restored, because that script
// wrote `aws s3 cp bin/spored-linux-amd64 \` and put the s3:// destination on the
// continuation line — so the gate was blind to the one script it exists to stop.
// Callers therefore join `\` continuations first, and this reads the arguments by
// position: in `aws s3 cp|mv|sync SRC DST`, it is a write exactly when DST is an
// s3:// URL, which is also what tells an upload from a download (install-spored.sh
// copies FROM S3).
func writesSporedToS3(cmd string) bool {
	f := strings.Fields(cmd)
	for i := 0; i+2 < len(f); i++ {
		if f[i] != "aws" {
			continue
		}
		if f[i+1] == "s3api" && f[i+2] == "put-object" {
			rest := strings.Join(f[i+3:], " ")
			return strings.Contains(rest, "--key") && strings.Contains(rest, "spored")
		}
		if f[i+1] != "s3" || (f[i+2] != "cp" && f[i+2] != "mv" && f[i+2] != "sync") {
			continue
		}
		var pos []string
		for _, a := range f[i+3:] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			pos = append(pos, strings.Trim(a, `"'`))
			if len(pos) == 2 {
				break
			}
		}
		if len(pos) < 2 {
			return false
		}
		src, dst := pos[0], pos[1]
		return strings.HasPrefix(dst, "s3://") &&
			(strings.Contains(dst, "spored") || strings.Contains(src, "spored"))
	}
	return false
}

// logicalLines joins backslash continuations, so a command split across lines is
// examined as the one command it is.
func logicalLines(text string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimRight(line, " \t")
		if strings.HasSuffix(t, "\\") {
			cur.WriteString(strings.TrimSuffix(t, "\\"))
			cur.WriteByte(' ')
			continue
		}
		cur.WriteString(line)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

const sporedPublisher = ".github/workflows/release.yaml"

func TestOnlyTheReleaseWorkflowPublishesSpored(t *testing.T) {
	var scanned int
	var offenders []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "docs-gen":
				return filepath.SkipDir
			}
			return nil
		}
		p := filepath.ToSlash(path)
		isShell := strings.HasSuffix(p, ".sh")
		isMake := info.Name() == "Makefile" || strings.HasSuffix(p, ".mk")
		isWorkflow := strings.HasPrefix(p, ".github/workflows/") &&
			(strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml"))
		if !isShell && !isMake && !isWorkflow {
			return nil
		}
		scanned++
		b, rerr := os.ReadFile(path) //nolint:gosec // the repo's own scripts and workflows
		if rerr != nil {
			return rerr
		}
		for _, line := range logicalLines(string(b)) {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue // a comment explaining the rule must not trip it (rule 3)
			}
			if writesSporedToS3(line) && p != sporedPublisher {
				offenders = append(offenders, p+": "+strings.Join(strings.Fields(line), " "))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 10 {
		t.Fatalf("scanned only %d scripts/workflows — the walk has broken, which would make "+
			"this pass vacuously", scanned)
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("writes a spored binary to S3 outside %s:\n    %s\n"+
			"  Only the release workflow can sign spored (its KMS key sits behind the "+
			"workflow's OIDC role). Anything else leaves a new binary beside the old "+
			".sha256/.sig, and every instance launched afterwards refuses to install "+
			"spored and boots with no TTL enforcement (#876). Ship spored by tagging a release.",
			sporedPublisher, o)
	}

	// And the publisher must still exist and still publish, or the rule above
	// guards an empty room.
	b, err := os.ReadFile(sporedPublisher)
	if err != nil {
		t.Fatalf("read %s: %v", sporedPublisher, err)
	}
	for _, want := range []string{"spored-linux-amd64.sig", "spored-linux-amd64.sha256", "kms"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s no longer mentions %q — the only sanctioned spored publisher may have "+
				"stopped signing or checksumming", sporedPublisher, want)
		}
	}
}

// The classifier itself, on the shapes that matter — including the one the first
// version missed.
func TestWritesSporedToS3(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		// deploy-spored.sh, after continuation-joining: the case that was missed
		{`aws s3 cp bin/spored-linux-amd64 "s3://${bucket}/${PROJECT}/spored-linux-amd64" --region "$region"`, true},
		{`aws s3 cp spored s3://spawn-binaries-us-east-1/spawn/spored-linux-amd64`, true},
		{`aws s3 sync dist/spored/ s3://b/spawn/`, true},
		{`aws s3api put-object --bucket b --key spawn/spored-linux-arm64 --body x`, true},
		// downloads: s3:// is the SOURCE — install-spored.sh does exactly this
		{`aws s3 cp "$S3_PATH_PREFIX" /usr/local/bin/spored --region "$REGION"`, false},
		{`aws s3 cp s3://spawn-binaries-us-east-1/spawn/spored-linux-amd64 /tmp/x`, false},
		// uploads of something else
		{`aws s3 cp function.zip s3://spawn-lambda-artifacts/ttl-reaper.zip`, false},
		{`echo "aws s3 cp is how you would do it"`, false},
	}
	for _, c := range cases {
		if got := writesSporedToS3(c.cmd); got != c.want {
			t.Errorf("writesSporedToS3(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestLogicalLinesJoinsContinuations(t *testing.T) {
	got := logicalLines("aws s3 cp bin/spored \\\n    \"s3://b/spored\" \\\n    --region r\nnext")
	if len(got) != 2 || !strings.Contains(got[0], `"s3://b/spored"`) || got[1] != "next" {
		t.Errorf("logicalLines = %q", got)
	}
}
