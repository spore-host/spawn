package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// Spored grants as customer-managed policies (#1573).
//
// #1558 made the spored grants versioned inline policies. That was right for
// correctness and wrong for size: IAM caps a role's inline policies at 10,240
// characters together (whitespace not counted), and spored-instance-role is
// shared with whatever else an account attaches to it inline. On a lived-in
// account it already carried 7.7 KB of other tenants' policies plus the legacy
// spored-policy (2,775), so writing spored-policy-v1 beside them failed with
// LimitExceeded and every launch on the default role failed before an instance
// existed. The deletes of the older copies never ran, because the write came
// first.
//
// So this CLI writes the grants as a customer-managed policy and attaches it
// to the role. A managed policy has its own 6,144-character budget and does
// not count against the role's inline 10,240. Everything #1558 established
// still holds:
//
//   - No released CLI touches it. The policy is at sporedPolicyPath, a path no
//     CLI before this one writes, and IAM grants the union of a role's inline
//     and managed policies, so an older CLI rewriting its inline name can only
//     add to what this one attached.
//   - The schema version is in the name, and same schema means same document
//     (TestSporedPolicySchemaPinsTheDocuments). A role carrying a newer schema,
//     inline or managed, is left alone with a warning.
//
// Naming. One policy per account per grant document per schema:
//
//	arn:aws:iam::<account>:policy/spore-host/<base>-v<schema>
//
// with <base> spored-policy (spored-instance-role), spored-baseline-policy
// (every spawn-instance-* role) or spored-nested-baseline-policy (the
// --nested-spawn role, whose baseline differs in one condition). The documents
// vary by nothing but the account they are bound to (bindPolicyAccount): not
// by region, since IAM is global and every Resource in them names all
// regions, and not by role, since every role of a kind gets the same one. So
// one policy serves every role of its kind in the account, and the number of
// spored policies is bounded by three per schema.
//
// A policy that exists with a different document. At one schema it cannot
// happen with released CLIs; it can with a branch build that changed a
// document without bumping the schema, or a hand edit. spawn never rewrites
// it: it never calls CreatePolicyVersion, so IAM's five-version cap never
// applies, and it does not attach a document it did not write. It warns,
// names the policy, and writes this CLI's grants inline as before, so the
// instance still gets exactly this CLI's grants (in union with whatever is
// attached already).
//
// Falling back. Every way the managed path can be unavailable falls back to
// the inline write #1558 did (writeInlineVersion), so this introduces no new
// failure: credentials without the managed-policy actions (AccessDenied), a
// role at its attached-policy quota, an account at its managed-policy quota,
// or the conflict above. If the inline write then hits the size cap, the
// error lists the role's inline policies with their sizes and names both
// fixes (inlineBudgetError), rather than the bare 409.
//
// Order. The policy is attached before any inline copy is deleted, so a
// running instance on the role always holds one of them. The superseded
// copies are the legacy unversioned inline policy, every inline version up to
// this schema (v0.162.0 and v0.163.0 wrote this schema inline, on accounts
// that had room), and every older managed version, which is detached from the
// role and then deleted if no other role still has it attached.
//
// Leave no trace. The deleters: the supersession above; pkg/aws.deleteRole
// (spawn cleanup, `spawn pool drain`) deletes a spored managed policy it
// detaches once nothing else has it attached; pkg/instancerole.Delete (the
// reaper and `spawn cleanup --instance-roles`) detaches it, and
// pkg/instancerole.SweepSporedPolicies, run by `spawn cleanup
// --instance-roles`, deletes spored policies at sporedPolicyPath that nothing
// has had attached for a day.

// sporedPolicyPath is the IAM path of every managed policy spawn creates. A
// policy there is spawn's, and only spawn attaches it.
const sporedPolicyPath = "/spore-host/"

// sporedNestedBaselinePolicyBase is the managed policy name base for the
// --nested-spawn role's baseline (nestedSpawnBaselinePolicyDoc). Inline, it
// shares spored-baseline-policy with every other spawn-instance-* role, which
// was harmless because an inline name is per role; a managed policy name is
// per account, so the two documents need two names.
const sporedNestedBaselinePolicyBase = "spored-nested-baseline-policy"

