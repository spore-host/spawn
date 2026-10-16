package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/dns"
)

var (
	dnsListAll   bool
	dnsDeleteYes bool
)

// dnsCmd represents the dns command group
var dnsCmd = &cobra.Command{
	Use: "dns",
	// Short and Long will be set after i18n initialization
}

// dnsListCmd lists all DNS-enabled instances
var dnsListCmd = &cobra.Command{
	Use:  "list",
	RunE: runDNSList,
	// Short and Long will be set after i18n initialization
}

// dnsRegisterCmd registers DNS for an instance
var dnsRegisterCmd = &cobra.Command{
	Use:  "register <instance-id> <dns-name>",
	Args: cobra.ExactArgs(2),
	RunE: runDNSRegister,
	// Short and Long will be set after i18n initialization
}

// dnsDeleteCmd deletes DNS for an instance
var dnsDeleteCmd = &cobra.Command{
	Use:  "delete <instance-id>",
	Args: cobra.ExactArgs(1),
	RunE: runDNSDelete,
	// Short and Long will be set after i18n initialization
}

func init() {
	rootCmd.AddCommand(dnsCmd)
	dnsCmd.AddCommand(dnsListCmd)
	dnsCmd.AddCommand(dnsRegisterCmd)
	dnsCmd.AddCommand(dnsDeleteCmd)
	dnsDeleteCmd.Flags().BoolVarP(&dnsDeleteYes, "yes", "y", false, "Skip the confirmation prompt")

	// Flags
	dnsCmd.PersistentFlags().StringVar(&dnsDomain, "domain", "", "DNS zone (default: the instance's spawn:dns-domain, else SPAWN_DNS_DOMAIN / ~/.spawn/config.yaml / SSM, else spore.host)")
	dnsCmd.PersistentFlags().StringVar(&dnsAPIEndpoint, "endpoint", "", "Custom DNS updater Function URL (default: the instance's spawn:dns-endpoint, else the same chain, else spore.host's)")
	dnsListCmd.Flags().BoolVarP(&dnsListAll, "all", "a", false, "Show all instances (including those without DNS)")

	// Register completions
	dnsRegisterCmd.ValidArgsFunction = completeInstanceID
	dnsDeleteCmd.ValidArgsFunction = completeInstanceID
}

func runDNSList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Create AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Get account ID
	accountID, err := getAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get account ID: %w", err)
	}

	// List instances
	instances, err := awsClient.ListInstances(ctx, "", "")
	if err != nil {
		return fmt.Errorf("failed to list instances: %w", err)
	}

	// Filter and display
	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "DNS NAME\tFQDN\tINSTANCE ID\tSTATE\tPUBLIC IP")
	_, _ = fmt.Fprintln(w, "--------\t----\t-----------\t-----\t---------")

	count := 0
	for _, instance := range instances {
		dnsName := instance.Tags["spawn:dns-name"]

		if !dnsListAll && dnsName == "" {
			continue
		}

		fqdn := ""
		if dnsName != "" {
			// The zone spored registered it under: its own tag (#891), else spore.host.
			zone := instance.Tags["spawn:dns-domain"]
			if zone == "" {
				zone = dns.DefaultDomain
			}
			fqdn = dns.GetFullDNSName(dnsName, accountID, zone)
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			valueOrDash(dnsName),
			valueOrDash(fqdn),
			instance.InstanceID,
			instance.State,
			valueOrDash(instance.PublicIP))
		count++
	}

	_ = w.Flush()

	if count == 0 {
		if dnsListAll {
			fmt.Fprintln(os.Stderr, "\nNo spawn-managed instances found")
		} else {
			fmt.Fprintln(os.Stderr, "\nNo DNS-enabled instances found")
			fmt.Fprintln(os.Stderr, "Use --all to show all instances")
		}
	} else {
		fmt.Fprintf(os.Stderr, "\n%d instance(s) found\n", count)
	}

	return nil
}

