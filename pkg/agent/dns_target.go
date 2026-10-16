package agent

import (
	"fmt"
	"os"

	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/provider"
)

// resolveDNSTarget returns the zone and updater endpoint this spore registers
// with (#891).
//
// The launch writes them as the spawn:dns-domain and spawn:dns-endpoint tags.
// Before that, spored read the domain only from SPORED_DNS_DOMAIN, which no
// launch set, and the endpoint only from its compiled-in default, so a custom
// DNS deployment could never be reached: every spore registered under
// spore.host. SPORED_DNS_DOMAIN still applies when no tag names a domain.
//
// An empty endpoint means the compiled-in spore.host updater (dns.NewClient's
// default). Both values come from tags, which are not a trusted input, so both
// are validated here. On an invalid value the error is returned with the domain
// still set, for display, and the caller must not register.
func resolveDNSTarget(cfg *provider.Config) (domain, endpoint string, err error) {
	domain = cfg.DNSDomain
	if domain == "" {
		domain = os.Getenv("SPORED_DNS_DOMAIN")
	}
	if domain == "" {
		domain = dns.DefaultDomain
	}
	if verr := dns.ValidateDomain(domain); verr != nil {
		return domain, "", fmt.Errorf("spawn:dns-domain: %w", verr)
	}
	if cfg.DNSEndpoint != "" {
		ep, verr := dns.ValidateEndpoint(cfg.DNSEndpoint)
		if verr != nil {
			return domain, "", fmt.Errorf("spawn:dns-endpoint: %w", verr)
		}
		endpoint = ep
	}
	return domain, endpoint, nil
}
