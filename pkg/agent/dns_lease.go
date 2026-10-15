package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/spore-host/spawn/pkg/dns"
)

// registerDNS performs this instance's DNS registration. It is the ONE place the
// request is built, called both at boot and by refreshDNSLease, so the renewal
// cannot become a subtly different request from the original registration — the
// same record name, the same alias, the same job-array grouping.
func (a *Agent) registerDNS(ctx context.Context) error {
	if a.dnsClient == nil {
		return fmt.Errorf("no DNS client")
	}
	if a.config.JobArrayID != "" && a.config.JobArrayName != "" {
		log.Printf("Registering job array DNS: %s -> %s (array: %s)",
			a.config.DNSName, a.publicIP(), a.config.JobArrayName)
		resp, err := a.dnsClient.RegisterJobArrayDNS(ctx, a.config.DNSName, a.publicIP(),
			a.config.JobArrayID, a.config.JobArrayName)
		if err != nil {
			log.Printf("Warning: Failed to register job array DNS: %v", err)
			return err
		}
		fqdn := dns.GetFullDNSName(a.config.DNSName, a.identity.AccountID, a.dnsDomain)
		log.Printf("✓ Job array DNS registered: %s -> %s (change: %s)", fqdn, a.publicIP(), resp.ChangeID)
		if resp.Message != "" {
			log.Printf("  %s", resp.Message)
		}
		return nil
	}
	log.Printf("Registering DNS: %s -> %s", a.config.DNSName, a.publicIP())
	resp, err := a.dnsClient.RegisterDNS(ctx, a.config.DNSName, a.publicIP())
	if err != nil {
		log.Printf("Warning: Failed to register DNS: %v", err)
		return err
	}
	fqdn := dns.GetFullDNSName(a.config.DNSName, a.identity.AccountID, a.dnsDomain)
	log.Printf("✓ DNS registered: %s -> %s (change: %s)", fqdn, a.publicIP(), resp.ChangeID)
	return nil
}

// dnsRetryAfterFailure is how soon a failed renewal is retried. Much shorter than
// the renewal interval, because a lease only survives LeaseMaxAge without one; but
// not so short that an updater outage turns every spore into a retry storm.
const dnsRetryAfterFailure = 5 * time.Minute

// nextLeaseRefresh returns when the next renewal is due.
//
// Jittered on success so a job array launched together does not renew in one
// burst — Route53 throttles ChangeResourceRecordSets at five per second per
// account. Pure, and the clock is a parameter, so the schedule is tested without
// waiting an hour.
func nextLeaseRefresh(now time.Time, succeeded bool) time.Time {
	if !succeeded {
		return now.Add(dnsRetryAfterFailure)
	}
	// crypto/rand rather than math/rand: not because jitter needs it, but so the
	// codebase carries no math/rand for semgrep to flag and nobody to suppress. On
	// the vanishingly unlikely read failure, fall back to no jitter — a renewal
	// that is merely un-spread is still a renewal.
	var j time.Duration
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(2*dns.LeaseRefreshJitter))); err == nil {
		j = time.Duration(n.Int64()) - dns.LeaseRefreshJitter
	}
	return now.Add(dns.LeaseRefreshInterval + j)
}

// refreshDNSLease renews this instance's DNS registration when it is due.
//
// Called every tick and cheap when not due. Does nothing unless the boot path set
// up a DNS client — i.e. this instance registered DNS in the first place — so a
// spore with no DNS name never starts calling the updater.
//
// spawn:dns-status is re-written only when it CHANGES. The boot path tags on every
// registration, and doing the same hourly would be two EC2 API calls an hour per
// spore to say nothing new.
func (a *Agent) refreshDNSLease(ctx context.Context) {
	if a.dnsClient == nil || a.config.DNSName == "" || a.publicIP() == "" {
		return
	}
	now := time.Now()
	if now.Before(a.nextDNSRefresh) {
		return
	}
	err := a.registerDNS(ctx)
	a.nextDNSRefresh = nextLeaseRefresh(now, err == nil)

	status, detail := "registered", ""
	if err != nil {
		status, detail = "failed", err.Error()
	}
	if status != a.lastDNSStatus {
		a.recordDNSStatus(ctx, status, detail)
		a.lastDNSStatus = status
	}
}
