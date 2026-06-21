package aws

import "testing"

func TestParseARN(t *testing.T) {
	tests := []struct {
		arn      string
		service  string
		resource string
		id       string
	}{
		{"arn:aws:ec2:us-east-1:123456789012:instance/i-0abc", "ec2", "instance", "i-0abc"},
		{"arn:aws:ec2:us-east-1:123456789012:security-group/sg-0abc", "ec2", "security-group", "sg-0abc"},
		{"arn:aws:ec2:us-east-1:123456789012:key-pair/key-0abc", "ec2", "key-pair", "key-0abc"},
		{"arn:aws:ec2:us-east-1:123456789012:volume/vol-0abc", "ec2", "volume", "vol-0abc"},
		{"arn:aws:iam::123456789012:role/spored-role", "iam", "role", "spored-role"},
		{"arn:aws:iam::123456789012:instance-profile/spored-profile", "iam", "instance-profile", "spored-profile"},
		{"arn:aws:dynamodb:us-east-1:123456789012:table/spore-installs", "dynamodb", "table", "spore-installs"},
		{"arn:aws:logs:us-east-1:123456789012:log-group:spawn-foo:*", "logs", "log-group", "spawn-foo:*"},
		{"garbage", "", "", ""},
	}
	for _, tt := range tests {
		svc, rt, id := parseARN(tt.arn)
		if svc != tt.service || rt != tt.resource || id != tt.id {
			t.Errorf("parseARN(%q) = (%q,%q,%q), want (%q,%q,%q)", tt.arn, svc, rt, id, tt.service, tt.resource, tt.id)
		}
	}
}

func TestIsRunningInstance(t *testing.T) {
	cases := []struct {
		r    ManagedResource
		want bool
	}{
		{ManagedResource{ResourceType: "instance", State: "running"}, true},
		{ManagedResource{ResourceType: "instance", State: "pending"}, true},
		{ManagedResource{ResourceType: "instance", State: "stopped"}, false},
		{ManagedResource{ResourceType: "instance", State: "terminated"}, false},
		{ManagedResource{ResourceType: "security-group"}, false},
	}
	for _, c := range cases {
		if got := c.r.IsRunningInstance(); got != c.want {
			t.Errorf("IsRunningInstance(%+v) = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestIsLikelyOrphan(t *testing.T) {
	cases := []struct {
		name       string
		r          ManagedResource
		hasRunning bool
		want       bool
	}{
		{"available volume", ManagedResource{ResourceType: "volume", State: "available"}, true, true},
		{"in-use volume", ManagedResource{ResourceType: "volume", State: "in-use"}, true, false},
		{"SG with running instance", ManagedResource{ResourceType: "security-group"}, true, false},
		{"SG with no instances", ManagedResource{ResourceType: "security-group"}, false, true},
		{"key-pair no instances", ManagedResource{ResourceType: "key-pair"}, false, true},
		{"iam role no instances", ManagedResource{Service: "iam", ResourceType: "role"}, false, true},
		{"iam role with instances", ManagedResource{Service: "iam", ResourceType: "role"}, true, false},
		{"instance never orphan", ManagedResource{ResourceType: "instance", State: "stopped"}, false, false},
		{"table never orphan", ManagedResource{Service: "dynamodb", ResourceType: "table"}, false, false},
	}
	for _, c := range cases {
		if got := IsLikelyOrphan(c.r, c.hasRunning); got != c.want {
			t.Errorf("%s: IsLikelyOrphan = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDeletionOrder(t *testing.T) {
	in := []ManagedResource{
		{Service: "dynamodb", ResourceType: "table"},
		{Service: "iam", ResourceType: "role"},
		{ResourceType: "instance"},
		{ResourceType: "security-group"},
		{Service: "iam", ResourceType: "instance-profile"},
		{ResourceType: "key-pair"},
	}
	got := DeletionOrder(in)
	// Instance must come before SG before key-pair before instance-profile
	// before role before table.
	wantOrder := []string{"instance", "security-group", "key-pair", "instance-profile", "role", "table"}
	for i, w := range wantOrder {
		if got[i].ResourceType != w {
			t.Errorf("position %d = %q, want %q (order: %+v)", i, got[i].ResourceType, w, resourceTypes(got))
		}
	}
}

func resourceTypes(rs []ManagedResource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ResourceType
	}
	return out
}
