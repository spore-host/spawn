package main

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/spf13/cobra"

	"github.com/spore-host/spawn/pkg/instancestate"
)

// newDiscardInheritedStateCmd is `spored discard-inherited-state` (#1241). The
// Linux bootstrap body runs it right after installing spored and before it
// starts the daemon, --command, MPI or the user's script, so none of them reads
// state the instance inherited from the image it was launched from. The daemon
// does the same at startup (provider.NewEC2Provider), which covers an instance
// launched from such an image by something other than spawn.
func newDiscardInheritedStateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discard-inherited-state",
		Short: "Remove per-instance state inherited from the image's source instance",
		Long: "When " + instancestate.OwnerFile + " names a different instance than this one, the\n" +
			"instance was launched from an image of that one: remove the per-instance state\n" +
			"(plugins, job-array and pipeline peers, queue, command, flush hook) written before\n" +
			"this boot, and record this instance as the owner (#1241). Nothing written during\n" +
			"this boot is touched. Also removes the MPI cluster-key lines (comment spawn-mpi-cohort)\n" +
			"root's authorized_keys inherited from the image, once per instance (#1343).\n" +
			"Always exits 0: a failure is reported, never fatal to a boot.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			discardInheritedState(cmd.Context(), cmd.ErrOrStderr(), imdsInstanceID, instancestate.ReconcileHost)
		},
	}
}

func imdsInstanceID(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	doc, err := imds.New(imds.Options{}).GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return "", err
	}
	return doc.InstanceID, nil
}

func discardInheritedState(ctx context.Context, w io.Writer, id func(context.Context) (string, error),
	reconcile func(string) ([]string, string, error)) {
	if ctx == nil {
		ctx = context.Background()
	}
	instanceID, err := id(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(w, "spored: cannot read the instance ID, not checking for inherited state: %v\n", err)
		return
	}
	logInherited(w, instanceID, reconcile)
	logClusterKeyStrip(w, instanceID, stripClusterKeys)
}

// stripClusterKeys is instancestate.StripClusterKeysHost on Linux, where
// root's authorized_keys is; a variable so tests can count the calls.
var stripClusterKeys = func(instanceID string) (int, error) {
	if runtime.GOOS != "linux" {
		return 0, nil
	}
	return instancestate.StripClusterKeysHost(instanceID)
}

// logClusterKeyStrip removes the MPI cluster-key lines this instance's root
// authorized_keys inherited from its image (#1343) and says how many, never
// which keys.
func logClusterKeyStrip(w io.Writer, instanceID string, strip func(string) (int, error)) {
	n, err := strip(instanceID)
	if n > 0 {
		_, _ = fmt.Fprintf(w, "spored: removed %d inherited MPI cluster-key line(s) from %s\n", n, instancestate.RootAuthorizedKeys)
	}
	if err != nil {
		_, _ = fmt.Fprintf(w, "spored: removing inherited MPI cluster keys: %v\n", err)
	}
}

// logInherited runs reconcile for instanceID and reports what it did.
func logInherited(w io.Writer, instanceID string, reconcile func(string) ([]string, string, error)) {
	removed, from, err := reconcile(instanceID)
	if from != "" {
		_, _ = fmt.Fprintf(w, "spored: %s was launched from an image of %s; discarded %d inherited state file(s): %v\n",
			instanceID, from, len(removed), removed)
	}
	if err != nil {
		_, _ = fmt.Fprintf(w, "spored: checking for inherited state: %v\n", err)
	}
}
