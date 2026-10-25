package cmd

import (
	"context"
	"fmt"
	"io"
	"time"
)

// launchCleaner is what a failed launch needs to clean up after itself. A seam:
// *aws.Client in the launch, a counting fake in tests.
type launchCleaner interface {
	Terminate(ctx context.Context, region, instanceID string) error
	DeleteFSxFilesystem(ctx context.Context, filesystemID, region string) error
}

// launchCleanupTimeout bounds the cleanup calls. They run on a context detached
// from the launch's, so a launch that failed by running out of time can still
// clean up.
const launchCleanupTimeout = 2 * time.Minute

// finishLaunch is deferred by launchWithProgress over its named error. When the
// launch fails AFTER RunInstances succeeded (instanceID is set), the instance is
// terminated by default, along with any ephemeral FSx the launch created for it
// (#1298). A success, or a failure before anything launched, passes through
// untouched.
//
// Why by default: spored enforces TTL, idle and cost limits from inside the
// instance. When a launch fails after RunInstances — spored never came up,
// WaitForRunning or the public-IP lookup failed — the caller sees a failed
// launch and moves on, while the instance it was told failed keeps billing with
// nothing inside it enforcing anything. One ran 74 hours past a 4 h TTL and a $5
// cost limit that way, in a region the account's reaper did not scan. Keeping
// the instance used to be the default and terminating it was the opt-in
// (--terminate-on-error); --keep-on-error is now the opt-in, for someone who
// wants to inspect the instance, and it says plainly what is being kept.
func finishLaunch(ctx context.Context, w io.Writer, c launchCleaner, region, instanceID, ephemeralFSx string, keep bool, err error) error {
	if err == nil || instanceID == "" {
		return err
	}
	if keep {
		fmt.Fprintf(w, "\n⚠️  Keeping %s (--keep-on-error). It has NO TTL, idle or cost-limit enforcement until spored is\n"+
			"   confirmed running — spored enforces them from inside the instance. Check it (spawn status %s)\n"+
			"   and terminate it when done (spawn terminate %s).\n", instanceID, instanceID, instanceID)
		if ephemeralFSx != "" {
			fmt.Fprintf(w, "   Its ephemeral FSx %s was kept too.\n", ephemeralFSx)
		}
		return fmt.Errorf("launch failed after instance %s was created; kept it (--keep-on-error): %w", instanceID, err)
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), launchCleanupTimeout)
	defer cancel()
	fmt.Fprintf(w, "\n⚠️  The launch failed after %s was created; terminating it, so nothing is left billing\n"+
		"   without a safety net (--keep-on-error keeps it).\n", instanceID)
	if terr := c.Terminate(cctx, region, instanceID); terr != nil {
		fmt.Fprintf(w, "   terminate failed — TERMINATE IT MANUALLY (spawn terminate %s): %v\n", instanceID, terr)
		if ephemeralFSx != "" {
			fmt.Fprintf(w, "   It also holds ephemeral FSx %s; delete that once the instance is gone (spawn fsx delete %s).\n", ephemeralFSx, ephemeralFSx)
		}
		return fmt.Errorf("launch failed after instance %s was created, and terminating it failed (%v) — it is STILL RUNNING: %w", instanceID, terr, err)
	}
	if ephemeralFSx != "" {
		if derr := c.DeleteFSxFilesystem(cctx, ephemeralFSx, region); derr != nil {
			fmt.Fprintf(w, "   deleting its ephemeral FSx %s failed — DELETE IT MANUALLY (spawn fsx delete %s): %v\n", ephemeralFSx, ephemeralFSx, derr)
		}
	}
	return fmt.Errorf("launch failed after instance %s was created; terminated it: %w", instanceID, err)
}

// resolveKeepOnError decides whether a failed launch keeps its instance, from
// --keep-on-error and the deprecated --terminate-on-error (now the default).
// An explicit --terminate-on-error=false asked to keep the instance, as it
// always has; asking for both is a contradiction.
func resolveKeepOnError(keepSet, keep, terminateSet, terminate bool) (bool, error) {
	if keepSet && keep && terminateSet && terminate {
		return false, fmt.Errorf("--keep-on-error contradicts --terminate-on-error; pass one (terminating is the default)")
	}
	if terminateSet && !terminate {
		return true, nil
	}
	return keepSet && keep, nil
}
