package aws

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	smithy "github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/instancerole"
)

// #1573: the spored grants as a customer-managed policy. These tests run
// putSporedGrants against a fake IAM that enforces the inline size cap, the
// managed policy size cap, the attached-policy quota, DeleteConflict on an
// attached policy, and records every call in order.

// fakeManagedPolicy is one customer-managed policy in the fake account.
type fakeManagedPolicy struct {
	doc         string
	attachments int
}

// fakeSporedIAM is one role ("r") in an account: its inline policies (the
// embedded #1558 fake), its attached managed policies, and the account's
// managed policies.
type fakeSporedIAM struct {
	*fakeRolePolicies
	managed     map[string]*fakeManagedPolicy // by ARN
	attached    []string                      // ARNs attached to the role, in order
	attachLimit int
	errs        map[string]error // operation -> the error it returns
	// beforeAttach, if set, runs once at the first AttachRolePolicy.
	beforeAttach func()
	// attachLag makes AttachRolePolicy answer NoSuchEntity this many more
	// times for a policy that exists: IAM's eventual consistency.
	attachLag int

	// The no-window check: once the role grants every action in required,
	// it must keep granting them after every call.
	required []string
	covered  bool
	gaps     []string
	checkT   *testing.T
}

func init() {
	// The fake is consistent; nothing to wait for.
	iamConsistencyWait = func(int) {}
}

func newFakeSporedIAM() *fakeSporedIAM {
	return &fakeSporedIAM{
		fakeRolePolicies: newFakeRole(),
		managed:          map[string]*fakeManagedPolicy{},
		attachLimit:      10,
		errs:             map[string]error{},
	}
}

func accessDenied(action string) error {
	return &smithy.GenericAPIError{Code: "AccessDenied", Message: "User: arn:aws:iam::123456789012:user/u is not authorized to perform: " + action}
}

func (f *fakeSporedIAM) fail(op string) error {
	f.calls = append(f.calls, op)
	return f.errs[op]
}

// grantedNow is every action the role grants: its inline policies and its
// attached managed policies together, as IAM evaluates them.
func (f *fakeSporedIAM) grantedNow(t *testing.T) map[string]bool {
	docs := map[string]string{}
	for n, d := range f.policies {
		docs["inline:"+n] = d
	}
	for _, a := range f.attached {
		if p, ok := f.managed[a]; ok {
			docs["managed:"+a] = p.doc
		}
	}
	return grantedActions(t, docs)
}

func (f *fakeSporedIAM) check(t *testing.T) {
	if len(f.required) == 0 {
		return
	}
	g := f.grantedNow(t)
	all := true
	for _, a := range f.required {
		if !g[a] {
			all = false
		}
	}
	if all {
		f.covered = true
	} else if f.covered {
		f.gaps = append(f.gaps, f.calls[len(f.calls)-1])
	}
}

func (f *fakeSporedIAM) withCheck(t *testing.T, required ...string) *fakeSporedIAM {
	f.required = required
	f.checkT = t
	f.check(t)
	return f
}

func (f *fakeSporedIAM) after() {
	if f.checkT != nil {
		f.check(f.checkT)
	}
}

func (f *fakeSporedIAM) PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, o ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	if err := f.errs["PutRolePolicy"]; err != nil {
		f.calls = append(f.calls, "Put "+aws.ToString(in.PolicyName))
		return nil, err
	}
	out, err := f.fakeRolePolicies.PutRolePolicy(ctx, in, o...)
	f.after()
	return out, err
}

func (f *fakeSporedIAM) DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, o ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	out, err := f.fakeRolePolicies.DeleteRolePolicy(ctx, in, o...)
	f.after()
	return out, err
}

func (f *fakeSporedIAM) GetRolePolicy(_ context.Context, in *iam.GetRolePolicyInput, _ ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
	if err := f.fail("GetRolePolicy " + aws.ToString(in.PolicyName)); err != nil {
		return nil, err
	}
	d, ok := f.policies[aws.ToString(in.PolicyName)]
	if !ok {
		return nil, &types.NoSuchEntityException{Message: aws.String("no such policy")}
	}
	return &iam.GetRolePolicyOutput{PolicyName: in.PolicyName, PolicyDocument: aws.String(url.QueryEscape(d))}, nil
}

