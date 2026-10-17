package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// CrossAccountRoleName is the role `spawn notify register` creates in the
// caller's account for spore-bot to assume.
const CrossAccountRoleName = "SpawnBotCrossAccount"

// RoleIAM is the slice of the IAM API that managing the SpawnBotCrossAccount
// role needs; *iam.Client satisfies it, tests use a fake.
type RoleIAM interface {
	GetRole(ctx context.Context, in *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(ctx context.Context, in *iam.CreateRoleInput, optFns ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, optFns ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	UpdateAssumeRolePolicy(ctx context.Context, in *iam.UpdateAssumeRolePolicyInput, optFns ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error)
}

// botPurposeTag* mark a SpawnBotCrossAccount role that `spawn notify register`
// created. --repair only rewrites roles carrying it: a role from the
// bot-cross-account-role.yaml stack is owned by CloudFormation, and editing its
// trust in place would be drift the next stack update reverts.
const (
	botPurposeTagKey   = "spawn:purpose"
	botPurposeTagValue = "bot-cross-account"
)

// sharedBotExternalIDs are the ExternalIds every account used to share. spore-bot
// stopped presenting them in spore-host#413, so a trust policy conditioned only
// on one of these cannot be assumed by the bot.
var sharedBotExternalIDs = map[string]bool{"spawn-bot": true, "prism-bot": true}

// botTrust is what a role's trust policy grants for sts:AssumeRole.
type botTrust struct {
	principals  []string // AWS principals allowed to assume the role
	externalIDs []string // sts:ExternalId values the grant is conditioned on
}

// perAccountExternalID returns the first ExternalId that is not a retired
// shared value, or "".
func (t botTrust) perAccountExternalID() string {
	for _, x := range t.externalIDs {
		if !sharedBotExternalIDs[x] {
			return x
		}
	}
	return ""
}

func (t botTrust) trusts(principal string) bool {
	for _, p := range t.principals {
		if p == principal {
			return true
		}
	}
	return false
}

// parseBotTrust reads the sts:AssumeRole grants out of a trust policy. IAM
// returns the document from GetRole URL-encoded per RFC 3986 (a space is %20,
// so PathUnescape, not QueryUnescape, which would turn a literal + into a
// space); a plain JSON one is accepted too.
func parseBotTrust(doc string) (botTrust, error) {
	if dec, err := url.PathUnescape(doc); err == nil {
		doc = dec
	}
	var p struct {
		Statement []struct {
			Effect    string
			Action    json.RawMessage
			Principal json.RawMessage
			Condition map[string]map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return botTrust{}, fmt.Errorf("parse trust policy: %w", err)
	}
	var t botTrust
	for _, st := range p.Statement {
		if st.Effect != "Allow" || !containsString(stringOrList(st.Action), "sts:AssumeRole") {
			continue
		}
		var pr struct{ AWS json.RawMessage }
		if len(st.Principal) > 0 && json.Unmarshal(st.Principal, &pr) == nil {
			t.principals = append(t.principals, stringOrList(pr.AWS)...)
		}
		for op, keys := range st.Condition {
			if op != "StringEquals" {
				continue
			}
			for k, v := range keys {
				if strings.EqualFold(k, "sts:ExternalId") {
					t.externalIDs = append(t.externalIDs, stringOrList(v)...)
				}
			}
		}
	}
	return t, nil
}

// stringOrList decodes an IAM policy value that may be a string or a list.
func stringOrList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// botTrustPolicy is the trust policy for SpawnBotCrossAccount: principal may
// assume it only when presenting externalID.
func botTrustPolicy(principal, externalID string) (string, error) {
	b, err := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Principal": map[string]string{"AWS": principal},
			"Action":    "sts:AssumeRole",
			"Condition": map[string]any{"StringEquals": map[string]string{"sts:ExternalId": externalID}},
		}},
	})
	return string(b), err
}

// botRolePermissions is the inline policy on an auto-created SpawnBotCrossAccount.
const botRolePermissions = `{
	"Version": "2012-10-17",
	"Statement": [{
		"Effect": "Allow",
		"Action": [
			"ec2:DescribeInstances",
			"ec2:DescribeTags",
			"ec2:StartInstances",
			"ec2:StopInstances",
			"ec2:CreateTags",
			"ec2:DeleteTags"
		],
		"Resource": "*"
	}]
}`

