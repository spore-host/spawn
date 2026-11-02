package userdata

import (
	"bytes"
	"fmt"
	"text/template" // nosemgrep: go.lang.security.audit.xss.import-text-template.import-text-template

	"github.com/spore-host/spawn/pkg/security"
)

// MPIConfig contains configuration for MPI user-data generation
type MPIConfig struct {
	Region              string
	JobArrayID          string
	JobArrayIndex       int
	JobArraySize        int
	MPIProcessesPerNode int
	MPICommand          string
	SkipInstall         bool
	EFAEnabled          bool
	// ReadyGate, if set, is the sentinel file this script writes once the MPI
	// environment is usable — peers resolved and the hostfile built (#664). A
	// --command workload waits on it, because this script is appended AFTER the
	// bootstrap body that launches --command, so without the gate the workload
	// starts before mpirun has a hostfile or any peer to talk to.
	//
	// Signalled BEFORE the rank-0 mpirun, not after: that mpirun is the job and
	// can run for hours, so gating on its completion would make --command wait
	// out the entire run.
	//
	// The path is passed in rather than imported from pkg/launcher so this stays
	// a pure script generator with no dependency on the bootstrap assembler.
	ReadyGate string
	// CompletionFile is the path spored watches for the completion signal
	// (--completion-file, LaunchConfig.CompletionFile). Every completion record
	// this script writes, on rank 0 and over ssh on each peer, goes there, so the
	// two must agree or --on-complete never fires and the cohort bills to TTL
	// (#1170). Empty means /tmp/SPAWN_COMPLETE, the flag default.
	CompletionFile string
}

// MPIClusterKeyComment is the comment on every MPI cluster key spawn mints. An
// authorized_keys line ending with it is spawn-managed and is replaced, never
// accumulated (spawn#1338).
const MPIClusterKeyComment = "spawn-mpi-cohort"

// MPIClusterKeyStripOwnerFile holds the ID of the instance whose root
// authorized_keys has had inherited MPIClusterKeyComment lines stripped
// (spawn#1343). The bootstrap strips them once per instance, when this file
// names another instance or none; the MPI key install writes it too, so a
// later strip cannot remove the cohort's own key.
const MPIClusterKeyStripOwnerFile = "/var/lib/spawn/root-ak-owner"

// MPIClusterKeyOwnerFile holds the ID of the instance that minted rank 0's
// /root/.ssh/id_rsa. A different (or no) ID means the key came with the image.
const MPIClusterKeyOwnerFile = "/root/.ssh/spawn-mpi-key-owner"

// defaultCompletionFile is the completion sentinel used when no
// --completion-file is given. It matches the flag default and spored's own.
const defaultCompletionFile = "/tmp/SPAWN_COMPLETE"

// mpiTemplateData is what the template renders: the config plus the completion
// path pre-quoted in Go, so the template never interpolates a raw path.
type mpiTemplateData struct {
	MPIConfig
	// CompletionPath is the completion file, shell-quoted for this script.
	CompletionPath string
	// RemoteCompletionWrite is the command rank 0 hands to ssh to write the
	// record (read from stdin) on a peer, quoted once for the peer's shell and
	// once more for this script's.
	RemoteCompletionWrite string
	// KeyComment and KeyOwnerFile are MPIClusterKeyComment and
	// MPIClusterKeyOwnerFile, so the script and pkg/mpicohort cannot disagree.
	KeyComment   string
	KeyOwnerFile string
}

func newMPITemplateData(config MPIConfig) mpiTemplateData {
	p := config.CompletionFile
	if p == "" {
		p = defaultCompletionFile
	}
	q := security.ShellQuote(p)
	remote := `mkdir -p "$(dirname -- ` + q + `)" || true; cat > ` + q
	return mpiTemplateData{
		MPIConfig:             config,
		CompletionPath:        q,
		RemoteCompletionWrite: security.ShellQuote(remote),
		KeyComment:            MPIClusterKeyComment,
		KeyOwnerFile:          MPIClusterKeyOwnerFile,
	}
}