func (f *fakeSporedIAM) ListAttachedRolePolicies(_ context.Context, _ *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	if err := f.fail("ListAttached"); err != nil {
		return nil, err
	}
	out := &iam.ListAttachedRolePoliciesOutput{}
	for _, a := range f.attached {
		out.AttachedPolicies = append(out.AttachedPolicies, types.AttachedPolicy{
			PolicyArn: aws.String(a), PolicyName: aws.String(a[strings.LastIndex(a, "/")+1:]),
		})
	}
	return out, nil
}

func (f *fakeSporedIAM) GetPolicy(_ context.Context, in *iam.GetPolicyInput, _ ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.fail("GetPolicy " + arn); err != nil {
		return nil, err
	}
	p, ok := f.managed[arn]
	if !ok {
		return nil, &types.NoSuchEntityException{Message: aws.String("Policy " + arn + " was not found.")}
	}
	n := int32(p.attachments)
	return &iam.GetPolicyOutput{Policy: &types.Policy{Arn: in.PolicyArn, DefaultVersionId: aws.String("v1"), AttachmentCount: &n}}, nil
}

func (f *fakeSporedIAM) GetPolicyVersion(_ context.Context, in *iam.GetPolicyVersionInput, _ ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.fail("GetPolicyVersion " + arn); err != nil {
		return nil, err
	}
	p, ok := f.managed[arn]
	if !ok {
		return nil, &types.NoSuchEntityException{Message: aws.String("Policy " + arn + " was not found.")}
	}
	return &iam.GetPolicyVersionOutput{PolicyVersion: &types.PolicyVersion{Document: aws.String(url.QueryEscape(p.doc)), VersionId: in.VersionId}}, nil
}

func (f *fakeSporedIAM) CreatePolicy(_ context.Context, in *iam.CreatePolicyInput, _ ...func(*iam.Options)) (*iam.CreatePolicyOutput, error) {
	arn := "arn:aws:iam::" + testAccount + ":policy" + aws.ToString(in.Path) + aws.ToString(in.PolicyName)
	if err := f.fail("CreatePolicy " + arn); err != nil {
		return nil, err
	}
	if _, ok := f.managed[arn]; ok {
		return nil, &types.EntityAlreadyExistsException{Message: aws.String("A policy called " + aws.ToString(in.PolicyName) + " already exists.")}
	}
	if n := nonSpaceLen(aws.ToString(in.PolicyDocument)); n > managedPolicyMaxSize {
		return nil, &types.LimitExceededException{Message: aws.String(fmt.Sprintf("Cannot exceed quota for PolicySize: %d", managedPolicyMaxSize))}
	}
	f.managed[arn] = &fakeManagedPolicy{doc: aws.ToString(in.PolicyDocument)}
	return &iam.CreatePolicyOutput{Policy: &types.Policy{Arn: aws.String(arn)}}, nil
}

func (f *fakeSporedIAM) AttachRolePolicy(_ context.Context, in *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if f.beforeAttach != nil {
		f.beforeAttach()
		f.beforeAttach = nil
	}
	if err := f.fail("Attach " + arn); err != nil {
		return nil, err
	}
	p, ok := f.managed[arn]
	if ok && f.attachLag > 0 {
		f.attachLag--
		ok = false
	}
	if !ok {
		return nil, &types.NoSuchEntityException{Message: aws.String("Policy " + arn + " does not exist or is not attachable.")}
	}
	for _, a := range f.attached {
		if a == arn {
			return &iam.AttachRolePolicyOutput{}, nil
		}
	}
	if len(f.attached) >= f.attachLimit {
		return nil, &types.LimitExceededException{Message: aws.String(fmt.Sprintf("Cannot exceed quota for PoliciesPerRole: %d", f.attachLimit))}
	}
	f.attached = append(f.attached, arn)
	p.attachments++
	f.after()
	return &iam.AttachRolePolicyOutput{}, nil
}

func (f *fakeSporedIAM) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.fail("Detach " + arn); err != nil {
		return nil, err
	}
	for i, a := range f.attached {
		if a == arn {
			f.attached = append(f.attached[:i], f.attached[i+1:]...)
			if p, ok := f.managed[arn]; ok {
				p.attachments--
			}
			f.after()
			return &iam.DetachRolePolicyOutput{}, nil
		}
	}
	return nil, &types.NoSuchEntityException{Message: aws.String("Policy " + arn + " was not found.")}
}

