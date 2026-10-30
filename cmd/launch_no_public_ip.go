package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spore-host/spawn/pkg/aws"
)

// --no-public-ip (#1433): a launch with private IPs only. aws.Client.Launch
// does the work (no public IP requested, no Elastic IP) and refuses a subnet
// that cannot serve a private instance; the CLI refuses the flags that need a
// public IP before any AWS call, and runs the subnet check before anything is
// created.

// validateNoPublicIPFlags refuses what --no-public-ip cannot be combined with.
// Pure: flags only.
//   - --allow-missing-endpoints without --no-public-ip: nothing to allow.
//   - --dns, and the custom DNS target flags: spored registers the instance's
//     public IP, and there is none. (The record --name implies is skipped
//     instead, with a notice: every named launch would otherwise need --no-dns.)
//   - --wait-for-ssh: it waits for SSH on the public IP.
func validateNoPublicIPFlags() error {
	if !noPublicIP {
		if allowMissingEndpoints {
			return errors.New("--allow-missing-endpoints applies only with --no-public-ip")
		}
		return nil
	}
	switch {
	case dnsName != "" && dnsName != name && !noDNS:
		return fmt.Errorf("--dns %s cannot be used with --no-public-ip: spored registers the instance's public IP, and it has none. "+
			"Drop --dns (`spawn connect` reaches the instance over SSM by name or ID)", dnsName)
	case dnsDomain != "" || dnsAPIEndpoint != "" || dnsFunctionARN != "":
		return errors.New("--dns-domain, --dns-api-endpoint and --dns-function-arn cannot be used with --no-public-ip: " +
			"spored registers the instance's public IP, and it has none")
	case waitForSSH:
		return errors.New("--wait-for-ssh cannot be used with --no-public-ip: it waits for SSH on the public IP, and there is none. " +
			"spored is verified either way, and `spawn connect` uses SSM")
	}
	return nil
}

// validateNoPublicIPPath refuses --no-public-ip (and --allow-missing-endpoints)
// on the two launch paths that do not build their config from the launch
// flags, which would drop it and launch with a public IP: the batch queue
// (batchQueueLaunchConfig, which also registers DNS) and the wizard.
func validateNoPublicIPPath(batchQueue, wizard bool) error {
	if !noPublicIP && !allowMissingEndpoints {
		return nil
	}
	switch {
	case batchQueue:
		return errors.New("--no-public-ip is not supported with --batch-queue or --queue-template: the queue instance's config " +
			"is built from the queue file, not the launch flags, and registers DNS, which needs a public IP")
	case wizard:
		return errors.New("--no-public-ip is not supported by the interactive wizard, which does not read the launch flags; " +
			"pass --instance-type (and --subnet-id or --az) to launch with flags")
	}
	return nil
}

// noPublicIPInternetNeeds are what this launch's bootstrap fetches from outside
// AWS beyond what the config itself names (aws.CheckPrivateNetwork reads
// those): only a NAT route reaches them.
func noPublicIPInternetNeeds() []string {
	if mpiEnabled && efaEnabled {
		return []string{"the MPI bootstrap's EFA installer download (efa-installer.amazonaws.com)"}
	}
	return nil
}

// preflightNoPublicIPFor runs aws.Client.PreflightNoPublicIP. A package-level
// seam so tests need no AWS.
var preflightNoPublicIPFor = func(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, internet []string, out io.Writer) (aws.PrivateCheck, error) {
	return awsClient.PreflightNoPublicIP(ctx, config, internet, out)
}

// preflightNoPublicIP settles a --no-public-ip launch's subnet and checks it,
// before anything is created: with --az alone the subnet is picked and pinned,
// so a job array or MPI cohort launches every member in the checked subnet
// rather than falling back across AZs into subnets nobody checked.
func preflightNoPublicIP(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, out io.Writer) error {
	if !config.NoPublicIP {
		return nil
	}
	check, err := preflightNoPublicIPFor(ctx, awsClient, config, noPublicIPInternetNeeds(), out)
	if err != nil {
		return err
	}
	for _, m := range check.Met {
		fmt.Fprintf(out, "✓ --no-public-ip: %s\n", m)
	}
	return nil
}

// preflightSweepNoPublicIP is preflightNoPublicIP for every foreground sweep
// member: each lands in its own region and subnet, so each is checked
// (through the client's cache, once per region and subnet). indexes are the
// members' parameter-set numbers, for the error (nil: their positions).
func preflightSweepNoPublicIP(ctx context.Context, awsClient *aws.Client, cfgs []*aws.LaunchConfig, indexes []int, out io.Writer) error {
	for i, cfg := range cfgs {
		if !cfg.NoPublicIP {
			continue
		}
		idx := i
		if indexes != nil {
			idx = indexes[i]
		}
		if _, err := preflightNoPublicIPFor(ctx, awsClient, cfg, nil, out); err != nil {
			return fmt.Errorf("parameter set %d: %w", idx, err)
		}
	}
	return nil
}
