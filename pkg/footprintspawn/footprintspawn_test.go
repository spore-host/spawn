package footprintspawn

import (
	"strings"
	"testing"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/tags"
)

const account = "123456789012"

func spawnTags(extra ...string) map[string]string {
	m := map[string]string{tags.ManagedBy: string(tags.ToolSpawn), "spawn:managed": "true"}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

func finished(r footprint.Resource, f Facts) footprint.Resource {
	if f.Account == "" {
		f.Account = account
	}
	Finish(&r, f)
	return r
}

// One test per ownership value, from the issue's rules.
func TestOwnership(t *testing.T) {
	for _, c := range []struct {
		name string
		tags map[string]string
		want footprint.Ownership
	}{
		{"spawn-instance-abc", spawnTags(), footprint.OwnershipTagged},
		{"spawn-instance-abc", map[string]string{"spawn:managed": "true"}, footprint.OwnershipLegacyName},
		{"spawn-instance-abc", nil, footprint.OwnershipLegacyName},
		{"spawn-instance-abc", map[string]string{tags.ManagedBy: "calque"}, footprint.OwnershipForeign},
		{"spawn-instance-abc", map[string]string{tags.ManagedBy: "lagotto"}, footprint.OwnershipForeign},
		{"spore-host-lifecycle", map[string]string{tags.ManagedBy: string(tags.ToolShared)}, footprint.OwnershipForeign},
		// The name wins: calque's resources are calque's whatever they carry.
		{"calque-nfs-sg", spawnTags(), footprint.OwnershipForeign},
		{"/aws/lambda/lagotto-poller", nil, footprint.OwnershipForeign},
	} {
		if got := OwnershipOf(c.name, c.tags); got != c.want {
			t.Errorf("OwnershipOf(%q, %v) = %s, want %s", c.name, c.tags, got, c.want)
		}
	}
}

// One test per class, in the documented order of precedence.
func TestClassify(t *testing.T) {
	sg := footprint.Resource{ARN: "arn:aws:ec2:us-east-1:" + account + ":security-group/sg-1", Type: TypeSecurityGroup, Name: "spawn-dcv", Tags: spawnTags()}
	boundary := footprint.Resource{ARN: "arn:aws:iam::" + account + ":policy/spore-host/spawn-instance-boundary", Type: TypeManagedPolicy,
		Name: "spawn-instance-boundary", Tags: spawnTags()}
	withTag := func(r footprint.Resource, k, v string) footprint.Resource {
		r.Tags = spawnTags(k, v)
		return r
	}
	for _, c := range []struct {
		what string
		r    footprint.Resource
		f    Facts
		want footprint.Class
	}{
		{"the boundary", boundary, Facts{}, footprint.ClassPermanent},
		{"spore-host:permanent", withTag(sg, tags.Permanent, tags.PermanentTrue), Facts{}, footprint.ClassPermanent},
		{"role=service", withTag(sg, tags.Role, tags.RoleService), Facts{InUse: "x"}, footprint.ClassPermanent},
		{"the infra account", sg, Facts{Account: InfraAccountID}, footprint.ClassPermanent},
		{"user data beats in use", sg, Facts{UserData: "holds results", InUse: "x"}, footprint.ClassUserData},
		{"in use", sg, Facts{InUse: "on 1 network interface(s)", Residue: "x"}, footprint.ClassActive},
		{"residue", sg, Facts{Residue: "a deployed stack"}, footprint.ClassResidue},
		{"litter", sg, Facts{}, footprint.ClassLitter},
	} {
		if c.f.Account == "" {
			c.f.Account = account
		}
		if got, _ := Classify(c.r, c.f); got != c.want {
			t.Errorf("%s: class %s, want %s", c.what, got, c.want)
		}
	}
}

// Deletable: a tagged litter resource of a registered type and name.
func TestTaggedLitterIsDeletable(t *testing.T) {
	r := finished(footprint.Resource{ARN: "arn:aws:ec2:us-east-1:" + account + ":volume/vol-0123456789abcdef0",
		Type: TypeVolume, Name: "vol-0123456789abcdef0", Tags: spawnTags()}, Facts{})
	if !r.Deletable || r.Ownership != footprint.OwnershipTagged || r.Class != footprint.ClassLitter {
		t.Fatalf("tagged, unattached volume: %+v", r)
	}
}

// The never-deletable kinds: user data, the boundary, role=service, foreign
// (calque, lagotto), and legacy-name. Each is built to look as deletable as
// possible otherwise, so only the rule under test can refuse it.
func TestNeverDeletable(t *testing.T) {
	for _, c := range []struct {
		what string
		r    footprint.Resource
		f    Facts
		why  string
	}{
		{"a results bucket holding results",
			footprint.Resource{ARN: "arn:aws:s3:::spawn-results-" + account + "-us-east-1", Type: TypeBucket, Name: "spawn-results-" + account + "-us-east-1", Tags: spawnTags()},
			Facts{UserData: "holds results"}, "class user-data"},
		{"an AMI",
			footprint.Resource{ARN: "arn:aws:ec2:us-east-1::image/ami-1", Type: TypeImage, Name: "spawn-thing", Tags: spawnTags()},
			Facts{UserData: "an image"}, "class user-data"},
		{"the boundary",
			footprint.Resource{ARN: "arn:aws:iam::" + account + ":policy/spore-host/spawn-instance-boundary", Type: TypeManagedPolicy, Name: "spawn-instance-boundary", Tags: spawnTags()},
			Facts{}, "spawn-instance-boundary"},
		{"a role=service security group",
			footprint.Resource{ARN: "arn:aws:ec2:us-east-1:" + account + ":security-group/sg-1", Type: TypeSecurityGroup, Name: "spawn-dcv", Tags: spawnTags(tags.Role, tags.RoleService)},
			Facts{}, "own service"},
		{"calque's queue, by tag",
			footprint.Resource{ARN: "arn:aws:sqs:us-east-1:" + account + ":spawn-pool-x", Type: TypeQueue, Name: "spawn-pool-x", Tags: map[string]string{tags.ManagedBy: "calque"}},
			Facts{}, `ownership is "foreign"`},
		{"calque's NFS group, by name, carrying spawn's tag",
			footprint.Resource{ARN: "arn:aws:ec2:us-east-1:" + account + ":security-group/sg-2", Type: TypeSecurityGroup, Name: "calque-nfs-sg", Tags: spawnTags()},
			Facts{}, `ownership is "foreign"`},
		{"lagotto's role",
			footprint.Resource{ARN: "arn:aws:iam::" + account + ":role/spawn-instance-lag", Type: TypeRole, Name: "spawn-instance-lag", Tags: map[string]string{tags.ManagedBy: "lagotto", "spawn:managed": "true"}},
			Facts{}, `ownership is "foreign"`},
		{"a legacy-name (spawn:managed only) role",
			footprint.Resource{ARN: "arn:aws:iam::" + account + ":role/spawn-instance-old", Type: TypeRole, Name: "spawn-instance-old", Tags: map[string]string{"spawn:managed": "true"}},
			Facts{}, `ownership is "legacy-name"`},
		{"a type spawn only reports",
			footprint.Resource{ARN: "arn:aws:sns:us-east-1:" + account + ":spawn-pipeline-x", Type: TypeTopic, Name: "spawn-pipeline-x", Tags: spawnTags()},
			Facts{}, "matches none"},
	} {
		r := finished(c.r, c.f)
		if r.Deletable {
			t.Errorf("%s is deletable: %+v", c.what, r)
			continue
		}
		if !strings.Contains(r.Reason, c.why) {
			t.Errorf("%s: refused for %q, want a reason naming %q", c.what, r.Reason, c.why)
		}
	}
}

// The revert check for calque: a calque resource is foreign and never
// deletable, even when it carries spawn's tag and a name spawn's patterns
// match. Fails if ForeignByName's rule is dropped from OwnershipOf (the
// resource would be classified tagged and come out deletable).
func TestCalqueIsNeverTaggedOrDeletable(t *testing.T) {
	for _, r := range []footprint.Resource{
		{ARN: "arn:aws:iam::" + account + ":role/calque-worker", Type: TypeRole, Name: "calque-worker", Tags: spawnTags()},
		{ARN: "arn:aws:ec2:us-east-1:" + account + ":security-group/sg-9", Type: TypeSecurityGroup, Name: "calque-nfs-sg", Tags: spawnTags()},
		{ARN: "arn:aws:sqs:us-east-1:" + account + ":calque-jobs", Type: TypeQueue, Name: "calque-jobs", Tags: spawnTags()},
	} {
		// A pattern that matches the name, so only ownership can refuse it.
		f := finished(r, Facts{})
		if f.Ownership == footprint.OwnershipTagged {
			t.Errorf("%s classified %s", r.Name, f.Ownership)
		}
		if ok, _ := footprint.Deletable(f, Tool, append([]footprint.NamePattern{footprint.MustPattern("", `calque-.+`)}, Patterns...)); ok {
			t.Errorf("%s is deletable with a pattern that matches it", r.Name)
		}
	}
}

func TestPatterns(t *testing.T) {
	match := func(typ, name string) bool {
		for _, p := range Patterns {
			if p.Matches(footprint.Resource{Type: typ, Name: name}) {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		typ, name string
		want      bool
	}{
		{TypeManagedPolicy, "spored-policy-v3", true},
		{TypeManagedPolicy, "spored-baseline-policy-v12", true},
		{TypeManagedPolicy, "spored-policy-v0", false},
		{TypeManagedPolicy, "spawn-instance-boundary", false},
		{TypeBucket, "spawn-results-" + account + "-us-east-1", true},
		{TypeBucket, "spawn-staging-" + account + "-eu-west-2", true},
		{TypeBucket, "spawn-schedules-us-east-1", false},
		{TypeRole, "spawn-instance-af541989", true},
		{TypeRole, "spored-instance-role", true},
		{TypeRole, "spawn-ttl-reaper-ec2", false},
		{TypeQueue, "spawn-pool-run1", true},
		{TypeQueue, "calque-jobs", false},
		{TypeSecurityGroup, "default", false},
		{TypeStack, "spawn-bot", false},
	} {
		if got := match(c.typ, c.name); got != c.want {
			t.Errorf("%s %q: matched %v, want %v", c.typ, c.name, got, c.want)
		}
	}
}