func (f *fakeSporedIAM) DeletePolicy(_ context.Context, in *iam.DeletePolicyInput, _ ...func(*iam.Options)) (*iam.DeletePolicyOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.fail("DeletePolicy " + arn); err != nil {
		return nil, err
	}
	p, ok := f.managed[arn]
	if !ok {
		return nil, &types.NoSuchEntityException{Message: aws.String("Policy " + arn + " was not found.")}
	}
	if p.attachments > 0 {
		return nil, &types.DeleteConflictException{Message: aws.String("Cannot delete a policy attached to entities.")}
	}
	delete(f.managed, arn)
	f.after()
	return &iam.DeletePolicyOutput{}, nil
}

// attach puts a managed policy with doc on the role, as some earlier CLI did.
func (f *fakeSporedIAM) attach(name, doc string, otherRoles int) string {
	arn := sporedManagedPolicyARN(testAccount, name)
	f.managed[arn] = &fakeManagedPolicy{doc: doc, attachments: 1 + otherRoles}
	f.attached = append(f.attached, arn)
	return arn
}

// The three grant kinds and their documents.
func sporedGrantKinds(t *testing.T) []sporedGrant {
	c := &Client{}
	mk := func(inline, managed, doc string) sporedGrant {
		return sporedGrant{InlineBase: inline, ManagedBase: managed, Doc: bound(t, doc), Account: testAccount, Schema: 1}
	}
	return []sporedGrant{
		mk(sporedRolePolicyBase, sporedRolePolicyBase, sporedDCVRolePolicy),
		mk(sporedBaselinePolicyBase, sporedBaselinePolicyBase, c.sporedBaselinePolicyDoc()),
		mk(sporedBaselinePolicyBase, sporedNestedBaselinePolicyBase, c.nestedSpawnBaselinePolicyDoc()),
	}
}

func (g sporedGrant) at(schema int, doc string) sporedGrant {
	g.Schema, g.Doc = schema, doc
	return g
}

