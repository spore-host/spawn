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
	BinariesBucket      string // S3 bucket for binaries (defaults to spawn-binaries-{region})
}

// GenerateMPIUserData generates the MPI setup script for inclusion in user-data
func GenerateMPIUserData(config MPIConfig) (string, error) {
	// Register custom template function for shell escaping
	funcMap := template.FuncMap{
		"shellEscape": security.ShellEscape,
	}

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
if [ "{{.JobArrayIndex}}" -eq 0 ]; then
  mkdir -p /root/.ssh
  ssh-keygen -t rsa -N "" -f /root/.ssh/id_rsa -q
  aws s3 cp /root/.ssh/id_rsa.pub s3://{{.BinariesBucket}}/mpi-keys/{{.JobArrayID}}/id_rsa.pub
  cat /root/.ssh/id_rsa.pub >> /root/.ssh/authorized_keys
else
  for i in {1..60}; do
    aws s3 cp s3://{{.BinariesBucket}}/mpi-keys/{{.JobArrayID}}/id_rsa.pub /tmp/key.pub 2>/dev/null && break
    sleep 2
  done
  mkdir -p /root/.ssh
  cat /tmp/key.pub >> /root/.ssh/authorized_keys
fi
chmod 700 /root/.ssh; chmod 600 /root/.ssh/authorized_keys /root/.ssh/id_rsa 2>/dev/null || true
cat >> /root/.ssh/config <<'EOF'
Host *
  StrictHostKeyChecking no
  UserKnownHostsFile=/dev/null
EOF
chmod 600 /root/.ssh/config
while [ ! -f /etc/spawn/job-array-peers.json ]; do sleep 2; done
{{if .MPIProcessesPerNode}}SLOTS={{.MPIProcessesPerNode}}{{else}}SLOTS=$(nproc){{end}}
jq -r ".[] | \"\(.ip) slots=$SLOTS\"" /etc/spawn/job-array-peers.json > /tmp/mpi-hostfile
if [ "{{.JobArrayIndex}}" -eq 0 ]; then
  sleep 10
  {{if .MPICommand}}mpirun --mca orte_base_help_aggregate 0 -np $(({{.JobArraySize}} * SLOTS)) -hostfile /tmp/mpi-hostfile {{.MPICommand | shellEscape}}{{end}}
fi
`
