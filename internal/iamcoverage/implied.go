package iamcoverage

import (
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
)

// impliedRule is an IAM action an operation needs only when a field of its
// input is set (spawn#1377). The common case is tag-on-create: sqs:CreateQueue
// with Tags is also authorized as sqs:TagQueue, and a role holding only
// sqs:CreateQueue gets AccessDenied for the whole call — which pkg/taskpool
// turns into an untagged queue and a warning, so nothing fails loudly.
type impliedRule struct {
	// Op is the operation, as mapOp keys it: "<IAM prefix>:<SDK operation>"
	// for a service client, or "<package path>.<method>" for a featureOps
	// library.
	Op string
	// Field is the input field that triggers the rule, dotted for a nested
	// one (CreateBucketConfiguration.Tags).
	Field string
	// Actions are what the call additionally needs when Field is set.
	Actions []string
	// Docs are the AWS documentation pages the rule was taken from. Required:
	// a rule nobody can check is a guess (TestEveryImpliedRuleCitesItsSource).
	Docs []string
	// PassedTo is, for an iam:PassRole rule, the iam:PassedToService value the
	// Service Authorization Reference gives the pass (ec2.amazonaws.com for
	// RunInstances): the service a principal's PassRole grant must admit.
	PassedTo string
}

// The Service Authorization Reference's "API operations defined by <service>"
// table lists, per operation, every action the operation authorizes. The tag
// action in it is the conditional one: the service only authorizes it when the
// request carries tags. Each rule below cites that table's entry for the
// operation and, where the service documents the condition in prose, that page.
const (
	sarSQS          = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_sqs.html#list_sqs-operation-"
	sarIAM          = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_iam.html#list_iam-operation-iam-"
	sarEC2          = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_ec2.html#list_ec2-operation-"
	sarEBS          = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_ebs.html#list_ebs-operation-"
	sarS3           = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_s3.html#list_s3-operation-s3-"
	sarDynamoDB     = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_dynamodb.html#list_dynamodb-operation-dynamodb-"
	sarLambda       = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_lambda.html#list_lambda-operation-lambda-"
	sarEvents       = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_eventbridge.html#list_eventbridge-operation-events-"
	sarSNS          = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_sns.html#list_sns-operation-"
	sarFSx          = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_fsx.html#list_fsx-operation-"
	sarEFS          = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_efs.html#list_efs-operation-"
	sarImageBuilder = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_imagebuilder.html#list_imagebuilder-operation-"
	sarSTS          = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_sts.html#list_sts-operation-"
	sarScheduler    = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_scheduler.html#list_scheduler-operation-"
	sarSSM          = "https://docs.aws.amazon.com/service-authorization/latest/reference/list_ssm.html#list_ssm-operation-"

	// "If tags are specified in the resource-creating action, Amazon performs
	// additional authorization on the ec2:CreateTags action".
	docEC2TagOnCreate = "https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/supported-iam-actions-tagging.html"
)

// tagOnCreate builds the rules for operations whose Field carries tags and
// whose tag action is action, each citing its SAR operation entry.
func tagOnCreate(sar, prefix, field, action, prose string, ops ...string) []impliedRule {
	out := make([]impliedRule, len(ops))
	for i, op := range ops {
		docs := []string{sar + op}
		if prose != "" {
			docs = append(docs, prose)
		}
		out[i] = impliedRule{Op: prefix + ":" + op, Field: field, Actions: []string{action}, Docs: docs}
	}
	return out
}

// passRole builds an iam:PassRole rule (#1397): op, when field names a role,
// also needs iam:PassRole on that role, passed to the service passedTo. Every
// rule cites the SAR operation entry, which lists iam:PassRole among the
// operation's authorized actions with that iam:PassedToService context, and a
// service page that ties the pass to the field.
func passRole(op, field, passedTo string, docs ...string) impliedRule {
	return impliedRule{Op: op, Field: field, Actions: []string{"iam:PassRole"}, PassedTo: passedTo, Docs: docs}
}

