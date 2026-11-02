package userdata

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These are the #1486 gates. Rank 0's pre-mpirun gate (#752) tested only that
// each member accepted a BatchMode SSH login. Rank 0's key reaches a peer over
// SSM, independently of and usually before the peer's own bootstrap writes its
// MPI files, so mpirun could exec /etc/spawn/mpi-rank on a peer that had not
// written it yet: exit 127, a failed job on a healthy cohort (0.9 s late on AL2
// live). The gate must cover everything a rank reads on a member.

// TestMPIRankZeroWaitsForEveryMemberToFinishItsMPISetup runs rank 0's whole
// rendered script against peers that accept SSH at once but whose own MPI
// scripts run only at a later login (sandboxSSH), and a stand-in mpirun that
// runs each rank's program on that member's files (sandboxMpirun). Logins are
// counted, not timed.
func TestMPIRankZeroWaitsForEveryMemberToFinishItsMPISetup(t *testing.T) {
	const late = 3
	for _, tc := range []struct {
		name      string
		stale     string // a marker peerB carries from a parent cohort's image
		container bool   // in amazonlinux:2 (bash 4.2); skips without it
	}{
		{name: "peer setup finishes after SSH accepts"},
		// An image of a member carries its marker and its mpi-rank, and the
		// SSM key can arrive before the script removes them: a marker naming
		// another cohort must not release mpirun.
		{name: "peer carries a parent cohort's marker", stale: "arr-parent"},
		{name: "on AL2", container: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runSandbox(t, sandboxOpts{
				cfg: MPIConfig{Region: "us-east-1", JobArrayID: "arr-1", JobArrayIndex: 0, JobArraySize: 3,
					MPICommand: "echo ran > /tmp/rank-ran"},
				peers: []string{rank0IP, peerA, peerB}, self: rank0IP,
				peerScriptAt: map[string]int{peerB: late}, container: tc.container,
				setup: func(t *testing.T, root string) {
					if tc.stale == "" {
						return
					}
					writeFile(t, root+"/hosts/"+peerB+MPINodeReadyFile, tc.stale+"\n")
					writeFile(t, root+"/hosts/"+peerB+"/etc/spawn/mpi-rank", "exit 0\n")
				},
			})
			if r.rc != 0 {
				t.Fatalf("rank 0 exited %d on a healthy cohort whose last peer finished its MPI setup at login %d", r.rc, late)
			}
			calls, _ := r.read(t, "mpirun.log")
			if n := strings.Count(calls, "\n"); n != 1 {
				t.Fatalf("mpirun was invoked %d times, want 1:\n%s", n, calls)
			}
			// Every rank ran its command on its own member.
			for _, f := range []string{"tmp/rank-ran", "hosts/" + peerA + "/tmp/rank-ran", "hosts/" + peerB + "/tmp/rank-ran"} {
				if _, ok := r.read(t, f); !ok {
					log, _ := r.read(t, "var/log/spawn-command.log")
					t.Errorf("no %s: mpirun started a rank on a member that had not finished its MPI setup (#1486):\n%s", f, log)
				}
			}
			// mpirun started only after the readiness probe that found peerB
			// ready: exactly `late` probes of it, all before mpirun.
			ssh, _ := r.read(t, "ssh.log")
			probes := 0
			for _, line := range strings.Split(ssh, "\n") {
				if strings.HasPrefix(line, peerB+" grep -qxF ") {
					probes++
				}
			}
			if probes != late {
				t.Errorf("rank 0 probed %s %d times, want %d (ready at login %d):\n%s", peerB, probes, late, late, ssh)
			}
			rec, ok := r.record(t, "tmp/SPAWN_COMPLETE")
			if !ok || rec.Status != "completed" || rec.Source != "mpi" {
				t.Errorf("rank 0 record = %+v (exists=%v), want completed from mpi", rec, ok)
			}
		})
	}
}

// heredocWrite opens a file this script writes through a quoted here-doc.
var heredocWrite = regexp.MustCompile(`(?m)^cat >>? (/\S+) <<'(\w+)'$`)

// fileWrite is any redirection of output to an absolute path.
var fileWrite = regexp.MustCompile(`>>?\s*(/[^\s;|&)<>'"]+)`)

// absPath is an absolute path word.
var absPath = regexp.MustCompile(`(?:^|[\s=<>])(/[A-Za-z0-9_./\-]+)`)

