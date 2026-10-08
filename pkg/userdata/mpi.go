package userdata

import (
	"bytes"
	"fmt"
	"text/template" // nosemgrep: go.lang.security.audit.xss.import-text-template.import-text-template
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
	if err := tmpl.Execute(&buf, config); err != nil {
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
cd /tmp
curl -O https://efa-installer.amazonaws.com/aws-efa-installer-latest.tar.gz
tar -xf aws-efa-installer-latest.tar.gz
cd aws-efa-installer
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
# Cluster SSH key: rank 0 mints it; the control plane distributes the public
# half over SSM before any node is released (spawn#684).
mkdir -p /root/.ssh
chmod 700 /root/.ssh
if [ "{{.JobArrayIndex}}" -eq 0 ]; then
  if [ ! -f /root/.ssh/id_rsa ]; then
    ssh-keygen -t rsa -N "" -f /root/.ssh/id_rsa -q
  fi
  # Rank 0 also runs ranks locally, so it must authorize itself.
  cat /root/.ssh/id_rsa.pub >> /root/.ssh/authorized_keys
fi
touch /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
chmod 600 /root/.ssh/id_rsa 2>/dev/null || true
cat >> /root/.ssh/config <<'EOF'
Host *
  StrictHostKeyChecking no
  UserKnownHostsFile=/dev/null
EOF
chmod 600 /root/.ssh/config
# Wait for the control plane to push the peers file, bounded (spawn#684).
SPAWN_PEERS_WAITED=0
while [ ! -f /etc/spawn/job-array-peers.json ]; do
  if [ "$SPAWN_PEERS_WAITED" -ge 600 ]; then
    echo "spawn: no peers file after ${SPAWN_PEERS_WAITED}s; MPI wire-up never completed" >&2
    echo '{"status": "failed", "exit_code": 1, "source": "mpi"}' > /tmp/SPAWN_COMPLETE
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
{{end}}
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
  SPAWN_PEERS_READY_WAITED=0
  while :; do
    NOT_READY=0
    for PEER_IP in $(awk '{print $1}' /tmp/mpi-hostfile); do
      ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
          "$PEER_IP" true 2>/dev/null || NOT_READY=$((NOT_READY + 1))
    done
    [ "$NOT_READY" -eq 0 ] && break
    if [ "$SPAWN_PEERS_READY_WAITED" -ge 600 ]; then
      echo "spawn: $NOT_READY peer(s) never accepted SSH after ${SPAWN_PEERS_READY_WAITED}s; MPI cannot start" >&2
      echo '{"status": "failed", "exit_code": 1, "source": "mpi"}' > /tmp/SPAWN_COMPLETE
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
  {{if .MPICommand}}mpirun --mca orte_base_help_aggregate 0 -np $(({{.JobArraySize}} * SLOTS)) -hostfile /tmp/mpi-hostfile bash /etc/spawn/mpi-command{{end}}
fi
`
