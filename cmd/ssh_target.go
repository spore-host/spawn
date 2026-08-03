package cmd

import (
	"context"
	"fmt"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/service"
)

// resolveSSHTarget works out how to reach a running instance over SSH: which
// user, which address, and which private key.
//
// It is the same ladder `spawn connect` walks (cmd/connect.go), extracted so a
// second command can't drift from it — in particular the user resolution, which
// prefers the spawn:local-username tag the bootstrap actually created and only
// falls back to ec2-user for instances launched before that tag existed. A
// command that assumed ec2-user would fail to authenticate on every modern
// instance.
//
// The keyless case is real, not theoretical: instances launched headlessly (by
// lagotto, a cohort, or the task path) have no key on the launcher's disk, so the
// key is injected over SSM and the returned path is the matching private key.
//
// userOverride and keyOverride, when non-empty, win — they carry the command's
// --user/--key flags.
func resolveSSHTarget(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo, userOverride, keyOverride string, port int) (service.SSHTarget, error) {
	if instance.State != "running" {
		return service.SSHTarget{}, fmt.Errorf("instance %s is %s, not running", instance.InstanceID, instance.State)
	}
	if instance.Tags["spawn:os"] == "windows" {
		// The whole model here — a POSIX shell wrapper, stdout streaming, SIGTERM
		// on session end — assumes a Unix host. Better to say so than to fail
		// deep inside a remote command that can't work.
		return service.SSHTarget{}, fmt.Errorf("instance %s is Windows; long-lived HTTP services over SSH are Linux-only", instance.InstanceID)
	}
	if instance.PublicIP == "" {
		// SSM would be the fallback for an interactive shell, but a port-forward
		// and a streamed stdout both need a real SSH channel.
		return service.SSHTarget{}, fmt.Errorf("instance %s has no public IP; an SSH-reachable address is required to forward a port to it", instance.InstanceID)
	}

	user := userOverride
	if user == "" {
		user = instance.Tags["spawn:local-username"]
	}
	if user == "" {
		user = "ec2-user" // older instances / no local-username tag
	}

	keyPath := keyOverride
	if keyPath == "" {
		p, err := findSSHKey(instance.KeyName)
		if err != nil {
			// No key on disk (or none at launch — the headless/keyless case). Try
			// to inject spawn's managed public key over SSM and use the matching
			// private key, exactly as connect does.
			injected, ierr := injectSSHKeyViaSSM(ctx, client, instance, user)
			if ierr != nil {
				return service.SSHTarget{}, fmt.Errorf("no SSH key for instance %s (key name %q): %w; key injection over SSM also failed: %v",
					instance.InstanceID, instance.KeyName, err, ierr)
			}
			keyPath = injected
		} else {
			keyPath = p
		}
	}

	if port == 0 {
		port = 22
	}
	return service.SSHTarget{User: user, Host: instance.PublicIP, Port: port, KeyPath: keyPath}, nil
}