func runDNSRegister(cmd *cobra.Command, args []string) error {
	instanceIdentifier := args[0]
	dnsName := args[1]
	ctx := context.Background()

	// Validate DNS name format
	if !isValidDNSName(dnsName) {
		return fmt.Errorf("invalid DNS name: %s (must be alphanumeric and hyphens only)", dnsName)
	}

	// Create AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Resolve instance
	instance, err := resolveInstance(ctx, awsClient, instanceIdentifier)
	if err != nil {
		return err
	}

	// Check instance state
	if instance.State != "running" {
		return fmt.Errorf("instance must be running (current state: %s)", instance.State)
	}

	// Check for public IP
	if instance.PublicIP == "" {
		return fmt.Errorf("instance has no public IP address")
	}

	fmt.Fprintf(os.Stderr, "Instance: %s\n", instance.InstanceID)
	fmt.Fprintf(os.Stderr, "Region:   %s\n", instance.Region)
	fmt.Fprintf(os.Stderr, "State:    %s\n", instance.State)
	fmt.Fprintf(os.Stderr, "IP:       %s\n", instance.PublicIP)
	fmt.Fprintf(os.Stderr, "\n")

	// Get account ID
	accountID, err := getAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get account ID: %w", err)
	}

	domain, endpoint, err := dnsCommandTarget(ctx, awsClient, instance.Region, instance.Tags)
	if err != nil {
		return err
	}
	fqdn := dns.GetFullDNSName(dnsName, accountID, domain)

	fmt.Fprintf(os.Stderr, "Registering DNS...\n")
	fmt.Fprintf(os.Stderr, "  Short name: %s\n", dnsName)
	fmt.Fprintf(os.Stderr, "  Full FQDN:  %s\n", fqdn)
	fmt.Fprintf(os.Stderr, "\n")

	// Create DNS client
	dnsClient, err := dns.NewClient(ctx, domain, endpoint)
	if err != nil {
		return fmt.Errorf("failed to create DNS client: %w", err)
	}

	// Register DNS
	resp, err := dnsClient.RegisterDNS(ctx, dnsName, instance.PublicIP)
	if err != nil {
		return fmt.Errorf("failed to register DNS: %w", err)
	}

	// Update instance tag
	// Record the target beside the name, so `spawn dns list` / `delete` find the
	// record where it is rather than under spore.host (#891).
	regTags := map[string]string{"spawn:dns-name": dnsName}
	if domain != dns.DefaultDomain {
		regTags["spawn:dns-domain"] = domain
	}
	if endpoint != "" {
		regTags["spawn:dns-endpoint"] = endpoint
	}
	err = awsClient.UpdateInstanceTags(ctx, instance.Region, instance.InstanceID, regTags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: DNS registered but failed to update instance tag: %v\n", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "\n✅ DNS registered successfully!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   DNS:      %s\n", fqdn)
	_, _ = fmt.Fprintf(os.Stdout, "   IP:       %s\n", instance.PublicIP)
	_, _ = fmt.Fprintf(os.Stdout, "   Change:   %s\n", resp.ChangeID)
	_, _ = fmt.Fprintf(os.Stdout, "\n🔌 Connect:\n")
	_, _ = fmt.Fprintf(os.Stdout, "   ssh ec2-user@%s\n", fqdn)
	_, _ = fmt.Fprintf(os.Stdout, "   spawn connect %s\n", instance.InstanceID)

	return nil
}

