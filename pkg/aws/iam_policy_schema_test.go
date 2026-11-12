package aws

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	smithy "github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1558: the spored roles are shared by every spawn version that launches in
// the account. These tests replay the mixed-version sequences against a fake
// IAM that enforces the inline-policy size cap, and then once against the
// emulator. Those calling putVersionedRolePolicy test the versioned inline
// write, which the spored roles fall back to when they cannot use a managed
// policy (#1573, iam_spored_managed_test.go) and SpawnSweepCrossAccountRole
// always uses.

const testAccount = "123456789012"

// inlinePolicyLimit is IAM's cap on a role's inline policies together, in
// characters, whitespace not counted.
const inlinePolicyLimit = 10240

// fakeRolePolicies is one role's inline policies.
type fakeRolePolicies struct {
	policies  map[string]string
	listErr   error
	deleteErr error
	calls     []string
	peak      int // largest aggregate size the role reached
}

func newFakeRole() *fakeRolePolicies { return &fakeRolePolicies{policies: map[string]string{}} }

func (f *fakeRolePolicies) size() int {
	n := 0
	for _, d := range f.policies {
		n += nonSpaceLen(d)
	}
	return n
}

func (f *fakeRolePolicies) ListRolePolicies(_ context.Context, _ *iam.ListRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	f.calls = append(f.calls, "List")
	if f.listErr != nil {
		return nil, f.listErr
	}
	names := make([]string, 0, len(f.policies))
	for n := range f.policies {
		names = append(names, n)
	}
	sort.Strings(names)
	return &iam.ListRolePoliciesOutput{PolicyNames: names}, nil
}

func (f *fakeRolePolicies) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	name := aws.ToString(in.PolicyName)
	f.calls = append(f.calls, "Put "+name)
	prev, had := f.policies[name]
	f.policies[name] = aws.ToString(in.PolicyDocument)
	if n := f.size(); n > inlinePolicyLimit {
		if had {
			f.policies[name] = prev
		} else {
			delete(f.policies, name)
		}
		return nil, &smithy.GenericAPIError{Code: "LimitExceeded", Message: fmt.Sprintf("Maximum policy size of %d bytes exceeded for role (%d)", inlinePolicyLimit, n)}
	}
	if n := f.size(); n > f.peak {
		f.peak = n
	}
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *fakeRolePolicies) DeleteRolePolicy(_ context.Context, in *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	name := aws.ToString(in.PolicyName)
	f.calls = append(f.calls, "Delete "+name)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if _, ok := f.policies[name]; !ok {
		return nil, &types.NoSuchEntityException{Message: aws.String("no such policy " + name)}
	}
	delete(f.policies, name)
	return &iam.DeleteRolePolicyOutput{}, nil
}