// TestMPINodeReadyMarkerFollowsEveryFileARankReads: the marker rank 0 gates on
// must come after every file a rank reads that this script writes, and be the
// member's last step, so the gated set is exactly what the rank path needs.
// The set is discovered, not listed: the program on the mpirun line, then,
// transitively, each file a here-doc body this script writes refers to that
// this script also writes.
func TestMPINodeReadyMarkerFollowsEveryFileARankReads(t *testing.T) {
	for _, cfg := range []MPIConfig{
		{MPICommand: "./sim --steps 10"},
		{MPICommand: "./sim", EFAEnabled: true},
		{MPICommand: "./sim", SkipInstall: true},
	} {
		cfg.Region, cfg.JobArrayID, cfg.JobArrayIndex, cfg.JobArraySize = "us-east-1", "arr-1", 1, 3
		script := renderMPI(t, cfg)

		lastWrite := map[string]int{}
		for _, m := range fileWrite.FindAllStringSubmatchIndex(script, -1) {
			// A device (2>/dev/null) is not a file the script creates.
			if p := script[m[2]:m[3]]; !strings.HasPrefix(p, "/dev/") {
				lastWrite[p] = m[0]
			}
		}
		bodies := map[string]string{}
		for _, m := range heredocWrite.FindAllStringSubmatchIndex(script, -1) {
			body := script[m[1]+1:]
			end := strings.Index(body, "\n"+script[m[4]:m[5]]+"\n")
			if end < 0 {
				t.Fatalf("unterminated here-doc %s", script[m[4]:m[5]])
			}
			bodies[script[m[2]:m[3]]] = body[:end]
		}

		// The mpirun invocation, not the word (comments name it).
		var program string
		for _, line := range strings.Split(script, "\n") {
			l := strings.TrimSpace(line)
			if !strings.HasPrefix(l, "mpirun ") {
				continue
			}
			_, after, ok := strings.Cut(l, "-hostfile ")
			if !ok {
				t.Fatalf("the mpirun line has no -hostfile: %s", l)
			}
			_, after, _ = strings.Cut(after, " ")
			program, _, _ = strings.Cut(after, " 2>&1")
		}
		if program == "" {
			t.Fatal("no mpirun invocation in the rendered script: the discovery has broken")
		}

		reads := map[string]bool{}
		var queue []string
		for _, m := range absPath.FindAllStringSubmatch(program, -1) {
			if _, ok := lastWrite[m[1]]; !ok {
				t.Fatalf("mpirun runs %s, which this script never writes", m[1])
			}
			queue = append(queue, m[1])
		}
		for len(queue) > 0 {
			p := queue[0]
			queue = queue[1:]
			if reads[p] {
				continue
			}
			reads[p] = true
			for _, m := range absPath.FindAllStringSubmatch(bodies[p], -1) {
				if _, written := lastWrite[m[1]]; written {
					queue = append(queue, m[1])
				}
			}
		}
		// The wrapper runs the command file: fewer than two means the
		// discovery has broken, not that a rank reads less.
		if len(reads) < 2 {
			t.Fatalf("discovered %v as what a rank reads: the discovery has broken", reads)
		}

		t.Logf("a rank reads %v", reads)
		marker, ok := lastWrite[MPINodeReadyFile]
		if !ok {
			t.Fatalf("the script never writes %s, so no member can ever be ready (#1486)", MPINodeReadyFile)
		}
		for p := range reads {
			if lastWrite[p] > marker {
				t.Errorf("%s (read by every rank) is written after %s, so rank 0 can start mpirun before a member has it (#1486)", p, MPINodeReadyFile)
			}
		}

		// The marker is the member's last step: what follows it is the rank-0
		// block, which ends the script.
		rest := strings.Split(script[marker:], "\n")[1:]
		var next string
		for _, l := range rest {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				next = l
				break
			}
		}
		if next != `if [ "1" -eq 0 ]; then` {
			t.Errorf("a step follows %s outside the rank-0 block (%q): rank 0 would start mpirun before it ran", MPINodeReadyFile, next)
		}
		if last := strings.TrimSpace(script); !strings.HasSuffix(last, "\nfi") {
			t.Errorf("the rank-0 block does not end the script, so steps after it run after the marker")
		}

		// And it is the marker rank 0 probes for, through the same SSH login.
		probe := regexp.MustCompile(`"\$PEER_IP" 'grep -qxF '\\''arr-1'\\'' ` + regexp.QuoteMeta(MPINodeReadyFile) + `'`)
		if !probe.MatchString(script) {
			t.Errorf("rank 0's readiness probe does not test %s for this cohort's ID", MPINodeReadyFile)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