func runDNSDelete(cmd *cobra.Command, args []string) error {
	instanceIdentifier := args[0]
	ctx := context.Background()

	if !confirmYes(dnsDeleteYes, fmt.Sprintf("Delete the DNS record for %s?", instanceIdentifier)) {
		return newAborted("aborted: no DNS change was made")
	}

	// Create AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Resolve instance
	instance, err := resolveInstance(ctx, awsClient, instanceIdentifier)
	if err != nil {
		return err
	}

	// Check for DNS name
	dnsName := instance.Tags["spawn:dns-name"]
	if dnsName == "" {
		return fmt.Errorf("instance %s has no DNS name configured", instance.InstanceID)
	}

	// Get account ID
	accountID, err := getAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get account ID: %w", err)
	}

	domain, endpoint, err := dnsCommandTarget(ctx, awsClient, instance.Region, instance.Tags)
	if err != nil {
		return err
	}
	fqdn := dns.GetFullDNSName(dnsName, accountID, domain)

	fmt.Fprintf(os.Stderr, "Instance: %s\n", instance.InstanceID)
	fmt.Fprintf(os.Stderr, "DNS:      %s\n", fqdn)
	fmt.Fprintf(os.Stderr, "\n")

	// Create DNS client
	dnsClient, err := dns.NewClient(ctx, domain, endpoint)
	if err != nil {
		return fmt.Errorf("failed to create DNS client: %w", err)
	}

	// Delete DNS
	fmt.Fprintf(os.Stderr, "Deleting DNS record...\n")
	resp, err := dnsClient.DeleteDNS(ctx, dnsName, instance.PublicIP)
	if err != nil {
		return fmt.Errorf("failed to delete DNS: %w", err)
	}

	// Remove instance tag
	err = awsClient.UpdateInstanceTags(ctx, instance.Region, instance.InstanceID, map[string]string{
		"spawn:dns-name": "",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: DNS deleted but failed to remove instance tag: %v\n", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "\n✅ DNS deleted successfully!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   DNS:      %s (removed)\n", fqdn)
	_, _ = fmt.Fprintf(os.Stdout, "   Change:   %s\n", resp.ChangeID)

	return nil
}

// Helper functions

// dnsCommandTarget is where `spawn dns register` / `delete` send their request
// (#891), per field: --domain / --endpoint, then the instance's own
// spawn:dns-domain / spawn:dns-endpoint (where spored registered it), then the
// launch chain — SPAWN_DNS_*, ~/.spawn/config.yaml, SSM — then spore.host's.
// The endpoint is "" for spore.host's compiled-in updater.
func dnsCommandTarget(ctx context.Context, awsClient *aws.Client, region string, tags map[string]string) (string, string, error) {
	domainIn, endpointIn := dnsDomain, dnsAPIEndpoint
	domainSrc, endpointSrc := "--domain", "--endpoint"
	if domainIn == "" && tags["spawn:dns-domain"] != "" {
		domainIn, domainSrc = tags["spawn:dns-domain"], "the instance's spawn:dns-domain tag"
	}
	if endpointIn == "" && tags["spawn:dns-endpoint"] != "" {
		endpointIn, endpointSrc = tags["spawn:dns-endpoint"], "the instance's spawn:dns-endpoint tag"
	}
	t := awsClient.ResolveDNSTarget(ctx, region, domainIn, endpointIn)
	if t.DomainSource == "flag" {
		t.DomainSource = domainSrc
	}
	if t.EndpointSource == "flag" {
		t.EndpointSource = endpointSrc
	}

	domain := dns.DefaultDomain
	if t.Domain != "" {
		if err := dns.ValidateDomain(t.Domain); err != nil {
			return "", "", fmt.Errorf("DNS domain from %s: %w", t.DomainSource, err)
		}
		domain = t.Domain
	}
	endpoint := ""
	if t.Endpoint != "" {
		ep, err := dns.ValidateEndpoint(t.Endpoint)
		if err != nil {
			return "", "", fmt.Errorf("DNS endpoint from %s: %w", t.EndpointSource, err)
		}
		endpoint = ep
	}
	return domain, endpoint, nil
}

func isValidDNSName(name string) bool {
	// DNS name must be alphanumeric and hyphens only
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return len(name) > 0
}

func valueOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func getAccountID(ctx context.Context) (string, error) {
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return "", err
	}

	return awsClient.GetAccountID(ctx)
}
