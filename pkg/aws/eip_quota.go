package aws

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
)

// ElasticIPQuotaCode is the Service Quotas code of "EC2-VPC Elastic IPs", the
// per-region cap on allocated Elastic IPs (default 5). A multi-interface EFA
// launch allocates one address per instance (#661), so a fleet of them runs into
// it: the first field use hit it on its 9th instance.
const ElasticIPQuotaCode = "L-0263D0A3"

// ElasticIPQuota is how many Elastic IPs a region has allocated and how many it
// may.
type ElasticIPQuota struct {
	InUse int
	Limit int
	// LimitSource is "applied" (the account's own value) or "default" (no
	// applied value exists, so the AWS default holds).
	LimitSource string
}

type elasticIPCountAPI interface {
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
}

type serviceQuotaAPI interface {
	GetServiceQuota(ctx context.Context, in *servicequotas.GetServiceQuotaInput, optFns ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error)
	GetAWSDefaultServiceQuota(ctx context.Context, in *servicequotas.GetAWSDefaultServiceQuotaInput, optFns ...func(*servicequotas.Options)) (*servicequotas.GetAWSDefaultServiceQuotaOutput, error)
}

// ElasticIPQuotaInRegion counts the region's allocated Elastic IPs
// (ec2:DescribeAddresses) and reads the EC2-VPC Elastic IPs quota
// (servicequotas:GetServiceQuota, falling back to GetAWSDefaultServiceQuota when
// the account has no applied value). Read-only.
func (c *Client) ElasticIPQuotaInRegion(ctx context.Context, region string) (ElasticIPQuota, error) {
	return elasticIPQuota(ctx, c.regionalEC2(region), servicequotas.NewFromConfig(c.regionalConfig(region)))
}

func elasticIPQuota(ctx context.Context, ec2api elasticIPCountAPI, sq serviceQuotaAPI) (ElasticIPQuota, error) {
	var q ElasticIPQuota
	out, err := ec2api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err != nil {
		return q, fmt.Errorf("count Elastic IPs: %w", err)
	}
	q.InUse = len(out.Addresses)

	in := &servicequotas.GetServiceQuotaInput{ServiceCode: aws.String("ec2"), QuotaCode: aws.String(ElasticIPQuotaCode)}
	applied, err := sq.GetServiceQuota(ctx, in)
	var noApplied *sqtypes.NoSuchResourceException
	switch {
	case err == nil && applied.Quota != nil && applied.Quota.Value != nil:
		q.Limit, q.LimitSource = int(math.Floor(*applied.Quota.Value)), "applied"
		return q, nil
	case err != nil && !errors.As(err, &noApplied):
		return q, fmt.Errorf("read Elastic IP quota %s: %w", ElasticIPQuotaCode, err)
	}
	def, err := sq.GetAWSDefaultServiceQuota(ctx, &servicequotas.GetAWSDefaultServiceQuotaInput{
		ServiceCode: aws.String("ec2"), QuotaCode: aws.String(ElasticIPQuotaCode),
	})
	if err != nil {
		return q, fmt.Errorf("read default Elastic IP quota %s: %w", ElasticIPQuotaCode, err)
	}
	if def.Quota == nil || def.Quota.Value == nil {
		return q, fmt.Errorf("the Elastic IP quota %s has no value", ElasticIPQuotaCode)
	}
	q.Limit, q.LimitSource = int(math.Floor(*def.Quota.Value)), "default"
	return q, nil
}

// CheckElasticIPHeadroom refuses a launch that needs more Elastic IPs than the
// region has left, before anything is created. Without it the launch got as far
// as a running instance, failed at AllocateAddress and terminated the instance
// (#661) — correct, but only after paying for the boot.
func CheckElasticIPHeadroom(q ElasticIPQuota, region string, need int) error {
	if need < 1 || q.InUse+need <= q.Limit {
		return nil
	}
	quota := fmt.Sprintf("%d", q.Limit)
	if q.LimitSource == "default" {
		quota += " (the AWS default)"
	}
	return fmt.Errorf("%d of %s Elastic IPs in use in %s, and this launch needs %d more: "+
		"an instance with more than one EFA interface gets its public IP from an Elastic IP. "+
		"--efa-interfaces 1 needs no Elastic IP (one EFA interface, with an EC2-assigned public IP); "+
		"or release unused addresses (spawn cleanup releases spawn's own), "+
		"or raise quota %s (EC2-VPC Elastic IPs) in Service Quotas",
		q.InUse, quota, region, need, ElasticIPQuotaCode)
}
