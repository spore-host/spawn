package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
)

// amiImager is the seam `spawn ami create` runs through: the SSM call that
// cleans the source instance and the CreateImage call. Production wiring is
// newAMIImager; tests substitute a recording fake.
type amiImager struct {
	runSSM    func(ctx context.Context, region, instanceID, command string) (*aws.SSMRunResult, error)
	createAMI func(ctx context.Context, region string, input aws.CreateAMIInput) (string, error)
}

func newAMIImager(client *aws.Client) amiImager {
	return amiImager{
		runSSM: func(ctx context.Context, region, instanceID, command string) (*aws.SSMRunResult, error) {
			return client.RunShellScript(ctx, region, instanceID, command, 2*time.Minute)
		},
		createAMI: client.CreateAMI,
	}
}

// createFromInstance removes spawn's --command run state from the source
// instance, then creates the image (#1057). If the clean cannot be done or
// does not succeed, the image is not created — baking a stale SPAWN_COMPLETE
// makes --on-complete fire at the first boot of every instance launched from
// it — unless force, in which case it warns and goes ahead.
func (a amiImager) createFromInstance(ctx context.Context, instance *aws.InstanceInfo, input aws.CreateAMIInput, force bool, w io.Writer) (string, error) {
	if instance.Tags["spawn:os"] != "windows" {
		_, _ = fmt.Fprintf(w, "Removing spawn's --command run state from %s before imaging...\n", instance.InstanceID)
	}
	if err := a.cleanRunState(ctx, instance); err != nil {
		if !force {
			return "", fmt.Errorf("not creating the image: %w — its --command run state (completion file, exit code, "+
				"embedded command) would be baked in, and an instance launched from it could fire --on-complete at boot; "+
				"pass --force to image it anyway", err)
		}
		_, _ = fmt.Fprintf(w, "⚠️  %v\n   --force: creating the image anyway; spawn's run-state files may be baked into it.\n", err)
	}
	amiID, err := a.createAMI(ctx, instance.Region, input)
	if err != nil {
		return "", fmt.Errorf("failed to create AMI: %w", err)
	}
	return amiID, nil
}

// cleanRunState runs launcher.ImageStateCleanupScript on the instance over
// SSM. Windows instances run no --command (the Windows bootstrap has no
// workload, #1022), so there is nothing to clean there.
func (a amiImager) cleanRunState(ctx context.Context, instance *aws.InstanceInfo) error {
	if instance.Tags["spawn:os"] == "windows" {
		return nil
	}
	if instance.State != "running" {
		return fmt.Errorf("instance %s is %s, so spawn cannot remove its run-state files over SSM — start it first",
			instance.InstanceID, instance.State)
	}
	script := launcher.ImageStateCleanupScript(instance.InstanceID, instance.Tags["spawn:completion-file"])
	res, err := a.runSSM(ctx, instance.Region, instance.InstanceID, script)
	switch {
	case err != nil:
		return fmt.Errorf("could not clean spawn's run state on %s over SSM: %w", instance.InstanceID, err)
	case res == nil:
		return fmt.Errorf("could not clean spawn's run state on %s over SSM: no result", instance.InstanceID)
	case res.ResponseCode == launcher.ImageCleanCommandRunning:
		return fmt.Errorf("--command is still running on %s; wait for it to finish (spawn status %s --check-complete)",
			instance.InstanceID, instance.InstanceID)
	case res.Status != "Success" || res.ResponseCode != 0 || !strings.Contains(res.Stdout, launcher.ImageCleanOK):
		return fmt.Errorf("cleaning spawn's run state on %s failed (status %s, exit %d): %s",
			instance.InstanceID, res.Status, res.ResponseCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