// GenerateMPIUserData generates the MPI setup script for inclusion in user-data
func GenerateMPIUserData(config MPIConfig) (string, error) {
	// Register custom template function for shell escaping
	// No funcMap: the only consumer of shellEscape here was --mpi-command, which
	// #660 moved out of the template entirely (it is written to a file through a
	// quoted here-doc and run with bash). Registering an unused escaper invites
	// the next person to reach for it.
	funcMap := template.FuncMap{}

	tmpl, err := template.New("mpi").Funcs(funcMap).Parse(mpiUserDataTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse MPI template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, newMPITemplateData(config)); err != nil {
		return "", fmt.Errorf("failed to execute MPI template: %w", err)
	}

	return buf.String(), nil
}

// Cluster SSH key distribution (spawn#684).
//
// mpirun reaches the other ranks over ssh as root, so every node needs rank 0's
// public key in root's authorized_keys.
//
// This used to go through S3: rank 0 uploaded its pubkey and the others polled
// for it. It could never work. MPIConfig.BinariesBucket was never set by any
// caller, so the command rendered an empty S3 URI and failed parameter
// validation, which aborted cloud-init's scripts-user module — taking the rest
// of the script with it, so mpirun was never installed and enrollment could not
// pass. Had the bucket been named, the spored role grants only s3:GetObject on
// the binaries bucket, so the upload would have been AccessDenied instead.
//
// The public key is now distributed by the control plane over SSM, the same path
// the peers file already uses (pkg/mpicohort/assembler.go). No bucket, no IAM
// grant, no cross-node polling race.
//
// The PRIVATE key is generated on rank 0 and never leaves it: user-data would
// expose it to any local user via IMDS, and SSM would record it in CloudTrail.
//
// One key per instance, not per volume (spawn#1338). The key used to be minted
// only when /root/.ssh/id_rsa was missing, and every authorized_keys write
// appended. An image of a rank-0 instance (`spawn ami create`) carries both, so
// rank 0 of a cohort launched from it REUSED the parent cohort's private key,
// and every member of the new cohort authorized it: whoever held the parent's
// key (its other members, anyone with the image) could ssh in as root. Now:
//
//   - Rank 0 regenerates the key unless MPIClusterKeyOwnerFile names THIS
//     instance, so a reboot that re-runs this script (cloud-init clean, the
//     CreateImage reboot) keeps the cohort's key, and a new instance from an
//     image never inherits it.
//   - Every cluster key carries MPIClusterKeyComment, and each authorized_keys
//     write first removes the lines carrying it, so a node authorizes exactly
//     one cluster key, its own cohort's. Rank 0 also drops the line matching
//     the key it is replacing, which covers an image made before the comment
//     existed. A peer image made by such a release can still hold the old
//     untagged line; nothing on the peer identifies it as spawn's.
//   - pkg/mpicohort reads rank 0's public key only once the owner file names
//     that instance, so the inherited key cannot be distributed in the window
//     before ssh-keygen runs (on an image with MPI installed, enrollment passes
//     at once).
//   - The ssh config block is written once (a marker line), not appended on
//     every run.
//
// The owner file lives in /root/.ssh, beside the key it describes, rather than
// under the #1241 instance-state paths: it is discarded with the key or not at
// all.
//
// Completion of --mpi-command (spawn#894).
//
// --mpi-command must behave like --command on rank 0's node: its output goes to
// /var/log/spawn-command.log (where 'spawn array logs' reads), its exit code to
// /tmp/SPAWN_EXITCODE, and /tmp/SPAWN_COMPLETE is written so spored fires
// --on-complete. Before this, mpirun ran as the bare last line of the script:
// its output reached only cloud-init-output.log and nothing recorded that it
// had finished, so every member, rank 0 included, rode the job to idle or TTL.
// MPI is the most expensive launch type, so that is the costliest place for
// the --on-complete backstop to be missing.
//
// Every member has to learn that the job finished, not only rank 0, because the
// job is one mpirun spanning all of them. This adds no new channel for that. It
// uses the root SSH mesh mpirun itself needs: rank 0's key is authorized on
// every peer before release, and the readiness loop has just proven each peer
// accepts a BatchMode login. Rank 0 writes the same SPAWN_COMPLETE record on
// each peer, so each peer's spored runs its own --on-complete with its own
// grace period and pre-stop hook. Peers are signalled in parallel and BEFORE
// rank 0's own record, so rank 0 cannot start terminating while peers are
// still unsignalled. It skips its own address (hostname -I), which it writes
// locally anyway.
//
// 'set +e' before mpirun is required. This script is appended to the bootstrap,
// which runs under 'set -e'. That is the #566 failure: a failing command
// aborted the script before its exit code was read. PIPESTATUS[0] is mpirun's
// status. $? would be tee's, which reports every failure as a success.
//
// When rank 0 gives up waiting for peer SSH, it signals completion the same
// way. The peers that can be reached stop then, and do not ride to TTL.
//
// Every record goes to the launch's --completion-file (MPIConfig.CompletionFile,
// default /tmp/SPAWN_COMPLETE), not a hardcoded path: spored watches THAT file,
// so with a custom path no member ever fired --on-complete (#1170, the MPI twin
// of #1131). Each write is preceded by a mkdir of the file's directory, since a
// custom path's directory may not exist and the write would fail under set -e.
// The peer write pipes the record to ssh's stdin and runs a fixed command
// quoted for the peer's shell (RemoteCompletionWrite), so the path is quoted at
// both shells and the record is never re-parsed by either.
//
// Rank stragglers (spawn#1479, the MPI twin of #1459). mpirun ran each rank as
// 'bash /etc/spawn/mpi-command', and Open MPI keeps a rank, and so the job,
// alive until the I/O-forwarding channels it gave the rank (a pty for stdout,
// a pipe for stderr, held by mpirun or orted) close. A background job the
// command never waited for ('sleep 600 &', an un-waited 'aws s3 sync ... &')
// inherits them, so mpirun, SPAWN_EXITCODE and every member's --on-complete
// waited out its whole lifetime: 600 s live on 2x c7g.medium, bounded only by
// the TTL. The outer '| tee' was never the holder.
//
// Now mpirun runs /etc/spawn/mpi-rank on every rank. It runs the command with
// stdout and stderr on a per-rank FIFO that a 'cat' copies to the rank's real
// stdout, so the forwarded channels are held only by the wrapper and that cat.
// When the command's own process exits, the cat gets the same bounded drain as
// --command (pkg/launcher commandDrainPolls polls of commandDrainPoll, 10 s;
// TestMPIRankDrainMatchesCommandDrain ties them), so the last lines are
// forwarded; past it the wrapper names whatever still holds the FIFO, kills
// the cat and exits with the command's status. mpirun then sees the rank end.
// A straggler is left holding only the FIFO with no reader (SIGPIPE if it
// writes again) and dies with the instance at --on-complete; the owner's
// decision, as for #1459, is that completion follows the command's own process.
//
//   - The exit code is the command's: a rank that exits n still exits n, and a
//     rank whose shell is killed by signal n exits 128+n, which is also what
//     mpirun reported for the signal before. Open MPI still aborts the job on
//     a failing rank, now after that rank's drain.
//   - stderr is merged into stdout at the rank rather than at rank 0's tee;
//     the log was already one merged stream.
//   - If mkfifo fails the wrapper execs the command directly: output and exit
//     code are kept, and only the straggler bound is lost.
//   - Verified against real mpirun (Open MPI 4.1.2 from the AL2023 package,
//     and 5.0) with 'mpirun -np 2' on one host; mpi_rank_wrapper_test.go runs
//     it when mpirun is installed.
//
// Root (spawn#1481). The bootstrap runs as root, the SSH mesh above is root's,
// and the peers' completion write goes through it, so mpirun runs as root by
// design (the --mpi-command help says so). Open MPI refuses root unless told
// otherwise, and the only mechanism every supported release accepts is the
// --allow-run-as-root flag on the mpirun line. OMPI_ALLOW_RUN_AS_ROOT and
// _CONFIRM in /etc/profile.d/mpi.sh are newer: AL2's Open MPI 4.0.1 ignores
// them, so on an AL2 --ami mpirun refused every --mpi-command before any rank
// started. Verified as root in containers, with this line's --mca and '-np 2':
//
//	Open MPI (image)                    env vars  flag  both
//	4.0.1 (AL2)                         refused   ok    ok
//	4.1.2 (AL2023, Ubuntu 22.04)        ok        ok    ok
//	4.1.6 (Ubuntu 24.04)                ok        ok    ok
//	5.0.5 (Fedora 41; PRRTE prterun)    ok        ok    ok
//
// No release warned about the flag. The variables stay in mpi.sh for a user's
// own mpirun on a newer Open MPI. mpi_root_test.go gates both halves.
//
// Rationale lives HERE rather than in the template because every byte of the
// template ships in each instance's user-data, which EC2 caps at 16 KB.
const mpiUserDataTemplate = `
# MPI Setup
{{if not .SkipInstall}}
# Check if MPI is already installed
if ! command -v mpirun &> /dev/null; then
  echo "Installing OpenMPI..."
  yum install -y openmpi openmpi-devel
else
  echo "MPI already installed, skipping installation"
fi
{{else}}
echo "Skipping MPI installation (--skip-mpi-install specified)"
{{end}}

{{if .EFAEnabled}}
# Install EFA driver
echo "Installing EFA driver..."
# Each cd is checked explicitly rather than left to the bootstrap's set -e
# (#689): this fragment must not run ./efa_installer.sh from the wrong
# directory if it is ever emitted without errexit. Failure stays fatal, as it
# was under set -e.
cd /tmp || exit 1
curl -O https://efa-installer.amazonaws.com/aws-efa-installer-latest.tar.gz
tar -xf aws-efa-installer-latest.tar.gz
cd aws-efa-installer || exit 1
./efa_installer.sh -y -g

# Configure libfabric for EFA
echo "export FI_PROVIDER=efa" >> /etc/profile.d/efa.sh
echo "export FI_EFA_USE_DEVICE_RDMA=1" >> /etc/profile.d/efa.sh
source /etc/profile.d/efa.sh
{{end}}

# Configure MPI environment (always run, even if pre-installed)
cat >> /etc/profile.d/mpi.sh <<'EOF'
export PATH=/usr/lib64/openmpi/bin:$PATH
export LD_LIBRARY_PATH=/usr/lib64/openmpi/lib:$LD_LIBRARY_PATH
export OMPI_MCA_plm_rsh_agent=ssh
export OMPI_ALLOW_RUN_AS_ROOT=1
export OMPI_ALLOW_RUN_AS_ROOT_CONFIRM=1
EOF
source /etc/profile.d/mpi.sh
# Cluster SSH key: rank 0 mints one per instance; the control plane hands out
# the public half over SSM before any node is released (spawn#684, #1338).
mkdir -p /root/.ssh
chmod 700 /root/.ssh
touch /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
spawn_ak_drop() {
  { grep -v "$@" /root/.ssh/authorized_keys > /root/.ssh/.ak || [ $? -eq 1 ]; } && cat /root/.ssh/.ak > /root/.ssh/authorized_keys
  rm -f /root/.ssh/.ak
}
SPAWN_IID=${INSTANCE_ID:-$(ec2-metadata --instance-id | cut -d " " -f 2)}
# shellcheck disable=SC2050  # the rank is fixed at render time: one script per member
if [ "{{.JobArrayIndex}}" -eq 0 ]; then
  if [ -z "$SPAWN_IID" ] || [ "$(cat {{.KeyOwnerFile}} 2>/dev/null)" != "$SPAWN_IID" ]; then
    [ ! -s /root/.ssh/id_rsa.pub ] || spawn_ak_drop -xFf /root/.ssh/id_rsa.pub
    rm -f /root/.ssh/id_rsa /root/.ssh/id_rsa.pub
    ssh-keygen -t rsa -N "" -C {{.KeyComment}} -f /root/.ssh/id_rsa -q
    echo "$SPAWN_IID" > {{.KeyOwnerFile}}
  fi
  spawn_ak_drop ' {{.KeyComment}}$'
  cat /root/.ssh/id_rsa.pub >> /root/.ssh/authorized_keys
fi
grep -qx '# spawn-mpi' /root/.ssh/config 2>/dev/null || cat >> /root/.ssh/config <<'EOF'
# spawn-mpi
Host *
  StrictHostKeyChecking no
  UserKnownHostsFile=/dev/null
  LogLevel ERROR
EOF
chmod 600 /root/.ssh/config
# Wait for the control plane to push the peers file, bounded (spawn#684).
SPAWN_PEERS_WAITED=0
while [ ! -f /etc/spawn/job-array-peers.json ]; do
  if [ "$SPAWN_PEERS_WAITED" -ge 600 ]; then
    echo "spawn: no peers file after ${SPAWN_PEERS_WAITED}s; MPI wire-up never completed" >&2
    mkdir -p "$(dirname -- {{.CompletionPath}})" || true
    echo '{"status": "failed", "exit_code": 1, "source": "mpi"}' > {{.CompletionPath}}
    exit 1
  fi
  sleep 2
  SPAWN_PEERS_WAITED=$((SPAWN_PEERS_WAITED + 2))
done
{{if .MPIProcessesPerNode}}SLOTS={{.MPIProcessesPerNode}}{{else}}SLOTS=$(nproc){{end}}
jq -r ".[] | \"\(.ip) slots=$SLOTS\"" /etc/spawn/job-array-peers.json > /tmp/mpi-hostfile
{{if .ReadyGate}}
# Signal the --command gate here, BEFORE the mpirun below (spawn#664).
mkdir -p "$(dirname {{.ReadyGate}})"
if [ -s /tmp/mpi-hostfile ]; then
  echo ok > {{.ReadyGate}}
else
  echo "failed:no MPI peers resolved (empty /tmp/mpi-hostfile)" > {{.ReadyGate}}
fi
{{end}}
{{if .MPICommand}}
# The --mpi-command, delivered as a FILE rather than interpolated into this
# script (#660). It used to be rendered through shellEscape, i.e. strconv.Quote,
# which wrapped the whole command in one pair of double quotes -- so mpirun got
# a single argv word and tried to exec a binary literally named
# "./gchp --flag x". Only argument-free commands worked. Worse, strconv.Quote is
# Go escaping inside DOUBLE quotes, so $VAR, command substitution and backticks
# still expanded: it preserved neither argv nor safety.
#
# A command line is meant to be parsed by a shell at RUN time, which is what
# running it with bash below does. The quoted here-doc delimiter means nothing
# expands while WRITING the file, so the text lands byte-exact.
mkdir -p /etc/spawn
cat > /etc/spawn/mpi-command <<'EOFMPICMD'
{{.MPICommand}}
EOFMPICMD
chmod 600 /etc/spawn/mpi-command
# mpirun runs this per-rank wrapper, not the command (#1479): see mpi.go.
cat > /etc/spawn/mpi-rank <<'EOFMPIRANK'
#!/bin/bash
F=/tmp/spawn-mpi-rank.$$.fifo
mkfifo -m 600 $F || exec bash /etc/spawn/mpi-command
cat <$F &
C=$! N=0
bash /etc/spawn/mpi-command >$F 2>&1
R=$?
while kill -0 $C 2>/dev/null && [ $N -lt 100 ]; do
  sleep 0.1
  N=$((N + 1))
done
if kill -0 $C 2>/dev/null; then
  S=
  for P in /proc/[0-9]*/fd/*; do
    [ $P -ef $F ] || continue
    P=${P#/proc/} P=${P%%/*}
    [ $P = $C ] || S=$S,$P
  done
  kill $C
  echo "spawn: rank $OMPI_COMM_WORLD_RANK: not waiting for processes still holding --mpi-command's output after 10s; they run on:$(ps -o pid= -o args= -p "${S#,}" 2>/dev/null | tr -s ' \n' ' ')" >&2
fi
rm -f $F
exit $R
EOFMPIRANK
{{end}}
# shellcheck disable=SC2050  # the rank is fixed at render time: one script per member
if [ "{{.JobArrayIndex}}" -eq 0 ]; then
  # Every peer must accept a non-interactive SSH before mpirun, which SSHes to
  # all of them (#752).
  #
  # This was a bare 'sleep 10', with no comment. The peers file waited for
  # above means the CONTROLLER resolved every peer's IP — it says nothing
  # about whether a
  # peer's sshd is up, or whether rank 0's public key has reached that peer's
  # /root/.ssh/authorized_keys, which is the mechanism in
  # pkg/mpicohort/assembler.go. So the pad was a race, and one that WORSENS with
  # cohort size: constant wait, growing number of peers that must all be ready.
  #
  # BatchMode=yes is load-bearing — it tests sshd AND authorized_keys together.
  # A port probe (nc -z … 22) would pass while MPI still failed.
  #
  # Bounded and named-failing like the peers-file wait above, rather than
  # unbounded: a broken peer should produce a SPAWN_COMPLETE record saying so,
  # not an opaque mpirun error about an unreachable host. And a healthy 2-node
  # cohort now starts in ~0s instead of always paying 10.
{{if .MPICommand}}
  # Record rank 0's result and hand it to every member (spawn#894).
  spawn_mpi_done() {
    local s=completed; [ "$1" -eq 0 ] || s=failed
    local rec="{\"status\": \"$s\", \"exit_code\": $1, \"source\": \"mpi\"}" ip pids=""
    echo "$1" > /tmp/SPAWN_EXITCODE
    for ip in $(awk '{print $1}' /tmp/mpi-hostfile); do
      case " $(hostname -I 2>/dev/null) " in *" $ip "*) continue ;; esac
      printf '%s\n' "$rec" | ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=5 "$ip" \
        {{.RemoteCompletionWrite}} ||
        echo "spawn: could not signal completion to $ip" | tee -a /var/log/spawn-command.log &
      pids="$pids $!"
    done
    [ -z "$pids" ] || wait $pids
    mkdir -p "$(dirname -- {{.CompletionPath}})" || true
    echo "$rec" > {{.CompletionPath}}
  }
{{end}}
  SPAWN_PEERS_READY_WAITED=0
  while :; do
    NOT_READY=0
    for PEER_IP in $(awk '{print $1}' /tmp/mpi-hostfile); do
      ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
          "$PEER_IP" true 2>/dev/null || NOT_READY=$((NOT_READY + 1))
    done
    [ "$NOT_READY" -eq 0 ] && break
    if [ "$SPAWN_PEERS_READY_WAITED" -ge 600 ]; then
      echo "spawn: $NOT_READY peer(s) never accepted SSH after ${SPAWN_PEERS_READY_WAITED}s; MPI cannot start" | tee -a /var/log/spawn-command.log >&2
      {{if .MPICommand}}spawn_mpi_done 1{{else}}mkdir -p "$(dirname -- {{.CompletionPath}})" || true; echo '{"status": "failed", "exit_code": 1, "source": "mpi"}' > {{.CompletionPath}}{{end}}
      exit 1
    fi
    sleep 5
    SPAWN_PEERS_READY_WAITED=$((SPAWN_PEERS_READY_WAITED + 5))
  done
  # Both a stdout line and a FILE. The file is what a test can read: stdout
  # from this appended script does not reliably reach
  # /var/log/cloud-init-output.log — a 4-node hardware run looked for the line
  # there and found nothing, while mpirun's own output was present. A marker
  # file removes the ambiguity about whether the poll ran and for how long.
  echo "spawn: all peers accepted SSH after ${SPAWN_PEERS_READY_WAITED}s"
  echo "${SPAWN_PEERS_READY_WAITED}" > /var/log/spawn-mpi-peer-wait-seconds 2>/dev/null || true
{{if .MPICommand}}
  set +e
  mpirun --mca orte_base_help_aggregate 0 --allow-run-as-root -np $(({{.JobArraySize}} * SLOTS)) -hostfile /tmp/mpi-hostfile bash /etc/spawn/mpi-rank 2>&1 | tee -a /var/log/spawn-command.log
  spawn_mpi_done "${PIPESTATUS[0]}"
{{end}}
fi
`