func indexOf(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func countPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// fillerPolicy is an inline policy of exactly n characters, whitespace not
// counted, as another tenant of the role might have written.
func fillerPolicy(n int) string {
	const head, tail = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::`, `"}]}`
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

// The issue's account: spored-instance-role already carries 13 inline
// policies, 7,676 characters, the legacy spored-policy (2,775 of them) among
// them. v0.162.0 failed here with LimitExceeded on every default-role launch.
// The managed path succeeds without writing inline at all, and then removes
// the legacy copy it supersedes, leaving the other tenants' policies alone.
func TestSporedGrants_AFullInlineBudgetIsNoObstacle(t *testing.T) {
	for _, g := range sporedGrantKinds(t) {
		t.Run(g.managedName(), func(t *testing.T) {
			f := newFakeSporedIAM()
			legacy := docWithout(t, g.Doc, spotPrice) // what v0.161.0 wrote
			f.policies[g.InlineBase] = legacy
			others := map[string]string{"spored-instance-policy": fillerPolicy(303), "calque-spike-access": fillerPolicy(696)}
			for i, n := range []int{441, 441, 441, 442} {
				others[fmt.Sprintf("omics-demo-%d", i)] = fillerPolicy(n)
			}
			for i, n := range []int{356, 356, 356, 356, 356, 357} {
				others[fmt.Sprintf("microbiome-demo-%d", i)] = fillerPolicy(n)
			}
			// The spawn-instance-* baselines are smaller than spored-policy;
			// a user's --iam-policy-file takes up the difference.
			if n := inlinePolicyLimit - 4901 - nonSpaceLen(legacy) - nonSpaceLen(g.Doc) + 200; n > 0 {
				others["spawn-custom-policy"] = fillerPolicy(n + 120)
			}
			for n, d := range others {
				f.policies[n] = d
			}
			if got := f.size(); got+nonSpaceLen(g.Doc) <= inlinePolicyLimit {
				t.Fatalf("test setup: the role's inline policies are %d characters, which leaves room for %s", got, g.managedName())
			}
			if err := putSporedGrants(context.Background(), f, "r", g, noWarn(t)); err != nil {
				t.Fatalf("a full inline budget failed the launch again (#1573): %v", err)
			}
			if n := countPrefix(f.calls, "Put "); n != 0 {
				t.Errorf("wrote %d inline policies, want none: %v", n, f.calls)
			}
			if got, want := f.attached, []string{g.managedARN()}; !equalStrings(got, want) {
				t.Errorf("attached %v, want %v", got, want)
			}
			if p := f.managed[g.managedARN()]; p == nil || p.doc != g.Doc {
				t.Errorf("the managed policy is not this CLI's document")
			}
			if _, ok := f.policies[g.InlineBase]; ok {
				t.Errorf("the legacy inline %s is still on the role", g.InlineBase)
			}
			for n, d := range others {
				if f.policies[n] != d {
					t.Errorf("another tenant's inline policy %s was changed or removed", n)
				}
			}
			if a, d := indexOf(f.calls, "Attach "), indexOf(f.calls, "Delete "+g.InlineBase); a < 0 || d < a {
				t.Errorf("the legacy copy went before the managed policy was attached: %v", f.calls)
			}
		})
	}
}

// The calls a launch makes, by scenario. The live verification on #1573
// replays these against a real account.
func TestSporedGrants_CallSequences(t *testing.T) {
	g := sporedGrantKinds(t)[0]
	arn := g.managedARN()

	// A role an old CLI set up: create, attach, then remove the legacy copy.
	f := newFakeSporedIAM()
	f.policies[g.InlineBase] = g.Doc
	f.policies["spored-policy-v1"] = g.Doc // v0.162.0/v0.163.0, on an account with room
	if err := putSporedGrants(context.Background(), f, "r", g, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	want := []string{"List", "ListAttached", "GetPolicy " + arn, "CreatePolicy " + arn, "Attach " + arn,
		"Delete " + g.InlineBase, "Delete spored-policy-v1"}
	if !equalStrings(f.calls, want) {
		t.Errorf("first launch:\n got  %v\n want %v", f.calls, want)
	}

	// The next launch: nothing to write, the document checked.
	f.calls = nil
	if err := putSporedGrants(context.Background(), f, "r", g, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	want = []string{"List", "ListAttached", "GetPolicy " + arn, "GetPolicyVersion " + arn}
	if !equalStrings(f.calls, want) {
		t.Errorf("steady state:\n got  %v\n want %v", f.calls, want)
	}

	// Another role of the kind in the same account: the policy is reused.
	f2 := newFakeSporedIAM()
	f2.managed = f.managed
	if err := putSporedGrants(context.Background(), f2, "r2", g, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	want = []string{"List", "ListAttached", "GetPolicy " + arn, "GetPolicyVersion " + arn, "Attach " + arn}
	if !equalStrings(f2.calls, want) {
		t.Errorf("second role:\n got  %v\n want %v", f2.calls, want)
	}
	if f.managed[arn].attachments != 2 {
		t.Errorf("attachments = %d, want 2", f.managed[arn].attachments)
	}
}

// Credentials without the managed-policy actions get today's behaviour: the
// grants written inline, the legacy copy removed after, and a warning that
// names what to grant. Whichever managed call is refused.
func TestSporedGrants_AccessDeniedFallsBackToInline(t *testing.T) {
	g := sporedGrantKinds(t)[0]
	arn := g.managedARN()
	for op, action := range map[string]string{
		"ListAttached":        "iam:ListAttachedRolePolicies",
		"GetPolicy " + arn:    "iam:GetPolicy",
		"CreatePolicy " + arn: "iam:CreatePolicy",
		"Attach " + arn:       "iam:AttachRolePolicy",
	} {
		t.Run(action, func(t *testing.T) {
			f := newFakeSporedIAM().withCheck(t, spotPrice)
			f.policies[g.InlineBase] = g.Doc
			f.errs[op] = accessDenied(action)
			var w warnings
			if err := putSporedGrants(context.Background(), f, "r", g, w.add); err != nil {
				t.Fatalf("AccessDenied on %s failed the launch: %v", action, err)
			}
			if got := f.names(); !equalStrings(got, []string{versionedPolicyName(g.InlineBase, 1)}) {
				t.Errorf("inline policies %v, want only %s", got, versionedPolicyName(g.InlineBase, 1))
			}
			if len(f.attached) != 0 {
				t.Errorf("attached %v after a refused managed path", f.attached)
			}
			if len(w) != 1 || !strings.Contains(w[0], action) || !strings.Contains(w[0], "iam:CreatePolicy") || !strings.Contains(w[0], "inline") {
				t.Errorf("warnings %q, want one naming the refusal and the actions to grant", w)
			}
			if len(f.gaps) > 0 {
				t.Errorf("the role was without its grants after %v", f.gaps)
			}
		})
	}
}

// And when inline does not fit either, the error is one a user can act on.
func TestSporedGrants_InlineLimitIsExplained(t *testing.T) {
	g := sporedGrantKinds(t)[0]
	f := newFakeSporedIAM()
	f.policies[g.InlineBase] = docWithout(t, g.Doc, spotPrice)
	f.policies["omics-demo-a"] = fillerPolicy(3000)
	f.policies["microbiome-demo-b"] = fillerPolicy(3500)
	f.errs["CreatePolicy "+g.managedARN()] = accessDenied("iam:CreatePolicy")
	var w warnings
	err := putSporedGrants(context.Background(), f, "r", g, w.add)
	if err == nil {
		t.Fatal("an inline write over the cap succeeded")
	}
	msg := err.Error()
	total := nonSpaceLen(f.policies[g.InlineBase]) + 3000 + 3500
	for _, want := range []string{
		"spored-policy  (spawn's own",
		fmt.Sprintf("%8d  omics-demo-a", 3000),
		fmt.Sprintf("%8d  microbiome-demo-b", 3500),
		fmt.Sprintf("%8d  total, of 10240", total),
		"iam:CreatePolicy", "iam:AttachRolePolicy", "aws iam delete-role-policy --role-name r",
		"not allowed to create the managed policy",
		"LimitExceeded",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not say %q:\n%s", want, msg)
		}
	}
	if strings.HasPrefix(msg, "attach ") || strings.HasPrefix(msg, "operation error") {
		t.Errorf("the error leads with the raw IAM failure:\n%s", msg)
	}
	if _, ok := f.policies[g.InlineBase]; !ok {
		t.Error("the legacy copy was deleted although nothing replaced it")
	}
}

// A newer schema on the role, managed or inline: this CLI changes nothing.
func TestSporedGrants_NewerSchemaDefers(t *testing.T) {
	g := sporedGrantKinds(t)[0]
	for name, setup := range map[string]func(f *fakeSporedIAM){
		"managed v2": func(f *fakeSporedIAM) { f.attach(versionedPolicyName(g.ManagedBase, 2), g.Doc, 0) },
		"inline v2":  func(f *fakeSporedIAM) { f.policies[versionedPolicyName(g.InlineBase, 2)] = g.Doc },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSporedIAM()
			setup(f)
			f.policies[g.InlineBase] = g.Doc
			before := fmt.Sprint(f.policies, f.attached, len(f.managed))
			var w warnings
			if err := putSporedGrants(context.Background(), f, "r", g, w.add); err != nil {
				t.Fatal(err)
			}
			if after := fmt.Sprint(f.policies, f.attached, len(f.managed)); after != before {
				t.Errorf("a schema-1 CLI changed a schema-2 role:\n before %s\n after  %s\n calls %v", before, after, f.calls)
			}
			if !equalStrings(f.calls, []string{"List", "ListAttached"}) {
				t.Errorf("calls %v, want only the two lists", f.calls)
			}
			if len(w) != 1 || !strings.Contains(w[0], "newer spawn") || !strings.Contains(w[0], "v2") {
				t.Errorf("warnings %q, want one about the newer spawn", w)
			}
		})
	}
}

// No window without grants, in every transition: from the legacy inline copy,
// from v0.162.0's inline version, and across a schema bump of the managed
// policy. The fake checks the role's grants after every call.
func TestSporedGrants_NeverLeavesTheRoleWithoutItsGrants(t *testing.T) {
	g := sporedGrantKinds(t)[1]
	for name, setup := range map[string]func(f *fakeSporedIAM){
		"legacy inline":      func(f *fakeSporedIAM) { f.policies[g.InlineBase] = g.Doc },
		"v0.162.0 inline v1": func(f *fakeSporedIAM) { f.policies[versionedPolicyName(g.InlineBase, 1)] = g.Doc },
		"both inline": func(f *fakeSporedIAM) {
			f.policies[g.InlineBase] = g.Doc
			f.policies[versionedPolicyName(g.InlineBase, 1)] = g.Doc
		},
		"managed v1, to v2": func(f *fakeSporedIAM) { f.attach(versionedPolicyName(g.ManagedBase, 1), g.Doc, 0) },
		"all of them, to v2": func(f *fakeSporedIAM) {
			f.attach(versionedPolicyName(g.ManagedBase, 1), g.Doc, 0)
			f.policies[g.InlineBase] = g.Doc
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSporedIAM()
			setup(f)
			f.withCheck(t, "ec2:DescribeTags", "ec2:TerminateInstances", spotPrice)
			if !f.covered {
				t.Fatal("test setup: the role does not start with the grants")
			}
			target := g
			if strings.HasSuffix(name, "to v2") {
				target = g.at(2, g.Doc)
			}
			if err := putSporedGrants(context.Background(), f, "r", target, noWarn(t)); err != nil {
				t.Fatal(err)
			}
			if len(f.gaps) > 0 {
				t.Errorf("the role was without its grants after %v (calls %v)", f.gaps, f.calls)
			}
			if got := f.names(); len(got) != 0 {
				t.Errorf("inline copies left: %v", got)
			}
			if got, want := f.attached, []string{target.managedARN()}; !equalStrings(got, want) {
				t.Errorf("attached %v, want %v", got, want)
			}
		})
	}
}

// A schema bump detaches the older managed version and deletes it when no
// other role has it; when one does, IAM refuses (DeleteConflict) and it is
// kept, quietly.
func TestSporedGrants_SchemaBumpRetiresTheOlderManagedVersion(t *testing.T) {
	g := sporedGrantKinds(t)[0]
	for _, others := range []int{0, 1} {
		t.Run(fmt.Sprintf("%d other roles", others), func(t *testing.T) {
			f := newFakeSporedIAM()
			v1 := f.attach(versionedPolicyName(g.ManagedBase, 1), g.Doc, others)
			v2 := g.at(2, docWithout(t, g.Doc, spotPrice))
			if err := putSporedGrants(context.Background(), f, "r", v2, noWarn(t)); err != nil {
				t.Fatal(err)
			}
			if a, d := indexOf(f.calls, "Attach "+v2.managedARN()), indexOf(f.calls, "Detach "+v1); a < 0 || d < a {
				t.Errorf("v1 was detached before v2 was attached: %v", f.calls)
			}
			_, kept := f.managed[v1]
			if kept != (others > 0) {
				t.Errorf("v1 kept = %v with %d other roles attached", kept, others)
			}
			if f.grantedNow(t)[spotPrice] {
				t.Error("the grant v2 retired is still on the role")
			}
		})
	}
}

// A policy at this CLI's name with another document is never rewritten or
// attached; this CLI's grants go inline beside it, and the warning names it.
func TestSporedGrants_ExistingPolicyWithAnotherDocumentIsLeftAlone(t *testing.T) {
	g := sporedGrantKinds(t)[0]
	other := docWithout(t, g.Doc, spotPrice) // a development build's, say
	for _, onRole := range []bool{false, true} {
		t.Run(fmt.Sprintf("attached=%v", onRole), func(t *testing.T) {
			f := newFakeSporedIAM()
			if onRole {
				f.attach(g.managedName(), other, 0)
			} else {
				f.managed[g.managedARN()] = &fakeManagedPolicy{doc: other}
			}
			var w warnings
			if err := putSporedGrants(context.Background(), f, "r", g, w.add); err != nil {
				t.Fatal(err)
			}
			if f.managed[g.managedARN()].doc != other {
				t.Error("the existing policy was rewritten")
			}
			if onRole != (len(f.attached) == 1) {
				t.Errorf("attached %v: a policy this CLI did not write was attached, or one was detached", f.attached)
			}
			if f.policies[versionedPolicyName(g.InlineBase, 1)] != g.Doc {
				t.Error("this CLI's grants were not written inline beside it")
			}
			if len(w) != 1 || !strings.Contains(w[0], g.managedARN()) || !strings.Contains(w[0], "did not write") {
				t.Errorf("warnings %q, want one naming the policy", w)
			}
			if !f.grantedNow(t)[spotPrice] {
				t.Error("this CLI's grant is missing")
			}
		})
	}
}

// The attached-policy quota: a role with ten managed policies attached gets
// the grants inline, and the warning says why.
func TestSporedGrants_AttachedPolicyQuotaFallsBack(t *testing.T) {
	g := sporedGrantKinds(t)[1]
	f := newFakeSporedIAM()
	for i := 0; i < 10; i++ {
		arn := fmt.Sprintf("arn:aws:iam::%s:policy/team-%d", testAccount, i)
		f.managed[arn] = &fakeManagedPolicy{doc: "{}", attachments: 1}
		f.attached = append(f.attached, arn)
	}
	var w warnings
	if err := putSporedGrants(context.Background(), f, "r", g, w.add); err != nil {
		t.Fatal(err)
	}
	if f.policies[versionedPolicyName(g.InlineBase, 1)] != g.Doc {
		t.Error("the grants were not written inline")
	}
	if len(w) != 1 || !strings.Contains(w[0], "10 managed policies attached") || !strings.Contains(w[0], "team-3") {
		t.Errorf("warnings %q, want one with the count and the names", w)
	}
}

// A policy deleted between being found and being attached (the unattached
// sweep, or a concurrent schema bump) is created again.
func TestSporedGrants_PolicyGoneBeforeAttachIsRecreated(t *testing.T) {
	g := sporedGrantKinds(t)[0]
	f := newFakeSporedIAM()
	f.managed[g.managedARN()] = &fakeManagedPolicy{doc: g.Doc}
	f.beforeAttach = func() { delete(f.managed, g.managedARN()) }
	if err := putSporedGrants(context.Background(), f, "r", g, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(f.attached, []string{g.managedARN()}) || f.managed[g.managedARN()] == nil {
		t.Errorf("attached %v, calls %v", f.attached, f.calls)
	}
}

// Every released kind of CLI, in turn, on one role: this one (managed), a
// v0.161.0 (legacy inline, without the newer grant), a v0.162.0 (inline v1),
// and this one again. The newer grant never leaves the role, and the last
// launch leaves only the managed policy.
func TestSporedGrants_MixedReleasedCLIs(t *testing.T) {
	for _, g := range sporedGrantKinds(t) {
		t.Run(g.managedName(), func(t *testing.T) {
			ctx := context.Background()
			f := newFakeSporedIAM()
			if err := putSporedGrants(ctx, f, "r", g, noWarn(t)); err != nil {
				t.Fatal(err)
			}
			f.withCheck(t, spotPrice)
			f.oldCLIWrites(t, g.InlineBase, docWithout(t, g.Doc, spotPrice)) // v0.161.0
			// v0.162.0: lists inline only, writes v1 and deletes the legacy copy.
			if err := putVersionedRolePolicy(ctx, f, "r", g.InlineBase, g.Doc, 1, noWarn(t)); err != nil {
				t.Fatal(err)
			}
			if err := putSporedGrants(ctx, f, "r", g, noWarn(t)); err != nil {
				t.Fatal(err)
			}
			if len(f.gaps) > 0 {
				t.Errorf("%s left the role after %v", spotPrice, f.gaps)
			}
			if got := f.names(); len(got) != 0 {
				t.Errorf("inline copies left: %v", got)
			}
			if !equalStrings(f.attached, []string{g.managedARN()}) {
				t.Errorf("attached %v", f.attached)
			}
		})
	}
}

// Every spored document fits a managed policy, with room for the next grant.
func TestSporedPolicyDocumentsFitAManagedPolicy(t *testing.T) {
	for _, g := range sporedGrantKinds(t) {
		n := nonSpaceLen(g.Doc)
		t.Logf("%s: %d of %d characters", g.managedName(), n, managedPolicyMaxSize)
		if n > managedPolicyMaxSize {
			t.Errorf("%s is %d characters, over IAM's %d for a managed policy", g.managedName(), n, managedPolicyMaxSize)
		}
	}
}

// The managed policy names: one per document, per account, at the path.
func TestSporedManagedPolicyNames(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range sporedGrantKinds(t) {
		if seen[g.managedName()] {
			t.Errorf("two documents share the managed policy name %s", g.managedName())
		}
		seen[g.managedName()] = true
		if !isSporedManagedPolicyARN(g.managedARN()) {
			t.Errorf("%s is not recognised as a spored managed policy", g.managedARN())
		}
	}
	for arn, want := range map[string]bool{
		"arn:aws:iam::123456789012:policy/spore-host/spored-policy-v1":                  true,
		"arn:aws:iam::123456789012:policy/spore-host/spored-nested-baseline-policy-v12": true,
		"arn:aws:iam::123456789012:policy/spore-host/spored-policy":                     false,
		"arn:aws:iam::123456789012:policy/spored-policy-v1":                             false,
		"arn:aws:iam::123456789012:policy/spore-host/x/spored-policy-v1":                false,
		"arn:aws:iam::123456789012:policy/spore-host/team-policy-v1":                    false,
		"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore":                          false,
	} {
		if got := isSporedManagedPolicyARN(arn); got != want {
			t.Errorf("isSporedManagedPolicyARN(%s) = %v, want %v", arn, got, want)
		}
	}
	got := append([]string(nil), sporedManagedPolicyBases...)
	sort.Strings(got)
	if want := []string{sporedBaselinePolicyBase, sporedNestedBaselinePolicyBase, sporedRolePolicyBase}; !equalStrings(got, want) {
		t.Errorf("sporedManagedPolicyBases = %v", got)
	}
}

// The deleter refuses anything that is not a spored policy at the path, and
// counts "still attached elsewhere" as done.
func TestDeleteSporedManagedPolicy(t *testing.T) {
	f := newFakeSporedIAM()
	used := f.attach("spored-policy-v1", "{}", 0)
	if err := deleteSporedManagedPolicy(context.Background(), f, used); err != nil {
		t.Errorf("an attached policy: %v, want nil (kept)", err)
	}
	if f.managed[used] == nil {
		t.Error("an attached policy was deleted")
	}
	free := sporedManagedPolicyARN(testAccount, "spored-baseline-policy-v1")
	f.managed[free] = &fakeManagedPolicy{doc: "{}"}
	if err := deleteSporedManagedPolicy(context.Background(), f, free); err != nil || f.managed[free] != nil {
		t.Errorf("an unattached policy: err %v, still there %v", err, f.managed[free] != nil)
	}
	if err := deleteSporedManagedPolicy(context.Background(), f, "arn:aws:iam::123456789012:policy/team-policy"); err == nil {
		t.Error("deleted a policy that is not spawn's")
	}
}

// pkg/instancerole repeats the path and the names so the reaper need not
// import pkg/aws; they must not drift.
func TestInstanceroleAgreesOnTheSporedPolicies(t *testing.T) {
	if instancerole.SporedPolicyPath != sporedPolicyPath {
		t.Errorf("instancerole.SporedPolicyPath = %q, pkg/aws has %q", instancerole.SporedPolicyPath, sporedPolicyPath)
	}
	for _, b := range sporedManagedPolicyBases {
		for _, n := range []string{b + "-v1", b + "-v27"} {
			if !instancerole.IsSporedPolicyName(n) {
				t.Errorf("instancerole does not recognise %s", n)
			}
		}
	}
	for _, n := range []string{"spored-dns-invoke-0a1b2c3d", "team-v1", "spored-policy"} {
		if instancerole.IsSporedPolicyName(n) != isSporedManagedPolicyName(n) {
			t.Errorf("the two packages disagree on %s", n)
		}
	}
}

// Any other failure on the managed path falls back too: it must not become a
// launch failure that v0.163.0 did not have.
func TestSporedGrants_UnexpectedManagedErrorFallsBack(t *testing.T) {
	g := sporedGrantKinds(t)[0]
	f := newFakeSporedIAM()
	f.errs["GetPolicy "+g.managedARN()] = &smithy.GenericAPIError{Code: "InvalidInput", Message: "ARN arn:aws:iam::... is not valid."}
	var w warnings
	if err := putSporedGrants(context.Background(), f, "r", g, w.add); err != nil {
		t.Fatalf("an unexpected managed-path error failed the launch: %v", err)
	}
	if f.policies[versionedPolicyName(g.InlineBase, 1)] != g.Doc {
		t.Error("the grants were not written inline")
	}
	if len(w) != 1 || !strings.Contains(w[0], "InvalidInput") {
		t.Errorf("warnings %q", w)
	}
}

// The policy ARN is in the caller's partition.
func TestSporedManagedPolicyARNPartition(t *testing.T) {
	g := sporedGrant{ManagedBase: sporedRolePolicyBase, Account: testAccount, Schema: 1}
	if got := g.managedARN(); got != "arn:aws:iam::123456789012:policy/spore-host/spored-policy-v1" {
		t.Errorf("default partition: %s", got)
	}
	g.Partition = "aws-us-gov"
	if got := g.managedARN(); got != "arn:aws-us-gov:iam::123456789012:policy/spore-host/spored-policy-v1" || !isSporedManagedPolicyARN(got) {
		t.Errorf("GovCloud: %s", got)
	}
}

// A policy just created is not yet attachable (IAM's eventual consistency):
// spawn waits for it rather than falling back to inline, which on a full
// role would fail the launch.
func TestSporedGrants_NewPolicyNotYetVisibleIsWaitedFor(t *testing.T) {
	g := sporedGrantKinds(t)[0]
	f := newFakeSporedIAM()
	f.attachLag = attachConsistencyTries - 1
	if err := putSporedGrants(context.Background(), f, "r", g, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(f.attached, []string{g.managedARN()}) || countPrefix(f.calls, "Put ") != 0 {
		t.Errorf("attached %v, calls %v", f.attached, f.calls)
	}
	if n := countPrefix(f.calls, "CreatePolicy "); n != 1 {
		t.Errorf("CreatePolicy called %d times, want 1", n)
	}
}