// passRoleRules are the operations spawn calls whose SAR entry authorizes
// iam:PassRole, each verified against the operations table
// (servicereference.us-east-1.amazonaws.com/v1/<svc>/<svc>.json,
// Operations[].AuthorizedActions, read 2026-10-28) and a service page. A field
// the API requires (Lambda's Role, a schedule's Target.RoleArn) makes the rule
// unconditional in practice; it is still keyed on the field so the rule fails
// loudly if the SDK renames it.
//
// Not modelled, because spawn calls none of them (checked by grepping every
// *Input of the 99 SAR operations that authorize iam:PassRole):
// ec2:RequestSpotInstances, CreateFleet, CreateLaunchTemplate,
// AssociateIamInstanceProfile, ReplaceIamInstanceProfileAssociation;
// cloudformation:CreateStack/UpdateStack/CreateChangeSet (RoleARN);
// imagebuilder:CreateImage and the pipeline/workflow operations.
//
// sns:Subscribe and sns:CreateTopic pass a role only through an attribute
// (SubscriptionRoleArn; the <protocol>{Success,Failure}FeedbackRoleArn delivery
// status attributes), a map key this mechanism cannot see, so the rule keys on
// Attributes as a whole: an over-approximation in the safe direction.
var passRoleRules = []impliedRule{
	// "You must grant permission to use the following API actions as
	// required: iam:PassRole" (launching an instance with an IAM role).
	passRole("ec2:RunInstances", "IamInstanceProfile", "ec2.amazonaws.com", sarEC2+"RunInstances",
		"https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/permission-to-pass-iam-roles.html"),
	// "PassRole is used when you assign an execution role to a function."
	passRole("lambda:CreateFunction", "Role", "lambda.amazonaws.com", sarLambda+"CreateFunction",
		"https://docs.aws.amazon.com/lambda/latest/dg/permissions-user-function.html"),
	passRole("lambda:UpdateFunctionConfiguration", "Role", "lambda.amazonaws.com", sarLambda+"UpdateFunctionConfiguration",
		"https://docs.aws.amazon.com/lambda/latest/dg/permissions-user-function.html"),
	// The CreateSchedule and UpdateSchedule policy examples each pair the
	// action with iam:PassRole, PassedToService scheduler.amazonaws.com; the
	// role is Target.RoleArn (required).
	passRole("scheduler:CreateSchedule", "Target.RoleArn", "scheduler.amazonaws.com", sarScheduler+"CreateSchedule",
		"https://docs.aws.amazon.com/scheduler/latest/UserGuide/security_iam_id-based-policy-examples.html",
		"https://docs.aws.amazon.com/scheduler/latest/APIReference/API_Target.html"),
	passRole("scheduler:UpdateSchedule", "Target.RoleArn", "scheduler.amazonaws.com", sarScheduler+"UpdateSchedule",
		"https://docs.aws.amazon.com/scheduler/latest/UserGuide/security_iam_id-based-policy-examples.html",
		"https://docs.aws.amazon.com/scheduler/latest/APIReference/API_Target.html"),
	// "iam:PassRole – EventBridge requires this permission to pass an
	// invocation role to EventBridge to invoke the target of a rule." The role
	// is the rule's RoleArn, or a target's (Targets is a list; the rule
	// follows its elements).
	passRole("events:PutRule", "RoleArn", "events.amazonaws.com", sarEvents+"PutRule",
		"https://docs.aws.amazon.com/eventbridge/latest/APIReference/API_PutRule.html",
		"https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-use-identity-based.html"),
	passRole("events:PutTargets", "Targets.RoleArn", "events.amazonaws.com", sarEvents+"PutTargets",
		"https://docs.aws.amazon.com/eventbridge/latest/APIReference/API_PutTargets.html",
		"https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-use-identity-based.html"),
	// ServiceRoleArn is the role Run Command publishes SNS notifications
	// with; "iam:PassRole permissions for the role created in Task 3".
	passRole("ssm:SendCommand", "ServiceRoleArn", "ssm.amazonaws.com", sarSSM+"SendCommand",
		"https://docs.aws.amazon.com/systems-manager/latest/APIReference/API_SendCommand.html",
		"https://docs.aws.amazon.com/systems-manager/latest/userguide/monitoring-sns-notifications.html"),
	passRole("sns:Subscribe", "Attributes", "sns.amazonaws.com", sarSNS+"Subscribe",
		"https://docs.aws.amazon.com/sns/latest/api/API_Subscribe.html"),
	passRole("sns:CreateTopic", "Attributes", "sns.amazonaws.com", sarSNS+"CreateTopic",
		"https://docs.aws.amazon.com/sns/latest/api/API_CreateTopic.html"),
	// "The caller of this operation must be granted the PassRole permission
	// on the IAM role by a permissions policy."
	passRole("iam:AddRoleToInstanceProfile", "RoleName", "ec2.amazonaws.com", sarIAM+"AddRoleToInstanceProfile",
		"https://docs.aws.amazon.com/IAM/latest/APIReference/API_AddRoleToInstanceProfile.html"),
	// The build instance's instance profile (instanceProfileName, required),
	// passed to EC2 per the SAR context.
	passRole("imagebuilder:CreateInfrastructureConfiguration", "InstanceProfileName", "ec2.amazonaws.com",
		sarImageBuilder+"CreateInfrastructureConfiguration",
		"https://docs.aws.amazon.com/imagebuilder/latest/APIReference/API_CreateInfrastructureConfiguration.html"),
	// executionRole: "the IAM role you create that grants Image Builder
	// access to perform workflow actions", passed to imagebuilder.amazonaws.com.
	passRole("imagebuilder:ImportDiskImage", "ExecutionRole", "imagebuilder.amazonaws.com", sarImageBuilder+"ImportDiskImage",
		"https://docs.aws.amazon.com/imagebuilder/latest/APIReference/API_ImportDiskImage.html"),
}

