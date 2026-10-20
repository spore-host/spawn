package aws

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestLaunchTagsRoundTripThroughListInstances is #689 part A's round trip: the
// tags the REAL launch producer writes (buildTags), launched into Substrate and
// read back through ListInstances, the path `spawn list`/`status`, the
// job-array drain and the sweep commands read from.
//
// #683's drain read Tags["spawn:job-array-id"], which listInstancesInRegion
// never fills: it lifts every recognised key into a named field and keeps only
// the rest in Tags. A hand-built InstanceInfo fixture passed for ever because it
// encoded the author's belief about the data. Here the data comes from the
// producer, so the assertions are about what a reader actually receives:
//
//   - every tag buildTags writes reaches the InstanceInfo, either in a named
//     field or in Tags (or Parameters, for spawn:param:*), and in exactly one of
//     them, so a reader can only be right or loudly wrong;
//   - every tag-sourced InstanceInfo field is populated by some tag, so no field
//     exists that a reader could consult and always get "".
func TestLaunchTagsRoundTripThroughListInstances(t *testing.T) {
	env := testutil.SubstrateServer(t)
	c := NewClientFromConfig(env.AWSConfig)
	ctx := context.Background()

	// Every value is distinct, so a value found in a field identifies the tag
	// that filled it.
	cfg := LaunchConfig{
		Name: "rt-name", TTL: "7h", IdleTimeout: "37m", OnComplete: "terminate",
		CompletionFile: "/tmp/rt-done", CompletionDelay: "41s", SessionTimeout: "43m",
		DNSName: "rt-dns", PreStop: "echo rt-prestop", PreStopTimeout: "47s",
		Username: "rt-user", SpawnVersion: "9.9.9-rt",
		JobArrayID: "rt-ja-id", JobArrayName: "rt-ja-name", JobArraySize: 11, JobArrayIndex: 5,
		SweepID: "rt-sw-id", SweepName: "rt-sw-name", SweepSize: 13, SweepIndex: 6,
		Parameters:      map[string]string{"alpha": "rt-p-alpha", "beta": "rt-p-beta"},
		JobArrayCommand: "echo rt-cmd",
	}
	tags, err := buildTags(cfg, "123456789012", "arn:aws:iam::123456789012:user/rt", "rtacct")
	if err != nil {
		t.Fatalf("buildTags: %v", err)
	}

	out, err := env.EC2Client().RunInstances(ctx, &ec2.RunInstancesInput{
		InstanceType: ec2types.InstanceTypeT3Micro,
		ImageId:      aws.String(testutil.RegisterTestAMI(t, env.EC2Client())),
		MinCount:     aws.Int32(1), MaxCount: aws.Int32(1),
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInstance, Tags: tags}},
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	id := aws.ToString(out.Instances[0].InstanceId)

	list, err := c.ListInstances(ctx, "us-east-1", "")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	var info *InstanceInfo
	for i := range list {
		if list[i].InstanceID == id {
			info = &list[i]
		}
	}
	if info == nil {
		t.Fatalf("launched %s with spawn:managed=true, ListInstances did not return it", id)
	}

	// Named string fields, by value, so a tag can be traced to the field it filled.
	fields := map[string]string{}
	rv := reflect.ValueOf(*info)
	for i := 0; i < rv.NumField(); i++ {
		if f := rv.Field(i); f.Kind() == reflect.String && f.String() != "" {
			fields[f.String()] = rv.Type().Field(i).Name
		}
	}

	filledByTag := map[string]bool{}
	for _, tg := range tags {
		k, v := aws.ToString(tg.Key), aws.ToString(tg.Value)
		field, inField := fields[v]
		if !strings.HasPrefix(k, "spawn:") && k != "Name" {
			continue
		}
		tv, inTags := info.Tags[k]
		inTags = inTags && tv == v
		if name, ok := strings.CutPrefix(k, "spawn:param:"); ok {
			if info.Parameters[name] != v {
				t.Errorf("%s=%q did not reach InstanceInfo.Parameters[%q] (got %q)", k, v, name, info.Parameters[name])
			}
			if inTags {
				t.Errorf("%s is in both Parameters and Tags", k)
			}
			continue
		}
		// A value like "true" can coincide with a field by accident; only a
		// distinct value identifies the field.
		distinct := strings.Contains(v, "rt-") || v == "11" || v == "5" || v == "13" || v == "6" || v == "7h" || v == "37m"
		switch {
		case inField && distinct && inTags:
			t.Errorf("%s=%q is in both InstanceInfo.%s and Tags; a reader cannot tell which is authoritative", k, v, field)
			filledByTag[field] = true
		case inField && distinct:
			filledByTag[field] = true
		case inTags:
		default:
			t.Errorf("%s=%q written at launch is not readable from InstanceInfo: not in Tags and no field holds it", k, v)
		}
	}

	// Reverse: the fields listInstancesInRegion fills from tags (its switch)
	// must each have been filled by a tag above. Discovered from the struct:
	// every string field that is not one of the EC2 instance attributes.
	ec2Sourced := map[string]bool{
		"InstanceID": true, "InstanceType": true, "State": true, "Region": true,
		"AvailabilityZone": true, "PublicIP": true, "PrivateIP": true, "KeyName": true,
		"IAMRole": true, "StateTransitionReason": true, "StateReason": true,
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.String || ec2Sourced[f.Name] {
			continue
		}
		if !filledByTag[f.Name] {
			t.Errorf("InstanceInfo.%s is not filled by any tag buildTags writes: a reader consulting it always gets %q",
				f.Name, rv.Field(i).String())
		}
	}
}
