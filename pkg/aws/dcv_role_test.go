package aws

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TestSporedDCVRolePolicy_Valid asserts the DCV spored role policy is valid JSON
// and carries the grants the #282 reconciliation added — FSx mount + DNS-sign —
// while no longer granting unconditioned CreateTags (the #174 class).
func TestSporedDCVRolePolicy_Valid(t *testing.T) {
	if !json.Valid([]byte(sporedDCVRolePolicy)) {
		t.Fatal("sporedDCVRolePolicy is not valid JSON")
	}
	var doc struct {
		Statement []struct {
			Action    interface{}            `json:"Action"`
			Resource  interface{}            `json:"Resource"`
			Condition map[string]interface{} `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(sporedDCVRolePolicy), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	actionsOf := func(a interface{}) []string {
		switch v := a.(type) {
		case string:
			return []string{v}
		case []interface{}:
			var out []string
			for _, x := range v {
				if s, ok := x.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
		return nil
	}

	need := map[string]bool{
		"fsx:DescribeFileSystems":             false, // #221 — DCV instance can mount ephemeral FSx
		"fsx:CreateDataRepositoryAssociation": false,
		"lambda:InvokeFunctionUrl":            false, // #173 — DCV instance can sign DNS
		"s3:GetObject":                        false, // dcv-license + certs
		"ec2:CreateTags":                      false,
	}
	createTagsConditioned := false
	for _, st := range doc.Statement {
		acts := actionsOf(st.Action)
		for _, a := range acts {
			if _, ok := need[a]; ok {
				need[a] = true
			}
		}
		// CreateTags must be conditioned on spawn:managed=true (the #174 fix).
		for _, a := range acts {
			if a == "ec2:CreateTags" {
				se, _ := st.Condition["StringEquals"].(map[string]interface{})
				if se["ec2:ResourceTag/spawn:managed"] == "true" {
					createTagsConditioned = true
				}
			}
		}
	}
	for a, found := range need {
		if !found {
			t.Errorf("DCV role policy missing %s", a)
		}
	}
	if !createTagsConditioned {
		t.Error("ec2:CreateTags must be conditioned on spawn:managed=true (the #174 tag-then-terminate fix)")
	}
}

func TestSGHasUDP8443(t *testing.T) {
	udp := func(from, to int32) types.IpPermission {
		return types.IpPermission{IpProtocol: aws.String("udp"), FromPort: aws.Int32(from), ToPort: aws.Int32(to)}
	}
	tcp := func(from, to int32) types.IpPermission {
		return types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(from), ToPort: aws.Int32(to)}
	}

	cases := []struct {
		name string
		perm []types.IpPermission
		want bool
	}{
		{"exact udp 8443", []types.IpPermission{udp(8443, 8443)}, true},
		{"udp range covering 8443", []types.IpPermission{udp(8000, 9000)}, true},
		{"only tcp 8443", []types.IpPermission{tcp(8443, 8443)}, false},
		{"udp wrong port", []types.IpPermission{udp(443, 443)}, false},
		{"none", nil, false},
		{"tcp+udp mixed", []types.IpPermission{tcp(8443, 8443), udp(8443, 8443)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sgHasUDP8443(types.SecurityGroup{IpPermissions: tc.perm})
			if got != tc.want {
				t.Errorf("sgHasUDP8443 = %v, want %v", got, tc.want)
			}
		})
	}
}