// sporedManagedPolicyBases are the name bases of the managed policies spawn
// writes at sporedPolicyPath: "<base>-v<schema>". Exported for
// pkg/instancerole's sweep.
var sporedManagedPolicyBases = []string{sporedRolePolicyBase, sporedBaselinePolicyBase, sporedNestedBaselinePolicyBase}

// isSporedManagedPolicyName reports whether name is a managed policy name
// spawn writes at sporedPolicyPath.
func isSporedManagedPolicyName(name string) bool {
	for _, b := range sporedManagedPolicyBases {
		if _, ok := parseVersionedPolicyName(b, name); ok {
			return true
		}
	}
	return false
}

// isSporedManagedPolicyARN reports whether arn is one of spawn's spored
// managed policies (at sporedPolicyPath, with a spored name).
func isSporedManagedPolicyARN(arn string) bool {
	_, rest, ok := strings.Cut(arn, ":policy"+sporedPolicyPath)
	return ok && !strings.Contains(rest, "/") && isSporedManagedPolicyName(rest)
}

// inlinePolicyBudget is IAM's cap on a role's inline policies together, and
// managedPolicyMaxSize on one managed policy, both in characters with
// whitespace not counted.
const (
	inlinePolicyBudget   = 10240
	managedPolicyMaxSize = 6144
)

