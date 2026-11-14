package keypairsweep

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type fakeEC2 struct {
	keys      []types.KeyPairInfo
	instances []types.Instance
	deleted   []string
	delErr    error
	keyFilter []types.Filter
}

func (f *fakeEC2) DescribeKeyPairs(_ context.Context, in *ec2.DescribeKeyPairsInput, _ ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error) {
	f.keyFilter = in.Filters
	return &ec2.DescribeKeyPairsOutput{KeyPairs: f.keys}, nil
}

func (f *fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: f.instances}}}, nil
}

func (f *fakeEC2) DeleteKeyPair(_ context.Context, in *ec2.DeleteKeyPairInput, _ ...func(*ec2.Options)) (*ec2.DeleteKeyPairOutput, error) {
	if f.delErr != nil {
		return nil, f.delErr
	}
	f.deleted = append(f.deleted, aws.ToString(in.KeyPairId))
	return &ec2.DeleteKeyPairOutput{}, nil
}

func key(name, purpose string, age time.Duration, now time.Time) types.KeyPairInfo {
	created := now.Add(-age)
	return types.KeyPairInfo{
		KeyName: aws.String(name), KeyPairId: aws.String("key-" + name), CreateTime: &created,
		Tags: []types.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:purpose"), Value: aws.String(purpose)},
		},
	}
}

func outcomes(rs []Result) map[string]Outcome {
	m := map[string]Outcome{}
	for _, r := range rs {
		m[r.KeyName] = r.Outcome
	}
	return m
}

// The rule (#1621): a session key goes once nothing uses it and it is an hour
// old. A workstation key never goes, even if EC2's filter hands one back.
func TestSweep(t *testing.T) {
	now := time.Now()
	f := &fakeEC2{
		keys: []types.KeyPairInfo{
			key("spawn-key-spore", PurposeSession, 26*time.Hour, now),      // the leak in #1621
			key("spawn-key-busy", PurposeSession, 26*time.Hour, now),       // a child still uses it
			key("spawn-key-new", PurposeSession, 10*time.Minute, now),      // just imported
			key("spawn-key-alice", PurposeWorkstation, 400*time.Hour, now), // a user's own key
		},
		instances: []types.Instance{
			{InstanceId: aws.String("i-child"), KeyName: aws.String("spawn-key-busy"),
				State: &types.InstanceState{Name: types.InstanceStateNameStopped}},
		},
	}
	rs, err := Sweep(context.Background(), f, now, false)
	if err != nil {
		t.Fatal(err)
	}
	got := outcomes(rs)
	want := map[string]Outcome{"spawn-key-spore": Deleted, "spawn-key-busy": InUse, "spawn-key-new": TooYoung}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %q, want %q", k, got[k], w)
		}
	}
	if _, seen := got["spawn-key-alice"]; seen {
		t.Error("a workstation key was considered at all")
	}
	if len(f.deleted) != 1 || f.deleted[0] != "key-spawn-key-spore" {
		t.Errorf("deleted %v, want only key-spawn-key-spore", f.deleted)
	}
	// The request asks EC2 for session keys only.
	var purposeFilter []string
	for _, fl := range f.keyFilter {
		if aws.ToString(fl.Name) == "tag:spawn:purpose" {
			purposeFilter = fl.Values
		}
	}
	if len(purposeFilter) != 1 || purposeFilter[0] != PurposeSession {
		t.Errorf("DescribeKeyPairs purpose filter %v, want [%s]", purposeFilter, PurposeSession)
	}
}

func TestSweepDryRunAndFailures(t *testing.T) {
	now := time.Now()
	f := &fakeEC2{keys: []types.KeyPairInfo{key("spawn-key-old", PurposeSession, 2*time.Hour, now)}}
	rs, err := Sweep(context.Background(), f, now, true)
	if err != nil || len(rs) != 1 || rs[0].Outcome != WouldDelete || len(f.deleted) != 0 {
		t.Fatalf("dry run: %+v %v, deleted %v", rs, err, f.deleted)
	}
	f.delErr = errors.New("UnauthorizedOperation")
	rs, err = Sweep(context.Background(), f, now, false)
	if err != nil || len(rs) != 1 || rs[0].Outcome != Failed || rs[0].Err == nil {
		t.Fatalf("failed delete: %+v %v", rs, err)
	}
	// No CreateTime: never guessed old.
	f = &fakeEC2{keys: []types.KeyPairInfo{key("spawn-key-x", PurposeSession, 0, now)}}
	f.keys[0].CreateTime = nil
	rs, _ = Sweep(context.Background(), f, now, false)
	if len(rs) != 1 || rs[0].Outcome != TooYoung || len(f.deleted) != 0 {
		t.Fatalf("no CreateTime: %+v, deleted %v", rs, f.deleted)
	}
}

func TestIsSessionKey(t *testing.T) {
	now := time.Now()
	if !IsSessionKey(key("a", PurposeSession, 0, now).Tags) {
		t.Error("session key not recognised")
	}
	if IsSessionKey(key("a", PurposeWorkstation, 0, now).Tags) {
		t.Error("workstation key taken for a session key")
	}
	if IsSessionKey([]types.Tag{{Key: aws.String("spawn:purpose"), Value: aws.String(PurposeSession)}}) {
		t.Error("a key without spawn:managed=true taken for a session key")
	}
}
