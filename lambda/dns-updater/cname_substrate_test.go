package main

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/scttfrdmn/substrate/emulator"
)

// TestCNAMERecord_UpsertDelete exercises the #121 alias-CNAME helpers against
// the Substrate Route53 emulator — the real ChangeResourceRecordSets /
// ListResourceRecordSets path, not a hand mock. Substrate accepts the CNAME
// upsert/delete that real Route53 does, so this covers upsertCNAMERecord and
// deleteCNAMERecord end to end.
func TestCNAMERecord_UpsertDelete(t *testing.T) {
	ts := emulator.StartTestServer(t)
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithBaseEndpoint(ts.URL),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "test")),
	)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}

	// Point the package-level client (used by the helpers) at substrate.
	route53Client = route53.NewFromConfig(cfg)

	// A hosted zone to write into.
	zone, err := route53Client.CreateHostedZone(context.Background(), &route53.CreateHostedZoneInput{
		Name:            aws.String("spore.host"),
		CallerReference: aws.String("dns-updater-test"),
	})
	if err != nil {
		t.Skipf("substrate CreateHostedZone unavailable: %v", err)
	}
	zoneID := aws.ToString(zone.HostedZone.Id)

	alias := "job.mycelium-development.spore.host"
	target := "job.5k0zfnmq.spore.host"

	// Upsert the alias CNAME -> base36 target.
	if _, err := upsertCNAMERecord(context.Background(), alias, target, zoneID); err != nil {
		t.Fatalf("upsertCNAMERecord: %v", err)
	}

	// It should now exist as a CNAME pointing at the target.
	if !cnameExists(t, zoneID, alias, target) {
		t.Fatalf("CNAME %s -> %s not found after upsert", alias, target)
	}

	// Delete it; it should be gone.
	if _, err := deleteCNAMERecord(context.Background(), alias, zoneID); err != nil {
		t.Fatalf("deleteCNAMERecord: %v", err)
	}
	if cnameExists(t, zoneID, alias, target) {
		t.Errorf("CNAME %s still present after delete", alias)
	}

	// Deleting a missing record is a no-op, not an error.
	if _, err := deleteCNAMERecord(context.Background(), alias, zoneID); err != nil {
		t.Errorf("deleteCNAMERecord on a missing record should be a no-op, got: %v", err)
	}
}

func cnameExists(t *testing.T, zoneID, name, target string) bool {
	t.Helper()
	out, err := route53Client.ListResourceRecordSets(context.Background(), &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
	})
	if err != nil {
		t.Fatalf("ListResourceRecordSets: %v", err)
	}
	for _, rs := range out.ResourceRecordSets {
		if rs.Type != r53types.RRTypeCname {
			continue
		}
		if trimDot(aws.ToString(rs.Name)) != name {
			continue
		}
		for _, rr := range rs.ResourceRecords {
			if trimDot(aws.ToString(rr.Value)) == target {
				return true
			}
		}
	}
	return false
}

func trimDot(s string) string {
	if len(s) > 0 && s[len(s)-1] == '.' {
		return s[:len(s)-1]
	}
	return s
}