// sporedPolicyAPI is the part of the IAM client putSporedGrants uses.
type sporedPolicyAPI interface {
	rolePolicyAPI
	iam.ListAttachedRolePoliciesAPIClient
	GetRolePolicy(ctx context.Context, in *iam.GetRolePolicyInput, optFns ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	GetPolicy(ctx context.Context, in *iam.GetPolicyInput, optFns ...func(*iam.Options)) (*iam.GetPolicyOutput, error)
	GetPolicyVersion(ctx context.Context, in *iam.GetPolicyVersionInput, optFns ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error)
	CreatePolicy(ctx context.Context, in *iam.CreatePolicyInput, optFns ...func(*iam.Options)) (*iam.CreatePolicyOutput, error)
	AttachRolePolicy(ctx context.Context, in *iam.AttachRolePolicyInput, optFns ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	DetachRolePolicy(ctx context.Context, in *iam.DetachRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	DeletePolicy(ctx context.Context, in *iam.DeletePolicyInput, optFns ...func(*iam.Options)) (*iam.DeletePolicyOutput, error)
}

// sporedGrant is one spored grant document and where it goes.
type sporedGrant struct {
	// InlineBase is the inline name base: what CLIs up to v0.161.0 wrote
	// unversioned, and v0.162.0/v0.163.0 versioned.
	InlineBase string
	// ManagedBase is the managed policy name base.
	ManagedBase string
	// Doc is the document, bound to Account.
	Doc     string
	Account string
	// Partition is the account's AWS partition ("aws" when empty).
	Partition string
	Schema    int
}

func (g sporedGrant) managedName() string { return versionedPolicyName(g.ManagedBase, g.Schema) }

func (g sporedGrant) managedARN() string {
	p := g.Partition
	if p == "" {
		p = "aws"
	}
	return "arn:" + p + ":iam::" + g.Account + ":policy" + sporedPolicyPath + g.managedName()
}

// sporedManagedPolicyARN is the ARN of spawn's managed policy name in account,
// in the aws partition.
func sporedManagedPolicyARN(account, name string) string {
	return "arn:aws:iam::" + account + ":policy" + sporedPolicyPath + name
}

// putSporedGrants gives role g's grants: attached as a managed policy where it
// can, written inline (writeInlineVersion) where it cannot. See the comment at
// the top of this file.
//
// The IAM calls, in order: ListRolePolicies and ListAttachedRolePolicies on
// the role; GetPolicy, then CreatePolicy if there is none, or GetPolicyVersion
// to compare the document if there is; AttachRolePolicy unless it is attached
// already; then DeleteRolePolicy of each superseded inline copy and
// DetachRolePolicy + DeletePolicy of each superseded managed version.
func putSporedGrants(ctx context.Context, api sporedPolicyAPI, role string, g sporedGrant, warn func(string)) error {
	if n := nonSpaceLen(g.Doc); n > managedPolicyMaxSize {
		// Cannot happen with the documents spawn ships
		// (TestSporedPolicyDocumentsFitAManagedPolicy); inline still works.
		warn(fmt.Sprintf("the spored grants for IAM role %s are %d characters, over IAM's %d for a managed policy; "+
			"writing them inline (#1573).", role, n, managedPolicyMaxSize))
		inline, inlineErr := listRolePolicyNames(ctx, api, role)
		return g.inline(ctx, api, role, inline, inlineErr, "the document is too large for a managed policy", warn)
	}

	inline, inlineErr := listRolePolicyNames(ctx, api, role)
	attached, attErr := listAttachedRolePolicies(ctx, api, role)
	if attErr != nil {
		why := fmt.Sprintf("could not list the managed policies attached to IAM role %s (%v)", role, attErr)
		warn(why + fallbackAdvice(g))
		return g.inline(ctx, api, role, inline, inlineErr, why, warn)
	}

	managed := map[int]string{} // schema -> ARN, of spored managed versions on the role
	newestManaged := 0
	for _, a := range attached {
		arn := aws.ToString(a.PolicyArn)
		if !strings.Contains(arn, ":policy"+sporedPolicyPath) {
			continue
		}
		if v, ok := parseVersionedPolicyName(g.ManagedBase, aws.ToString(a.PolicyName)); ok {
			managed[v] = arn
			if v > newestManaged {
				newestManaged = v
			}
		}
	}
	newestInline := 0
	if inlineErr == nil {
		for _, n := range inline {
			if v, ok := parseVersionedPolicyName(g.InlineBase, n); ok && v > newestInline {
				newestInline = v
			}
		}
	}
	switch {
	case newestManaged > g.Schema:
		warnNewerSchema(warn, role, "managed policy "+managed[newestManaged], newestManaged, g.Schema)
		return nil
	case newestInline > g.Schema:
		warnNewerSchema(warn, role, versionedPolicyName(g.InlineBase, newestInline), newestInline, g.Schema)
		return nil
	}

	arn := g.managedARN()
	_, onRole := managed[g.Schema]
	if why := ensureSporedPolicyAttached(ctx, api, role, g, onRole, attached); why != "" {
		warn(why + fallbackAdvice(g))
		return g.inline(ctx, api, role, inline, inlineErr, why, warn)
	}

	// Attached. Everything below only removes what it supersedes.
	if inlineErr != nil {
		warn(fmt.Sprintf("could not list the inline policies of IAM role %s (%v), so the inline copies of the spored "+
			"grants that %s replaces were left on it. Grant iam:ListRolePolicies and iam:DeleteRolePolicy on the "+
			"role to let spawn keep it tidy (#1573).", role, inlineErr, arn))
	} else {
		for _, n := range inline {
			v, versioned := parseVersionedPolicyName(g.InlineBase, n)
			if n != g.InlineBase && (!versioned || v > g.Schema) {
				continue
			}
			if err := deleteRolePolicy(ctx, api, role, n); err != nil {
				warn(fmt.Sprintf("could not delete the superseded inline policy %s from IAM role %s (%v); the managed "+
					"policy %s carries all of its grants, so it is only clutter. `aws iam delete-role-policy "+
					"--role-name %s --policy-name %s` removes it (#1573).", n, role, err, arn, role, n))
			}
		}
	}
	older := make([]int, 0, len(managed))
	for v := range managed {
		if v < g.Schema {
			older = append(older, v)
		}
	}
	sort.Ints(older)
	for _, v := range older {
		old := managed[v]
		if err := detachRolePolicy(ctx, api, role, old); err != nil {
			warn(fmt.Sprintf("could not detach the superseded managed policy %s from IAM role %s (%v); %s carries "+
				"all of its grants, so it is only clutter. `aws iam detach-role-policy --role-name %s --policy-arn %s` "+
				"removes it (#1573).", old, role, err, arn, role, old))
			continue
		}
		if err := deleteSporedManagedPolicy(ctx, api, old); err != nil {
			warn(fmt.Sprintf("could not delete the superseded managed policy %s (%v); nothing attaches it any more, "+
				"so `spawn cleanup --instance-roles` will (#1573).", old, err))
		}
	}
	return nil
}

func warnNewerSchema(warn func(string), role, what string, newest, schema int) {
	warn(fmt.Sprintf("IAM role %s carries %s, written by a newer spawn (policy schema v%d; this spawn writes v%d). "+
		"Leaving the role's grants as that version set them: they include this version's, so instances "+
		"launched now get them. Upgrade spawn to stop this warning (#1558).", role, what, newest, schema))
}

// fallbackAdvice ends the warning that the managed path was not taken.
func fallbackAdvice(g sporedGrant) string {
	return fmt.Sprintf("; writing the spored grants inline instead, which counts against the role's %d-character "+
		"inline policy limit (#1573). %s", inlinePolicyBudget, managedPolicyGrantAdvice(g.Account))
}

// managedPolicyGrantAdvice names the actions that let spawn use a managed
// policy.
func managedPolicyGrantAdvice(account string) string {
	if account == "" {
		account = "<account>"
	}
	return fmt.Sprintf("To let spawn attach them as a managed policy, grant your spawn identity iam:GetPolicy, "+
		"iam:GetPolicyVersion, iam:CreatePolicy and iam:DeletePolicy on arn:aws:iam::%s:policy%s*, and "+
		"iam:ListAttachedRolePolicies, iam:AttachRolePolicy and iam:DetachRolePolicy on the spawn roles (%s).",
		account, sporedPolicyPath, IAMBaselineURL)
}

// ensureSporedPolicyAttached makes g's managed policy exist with g's document
// and be attached to role. It returns "" when it is, and otherwise the reason
// the managed path is unavailable, for the caller to write inline instead.
// Every failure here is a reason, never an error: the inline write is what
// every spawn before #1573 did, so falling back to it adds no failure.
func ensureSporedPolicyAttached(ctx context.Context, api sporedPolicyAPI, role string, g sporedGrant, onRole bool, attached []types.AttachedPolicy) string {
	arn := g.managedARN()
	// Twice at most: a policy that disappears between finding it and attaching
	// it (the unattached-policy sweep, or a concurrent detach and delete) is
	// created again once.
	for attempt := 0; attempt < 2; attempt++ {
		why, gone := ensureSporedPolicy(ctx, api, g)
		if why != "" {
			return why
		}
		if gone {
			continue
		}
		if onRole {
			return ""
		}
		// IAM is eventually consistent: a policy created a moment ago, here or
		// by a concurrent launch, can be NoSuchEntity to AttachRolePolicy for
		// a few seconds. Falling back to inline then would fail the very
		// launch #1573 is for, on a role with no inline room, so wait for it.
		var err error
		for try := 0; try < attachConsistencyTries; try++ {
			err = retryIAMWrite(func() error {
				_, e := api.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
					RoleName:  aws.String(role),
					PolicyArn: aws.String(arn),
				})
				return e
			})
			if !isNoSuchEntity(err) || try == attachConsistencyTries-1 {
				break
			}
			iamConsistencyWait(try)
		}
		switch {
		case err == nil:
			return ""
		case isNoSuchEntity(err):
			continue // the policy went, or the role did; the next pass tells which
		case IsAccessDenied(err):
			return fmt.Sprintf("not allowed to attach the managed policy %s to IAM role %s (%v)", arn, role, err)
		case isLimitExceeded(err):
			names := make([]string, 0, len(attached))
			for _, a := range attached {
				names = append(names, aws.ToString(a.PolicyName))
			}
			return fmt.Sprintf("IAM role %s already has %d managed policies attached (%s), the most IAM allows the "+
				"role (10 unless the account's quota was raised), so the managed policy %s could not be attached "+
				"(%v). Detaching one the role no longer needs makes room", role, len(attached),
				strings.Join(names, ", "), arn, err)
		default:
			return fmt.Sprintf("could not attach the managed policy %s to IAM role %s (%v)", arn, role, err)
		}
	}
	return fmt.Sprintf("the managed policy %s, or IAM role %s, disappeared while spawn was attaching it", arn, role)
}

// attachConsistencyTries bounds the wait for a new policy to be attachable:
// iamConsistencyWait sleeps 0.5 s, 1 s, 1.5 s, 2 s between the tries, so 5 s
// in all.
const attachConsistencyTries = 5

// iamConsistencyWait sleeps before retry try+1 of an IAM call that may be
// waiting on IAM's eventual consistency. A variable so tests need not wait.
var iamConsistencyWait = func(try int) { time.Sleep(time.Duration(try+1) * 500 * time.Millisecond) }

// ensureSporedPolicy makes g's managed policy exist with g's document. It
// returns a reason the managed path is unavailable, or gone when the policy
// was found and then deleted before it could be read.
func ensureSporedPolicy(ctx context.Context, api sporedPolicyAPI, g sporedGrant) (why string, gone bool) {
	arn := g.managedARN()
	versionID, err := sporedPolicyDefaultVersion(ctx, api, arn)
	switch {
	case err == nil:
	case isNoSuchEntity(err):
		cerr := retryIAMWrite(func() error {
			_, e := api.CreatePolicy(ctx, &iam.CreatePolicyInput{
				PolicyName:     aws.String(g.managedName()),
				Path:           aws.String(sporedPolicyPath),
				PolicyDocument: aws.String(g.Doc),
				Description: aws.String(fmt.Sprintf("spawn: the spored grants (policy schema v%d), attached to spawn's "+
					"instance roles. Created by spawn; spawn deletes it once no role uses it.", g.Schema)),
			})
			return e
		})
		switch {
		case cerr == nil:
			return "", false // this document, just written
		case isEntityAlreadyExists(cerr):
			// A concurrent launch created it: compare it, as any other.
			if versionID, err = sporedPolicyDefaultVersion(ctx, api, arn); err != nil {
				if isNoSuchEntity(err) {
					return "", true
				}
				return readFailure(arn, err), false
			}
		case IsAccessDenied(cerr):
			return fmt.Sprintf("not allowed to create the managed policy %s (%v)", arn, cerr), false
		case isLimitExceeded(cerr):
			// IAM answers LimitExceeded both for an account at its managed
			// policy quota and for a document over the size cap (the latter
			// checked by the caller, so the former).
			return fmt.Sprintf("could not create the managed policy %s: IAM refused it with LimitExceeded, "+
				"most likely the account's quota of customer-managed policies (%v)", arn, cerr), false
		default:
			return fmt.Sprintf("could not create the managed policy %s (%v)", arn, cerr), false
		}
	default:
		return readFailure(arn, err), false
	}

	var doc string
	err = retryIAMWrite(func() error {
		out, e := api.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{PolicyArn: aws.String(arn), VersionId: aws.String(versionID)})
		if e == nil && out.PolicyVersion != nil {
			doc = aws.ToString(out.PolicyVersion.Document)
		}
		return e
	})
	if err != nil {
		if isNoSuchEntity(err) {
			return "", true
		}
		return readFailure(arn, err), false
	}
	if !samePolicyDocument(doc, g.Doc) {
		return fmt.Sprintf("the managed policy %s exists with a document this spawn did not write (same policy schema "+
			"v%d, different grants: a development build, or edited by hand). spawn never rewrites it; if it is not "+
			"wanted, detach it from every role and `aws iam delete-policy --policy-arn %s`, and the next launch "+
			"creates it with this spawn's document", arn, g.Schema, arn), false
	}
	return "", false
}

