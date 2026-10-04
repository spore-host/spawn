package taskproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mountDirSpec(inputDest, outputSrc string) *TaskSpec {
	return &TaskSpec{
		TaskID:    "mountdirs",
		Container: "img",
		Command:   []string{"sh", "-c", "true"},
		Inputs:    []Manifest{{Source: "s3://b/in", Destination: inputDest}},
		Outputs:   []Manifest{{Source: outputSrc, Destination: "s3://b/out/"}},
	}
}

// TestMountDirCreationEscalatesToSudo is spawn#564, confirmed on a real
// t4g.medium rather than reasoned about.
//
// The wrapper emitted a plain `mkdir -p` for every bind-mount directory, which
// runs as the UNPRIVILEGED instance user — so it could not create any directory
// whose parent it does not own. That is every top-level path: /out, /data,
// /work (spawn's own documented example). Observed:
//
//	mkdir: cannot create directory ‘/out’: Permission denied
//	spawn: [...] stage-in done rc=0              <- swallowed
//	drwxr-xr-x 2 0 0 /out/results                <- docker -v created it, as root
//	sh: can't create /out/results/a.txt: Permission denied
//
// i.e. the auto-creation-as-root that loop exists to PREVENT happened anyway,
// surfacing 90s later as a container permission error with nothing pointing at
// the real cause. A code read cannot catch this — it sees the mkdir emitted and
// stops.
func TestMountDirCreationEscalatesToSudo(t *testing.T) {
	w := GenerateWrapper(mountDirSpec("/tmp/in.bin", "/out/results/"), "b", "us-east-1", false, "r")

	if !strings.Contains(w, "sudo mkdir -p '/out/results'") {
		t.Errorf("a mount dir must be creatable with privilege; an unprivileged mkdir cannot "+
			"create a top-level path and docker -v then makes it root-owned\n---\n%s", w)
	}
	if !strings.Contains(w, `sudo chown "$(id -u):$(id -g)" '/out/results'`) {
		t.Error("a sudo-created mount dir is owned by root; without a chown to the invoking " +
			"uid the container still cannot write into it")
	}
	// Unprivileged attempt first: where the parent is already writable, no
	// privilege is needed and the dir is already correctly owned.
	plain := strings.Index(w, "mkdir -p '/out/results' 2>/dev/null")
	sudoAt := strings.Index(w, "sudo mkdir -p '/out/results'")
	if plain < 0 || sudoAt < 0 || plain > sudoAt {
		t.Errorf("the unprivileged mkdir must be tried before escalating (plain@%d sudo@%d)", plain, sudoAt)
	}
}

// TestMountDirFailureFailsStageIn: the original defect was as much the SILENCE as
// the permission error. stage-in reported rc=0 after the mkdir failed, so the
// task ran against an unwritable mount and failed later for an unrelated-looking
// reason. It must fail at stage-in, classified staging_error.
func TestMountDirFailureFailsStageIn(t *testing.T) {
	w := GenerateWrapper(mountDirSpec("/tmp/in.bin", "/out/results/"), "b", "us-east-1", false, "r")

	idx := strings.Index(w, "cannot create mount dir /out/results")
	if idx < 0 {
		t.Fatalf("a failure must say which directory could not be created\n---\n%s", w)
	}
	// STAGE_RC=1 must be set in the same else-branch, before stage-in is reported.
	after := w[idx:]
	stageRC := strings.Index(after, "STAGE_RC=1")
	done := strings.Index(after, `spawn_phase "stage-in done`)
	if stageRC < 0 || done < 0 || stageRC > done {
		t.Errorf("a failed mount dir must set STAGE_RC before stage-in is reported done "+
			"(STAGE_RC@%d done@%d) — otherwise the task runs against an unwritable mount",
			stageRC, done)
	}
}

// TestMountDirNeverChownsAnExistingDirectory is the destructive-action guard, and
// the reason the fix is gated on `[ ! -d ]` rather than chowning unconditionally.
//
// A destination of /tmp/staged.bin yields a MOUNT DIR of /tmp. Chowning that
// would strip the sticky 1777 ownership /tmp needs for every other user on the
// box — a far worse outcome than the bug being fixed, and exactly the kind of
// thing that looks harmless in a diff.
func TestMountDirNeverChownsAnExistingDirectory(t *testing.T) {
	w := GenerateWrapper(mountDirSpec("/tmp/staged.bin", "/out/results/"), "b", "us-east-1", false, "r")

	// /tmp is a mount dir here, so it must appear — but only behind an existence
	// test, so neither the mkdir nor the chown runs for it.
	guard := "if [ ! -d '/tmp' ]; then"
	if !strings.Contains(w, guard) {
		t.Fatalf("expected /tmp's setup to be gated on non-existence\n---\n%s", w)
	}
	block := w[strings.Index(w, guard):]
	block = block[:strings.Index(block, "\nfi\n")+4]
	if !strings.Contains(block, "chown") {
		t.Skip("no chown emitted for /tmp at all — even safer than required")
	}
	// The chown exists but is unreachable for an existing dir. Prove it by running
	// the block with a directory that DOES exist and asserting chown is not called.
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir() // exists
	probe := strings.ReplaceAll(block, "'/tmp'", shQuote(dir))

	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "chown-was-called")
	writeFakeExe(t, filepath.Join(binDir, "sudo"), "#!/bin/bash\ntouch "+marker+"\nexit 0\n")

	script := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\nSTAGE_RC=0\n"+probe+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("sudo (mkdir/chown) ran against an ALREADY-EXISTING directory. For a /tmp " +
			"destination that strips the sticky ownership the whole instance depends on.")
	}
}

// TestMountDirsStillExcludePlacementMounts preserves spawn#570: EFS/FSx/volume
// mount points are bind-mounted but must NEVER be mkdir'd, because creating a
// plain local directory at a mount point that the boot-time storage script is
// about to mount onto means anything written there lands on local disk and
// vanishes when the mount completes. Now that the mkdir escalates to sudo, that
// separation matters more, not less.
func TestMountDirsStillExcludePlacementMounts(t *testing.T) {
	spec := mountDirSpec("/tmp/in.bin", "/out/results/")
	// FSxMountPoint only takes effect alongside an FSxLustreID — containerMountDirs
	// gates on the id, since a mount point with nothing to mount is meaningless.
	spec.Placement.FSxLustreID = "fs-0123456789abcdef0"
	spec.Placement.FSxMountPoint = "/mnt/fsx"
	w := GenerateWrapper(spec, "b", "us-east-1", false, "r")

	if strings.Contains(w, "mkdir -p '/mnt/fsx'") {
		t.Error("a placement mount point must not be mkdir'd (spawn#570) — it would shadow " +
			"the real mount and silently discard writes")
	}
	if !strings.Contains(w, "-v '/mnt/fsx':'/mnt/fsx'") {
		t.Error("a placement mount point must still be bind-mounted into the container")
	}
}
