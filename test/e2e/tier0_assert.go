//go:build e2e_tier0

package e2e

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// describeInstance fetches a single instance from Substrate by ID and returns
// its state name and tag map, so lifecycle tests can assert what actually
// landed in the emulator rather than just that the command exited 0.
func (e *spawnEnv) describeInstance(id string) (state string, tags map[string]string) {
	e.t.Helper()
	out, err := e.EC2Client().DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{
		InstanceIds: []string{id},
	})
	if err != nil {
		e.t.Fatalf("DescribeInstances %s: %v", id, err)
	}
	tags = map[string]string{}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if aws.ToString(inst.InstanceId) != id {
				continue
			}
			if inst.State != nil {
				state = string(inst.State.Name)
			}
			for _, t := range inst.Tags {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
		}
	}
	return state, tags
}

// requireState fails unless the instance's Substrate state matches want.
func (e *spawnEnv) requireState(id, want string) {
	e.t.Helper()
	if got, _ := e.describeInstance(id); got != want {
		e.t.Errorf("instance %s state = %q, want %q", id, got, want)
	}
}

// requireTag fails unless the instance carries tag key=want.
func (e *spawnEnv) requireTag(id, key, want string) {
	e.t.Helper()
	_, tags := e.describeInstance(id)
	if got, ok := tags[key]; !ok {
		e.t.Errorf("instance %s missing tag %q (have: %v)", id, key, tagKeys(tags))
	} else if want != "" && got != want {
		e.t.Errorf("instance %s tag %q = %q, want %q", id, key, got, want)
	}
}

func tagKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