func readFailure(arn string, err error) string {
	if IsAccessDenied(err) {
		return fmt.Sprintf("not allowed to read the managed policy %s (%v)", arn, err)
	}
	return fmt.Sprintf("could not read the managed policy %s (%v)", arn, err)
}

// sporedPolicyDefaultVersion returns the default version id of the managed
// policy arn.
func sporedPolicyDefaultVersion(ctx context.Context, api sporedPolicyAPI, arn string) (string, error) {
	var id string
	err := retryIAMWrite(func() error {
		out, e := api.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)})
		if e == nil && out.Policy != nil {
			id = aws.ToString(out.Policy.DefaultVersionId)
		}
		return e
	})
	if err == nil && id == "" {
		id = "v1"
	}
	return id, err
}

// samePolicyDocument reports whether two policy documents grant the same. IAM
// returns a stored document URL-encoded, and an IAM implementation may store
// it normalized: whitespace dropped, and a one-element list as its element
// (the emulator does). Both are the same policy, so the comparison is of the
// parsed JSON with each one-element list replaced by its element and each list
// of strings (an Action or Resource set) sorted.
func samePolicyDocument(stored, want string) bool {
	if d, err := url.QueryUnescape(stored); err == nil {
		stored = d
	}
	var a, b interface{}
	if json.Unmarshal([]byte(stored), &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		return false
	}
	return reflect.DeepEqual(normalizePolicyJSON(a), normalizePolicyJSON(b))
}

