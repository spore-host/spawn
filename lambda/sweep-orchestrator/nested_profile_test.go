package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// runInstancesCounter is an EC2 transport that counts RunInstances requests
// and fails every request, so nothing reaches AWS.
type runInstancesCounter struct {
	mu   sync.Mutex
	runs int
}

func (c *runInstancesCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	if strings.Contains(string(body), "Action=RunInstances") {
		c.mu.Lock()
		c.runs++
		c.mu.Unlock()
	}
	return nil, errors.New("no AWS in tests")
}

// dynamoSignal fails every DynamoDB request and reports each one, so a test
// can wait for a launch path's availability-tracking goroutine to have used
// dynamodbClient before restoring it.
type dynamoSignal chan struct{}

func (d dynamoSignal) RoundTrip(*http.Request) (*http.Response, error) {
	d <- struct{}{}
	return nil, errors.New("no AWS in tests")
}

func (c *runInstancesCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

// TestNestedSpawnProfileMemberIsNeverLaunched is #1372's Lambda gate: a member
// whose iam_role is a nested-spawn role (spawn-instance-nested-*) is refused
// before RunInstances, on both launch paths and for both param shapes, a
// CLI-rendered member's and a legacy (v0.146.0-era, #1368) one's. The control
// is the same member with an ordinary spawn-instance-* role, which does reach
// RunInstances, so the counter is not blind.
func TestNestedSpawnProfileMemberIsNeverLaunched(t *testing.T) {
	releaseOrchestrator(t, "0.147.0")
	// A RunInstances call records its outcome in DynamoDB from a goroutine;
	// keep that off the network, and wait for it before restoring the client.
	dynamo := make(dynamoSignal, 64)
	prevDynamo := dynamodbClient
	t.Cleanup(func() { dynamodbClient = prevDynamo })
	dynamodbClient = dynamodb.New(dynamodb.Options{
		Region:           "us-east-1",
		Credentials:      awssdk.AnonymousCredentials{},
		HTTPClient:       &http.Client{Transport: dynamo},
		RetryMaxAttempts: 1,
	})
	nested := spawnaws.NestedSpawnRoleName()
	if !strings.HasPrefix(nested, spawnaws.NestedSpawnRolePrefix) {
		t.Fatalf("NestedSpawnRoleName() = %q has no %s prefix", nested, spawnaws.NestedSpawnRolePrefix)
	}
	rendered := func(role string) map[string]interface{} {
		return map[string]interface{}{
			userDataParam: "x", tagsParam: cliTags(nil), launchSettingsParam: cliSettings(t, nil),
			"iam_role": role, "instance_type": "t3.micro",
		}
	}
	legacy := func(role string) map[string]interface{} {
		p := v0146Params(false, "1h")
		p["iam_role"] = role
		return p
	}
	paths := map[string]func(*ec2.Client, map[string]interface{}) error{
		"single-region": func(c *ec2.Client, p map[string]interface{}) error {
			return tryLaunchInstanceSingleRegion(context.Background(), c, legacyState(), p, 2, "t3.micro", "t3.micro")
		},
		"multi-region": func(c *ec2.Client, p map[string]interface{}) error {
			return tryLaunchInstance(context.Background(), c, legacyState(), p, 2, "us-east-1", "t3.micro", "t3.micro")
		},
	}
	shapes := map[string]func(string) map[string]interface{}{"rendered": rendered, "legacy": legacy}
	for pathName, launch := range paths {
		for shapeName, shape := range shapes {
			for _, tc := range []struct {
				role   string
				refuse bool
			}{
				{nested, true},
				{spawnaws.NestedSpawnRolePrefix + "0123abcd", true},
				{"spawn-instance-abc12345", false},
			} {
				counter := &runInstancesCounter{}
				client := ec2.New(ec2.Options{
					Region:           "us-east-1",
					Credentials:      awssdk.AnonymousCredentials{},
					HTTPClient:       &http.Client{Transport: counter},
					RetryMaxAttempts: 1,
				})
				err := launch(client, shape(tc.role))
				name := pathName + "/" + shapeName + "/" + tc.role
				if tc.refuse {
					if counter.count() != 0 {
						t.Errorf("%s: %d RunInstances call(s) for a nested-spawn member, want 0", name, counter.count())
					}
					if err == nil || !strings.Contains(err.Error(), "nested spawn isn't available for detached sweeps") ||
						!strings.Contains(err.Error(), "--ttl") {
						t.Errorf("%s: err = %v, want the nested-spawn refusal naming --nested-spawn's --ttl", name, err)
					}
					continue
				}
				if counter.count() != 1 {
					t.Errorf("%s: %d RunInstances call(s), want 1: the control never reached RunInstances, so the gate is blind", name, counter.count())
					continue
				}
				// The failure's availability record reads dynamodbClient
				// before its one request; wait for it.
				select {
				case <-dynamo:
				case <-time.After(30 * time.Second):
					t.Fatalf("%s: the launch's availability record never ran", name)
				}
			}
		}
	}
}

// The check reads the request as composed, so a profile given by ARN is
// refused too.
func TestNestedSpawnProfileIsRefusedByARN(t *testing.T) {
	in := &ec2.RunInstancesInput{IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
		Arn: awssdk.String("arn:aws:iam::123456789012:instance-profile/" + spawnaws.NestedSpawnRolePrefix + "0123abcd"),
	}}
	if err := refuseNestedSpawnProfile(in, 0); err == nil {
		t.Error("a nested-spawn profile named by ARN was not refused")
	}
	in.IamInstanceProfile.Arn = awssdk.String("arn:aws:iam::123456789012:instance-profile/spawn-instance-abc12345")
	if err := refuseNestedSpawnProfile(in, 0); err != nil {
		t.Errorf("an ordinary spawn-instance-* profile by ARN was refused: %v", err)
	}
}
