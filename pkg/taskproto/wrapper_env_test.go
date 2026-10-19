package taskproto

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// envGateSpec carries env values chosen to break each wrong way of passing them:
// a space and a quote (word splitting / quoting), a $ and a backtick (expansion),
// and STAGE_RC, a name the wrapper itself reassigns after the export block, so a
// `-e "KEY=$KEY"` reference would hand the container the wrapper's value.
func envGateSpec(container string) *TaskSpec {
	return &TaskSpec{
		TaskID:    "envtask",
		Container: container,
		Command:   []string{"printenv"},
		Lifecycle: Lifecycle{TTL: "1h"},
		Env: map[string]string{
			"PLAIN":      "reached-the-container",
			"WITH_SPACE": "a b  c",
			"TRICKY":     "it's $HOME `id` \"q\"",
			"STAGE_RC":   "the-spec-value",
		},
	}
}

// containerRunCommand extracts the full `sudo docker run ...` command from a
// wrapper, up to the rc capture that follows it.
func containerRunCommand(t *testing.T, w string) string {
	t.Helper()
	start := strings.Index(w, "  sudo docker run ")
	if start < 0 {
		t.Fatalf("no `sudo docker run` in the container wrapper\n---\n%s", w)
	}
	end := strings.Index(w[start:], "\n  rc=$?")
	if end < 0 {
		t.Fatalf("no rc capture after `sudo docker run`\n---\n%s", w[start:])
	}
	return strings.TrimSpace(w[start : start+end])
}

// TestContainerTaskEnvReachesTheContainer is the #1059 gate. TaskSpec.Env was
// exported in the host wrapper shell and never passed to `docker run`, so a
// container task silently ran without it.
//
// It executes the generated `docker run` under bash, after the wrapper's own
// export block and a reassignment of STAGE_RC, with stand-ins on PATH:
//   - `sudo` clears the environment before exec'ing, as real sudo's env_reset
//     does, so a value that only lives in the host shell's env is lost;
//   - `docker` resolves each -e the way docker does (KEY=VALUE as given, a bare
//     KEY from its own environment) and prints the result.
//
// Every key in the spec must arrive with exactly the spec's value.
func TestContainerTaskEnvReachesTheContainer(t *testing.T) {
	skipOnWindows(t, "runs the generated docker run line under bash")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	spec := envGateSpec("docker.io/library/alpine:3")
	w := genWrapper(spec, "b", "us-east-1", false, testRunID)

	var exports []string
	for _, line := range strings.Split(w, "\n") {
		if strings.HasPrefix(line, "export ") {
			exports = append(exports, line)
		}
	}
	frag := strings.Join(exports, "\n") + "\nSTAGE_RC=0\n" + containerRunCommand(t, w) + "\n"

	binDir := t.TempDir()
	writeFakeExe(t, filepath.Join(binDir, "sudo"), "#!/bin/bash\nexec env -i PATH=\"$PATH\" \"$@\"\n")
	writeFakeExe(t, filepath.Join(binDir, "docker"), `#!/bin/bash
while [ $# -gt 0 ]; do
  if [ "$1" = "-e" ]; then
    case "$2" in
      *=*) printf 'ENV %s\n' "$2" ;;
      *)   printf 'ENV %s=%s\n' "$2" "${!2}" ;;
    esac
    shift
  fi
  shift
done
`)
	cmd := exec.Command("bash", "-c", frag) //nolint:gosec // nosemgrep
	cmd.Env = []string{"PATH=" + binDir + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the generated docker run failed: %v\n%s\n---\n%s", err, out, frag)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if kv, ok := strings.CutPrefix(line, "ENV "); ok {
			k, v, _ := strings.Cut(kv, "=")
			got[k] = v
		}
	}
	for k, want := range spec.Env {
		v, ok := got[k]
		if !ok {
			t.Errorf("env key %s never reached the container's docker run (#1059)\nline: %s", k, containerRunCommand(t, w))
			continue
		}
		if v != want {
			t.Errorf("env %s reached the container as %q, want the spec's %q", k, v, want)
		}
	}
}

// The -e flags must sit before the image: anything after it is the container's
// argv, where `-e K=V` would be an argument to the command, not an env var.
func TestContainerTaskEnvFlagsPrecedeTheImage(t *testing.T) {
	spec := envGateSpec("docker.io/library/alpine:3")
	run := containerRunCommand(t, genWrapper(spec, "b", "us-east-1", false, testRunID))
	img := strings.Index(run, shQuote(spec.Container))
	if img < 0 {
		t.Fatalf("image not found in %q", run)
	}
	for k := range spec.Env {
		i := strings.Index(run, "-e "+shQuote(k+"="+spec.Env[k]))
		if i < 0 {
			t.Errorf("no -e for %s in %q", k, run)
		} else if i > img {
			t.Errorf("-e for %s comes after the image, so it is container argv: %q", k, run)
		}
	}
}

// Host mode is unchanged: the command runs in the wrapper's own shell, so the
// export block is what carries the env, one anchored `export KEY=` per key.
func TestHostTaskEnvIsStillExported(t *testing.T) {
	spec := envGateSpec("")
	w := genWrapper(spec, "b", "us-east-1", false, testRunID)
	if strings.Contains(w, "docker run") {
		t.Fatal("host-mode wrapper contains docker run")
	}
	for k, v := range spec.Env {
		re := regexp.MustCompile(`(?m)^export ` + regexp.QuoteMeta(k+"="+shQuote(v)) + `$`)
		if !re.MatchString(w) {
			t.Errorf("host wrapper does not export %s=%s", k, shQuote(v))
		}
	}
}
