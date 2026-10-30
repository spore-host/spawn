package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type fakeSpotEC2 struct {
	instance   *types.Instance
	history    []types.SpotPrice
	historyIn  *ec2.DescribeSpotPriceHistoryInput
	historyErr error
	tagged     []types.Tag
	describes  int
}

func (f *fakeSpotEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.describes++
	if f.instance == nil {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{*f.instance}}}}, nil
}

func (f *fakeSpotEC2) DescribeSpotPriceHistory(_ context.Context, in *ec2.DescribeSpotPriceHistoryInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
	f.historyIn = in
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return &ec2.DescribeSpotPriceHistoryOutput{SpotPriceHistory: f.history}, nil
}

func (f *fakeSpotEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.tagged = append(f.tagged, in.Tags...)
	return &ec2.CreateTagsOutput{}, nil
}

func spotHistory(az, price string, at time.Time) types.SpotPrice {
	return types.SpotPrice{AvailabilityZone: aws.String(az), SpotPrice: aws.String(price), Timestamp: aws.Time(at),
		InstanceType: types.InstanceType("m8a.4xlarge"), ProductDescription: types.RIProductDescription("Linux/UNIX")}
}

func launchedSpot() types.Instance {
	return types.Instance{
		InstanceId:      aws.String("i-028c23f816d2eb68e"),
		InstanceType:    types.InstanceType("m8a.4xlarge"),
		Placement:       &types.Placement{AvailabilityZone: aws.String("us-west-2b")},
		PlatformDetails: aws.String("Linux/UNIX"),
	}
}

// A launched spot instance gets the price for ITS zone, type and product — the
// most recent observation, not another zone's — recorded in SpotPriceTag (#896).
func TestRecordLaunchSpotPrice_TagsTheInstancesOwnZonePrice(t *testing.T) {
	now := time.Now()
	f := &fakeSpotEC2{history: []types.SpotPrice{
		spotHistory("us-west-2a", "0.2900", now),
		spotHistory("us-west-2b", "0.3600", now.Add(-time.Minute)),
		spotHistory("us-west-2b", "0.3500", now.Add(-2*time.Hour)),
	}}
	recordLaunchSpotPrice(context.Background(), f, launchedSpot())

	if f.historyIn == nil || aws.ToString(f.historyIn.AvailabilityZone) != "us-west-2b" ||
		len(f.historyIn.InstanceTypes) != 1 || f.historyIn.InstanceTypes[0] != "m8a.4xlarge" ||
		len(f.historyIn.ProductDescriptions) != 1 || f.historyIn.ProductDescriptions[0] != "Linux/UNIX" {
		t.Fatalf("spot price query = %+v, want the instance's zone, type and product", f.historyIn)
	}
	if len(f.tagged) != 1 || aws.ToString(f.tagged[0].Key) != SpotPriceTag || aws.ToString(f.tagged[0].Value) != "0.360000" {
		t.Errorf("tagged %+v, want %s=0.360000 (the latest us-west-2b price)", f.tagged, SpotPriceTag)
	}
	if f.describes != 0 {
		t.Errorf("re-described an instance whose launch response already had zone and platform")
	}
}

// A failed lookup must not fail the launch or write a made-up price.
func TestRecordLaunchSpotPrice_FailureWritesNothing(t *testing.T) {
	f := &fakeSpotEC2{historyErr: errors.New("UnauthorizedOperation")}
	recordLaunchSpotPrice(context.Background(), f, launchedSpot())
	if len(f.tagged) != 0 {
		t.Errorf("tagged %+v after a failed lookup", f.tagged)
	}
}

// A platform with no published spot price (Windows with SQL Server, BYOL, ...)
// is unknown, not priced as Linux — which would understate it.
func TestSpotProductDescription(t *testing.T) {
	cases := []struct {
		inst types.Instance
		want string
		ok   bool
	}{
		{types.Instance{PlatformDetails: aws.String("Linux/UNIX")}, "Linux/UNIX", true},
		{types.Instance{PlatformDetails: aws.String("Red Hat Enterprise Linux")}, "Red Hat Enterprise Linux", true},
		{types.Instance{PlatformDetails: aws.String("Windows")}, "Windows", true},
		{types.Instance{Platform: types.PlatformValuesWindows}, "Windows", true},
		{types.Instance{PlatformDetails: aws.String("Windows with SQL Server Standard")}, "", false},
		{types.Instance{}, "", false},
	}
	for _, c := range cases {
		got, ok := spotProductDescription(c.inst)
		if got != c.want || ok != c.ok {
			t.Errorf("spotProductDescription(%v/%v) = %q,%v want %q,%v",
				aws.ToString(c.inst.PlatformDetails), c.inst.Platform, got, ok, c.want, c.ok)
		}
	}
}

// When the response lacks the platform (as a bare instance id does, which is all
// CurrentSpotPrice starts from), the instance is described once to get it.
func TestSpotPriceForInstance_DescribesWhenTheResponseIsIncomplete(t *testing.T) {
	full := launchedSpot()
	f := &fakeSpotEC2{instance: &full, history: []types.SpotPrice{spotHistory("us-west-2b", "0.3600", time.Now())}}
	sp, err := spotPriceForInstance(context.Background(), f, types.Instance{InstanceId: full.InstanceId}, nil)
	if err != nil {
		t.Fatalf("spotPriceForInstance: %v", err)
	}
	if f.describes != 1 || sp.PricePerHour != 0.36 || sp.AvailabilityZone != "us-west-2b" {
		t.Errorf("describes=%d price=%+v, want one describe and $0.36 in us-west-2b", f.describes, sp)
	}

	empty := &fakeSpotEC2{history: []types.SpotPrice{spotHistory("us-west-2a", "0.29", time.Now())}}
	if _, err := lookupSpotPrice(context.Background(), empty, "m8a.4xlarge", "us-west-2b", "Linux/UNIX"); err == nil {
		t.Error("a price for another zone was accepted for us-west-2b")
	}
}