func normalizePolicyJSON(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, e := range x {
			out[k] = normalizePolicyJSON(e)
		}
		return out
	case []interface{}:
		if len(x) == 1 {
			return normalizePolicyJSON(x[0])
		}
		out := make([]interface{}, len(x))
		strs := make([]string, 0, len(x))
		for i, e := range x {
			out[i] = normalizePolicyJSON(e)
			if s, ok := out[i].(string); ok {
				strs = append(strs, s)
			}
		}
		if len(strs) == len(out) {
			sort.Strings(strs)
			for i, s := range strs {
				out[i] = s
			}
		}
		return out
	default:
		return v
	}
}

// inline is the fallback: the versioned inline write of #1558, with a
// size-cap failure explained.
func (g sporedGrant) inline(ctx context.Context, api sporedPolicyAPI, role string, names []string, listErr error, why string, warn func(string)) error {
	err := writeInlineVersion(ctx, api, role, g.InlineBase, g.Doc, g.Schema, names, listErr, warn)
	if err != nil && isLimitExceeded(err) {
		return inlineBudgetError(ctx, api, role, g, names, listErr, why, err)
	}
	return err
}

// inlineBudgetError explains a PutRolePolicy refused for the role's inline
// size cap: every inline policy on the role with its size as IAM counts it,
// the total against the cap, and the two ways out.
func inlineBudgetError(ctx context.Context, api sporedPolicyAPI, role string, g sporedGrant, names []string, listErr error, why string, cause error) error {
	mine := versionedPolicyName(g.InlineBase, g.Schema)
	var b strings.Builder
	fmt.Fprintf(&b, "cannot give IAM role %s spored's grants: writing them inline as %s (%d characters) would take the "+
		"role's inline policies past IAM's limit of %d characters for one role, whitespace not counted.\n",
		role, mine, nonSpaceLen(g.Doc), inlinePolicyBudget)
	if listErr != nil {
		fmt.Fprintf(&b, "Its inline policies could not be listed (%v).\n", listErr)
	} else {
		b.WriteString("Inline policies on the role now:\n")
		total := 0
		sorted := append([]string(nil), names...)
		sort.Strings(sorted)
		for _, n := range sorted {
			size, err := inlinePolicySize(ctx, api, role, n)
			note := ""
			if v, ok := parseVersionedPolicyName(g.InlineBase, n); n == g.InlineBase || (ok && v <= g.Schema) {
				note = "  (spawn's own; superseded once spored's grants are attached as a managed policy)"
			}
			if err != nil {
				fmt.Fprintf(&b, "  %8s  %s%s  (size unreadable: %v)\n", "?", n, note, err)
				continue
			}
			total += size
			fmt.Fprintf(&b, "  %8d  %s%s\n", size, n, note)
		}
		fmt.Fprintf(&b, "  %8d  total, of %d\n", total, inlinePolicyBudget)
	}
	fmt.Fprintf(&b, "spawn attaches these grants as the managed policy %s when it can, and a managed policy does not "+
		"count against that limit; it did not here: %s.\n", g.managedARN(), why)
	b.WriteString("To fix it, either:\n")
	fmt.Fprintf(&b, "  - %s\n", managedPolicyGrantAdvice(g.Account))
	fmt.Fprintf(&b, "  - or remove inline policies the role no longer needs: aws iam delete-role-policy --role-name %s "+
		"--policy-name <name>\n", role)
	fmt.Fprintf(&b, "(IAM said: %v) (#1573)", cause)
	return fmt.Errorf("%s", b.String())
}