func (f *fakeRolePolicies) names() []string {
	out := make([]string, 0, len(f.policies))
	for n := range f.policies {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// oldCLIWrites is what every spawn up to v0.161.0 does to the role: overwrite
// the unversioned name with its own document, and nothing else.
func (f *fakeRolePolicies) oldCLIWrites(t *testing.T, base, doc string) {
	t.Helper()
	if _, err := f.PutRolePolicy(context.Background(), &iam.PutRolePolicyInput{
		RoleName: aws.String("r"), PolicyName: aws.String(base), PolicyDocument: aws.String(doc),
	}); err != nil {
		t.Fatalf("an old CLI's write of %s failed: %v", base, err)
	}
}

// allowedActions is every action an Allow statement in any of the role's
// policies grants: what IAM gives the instance, the role's policies being a
// union.
func grantedActions(t *testing.T, docs map[string]string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for name, doc := range docs {
		for _, st := range parseStatementsLoose(t, name, doc) {
			if st["Effect"] != "Allow" {
				continue
			}
			for a := range actionSet(st["Action"]) {
				out[a] = true
			}
		}
	}
	return out
}

func parseStatementsLoose(t *testing.T, what, doc string) []map[string]interface{} {
	t.Helper()
	var p struct {
		Statement []map[string]interface{}
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", what, err, doc)
	}
	return p.Statement
}

// docWithout is doc with action removed from every statement, and a statement
// left with no action dropped: an older (or newer) version of the document.
func docWithout(t *testing.T, doc, action string) string {
	t.Helper()
	var p map[string]interface{}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	var kept []interface{}
	for _, s := range p["Statement"].([]interface{}) {
		st := s.(map[string]interface{})
		switch a := st["Action"].(type) {
		case string:
			if a == action {
				continue
			}
		case []interface{}:
			var rest []interface{}
			for _, x := range a {
				if x != action {
					rest = append(rest, x)
				}
			}
			if len(rest) == 0 {
				continue
			}
			st["Action"] = rest
		}
		kept = append(kept, st)
	}
	p["Statement"] = kept
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"`+action+`"`) {
		t.Fatalf("docWithout left %s in place", action)
	}
	return string(b)
}

func bound(t *testing.T, doc string) string {
	t.Helper()
	b, err := bindPolicyAccount(doc, testAccount)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type warnings []string

// noWarn fails the test on any warning.
func noWarn(t *testing.T) func(string) {
	return func(s string) { t.Helper(); t.Errorf("unexpected warning: %s", s) }
}

func (w *warnings) add(s string) { *w = append(*w, s) }

// The two shared role kinds and the document each one carries.
func sporedRoleKinds(t *testing.T) []struct{ base, doc string } {
	return []struct{ base, doc string }{
		{sporedRolePolicyBase, bound(t, sporedDCVRolePolicy)},
		{sporedBaselinePolicyBase, bound(t, (&Client{}).sporedBaselinePolicyDoc())},
	}
}

const spotPrice = "ec2:DescribeSpotPriceHistory"

// The issue's sequence: a new CLI writes the grant, a v0.161.0 launch then
// writes its document without it, and a new CLI launches again. The grant must
// never leave the role.
func TestSporedPolicy_NewThenOldThenNew_KeepsTheNewerGrant(t *testing.T) {
	for _, k := range sporedRoleKinds(t) {
		t.Run(k.base, func(t *testing.T) {
			ctx := context.Background()
			role := newFakeRole()
			old := docWithout(t, k.doc, spotPrice) // what v0.161.0 writes
			var w warnings

			if err := putVersionedRolePolicy(ctx, role, "r", k.base, k.doc, 1, w.add); err != nil {
				t.Fatal(err)
			}
			if got, want := role.names(), []string{versionedPolicyName(k.base, 1)}; !equalStrings(got, want) {
				t.Fatalf("after the new CLI: policies %v, want %v", got, want)
			}

			role.oldCLIWrites(t, k.base, old)
			if !grantedActions(t, role.policies)[spotPrice] {
				t.Fatalf("a v0.161.0 launch after the new CLI stripped %s from the role (#1558): %v", spotPrice, role.names())
			}

			if err := putVersionedRolePolicy(ctx, role, "r", k.base, k.doc, 1, w.add); err != nil {
				t.Fatal(err)
			}
			if got, want := role.names(), []string{versionedPolicyName(k.base, 1)}; !equalStrings(got, want) {
				t.Errorf("after the new CLI again: policies %v, want %v (the old CLI's copy deleted)", got, want)
			}
			if !grantedActions(t, role.policies)[spotPrice] {
				t.Errorf("%s missing after new, old, new", spotPrice)
			}
			if len(w) != 0 {
				t.Errorf("unexpected warnings: %v", w)
			}
		})
	}
}

// A role an old CLI set up: the first new launch writes its version and only
// then deletes the old copy, so the role is never without the grants.
func TestSporedPolicy_OldThenNew_ReplacesTheUnversionedPolicy(t *testing.T) {
	for _, k := range sporedRoleKinds(t) {
		t.Run(k.base, func(t *testing.T) {
			role := newFakeRole()
			role.oldCLIWrites(t, k.base, docWithout(t, k.doc, spotPrice))
			role.calls = nil
			if err := putVersionedRolePolicy(context.Background(), role, "r", k.base, k.doc, 1, noWarn(t)); err != nil {
				t.Fatal(err)
			}
			mine := versionedPolicyName(k.base, 1)
			if want := []string{"List", "Put " + mine, "Delete " + k.base}; !equalStrings(role.calls, want) {
				t.Errorf("calls %v, want %v: the old copy may go only once the new one is written", role.calls, want)
			}
			if got := role.names(); !equalStrings(got, []string{mine}) {
				t.Errorf("policies %v, want only %s", got, mine)
			}
			if !grantedActions(t, role.policies)[spotPrice] {
				t.Errorf("%s missing", spotPrice)
			}
		})
	}
}

// The removal path. A CLI at schema 2 drops a grant; an older versioned CLI
// (schema 1) defers to it and writes nothing; a pre-#1558 CLI puts the grant
// back in its own copy, and the next schema-2 launch takes it away again.
func TestSporedPolicy_RemovalShipsWithASchemaBump(t *testing.T) {
	for _, k := range sporedRoleKinds(t) {
		t.Run(k.base, func(t *testing.T) {
			ctx := context.Background()
			role := newFakeRole()
			v1 := k.doc
			v2 := docWithout(t, k.doc, spotPrice) // the retirement, for the test
			var w warnings

			if err := putVersionedRolePolicy(ctx, role, "r", k.base, v1, 1, w.add); err != nil {
				t.Fatal(err)
			}
			// The newer CLI retires the grant.
			if err := putVersionedRolePolicy(ctx, role, "r", k.base, v2, 2, w.add); err != nil {
				t.Fatal(err)
			}
			if got := role.names(); !equalStrings(got, []string{versionedPolicyName(k.base, 2)}) {
				t.Fatalf("after the schema-2 CLI: %v, want only v2 (v1 deleted)", got)
			}
			if grantedActions(t, role.policies)[spotPrice] {
				t.Fatal("the retired grant is still on the role after the schema bump")
			}

			// An older versioned CLI launches: it must not write v1 back.
			before := fmt.Sprint(role.policies)
			role.calls = nil
			if err := putVersionedRolePolicy(ctx, role, "r", k.base, v1, 1, w.add); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(role.policies) != before || !equalStrings(role.calls, []string{"List"}) {
				t.Errorf("a schema-1 CLI changed a schema-2 role: calls %v, policies %v", role.calls, role.names())
			}
			if len(w) != 1 || !strings.Contains(w[0], "newer spawn") || !strings.Contains(w[0], "v2") {
				t.Errorf("the schema-1 CLI should warn once that a newer spawn owns the role, got %q", w)
			}

			// A pre-#1558 CLI cannot be stopped from writing its own copy...
			role.oldCLIWrites(t, k.base, v1)
			if !grantedActions(t, role.policies)[spotPrice] {
				t.Fatal("test setup: the old CLI's copy should carry the retired grant")
			}
			// ...and the next CLI at the new schema deletes it.
			if err := putVersionedRolePolicy(ctx, role, "r", k.base, v2, 2, w.add); err != nil {
				t.Fatal(err)
			}
			if got := role.names(); !equalStrings(got, []string{versionedPolicyName(k.base, 2)}) {
				t.Errorf("after schema 2 again: %v, want only v2", got)
			}
			if grantedActions(t, role.policies)[spotPrice] {
				t.Error("the retired grant survived the schema-2 launch after the old CLI")
			}
		})
	}
}

// A schema bump on a role that also carries an old CLI's copy deletes that copy
// before writing, so the role never holds three: the nested-spawn role would
// not fit them (TestSporedRolesFitTheInlineLimitWhenMixed).
func TestSporedPolicy_SchemaBumpDeletesTheCoveredCopiesFirst(t *testing.T) {
	role := newFakeRole()
	doc := bound(t, (&Client{}).sporedBaselinePolicyDoc())
	role.policies[sporedBaselinePolicyBase] = doc
	role.policies[versionedPolicyName(sporedBaselinePolicyBase, 1)] = doc
	role.policies[versionedPolicyName(sporedBaselinePolicyBase, 2)] = doc
	if err := putVersionedRolePolicy(context.Background(), role, "r", sporedBaselinePolicyBase, doc, 3, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"List",
		"Delete " + sporedBaselinePolicyBase,
		"Delete " + versionedPolicyName(sporedBaselinePolicyBase, 1),
		"Put " + versionedPolicyName(sporedBaselinePolicyBase, 3),
		"Delete " + versionedPolicyName(sporedBaselinePolicyBase, 2),
	}
	if !equalStrings(role.calls, want) {
		t.Errorf("calls %v,\n want %v", role.calls, want)
	}
	if role.peak > 2*nonSpaceLen(doc) {
		t.Errorf("the role reached %d characters, more than two copies (%d)", role.peak, 2*nonSpaceLen(doc))
	}
}

// Other spawn policies and other versioned names on the role are not touched.
func TestSporedPolicy_LeavesOtherPoliciesAlone(t *testing.T) {
	role := newFakeRole()
	keep := map[string]string{
		"spawn-inline-policy":                   `{"x":1}`,
		"spored-dns-invoke-0a1b2c3d":            `{"x":2}`,
		"spored-baseline-policy-v1":             `{"x":3}`, // the other base
		"spored-policy-vX":                      `{"x":4}`, // not a version
		"spored-policy-v0":                      `{"x":5}`, // not a version
		"spored-policy-v1-something-else-later": `{"x":6}`,
	}
	for n, d := range keep {
		role.policies[n] = d
	}
	doc := bound(t, sporedDCVRolePolicy)
	if err := putVersionedRolePolicy(context.Background(), role, "r", sporedRolePolicyBase, doc, 1, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	for n, d := range keep {
		if role.policies[n] != d {
			t.Errorf("%s was changed or deleted", n)
		}
	}
}

// Without iam:ListRolePolicies the CLI still writes its version (which only
// adds to the role) and says it could not tidy. The launch is not failed.
func TestSporedPolicy_ListDeniedStillWritesAndWarns(t *testing.T) {
	role := newFakeRole()
	role.policies[sporedRolePolicyBase] = `{"old":true}`
	role.listErr = &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to perform iam:ListRolePolicies"}
	var w warnings
	doc := bound(t, sporedDCVRolePolicy)
	if err := putVersionedRolePolicy(context.Background(), role, "r", sporedRolePolicyBase, doc, sporedPolicySchema, w.add); err != nil {
		t.Fatalf("a denied list failed the launch: %v", err)
	}
	if role.policies[sporedRolePolicyName()] != doc {
		t.Error("the versioned policy was not written")
	}
	if _, ok := role.policies[sporedRolePolicyBase]; !ok {
		t.Error("the old copy was deleted without listing")
	}
	if len(w) != 1 || !strings.Contains(w[0], "iam:ListRolePolicies") {
		t.Errorf("warnings %q, want one naming iam:ListRolePolicies", w)
	}
}

// Without iam:DeleteRolePolicy the superseded copy stays; that is clutter, not
// a lost grant, so it is a warning.
func TestSporedPolicy_DeleteDeniedWarns(t *testing.T) {
	role := newFakeRole()
	role.policies[sporedRolePolicyBase] = `{"old":true}`
	role.deleteErr = &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to perform iam:DeleteRolePolicy"}
	var w warnings
	if err := putVersionedRolePolicy(context.Background(), role, "r", sporedRolePolicyBase, bound(t, sporedDCVRolePolicy), 1, w.add); err != nil {
		t.Fatalf("a denied delete failed the launch: %v", err)
	}
	if len(w) != 1 || !strings.Contains(w[0], "delete-role-policy") {
		t.Errorf("warnings %q, want one with the command to remove the copy", w)
	}
}

// A concurrent launch deleting the copy first is success.
func TestSporedPolicy_AlreadyDeletedIsSuccess(t *testing.T) {
	if err := deleteRolePolicy(context.Background(), newFakeRole(), "r", "spored-policy"); err != nil {
		t.Errorf("deleting a policy that is already gone: %v", err)
	}
}

// retryIAM counts LimitExceeded as success. For PutRolePolicy it means the
// policy was not written, so it must fail the launch.
func TestSporedPolicy_SizeLimitIsAnError(t *testing.T) {
	role := newFakeRole()
	role.policies["spawn-scoped-policy"] = `{"filler":"` + strings.Repeat("x", inlinePolicyLimit-100) + `"}`
	err := putVersionedRolePolicy(context.Background(), role, "r", sporedRolePolicyBase, bound(t, sporedDCVRolePolicy), 1, noWarn(t))
	if err == nil || !strings.Contains(err.Error(), "LimitExceeded") {
		t.Errorf("a PutRolePolicy over the size cap returned %v, want the LimitExceeded error", err)
	}
}

func TestParseVersionedPolicyName(t *testing.T) {
	for name, want := range map[string]int{
		"spored-policy-v1":   1,
		"spored-policy-v12":  12,
		"spored-policy":      0,
		"spored-policy-v":    0,
		"spored-policy-v0":   0,
		"spored-policy-v1a":  0,
		"spored-policy-v-1":  0,
		"spored-policy-1":    0,
		"xspored-policy-v1":  0,
		"spored-policy-v1 ":  0,
		"spored-policy-v+1":  0,
		"spored-policy-v1.0": 0,
	} {
		got, ok := parseVersionedPolicyName("spored-policy", name)
		if (want == 0) == ok || got != want {
			t.Errorf("parse %q = (%d, %v), want %d", name, got, ok, want)
		}
	}
	if _, ok := parseVersionedPolicyName(sporedRolePolicyBase, sporedBaselinePolicyName()); ok {
		t.Error("the baseline's versioned name parses as a spored-policy version")
	}
}

// Same schema must mean the same document: two CLIs at one schema overwrite
// each other's copy, which is only harmless if the copies are identical. So a
// change to any spored grant document fails here until sporedPolicySchema is
// bumped (the bump is what lets the change reach roles an older CLI shares),
// and the pins below are updated to the new documents.
func TestSporedPolicySchemaPinsTheDocuments(t *testing.T) {
	const pinnedSchema = 3
	pins := map[string]string{
		"sporedDCVRolePolicy (spored-policy)":            "853f81444f2687748fe3a81622328405127a6a456d0f83fa1902951eac4c7c63",
		"spored baseline (spored-baseline-policy)":       "46fd6e493f4b1449474bf2beda016347e11387e7c0866e5cb5faa71807a88d21",
		"nested-spawn baseline (spored-baseline-policy)": "7a183ec73eb1e0c6910317655e48518ce553a7990dab7811e3a435d6505f3b23",
	}
	docs := map[string]string{
		"sporedDCVRolePolicy (spored-policy)":            sporedDCVRolePolicy,
		"spored baseline (spored-baseline-policy)":       (&Client{}).sporedBaselinePolicyDoc(),
		"nested-spawn baseline (spored-baseline-policy)": (&Client{}).nestedSpawnBaselinePolicyDoc(),
	}
	if sporedPolicySchema != pinnedSchema {
		t.Fatalf("sporedPolicySchema is %d and these pins are for %d: update pinnedSchema and the pins together", sporedPolicySchema, pinnedSchema)
	}
	for what, doc := range docs {
		sum := sha256.Sum256([]byte(doc))
		got := fmt.Sprintf("%x", sum)
		if got != pins[what] {
			t.Errorf("%s changed (sha256 %s, pinned %s) but sporedPolicySchema is still %d.\n"+
				"Bump sporedPolicySchema and update the pins in this test: at the same schema an older "+
				"CLI's copy would replace this one on every role they share (#1558).", what, got, pins[what], sporedPolicySchema)
		}
	}
}

// The inline cap on every role kind, in both of the ways a role holds the
// spored grants (#1573).
//
// Managed (the normal case): the grants are not inline at all, so what must
// fit is the role's other policies plus the copy an old CLI then writes,
// or the old CLI's own launch fails at PutRolePolicy. A schema bump after it
// writes nothing inline either.
//
// Inline (the fallback, when the caller may not create or attach a managed
// policy): the state an old CLI leaves after a new one (two spored copies),
// and the most a schema bump makes while it runs (two again), must fit.
func TestSporedRolesFitTheInlineLimitWhenMixed(t *testing.T) {
	c := &Client{}
	baseline := bound(t, c.sporedBaselinePolicyDoc())
	allTemplates := make([]string, 0, len(PolicyTemplates))
	for n := range PolicyTemplates {
		allTemplates = append(allTemplates, n)
	}
	sort.Strings(allTemplates)
	// What a pre-#1558 CLI writes as spawn-inline-policy: the baseline's
	// statements and the templates'.
	oldInline := func() string {
		p := c.sporedBaselinePolicy()
		st := p["Statement"].([]interface{})
		st = append(st, c.buildInlinePolicy(allTemplates)["Statement"].([]interface{})...)
		p["Statement"] = st
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return bound(t, string(b))
	}()
	newInline, err := json.Marshal(c.buildInlinePolicy(allTemplates))
	if err != nil {
		t.Fatal(err)
	}
	dnsGrant := func(fn string) string {
		b, _ := json.Marshal(map[string]interface{}{"Version": "2012-10-17", "Statement": []interface{}{sporedDNSInvokeStatement(fn)}})
		return string(b)
	}

	cases := []struct {
		name        string
		base        string
		managedBase string
		doc         string
		others      map[string]string // what else the new CLI leaves on the role
		oldOnes     map[string]string // what an old CLI then writes
	}{
		{
			name: "spored-instance-role, two custom DNS updaters",
			base: sporedRolePolicyBase, managedBase: sporedRolePolicyBase, doc: bound(t, sporedDCVRolePolicy),
			others: map[string]string{
				DNSInvokePolicyName("arn:aws:lambda:us-east-1:111122223333:function:a"): dnsGrant("arn:aws:lambda:us-east-1:111122223333:function:a"),
				DNSInvokePolicyName("arn:aws:lambda:us-east-1:111122223333:function:b"): dnsGrant("arn:aws:lambda:us-east-1:111122223333:function:b"),
			},
			oldOnes: map[string]string{sporedRolePolicyBase: bound(t, sporedDCVRolePolicy)},
		},
		{
			name: "nested-spawn role",
			base: sporedBaselinePolicyBase, managedBase: sporedNestedBaselinePolicyBase, doc: bound(t, c.nestedSpawnBaselinePolicyDoc()),
			others:  map[string]string{"spawn-scoped-policy": bound(t, nestedSpawnPolicyDoc())},
			oldOnes: map[string]string{sporedBaselinePolicyBase: bound(t, c.nestedSpawnBaselinePolicyDoc())},
		},
		{
			name: "spawn-instance-* with every --iam-policy template",
			base: sporedBaselinePolicyBase, managedBase: sporedBaselinePolicyBase, doc: baseline,
			others: map[string]string{"spawn-inline-policy": string(newInline)},
			oldOnes: map[string]string{
				sporedBaselinePolicyBase: baseline,
				"spawn-inline-policy":    oldInline,
			},
		},
	}
	for _, tc := range cases {
		for _, managed := range []bool{true, false} {
			mode := map[bool]string{true: "managed", false: "inline fallback"}[managed]
			t.Run(tc.name+", "+mode, func(t *testing.T) {
				ctx := context.Background()
				role := newFakeSporedIAM()
				for n, d := range tc.others {
					role.policies[n] = d
				}
				g := sporedGrant{InlineBase: tc.base, ManagedBase: tc.managedBase, Doc: tc.doc, Account: testAccount, Schema: 1}
				warn := noWarn(t)
				if !managed {
					for _, v := range []int{1, 2} {
						role.errs["CreatePolicy "+g.at(v, tc.doc).managedARN()] = accessDenied("iam:CreatePolicy")
					}
					warn = func(string) {} // the fallback warns, by design
				}
				// new, old, then a schema bump: the fake refuses any write over the cap.
				if err := putSporedGrants(ctx, role, "r", g, warn); err != nil {
					t.Fatalf("new CLI: %v", err)
				}
				for n, d := range tc.oldOnes {
					role.oldCLIWrites(t, n, d)
				}
				if err := putSporedGrants(ctx, role, "r", g.at(2, tc.doc), warn); err != nil {
					t.Fatalf("schema bump on a mixed role: %v", err)
				}
				if managed && countPrefix(role.calls, "Put "+tc.base+"-v") != 0 {
					t.Errorf("the managed path wrote inline: %v", role.calls)
				}
				t.Logf("peak %d of %d characters", role.peak, inlinePolicyLimit)
			})
		}
	}
}

// Against the emulator: the policies a real launch writes, an old CLI's write
// in between, and what is left after each step. Since #1573 the spored grants
// are the managed policy; inline, only what older CLIs wrote, until the next
// launch removes it.
func TestSporedRoles_MixedVersionsAgainstIAM(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	c := NewClientFromConfig(env.AWSConfig)
	ic := env.IAMClient()
	acct, _, err := c.GetCallerIdentityInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Inline policies by name, and spawn's attached managed ones as
	// "managed:<name>".
	roleDocs := func(role string) map[string]string {
		t.Helper()
		out, err := ic.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(role)})
		if err != nil {
			t.Fatal(err)
		}
		docs := map[string]string{}
		for _, n := range out.PolicyNames {
			docs[n] = storedPolicy(t, ic, role, n)
		}
		att, err := ic.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(role)})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range att.AttachedPolicies {
			if n := aws.ToString(a.PolicyName); isSporedManagedPolicyARN(aws.ToString(a.PolicyArn)) {
				docs["managed:"+n] = storedSporedGrant(t, ic, role, n)
			}
		}
		return docs
	}
	oldWrite := func(role, name, doc string) {
		t.Helper()
		b, err := bindPolicyAccount(doc, acct)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ic.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName: aws.String(role), PolicyName: aws.String(name), PolicyDocument: aws.String(b),
		}); err != nil {
			t.Fatalf("old CLI write: %v", err)
		}
	}
	has := func(docs map[string]string, name string) bool { _, ok := docs[name]; return ok }
	only := func(docs map[string]string, want ...string) bool {
		spawn := []string{}
		for _, k := range keys(docs) {
			if strings.HasPrefix(k, "managed:") || strings.HasPrefix(k, "spored-") {
				spawn = append(spawn, k)
			}
		}
		sort.Strings(want)
		return equalStrings(spawn, want)
	}

	// spored-instance-role: new, then v0.161.0, then v0.162.0, then new.
	if _, err := c.SetupSporedIAMRole(ctx); err != nil {
		t.Fatal(err)
	}
	docs := roleDocs(SporedRoleName)
	if !only(docs, "managed:"+sporedRolePolicyName()) {
		t.Fatalf("after a new launch %s carries %v, want only the managed %s", SporedRoleName, keys(docs), sporedRolePolicyName())
	}
	oldWrite(SporedRoleName, sporedRolePolicyBase, docWithout(t, sporedDCVRolePolicy, spotPrice))
	oldWrite(SporedRoleName, sporedRolePolicyName(), sporedDCVRolePolicy)
	if !grantedActions(t, roleDocs(SporedRoleName))[spotPrice] {
		t.Fatalf("older CLIs after the new one stripped %s from %s (#1558)", spotPrice, SporedRoleName)
	}
	if _, err := c.SetupSporedIAMRole(ctx); err != nil {
		t.Fatal(err)
	}
	if docs := roleDocs(SporedRoleName); !only(docs, "managed:"+sporedRolePolicyName()) || !grantedActions(t, docs)[spotPrice] {
		t.Errorf("after new, old, new %s carries %v", SporedRoleName, keys(docs))
	}

	// A spawn-instance-* role an old CLI set up, then a new launch.
	const role = "spawn-test-1558"
	if _, err := c.CreateOrGetInstanceProfile(ctx, IAMRoleConfig{RoleName: role, Policies: []string{"s3:ReadOnly"}}); err != nil {
		t.Fatal(err)
	}
	baselineARN := sporedManagedPolicyARN(acct, sporedBaselinePolicyName())
	if _, err := ic.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(role), PolicyArn: aws.String(baselineARN)}); err != nil {
		t.Fatal(err)
	}
	oldWrite(role, sporedBaselinePolicyBase, docWithout(t, c.sporedBaselinePolicyDoc(), spotPrice))
	if _, err := c.CreateOrGetInstanceProfile(ctx, IAMRoleConfig{RoleName: role, Policies: []string{"s3:ReadOnly"}}); err != nil {
		t.Fatal(err)
	}
	docs = roleDocs(role)
	if !only(docs, "managed:"+sporedBaselinePolicyName()) || !grantedActions(t, docs)[spotPrice] {
		t.Errorf("old then new: %s carries %v", role, keys(docs))
	}
	if !has(docs, "spawn-inline-policy") || strings.Contains(docs["spawn-inline-policy"], "ec2:DescribeTags") {
		t.Error("spawn-inline-policy is missing, or still carries a copy of the spored grants")
	}
}

// The deleters, against the emulator: deleting a spawn-instance-* role
// deletes the baseline's managed policy only with the last role that has it,
// and never spored-instance-role's.
func TestSporedManagedPolicyIsDeletedWithItsLastRole(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	c := NewClientFromConfig(env.AWSConfig)
	ic := env.IAMClient()
	acct, _, err := c.GetCallerIdentityInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetupSporedIAMRole(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"spawn-instance-a1", "spawn-instance-b2"} {
		if _, err := c.CreateOrGetInstanceProfile(ctx, IAMRoleConfig{RoleName: r, Policies: []string{"s3:ReadOnly"}}); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(name string) bool {
		t.Helper()
		_, err := ic.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(sporedManagedPolicyARN(acct, name))})
		if err != nil && !isNoSuchEntity(err) {
			t.Fatal(err)
		}
		return err == nil
	}
	if err := c.DeleteInstanceProfileAndRole(ctx, "spawn-instance-a1"); err != nil {
		t.Fatal(err)
	}
	if !exists(sporedBaselinePolicyName()) {
		t.Fatal("the baseline policy was deleted while spawn-instance-b2 still has it attached")
	}
	if err := c.DeleteInstanceProfileAndRole(ctx, "spawn-instance-b2"); err != nil {
		t.Fatal(err)
	}
	if exists(sporedBaselinePolicyName()) {
		t.Error("the baseline policy outlived the last role that had it")
	}
	if !exists(sporedRolePolicyName()) {
		t.Error("spored-instance-role's policy was deleted")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