// EnsureCrossAccountRole makes sure the SpawnBotCrossAccount role in the
// caller's account is one spore-bot can assume, and returns its ARN and the
// ExternalId registrations must carry.
//
//   - No role: it is created trusting principal under a fresh per-account
//     ExternalId.
//   - A role that trusts principal under a per-account ExternalId: reused
//     unchanged, and its ExternalId returned so every registration in the
//     account shares it.
//   - Anything else (it trusts another principal, such as the prism-bot stack
//     role spawn defaulted to before #1046, or only a retired shared ExternalId):
//     an error naming the problem, unless repair is set. Then a role spawn
//     created has its trust policy replaced with principal + a per-account
//     ExternalId (the existing one if it has one). A role spawn did not create
//     is never modified.
//
// genID supplies new ExternalIds; out receives progress messages.
func EnsureCrossAccountRole(ctx context.Context, client RoleIAM, principal string, repair bool, genID func() (string, error), out io.Writer) (roleARN, externalID string, err error) {
	existing, err := client.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(CrossAccountRoleName)})
	if err != nil {
		var notFound *iamtypes.NoSuchEntityException
		if !errors.As(err, &notFound) {
			return "", "", fmt.Errorf("get role: %w", err)
		}
		return createCrossAccountRole(ctx, client, principal, genID)
	}

	role := existing.Role
	arn := aws.ToString(role.Arn)
	trust, err := parseBotTrust(aws.ToString(role.AssumeRolePolicyDocument))
	if err != nil {
		return "", "", fmt.Errorf("role %s: %w", CrossAccountRoleName, err)
	}
	extID := trust.perAccountExternalID()
	if trust.trusts(principal) && extID != "" {
		return arn, extID, nil
	}

	var problems []string
	if !trust.trusts(principal) {
		problems = append(problems, fmt.Sprintf("it trusts %s, not spore-bot's role %s",
			describePrincipals(trust.principals), principal))
	}
	if extID == "" {
		problems = append(problems, "it has no per-account ExternalId (spore-bot stopped presenting the shared one in spore-host#413)")
	}
	why := strings.Join(problems, ", and ")
	createdBySpawn := hasTag(role.Tags, botPurposeTagKey, botPurposeTagValue)

	if !repair {
		hint := "Re-run with --repair to rewrite its trust policy"
		if !createdBySpawn {
			hint = "It was not created by spawn (no " + botPurposeTagKey + "=" + botPurposeTagValue +
				" tag), so update it where it is managed, e.g. the bot-cross-account-role.yaml stack's BotLambdaRoleArn and ExternalId parameters, then pass --role-arn and --external-id"
		}
		return "", "", fmt.Errorf("the bot cannot assume role %s: %s. %s", arn, why, hint)
	}
	if !createdBySpawn {
		return "", "", fmt.Errorf("--repair refuses to modify role %s: it was not created by spawn (no %s=%s tag), and %s. Update it where it is managed (e.g. the bot-cross-account-role.yaml stack), then pass --role-arn and --external-id",
			arn, botPurposeTagKey, botPurposeTagValue, why)
	}
	if extID == "" {
		if extID, err = genID(); err != nil {
			return "", "", fmt.Errorf("generate external id: %w", err)
		}
	}
	doc, err := botTrustPolicy(principal, extID)
	if err != nil {
		return "", "", err
	}
	if _, err := client.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
		RoleName:       aws.String(CrossAccountRoleName),
		PolicyDocument: aws.String(doc),
	}); err != nil {
		return "", "", fmt.Errorf("update trust policy of %s: %w", CrossAccountRoleName, err)
	}
	_, _ = fmt.Fprintf(out, "  ✓ Repaired %s: %s. It now trusts %s under a per-account ExternalId.\n", CrossAccountRoleName, why, principal)
	_, _ = fmt.Fprintf(out, "    Re-register every other instance in this account: their registrations need this ExternalId.\n")
	return arn, extID, nil
}

func createCrossAccountRole(ctx context.Context, client RoleIAM, principal string, genID func() (string, error)) (string, string, error) {
	extID, err := genID()
	if err != nil {
		return "", "", fmt.Errorf("generate external id: %w", err)
	}
	doc, err := botTrustPolicy(principal, extID)
	if err != nil {
		return "", "", err
	}
	created, err := client.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(CrossAccountRoleName),
		AssumeRolePolicyDocument: aws.String(doc),
		Description:              aws.String("Allows spore-bot Lambda to control EC2 instances in this account via Slack/Teams commands"),
		// Tag so `spawn cleanup`/`resources` can find and attribute it (#258),
		// and so --repair knows spawn owns it.
		Tags: []iamtypes.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:created-by"), Value: aws.String("spawn")},
			{Key: aws.String("spawn:created-at"), Value: aws.String(time.Now().UTC().Format(time.RFC3339))},
			{Key: aws.String(botPurposeTagKey), Value: aws.String(botPurposeTagValue)},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("create role: %w", err)
	}
	if _, err := client.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(CrossAccountRoleName),
		PolicyName:     aws.String("SpawnBotEC2Control"),
		PolicyDocument: aws.String(botRolePermissions),
	}); err != nil {
		return "", "", fmt.Errorf("attach role policy: %w", err)
	}
	return aws.ToString(created.Role.Arn), extID, nil
}

func describePrincipals(ps []string) string {
	if len(ps) == 0 {
		return "no AWS principal"
	}
	return strings.Join(ps, ", ")
}

func hasTag(tags []iamtypes.Tag, key, value string) bool {
	for _, t := range tags {
		if aws.ToString(t.Key) == key && aws.ToString(t.Value) == value {
			return true
		}
	}
	return false
}
