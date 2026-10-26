package main

import (
	"context"
	"fmt"
	"io"
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
			"this boot is touched. Always exits 0: a failure is reported, never fatal to a boot.",
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
