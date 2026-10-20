package launcher

import (
	"testing"

	"github.com/spore-host/spawn/internal/shellcheckgate"
	"github.com/spore-host/spawn/pkg/plugin"
)

// shellcheck on the rendered spored bootstrap (#689 part C). Every optional
// block is switched on in the "full" config so the whole template renders, and
// the inputs carry spaces and quotes, the values the quoting class (#660) gets
// wrong.
func TestBootstrap_Shellcheck(t *testing.T) {
	cases := map[string]BootstrapConfig{
		"minimal": {Username: "ec2-user"},
		"full": {
			Username:         "ec2-user",
			PublicKey:        []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample user@host"),
			Plugins:          []plugin.Declaration{{Ref: "github:spore-host/spore-plugins/tailscale", Config: map[string]string{"k": "v w"}}},
			CustomUserData:   "#!/bin/bash\necho \"custom user data\"\n",
			StorageScript:    "echo 'mounting storage'\n",
			ContainerScript:  "echo 'running container'\n",
			Command:          `python3 -c "print('it''s a test')" --out "/data/my results"`,
			TaskFlushScript:  "#!/bin/bash\necho flush\n",
			ReadyGates:       []string{"/run/spawn/mpi-ready"},
			ReadyMountPoints: []string{"/efs", "/fsx data"},
			CompletionFile:   "/var/run/my complete.json",
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			script, err := BuildLinuxBootstrap(cfg)
			if err != nil {
				t.Fatalf("BuildLinuxBootstrap: %v", err)
			}
			shellcheckgate.Check(t, "pkg/launcher.BuildLinuxBootstrap", script)
			// linuxBootstrapBody is spliced into the script above and has no
			// other renderer; it is named separately so discovery sees it covered.
			shellcheckgate.Check(t, "pkg/launcher.linuxBootstrapBody", script)
		})
	}
}
