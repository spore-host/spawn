package deploy

import (
	"fmt"
	"time"

	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/launcher"
)

// UnitDropIn is the systemd drop-in that turns the bootstrap's spored unit
// into the proxy's: the same unit (restart policy, NoNewPrivileges, journal)
// running `spored egress-proxy` instead of the bare daemon. A drop-in rather
// than a second unit, so exactly one spored runs, and so the published,
// signed bootstrap body that writes the unit is not changed.
const UnitDropIn = "/etc/systemd/system/spored.service.d/egress-proxy.conf"

// UnitScript is the shell that installs [UnitDropIn] for idle and restarts
// spored into it. It runs after the bootstrap has installed and verified
// spored and started the bare daemon, so the proxy's lifetime is enforced
// from the first boot whether or not the restart takes.
func UnitScript(idle time.Duration) string {
	return fmt.Sprintf(`
# spawn egress proxy (#1515): run spored as the proxy.
mkdir -p /etc/systemd/system/spored.service.d
cat > %[1]s <<'EOFDROPIN'
[Service]
ExecStart=
ExecStart=/usr/local/bin/spored egress-proxy --port %[2]d --idle %[3]s
EOFDROPIN
systemctl daemon-reload
systemctl restart spored
`, UnitDropIn, egressproxy.Port, idle.String())
}

// UserData is the proxy's encoded user data: spawn's Linux bootstrap, which
// installs the spored of this release through the same pinned, checksummed
// and signature-verified path every launch uses, then [UnitScript].
func UserData(idle time.Duration) (string, error) {
	script, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
		Username:       "ec2-user",
		CustomUserData: UnitScript(idle),
	})
	if err != nil {
		return "", err
	}
	return launcher.EncodeLinuxUserData(script), nil
}
