package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extractParamLoop pulls the spawn:param:* export loop out of the generated
// bootstrap and rewrites its output path to dst, so the real generated shell can
// be executed against a controlled input.
//
// Extracting rather than restating it is the point: a copy of the loop in the
// test would verify the copy. #531 was fixed in the generated script, and
// nothing ever ran it.
func extractParamLoop(t *testing.T, script, dst string) string {
	t.Helper()
	const start = `echo "$PARAM_TAGS" | while IFS=`
	i := strings.Index(script, start)
	if i < 0 {
		t.Fatal("could not find the param export loop in the generated bootstrap; " +
			"this test would otherwise pass vacuously")
	}
	const end = "    done\n"
	j := strings.Index(script[i:], end)
	if j < 0 {
		t.Fatal("param loop has no terminating done")
	}
	loop := script[i : i+j+len(end)]
	return strings.ReplaceAll(loop, "/etc/profile.d/spawn-params.sh", dst)
}

// TestSweepParamsSurviveAsLiterals is spawn#531, executed rather than read.
//
// The generated script built /etc/profile.d/spawn-params.sh by interpolating raw
// EC2 tag values into DOUBLE quotes, so every login shell that sourced it
// re-interpreted them: a value of `$HOME/out` became the instance's real home
// directory instead of the literal string the user wrote, and a value containing
// a double quote broke the generated line's quoting outright — corrupting the
// environment for the whole instance, not just that one variable.
//
// It is now single-quoted with the close/escape/reopen idiom. That fix was in
// place and verified only by reading, which is exactly how the nested-quote bug
// in #680's storage template survived — it was syntactically valid and silently
// wrong. So this runs the real loop and sources its output.
func TestSweepParamsSurviveAsLiterals(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "spawn-params.sh")
	loop := extractParamLoop(t, script, out)

	// One tag per line, key<TAB>value — the shape `aws ec2 describe-tags
	// --output text` returns and the loop parses.
	cases := map[string]string{
		"lr":   "$HOME/out", // must NOT expand
		"msg":  `run "A"`,   // must not break the line's quoting
		"q":    "it's",      // the one character single-quoting must escape
		"cmd":  "$(id -u)",  // must not execute
		"bt":   "`id`",      // must not execute
		"mix":  `a'b"c$d` + "`e`",
		"path": "/a b/c", // a space must survive
	}
	var tags strings.Builder
	for k, v := range cases {
		tags.WriteString("spawn:param:" + k + "\t" + v + "\n")
	}

	runner := filepath.Join(dir, "run.sh")
	body := "#!/bin/bash\nset -u\nPARAM_TAGS=$(cat)\n" + loop
	if err := os.WriteFile(runner, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command("bash", runner)
	cmd.Stdin = strings.NewReader(tags.String())
	// A home directory that would be obvious if $HOME ever expanded.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/SHOULD-NOT-APPEAR"}
	if res, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated param loop failed: %v\n%s", err, res)
	}

	generated, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("loop wrote no params file: %v", err)
	}

	// 1. It must be valid shell. A value containing a quote used to terminate the
	//    line early, which breaks every variable after it too.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	parse := exec.Command("bash", "-n", out)
	if res, err := parse.CombinedOutput(); err != nil {
		t.Fatalf("generated params file is not valid shell: %v\n%s\n--- file ---\n%s",
			err, res, generated)
	}

	// 2. Sourcing it must yield the values byte for byte.
	var probe strings.Builder
	probe.WriteString("set +u\n. " + out + "\n")
	for k := range cases {
		probe.WriteString("printf '%s\\n' \"$PARAM_" + k + "\"\n")
	}
	probeFile := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(probeFile, []byte(probe.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	sourceCmd := exec.Command("bash", probeFile)
	sourceCmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/SHOULD-NOT-APPEAR"}
	got, err := sourceCmd.Output()
	if err != nil {
		t.Fatalf("sourcing the generated params file failed: %v\n--- file ---\n%s", err, generated)
	}

	// Order follows the probe's own iteration, so compare as a set.
	gotLines := map[string]bool{}
	for _, l := range strings.Split(strings.TrimRight(string(got), "\n"), "\n") {
		gotLines[l] = true
	}
	for k, want := range cases {
		if !gotLines[want] {
			t.Errorf("PARAM_%s did not survive as a literal.\n  want: %q\n  got set: %v\n"+
				"--- generated file ---\n%s", k, want, keysOf(gotLines), generated)
		}
	}
	if strings.Contains(string(got), "/SHOULD-NOT-APPEAR") {
		t.Error("$HOME expanded: a param value is being re-interpreted by the login shell, " +
			"which is #531")
	}
}

// TestSweepParamsAreSingleQuoted guards the shape too, so a regression to double
// quotes is named rather than showing up as a confusing value mismatch.
func TestSweepParamsAreSingleQuoted(t *testing.T) {
	script, err := BuildLinuxBootstrap(BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	if !strings.Contains(script, `export PARAM_${param_name}='${escaped_value}'`) {
		t.Error("the PARAM_* export is not single-quoted; double quotes let every login " +
			"shell re-interpret $, backticks and quotes in a tag value (#531)")
	}
	if !strings.Contains(script, `escaped_value=${value//`) {
		t.Error("no single-quote escaping of the value; a value containing ' would " +
			"terminate the quoting and corrupt the rest of the file")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
