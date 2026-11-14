package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/keypairsweep"
)

type fakeKeyEC2 struct {
	keys    []types.KeyPairInfo
	deleted []string
}

func (f *fakeKeyEC2) DescribeKeyPairs(context.Context, *ec2.DescribeKeyPairsInput, ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error) {
	return &ec2.DescribeKeyPairsOutput{KeyPairs: f.keys}, nil
}
func (f *fakeKeyEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}
func (f *fakeKeyEC2) DeleteKeyPair(_ context.Context, in *ec2.DeleteKeyPairInput, _ ...func(*ec2.Options)) (*ec2.DeleteKeyPairOutput, error) {
	f.deleted = append(f.deleted, aws.ToString(in.KeyPairId))
	return &ec2.DeleteKeyPairOutput{}, nil
}

func sessionKey(name, purpose string, age time.Duration, now time.Time) types.KeyPairInfo {
	c := now.Add(-age)
	return types.KeyPairInfo{KeyName: aws.String(name), KeyPairId: aws.String("key-" + name), CreateTime: &c,
		Tags: []types.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:purpose"), Value: aws.String(purpose)},
		}}
}

// `spawn cleanup --session-keys` applies the reaper's rule (#1621): --dry-run
// deletes nothing, a run deletes the unused session key, and the workstation
// key is never offered.
func TestCleanupSessionKeys(t *testing.T) {
	now := time.Now()
	f := &fakeKeyEC2{keys: []types.KeyPairInfo{
		sessionKey("spawn-key-spore", keypairsweep.PurposeSession, 2*time.Hour, now),
		sessionKey("spawn-key-alice", keypairsweep.PurposeWorkstation, 400*time.Hour, now),
	}}
	apiFor := func(string) keypairsweep.API { return f }
	run := func(dry, yes bool) string {
		prevDry, prevYes := cleanupDryRun, cleanupYes
		cleanupDryRun, cleanupYes = dry, yes
		t.Cleanup(func() { cleanupDryRun, cleanupYes = prevDry, prevYes })
		var out bytes.Buffer
		c := &cobra.Command{}
		c.SetOut(&out)
		c.SetErr(&out)
		if err := runCleanupSessionKeys(context.Background(), c, []string{"us-east-1"}, apiFor, now); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	out := run(true, false)
	if len(f.deleted) != 0 || !strings.Contains(out, "spawn-key-spore") || strings.Contains(out, "spawn-key-alice") {
		t.Fatalf("dry run deleted %v; output:\n%s", f.deleted, out)
	}
	out = run(false, true)
	if len(f.deleted) != 1 || f.deleted[0] != "key-spawn-key-spore" {
		t.Fatalf("deleted %v, want only key-spawn-key-spore; output:\n%s", f.deleted, out)
	}
	if strings.Contains(out, "spawn-key-alice") {
		t.Errorf("the workstation key appeared in the output:\n%s", out)
	}
}