// impliedRules is every implied-action rule (#1377, #1397). Only rules
// verified against AWS's documentation are here. Checked and left out:
// scheduler:CreateSchedule's tags (a schedule takes no tags; the SAR lists no
// tag action for it); fsx:DeleteFileSystem's final-backup tags (nested per file
// system type, and spawn sets none).
var impliedRules = concatRules(
	passRoleRules,
	// "To be able to tag a queue on creation, you must have the
	// sqs:CreateQueue and sqs:TagQueue permissions."
	tagOnCreate(sarSQS, "sqs", "Tags", "sqs:TagQueue",
		"https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_CreateQueue.html", "CreateQueue"),
	tagOnCreate(sarIAM, "iam", "Tags", "iam:TagRole", "", "CreateRole"),
	tagOnCreate(sarIAM, "iam", "Tags", "iam:TagInstanceProfile", "", "CreateInstanceProfile"),
	tagOnCreate(sarIAM, "iam", "Tags", "iam:TagPolicy", "", "CreatePolicy"),
	tagOnCreate(sarEC2, "ec2", "TagSpecifications", "ec2:CreateTags", docEC2TagOnCreate,
		"RunInstances", "CreateSecurityGroup", "CreateVolume", "CreateImage", "CopyImage",
		"RegisterImage", "CreateKeyPair", "ImportKeyPair", "AllocateAddress",
		"CreatePlacementGroup", "AuthorizeSecurityGroupIngress", "AuthorizeSecurityGroupEgress",
		"PurchaseCapacityBlock"),
	// EBS direct APIs tag the snapshot they create through ec2:CreateTags.
	tagOnCreate(sarEBS, "ebs", "Tags", "ec2:CreateTags", "", "StartSnapshot"),
	tagOnCreate(sarDynamoDB, "dynamodb", "Tags", "dynamodb:TagResource", "", "CreateTable"),
	// "lambda:TagResource – Grant this permission to anyone who needs to call
	// TagResource or perform a tag on create."
	tagOnCreate(sarLambda, "lambda", "Tags", "lambda:TagResource",
		"https://docs.aws.amazon.com/lambda/latest/dg/configuration-tags.html", "CreateFunction"),
	// "To use the PutRule operation and assign tags, you must have both the
	// events:PutRule and events:TagResource permissions."
	tagOnCreate(sarEvents, "events", "Tags", "events:TagResource",
		"https://docs.aws.amazon.com/eventbridge/latest/APIReference/API_PutRule.html", "PutRule"),
	// "To be able to tag a topic on creation, you must have the
	// sns:CreateTopic and sns:TagResource permissions."
	tagOnCreate(sarSNS, "sns", "Tags", "sns:TagResource",
		"https://docs.aws.amazon.com/sns/latest/api/API_CreateTopic.html", "CreateTopic"),
	// "If tags are specified in the resource-creating action, IAM performs
	// additional authorization on the fsx:TagResource action".
	tagOnCreate(sarFSx, "fsx", "Tags", "fsx:TagResource",
		"https://docs.aws.amazon.com/fsx/latest/LustreGuide/using-tags-fsx.html",
		"CreateFileSystem", "CreateDataRepositoryAssociation"),
	// "If tags are specified in the creation action, IAM performs additional
	// authorization on the elasticfilesystem:TagResource action".
	tagOnCreate(sarEFS, "elasticfilesystem", "Tags", "elasticfilesystem:TagResource",
		"https://docs.aws.amazon.com/efs/latest/ug/API_CreateFileSystem.html",
		"CreateFileSystem", "CreateAccessPoint"),
	tagOnCreate(sarImageBuilder, "imagebuilder", "Tags", "imagebuilder:TagResource", "",
		"CreateInfrastructureConfiguration", "ImportDiskImage"),
	// "Permissions required to add session tags: In addition to the action
	// that matches the API operation, you must have ... sts:TagSession".
	tagOnCreate(sarSTS, "sts", "Tags", "sts:TagSession",
		"https://docs.aws.amazon.com/IAM/latest/UserGuide/id_session-tags.html", "AssumeRole"),
	// "s3:PutObjectTagging - To successfully set the tag-set with your
	// PutObject request, you must have the s3:PutObjectTagging."
	tagOnCreate(sarS3, "s3", "Tagging", "s3:PutObjectTagging",
		"https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html",
		"PutObject", "CopyObject", "CreateMultipartUpload"),
	[]impliedRule{
		// "You must have the s3:TagResource permission to create a general
		// purpose bucket with tags". The SAR's CreateBucket entry does not
		// list s3:TagResource; the API reference does, and is the source.
		{Op: "s3:CreateBucket", Field: "CreateBucketConfiguration.Tags", Actions: []string{"s3:TagResource"},
			Docs: []string{"https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucket.html"}},
		// The transfer manager passes Tagging to the PutObject or
		// CreateMultipartUpload it makes (see the s3 rules above).
		{Op: sdkRoot + "feature/s3/transfermanager.UploadObject", Field: "Tagging", Actions: []string{"s3:PutObjectTagging"},
			Docs: []string{sarS3 + "PutObject", "https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html"}},
	},
)