// inlinePolicySize is the size IAM counts for role's inline policy name.
func inlinePolicySize(ctx context.Context, api sporedPolicyAPI, role, name string) (int, error) {
	var doc string
	err := retryIAMWrite(func() error {
		out, e := api.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String(name)})
		if e == nil {
			doc = aws.ToString(out.PolicyDocument)
		}
		return e
	})
	if err != nil {
		return 0, err
	}
	if d, derr := url.QueryUnescape(doc); derr == nil {
		doc = d
	}
	return nonSpaceLen(doc), nil
}

// nonSpaceLen counts s as IAM counts a policy against its size limits:
// whitespace is not counted.
func nonSpaceLen(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// listAttachedRolePolicies returns every managed policy attached to role.
func listAttachedRolePolicies(ctx context.Context, api iam.ListAttachedRolePoliciesAPIClient, role string) ([]types.AttachedPolicy, error) {
	var out []types.AttachedPolicy
	p := iam.NewListAttachedRolePoliciesPaginator(api, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(role)})
	for p.HasMorePages() {
		var page *iam.ListAttachedRolePoliciesOutput
		err := retryIAMWrite(func() error {
			var e error
			page, e = p.NextPage(ctx)
			return e
		})
		if err != nil {
			return nil, err
		}
		out = append(out, page.AttachedPolicies...)
	}
	return out, nil
}

