package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/spore-host/spawn/pkg/accesstoken"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	appURLTTL  time.Duration
	appURLOpen bool
)

// appURLCmd prints a fresh link to a running app's DCV session or web UI. It
// is how a second machine, or the same one after the link expired, gets in:
// each run mints a new token and registers its hash on the instance over SSM
// (pkg/accesstoken), so it needs ssm:SendCommand on the instance, the same
// authority as a shell there.
var appURLCmd = &cobra.Command{
	Use:   "url <instance>",
	Short: "Print a fresh, expiring link to a running app (DCV session or web UI)",
	Long: `Print a fresh link to a running app's DCV session or web UI.

Each run mints a new access token on this machine and registers only its
SHA-256 on the instance over SSM; the token itself is never stored in AWS.
The link expires after --ttl (default 12h). Run it again for a new one, from
any machine whose credentials can run SSM commands on the instance.`,
	Args: cobra.ExactArgs(1),
	RunE: runAppURL,
}

func init() {
	appGroupCmd.AddCommand(appURLCmd)
	appURLCmd.Flags().DurationVar(&appURLTTL, "ttl", accesstoken.DefaultTTL, "How long the link stays valid (at most 168h)")
	appURLCmd.Flags().BoolVar(&appURLOpen, "open", false, "Open the link in the browser")
}

func runAppURL(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := aws.NewClient(ctx)
	if err != nil {
		return err
	}
	inst, err := resolveInstance(ctx, client, args[0])
	if err != nil {
		return err
	}
	link, err := appURLFor(ctx, clientShellRunner(client), inst, appURLTTL)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), link)
	if appURLOpen {
		return openBrowser(link)
	}
	return nil
}

// appURLFor builds the link for inst from its readiness tags, registering a
// token when the session is gated. Pure apart from run.
func appURLFor(ctx context.Context, run shellRunner, inst *aws.InstanceInfo, ttl time.Duration) (string, error) {
	sessionID := inst.Tags["spawn:dcv-session-id"]
	kind := accesstoken.KindWeb
	if sessionID != "" {
		kind = accesstoken.KindDCV
	}
	_, legacy, host, status, auth := extractReadyFromTags(inst.Tags)
	scan := dcvScanResult{legacyToken: legacy, host: host, status: status, auth: auth}
	if inst.State != "running" {
		return "", fmt.Errorf("%s is %s; start it first (spawn connect %s starts a DCV app)", inst.InstanceID, inst.State, inst.InstanceID)
	}
	if !scan.ready() {
		if dcvStatusTerminal(status) {
			return "", fmt.Errorf("%s is not serving its app:%s", inst.InstanceID, dcvFailureMessage(status, inst.InstanceID))
		}
		return "", fmt.Errorf("%s has not reported its app ready yet (spawn:ready-status %q); retry in a minute", inst.InstanceID, status)
	}
	if host == "" {
		host = dcvFallbackHost(inst, "")
	}
	if host == "" {
		return "", fmt.Errorf("%s has no public IP or DNS name", inst.InstanceID)
	}
	token, err := sessionToken(ctx, run, inst.Region, inst.InstanceID, scan, kind, ttl, os.Stderr)
	if err != nil {
		return "", err
	}
	return sessionURL(kind, host, token, sessionID), nil
}