func concatRules(parts ...[]impliedRule) []impliedRule {
	var out []impliedRule
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// impliedByOp indexes impliedRules by Op.
var impliedByOp = func() map[string][]impliedRule {
	m := map[string][]impliedRule{}
	for _, r := range impliedRules {
		m[r.Op] = append(m[r.Op], r)
	}
	return m
}()

// applyImplied adds to op the actions of every rule for its operation whose
// field the call may set. input is the call's *<Op>Input argument, or nil when
// the operation is referenced rather than called (a method value), in which
// case nothing is known about the input and every rule applies.
//
// "May set" is deliberately an over-approximation, like the rest of the
// package: the field counts as set unless the input is provably built here
// without it. A rule whose field does not exist on the input type is an error,
// not a skip — an SDK rename would otherwise turn the rule off silently.
func applyImplied(op foundOp, input ssa.Value, call ssa.Instruction, cg *callgraph.Graph) foundOp {
	rules := impliedByOp[op.key]
	if len(rules) == 0 || op.err != "" {
		return op
	}
	var fields []string
	for _, r := range rules {
		path := strings.Split(r.Field, ".")
		if op.input != nil && !hasFieldPath(op.input, path) {
			op.err = "implied-action rule names field " + r.Field + ", which " + types.TypeString(op.input, nil) +
				" does not have (fix impliedRules)"
			op.actions = nil
			return op
		}
		if input != nil && !fieldMaybeSet(input, path, call, cg) {
			continue
		}
		fields = append(fields, r.Field)
		for _, a := range r.Actions {
			if !containsString(op.actions, a) {
				op.actions = append(append([]string(nil), op.actions...), a)
			}
		}
	}
	if len(fields) > 0 {
		op.display += " [" + strings.Join(fields, ",") + " set]"
	}
	return op
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// hasFieldPath reports whether t (a pointer to a struct) has the dotted field.
// A list along the path is followed to its element type: Targets.RoleArn is
// the RoleArn of any element of Targets.
func hasFieldPath(t types.Type, path []string) bool {
	for _, name := range path {
		if p, ok := t.Underlying().(*types.Pointer); ok {
			t = p.Elem()
		}
		if sl, ok := t.Underlying().(*types.Slice); ok {
			t = sl.Elem()
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok {
			return false
		}
		found := false
		for i := 0; i < st.NumFields(); i++ {
			if f := st.Field(i); f.Name() == name {
				t, found = f.Type(), true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// fieldMaybeSet reports whether the struct v points to may have path set when
// call uses it. It is false only when every way v is built is visible here and
// none stores a non-nil value into the field.
func fieldMaybeSet(v ssa.Value, path []string, call ssa.Instruction, cg *callgraph.Graph) bool {
	return pointerMaybeSet(v, path, &walk{cg: cg, expected: map[ssa.Instruction]bool{call: true}, seen: map[ssa.Value]bool{}})
}

// walk is one fieldMaybeSet traversal. expected are the uses of a pointer that
// are how the traversal reached it — the operation's own call, and the store
// of a nested struct's pointer into its parent's field — and so are not
// escapes.
type walk struct {
	cg       *callgraph.Graph // follows a parameter to its callers' arguments
	expected map[ssa.Instruction]bool
	seen     map[ssa.Value]bool
}

func pointerMaybeSet(v ssa.Value, path []string, w *walk) bool {
	switch v := v.(type) {
	case *ssa.Alloc:
		return referrersMaybeSet(v, path, w)
	case *ssa.Phi:
		for _, e := range v.Edges {
			if pointerMaybeSet(e, path, w) {
				return true
			}
		}
		return false
	case *ssa.Const:
		return !v.IsNil() // a nil input sets nothing
	case *ssa.Parameter:
		return parameterMaybeSet(v, path, w)
	case *ssa.Slice:
		// A list literal, []T{{...}}: its backing array is built here, and
		// slicing it is how the traversal reached the array.
		w.expected[v] = true
		return pointerMaybeSet(v.X, path, w)
	default:
		// A parameter, a call's result, a load from somewhere: built where
		// this analysis does not look, so assume the field is set.
		return true
	}
}

// parameterMaybeSet follows a parameter to the argument at every call site the
// call graph knows. A function with no known caller, or a caller whose
// argument cannot be matched to the parameter, counts as setting the field.
func parameterMaybeSet(v *ssa.Parameter, path []string, w *walk) bool {
	if w.seen[v] {
		return false
	}
	w.seen[v] = true
	fn := v.Parent()
	idx := -1
	for i, p := range fn.Params {
		if p == v {
			idx = i
		}
	}
	var node *callgraph.Node
	if w.cg != nil {
		node = w.cg.Nodes[fn]
	}
	if idx < 0 || node == nil || len(node.In) == 0 {
		return true
	}
	for _, e := range node.In {
		if e.Site == nil {
			return true
		}
		com := e.Site.Common()
		i := idx
		if com.IsInvoke() {
			i-- // the receiver is com.Value, not an argument
		}
		if i < 0 || i >= len(com.Args) {
			return true
		}
		w.expected[e.Site] = true
		if pointerMaybeSet(com.Args[i], path, w) {
			return true
		}
	}
	return false
}

// referrersMaybeSet inspects every use of the pointer p to a struct.
func referrersMaybeSet(p ssa.Value, path []string, w *walk) bool {
	if w.seen[p] {
		return false
	}
	w.seen[p] = true
	refs := p.Referrers()
	if refs == nil {
		return true
	}
	for _, r := range *refs {
		switch r := r.(type) {
		case *ssa.FieldAddr:
			if r.X == p && fieldName(r) == path[0] && fieldAddrMaybeSet(r, path, w) {
				return true
			}
		case *ssa.IndexAddr:
			// &list[i]: an element of a list literal, whose fields are the
			// path's (hasFieldPath follows a list to its element).
			if r.X != p || referrersMaybeSet(r, path, w) {
				return true
			}
		case *ssa.Slice:
			if !w.expected[r] {
				return true // the array is sliced somewhere else too
			}
		case *ssa.DebugRef, *ssa.UnOp:
			// A load (*p): reading the struct does not set a field.
		case *ssa.Store:
			if w.expected[r] {
				continue
			}
			if r.Addr != p {
				return true // the pointer escapes into memory
			}
			// The whole struct is assigned: a zero value, or a copy of
			// another struct built here.
			switch val := r.Val.(type) {
			case *ssa.Const:
			case *ssa.UnOp:
				if val.Op != token.MUL || pointerMaybeSet(val.X, path, w) {
					return true
				}
			default:
				return true
			}
		case *ssa.Phi:
			// Merged with other inputs; what matters is what the merge
			// flows into.
			if referrersMaybeSet(r, path, w) {
				return true
			}
		case ssa.CallInstruction:
			if !w.expected[r] {
				return true // handed to other code, which may set it
			}
		default:
			return true
		}
	}
	return false
}

// fieldAddrMaybeSet inspects every use of &s.<path[0]>.
func fieldAddrMaybeSet(fa *ssa.FieldAddr, path []string, w *walk) bool {
	refs := fa.Referrers()
	if refs == nil {
		return true
	}
	rest := path[1:]
	for _, r := range *refs {
		switch r := r.(type) {
		case *ssa.Store:
			if r.Addr != fa {
				return true
			}
			if len(rest) == 0 {
				if c, ok := r.Val.(*ssa.Const); ok && c.IsNil() {
					continue // Tags: nil
				}
				return true
			}
			w.expected[r] = true
			if pointerMaybeSet(r.Val, rest, w) {
				return true
			}
		case *ssa.UnOp:
			// A load of the field: for a leaf it sets nothing (append and a map
			// update store back or need a prior store); for a nested pointer,
			// its own fields may be set through what was loaded.
			if len(rest) > 0 && referrersMaybeSet(r, rest, w) {
				return true
			}
		case *ssa.FieldAddr:
			// A nested struct value: &s.Config.Tags.
			if len(rest) > 0 && fieldName(r) == rest[0] && fieldAddrMaybeSet(r, rest, w) {
				return true
			}
		case *ssa.DebugRef:
		default:
			return true
		}
	}
	return false
}

func fieldName(fa *ssa.FieldAddr) string {
	ptr, ok := fa.X.Type().Underlying().(*types.Pointer)
	if !ok {
		return ""
	}
	st, ok := ptr.Elem().Underlying().(*types.Struct)
	if !ok || fa.Field >= st.NumFields() {
		return ""
	}
	return st.Field(fa.Field).Name()
}

// inputArg is the argument of args whose type is the operation's input type.
func inputArg(args []ssa.Value, input types.Type) ssa.Value {
	if input == nil {
		return nil
	}
	for _, a := range args {
		if types.Identical(a.Type(), input) {
			return a
		}
	}
	return nil
}