// detachRolePolicy detaches arn from role. Not attached is success.
func detachRolePolicy(ctx context.Context, api sporedPolicyAPI, role, arn string) error {
	err := retryIAMWrite(func() error {
		_, e := api.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(role), PolicyArn: aws.String(arn)})
		return e
	})
	if err != nil && isNoSuchEntity(err) {
		return nil
	}
	return err
}

// sporedPolicyDeleter is the call deleteSporedManagedPolicy makes.
type sporedPolicyDeleter interface {
	DeletePolicy(ctx context.Context, in *iam.DeletePolicyInput, optFns ...func(*iam.Options)) (*iam.DeletePolicyOutput, error)
}

// deleteSporedManagedPolicy deletes spawn's managed policy arn if nothing has
// it attached. IAM refuses to delete an attached policy (DeleteConflict), and
// that refusal is the check: a policy another role still uses is kept, which
// is success here. Already gone is success too.
func deleteSporedManagedPolicy(ctx context.Context, api sporedPolicyDeleter, arn string) error {
	if !isSporedManagedPolicyARN(arn) {
		return fmt.Errorf("refusing to delete %s: not a spored managed policy at %s", arn, sporedPolicyPath)
	}
	err := retryIAMWrite(func() error {
		_, e := api.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: aws.String(arn)})
		return e
	})
	if err == nil || isNoSuchEntity(err) || isDeleteConflict(err) {
		return nil
	}
	return err
}

func isLimitExceeded(err error) bool {
	return strings.HasPrefix(iamErrorCode(err), "LimitExceeded")
}

func isEntityAlreadyExists(err error) bool {
	return strings.HasPrefix(iamErrorCode(err), "EntityAlreadyExists")
}

func isDeleteConflict(err error) bool {
	return strings.HasPrefix(iamErrorCode(err), "DeleteConflict")
}
