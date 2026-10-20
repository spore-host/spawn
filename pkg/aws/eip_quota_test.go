package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
)

type fakeAddrCount struct{ n int }

func (f fakeAddrCount) DescribeAddresses(_ context.Context, in *ec2.DescribeAddressesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	if len(in.Filters) != 0 || len(in.AllocationIds) != 0 {
		// The quota counts every address in the region, the user's too.
		return nil, errors.New("count must not be filtered")
	}
	return &ec2.DescribeAddressesOutput{Addresses: make([]types.Address, f.n)}, nil
}

type fakeSQ struct {
	applied    *float64
	appliedErr error
	def        float64
	codes      []string
}

func (f *fakeSQ) GetServiceQuota(_ context.Context, in *servicequotas.GetServiceQuotaInput, _ ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error) {
	f.codes = append(f.codes, aws.ToString(in.ServiceCode)+"/"+aws.ToString(in.QuotaCode))
	if f.appliedErr != nil {
		return nil, f.appliedErr
	}
	return &servicequotas.GetServiceQuotaOutput{Quota: &sqtypes.ServiceQuota{Value: f.applied}}, nil
}

func (f *fakeSQ) GetAWSDefaultServiceQuota(_ context.Context, in *servicequotas.GetAWSDefaultServiceQuotaInput, _ ...func(*servicequotas.Options)) (*servicequotas.GetAWSDefaultServiceQuotaOutput, error) {
	f.codes = append(f.codes, "default:"+aws.ToString(in.ServiceCode)+"/"+aws.ToString(in.QuotaCode))
	return &servicequotas.GetAWSDefaultServiceQuotaOutput{Quota: &sqtypes.ServiceQuota{Value: aws.Float64(f.def)}}, nil
}

// The quota read is ec2/L-0263D0A3, "EC2-VPC Elastic IPs", the applied value.
func TestElasticIPQuota_ReadsTheAppliedEC2VPCElasticIPsQuota(t *testing.T) {
	sq := &fakeSQ{applied: aws.Float64(10)}
	q, err := elasticIPQuota(context.Background(), fakeAddrCount{n: 9}, sq)
	if err != nil {
		t.Fatal(err)
	}
	if q.InUse != 9 || q.Limit != 10 || q.LimitSource != "applied" {
		t.Errorf("quota = %+v, want 9 of 10 applied", q)
	}
	if strings.Join(sq.codes, ",") != "ec2/L-0263D0A3" {
		t.Errorf("quota calls = %v, want ec2/L-0263D0A3", sq.codes)
	}
}

// An account that never changed the quota has no applied value
// (NoSuchResourceException); the AWS default then holds.
func TestElasticIPQuota_FallsBackToTheDefault(t *testing.T) {
	sq := &fakeSQ{appliedErr: &sqtypes.NoSuchResourceException{Message: aws.String("none")}, def: 5}
	q, err := elasticIPQuota(context.Background(), fakeAddrCount{n: 2}, sq)
	if err != nil {
		t.Fatal(err)
	}
	if q.Limit != 5 || q.LimitSource != "default" {
		t.Errorf("quota = %+v, want the default 5", q)
	}
}

// Any other failure is returned, not guessed: a made-up limit could refuse a
// launch the account can make.
func TestElasticIPQuota_OtherErrorsAreReturned(t *testing.T) {
	sq := &fakeSQ{appliedErr: errors.New("AccessDeniedException: no servicequotas:GetServiceQuota")}
	if _, err := elasticIPQuota(context.Background(), fakeAddrCount{n: 2}, sq); err == nil {
		t.Fatal("an unreadable quota was not reported")
	}
	if len(sq.codes) != 1 {
		t.Errorf("calls = %v: fell back to the default after an access error", sq.codes)
	}
}

// The field-note case (#661): 10 in use of 10 refuses with the counts, the
// region and the --efa-interfaces 1 way out. Within the quota is allowed.
func TestCheckElasticIPHeadroom(t *testing.T) {
	err := CheckElasticIPHeadroom(ElasticIPQuota{InUse: 10, Limit: 10, LimitSource: "applied"}, "us-west-2", 1)
	if err == nil {
		t.Fatal("a launch past the quota was allowed")
	}
	for _, want := range []string{"10 of 10 Elastic IPs in use in us-west-2", "--efa-interfaces 1", "L-0263D0A3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	// --count: 8 in use of 10 has room for 2, not 3.
	if err := CheckElasticIPHeadroom(ElasticIPQuota{InUse: 8, Limit: 10}, "us-west-2", 2); err != nil {
		t.Errorf("8+2 of 10 refused: %v", err)
	}
	if err := CheckElasticIPHeadroom(ElasticIPQuota{InUse: 8, Limit: 10}, "us-west-2", 3); err == nil {
		t.Error("8+3 of 10 allowed")
	}
}
