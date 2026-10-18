package provenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Phase 3 of #1076: the tiering rules (design, Decisions 3, 5 and 6). They
// only move objects to cheaper storage, which is reversible. Nothing here
// deletes anything.

// RuleIDPrefix marks the lifecycle rules spawn owns. A bucket's lifecycle
// configuration is one document that a write replaces whole, so spawn reads
// it, changes only the rules carrying this prefix, and keeps every other rule
// exactly as it found it.
const RuleIDPrefix = "spawn-regen-"

// minTransitionSize is the smallest object the rules transition. S3's own
// default (all_storage_classes_128K) already skips smaller objects, but that
// is a bucket setting its owner can change, so each rule states the bound in
// its filter and behaves the same either way. It matches TierTarget and the
// minimum Standard-IA and Glacier IR bill for.
const minTransitionSize = minTransitionBytes - 1 // ObjectSizeGreaterThan is strict

// Rules are the tiering rules, built from Decisions 3. Each requires
// spawn:retain=false, because a lifecycle filter can't negate: setting
// spawn:retain=true takes an object out of every rule, and an object with no
// spawn:retain tag (written before it existed) matches none.
func Rules() []s3types.LifecycleRule {
	rule := func(class Class, transitions ...s3types.Transition) s3types.LifecycleRule {
		return s3types.LifecycleRule{
			ID:     aws.String(RuleIDPrefix + string(class)),
			Status: s3types.ExpirationStatusEnabled,
			Filter: &s3types.LifecycleRuleFilter{And: &s3types.LifecycleRuleAndOperator{
				Tags: []s3types.Tag{
					{Key: aws.String(TagClass), Value: aws.String(string(class))},
					{Key: aws.String(TagRetain), Value: aws.String("false")},
				},
				ObjectSizeGreaterThan: aws.Int64(minTransitionSize),
			}},
			Transitions: transitions,
		}
	}
	days := func(d int32, sc s3types.TransitionStorageClass) s3types.Transition {
		return s3types.Transition{Days: aws.Int32(d), StorageClass: sc}
	}
	return []s3types.LifecycleRule{
		rule(ClassCheap,
			days(int32(CheapToIA.Hours()/24), s3types.TransitionStorageClassStandardIa),
			days(int32(CheapToGIR.Hours()/24), s3types.TransitionStorageClassGlacierIr)),
		rule(ClassModerate,
			days(int32(ModerateToIA.Hours()/24), s3types.TransitionStorageClassStandardIa)),
	}
}

// LifecycleAPI is what installing the rules needs from S3. IAM:
// s3:GetLifecycleConfiguration and s3:PutLifecycleConfiguration (which also
// covers DeleteBucketLifecycle).
type LifecycleAPI interface {
	GetBucketLifecycleConfiguration(ctx context.Context, in *s3.GetBucketLifecycleConfigurationInput, optFns ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error)
	PutBucketLifecycleConfiguration(ctx context.Context, in *s3.PutBucketLifecycleConfigurationInput, optFns ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error)
	DeleteBucketLifecycle(ctx context.Context, in *s3.DeleteBucketLifecycleInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketLifecycleOutput, error)
}

// LifecyclePlan is what applying would do to one bucket. It is computed
// without writing anything, so a dry run and an apply print the same plan.
type LifecyclePlan struct {
	Bucket string `json:"bucket"`
	Remove bool   `json:"remove"`
	// Others are the bucket's rules spawn doesn't own. They are written back
	// unchanged.
	Others []s3types.LifecycleRule `json:"other_rules"`
	// Current are spawn's rules as the bucket has them now; Desired as they
	// will be. Desired is empty when Remove is set.
	Current []s3types.LifecycleRule `json:"current_spawn_rules"`
	Desired []s3types.LifecycleRule `json:"desired_spawn_rules"`
	// MinimumObjectSize is the bucket's TransitionDefaultMinimumObjectSize,
	// written back as found so the owner's setting survives the write.
	MinimumObjectSize s3types.TransitionDefaultMinimumObjectSize `json:"transition_default_minimum_object_size,omitempty"`
	Changed           bool                                       `json:"changed"`
}

// PlanLifecycle reads the bucket's lifecycle configuration and works out the
// change. owner is the expected bucket owner's account id: every call carries
// it, so a mistyped bucket name that belongs to another account fails rather
// than being read or written.
func PlanLifecycle(ctx context.Context, api LifecycleAPI, bucket, owner string, remove bool) (*LifecyclePlan, error) {
	if owner == "" {
		return nil, errors.New("plan lifecycle: the expected bucket owner is required")
	}
	p := &LifecyclePlan{Bucket: bucket, Remove: remove}
	out, err := api.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucket), ExpectedBucketOwner: aws.String(owner)})
	switch {
	case err == nil:
		p.MinimumObjectSize = out.TransitionDefaultMinimumObjectSize
		for _, r := range out.Rules {
			if strings.HasPrefix(aws.ToString(r.ID), RuleIDPrefix) {
				p.Current = append(p.Current, r)
			} else {
				p.Others = append(p.Others, r)
			}
		}
	case errorCode(err) == "NoSuchLifecycleConfiguration":
		// No configuration yet: nothing to keep.
	default:
		return nil, fmt.Errorf("read lifecycle configuration of s3://%s: %w", bucket, err)
	}
	if !remove {
		p.Desired = Rules()
	}
	p.Changed = !sameRules(p.Current, p.Desired)
	return p, nil
}

// ApplyLifecycle writes the plan, then reads the configuration back and checks
// that spawn's rules are what was planned and every other rule survived. A
// write that S3 accepted is not taken as proof that the bucket now has it.
func ApplyLifecycle(ctx context.Context, api LifecycleAPI, p *LifecyclePlan, owner string) error {
	if !p.Changed {
		return nil
	}
	rules := append(append([]s3types.LifecycleRule{}, p.Others...), p.Desired...)
	if len(rules) == 0 {
		// A configuration can't be empty; removing spawn's only rules deletes it.
		if _, err := api.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{
			Bucket: aws.String(p.Bucket), ExpectedBucketOwner: aws.String(owner)}); err != nil {
			return fmt.Errorf("delete lifecycle configuration of s3://%s: %w", p.Bucket, err)
		}
	} else {
		in := &s3.PutBucketLifecycleConfigurationInput{
			Bucket:                 aws.String(p.Bucket),
			ExpectedBucketOwner:    aws.String(owner),
			LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: rules},
		}
		if p.MinimumObjectSize != "" {
			in.TransitionDefaultMinimumObjectSize = p.MinimumObjectSize
		}
		if _, err := api.PutBucketLifecycleConfiguration(ctx, in); err != nil {
			return fmt.Errorf("write lifecycle configuration of s3://%s: %w", p.Bucket, err)
		}
	}
	after, err := PlanLifecycle(ctx, api, p.Bucket, owner, p.Remove)
	if err != nil {
		return fmt.Errorf("verify lifecycle configuration of s3://%s: %w", p.Bucket, err)
	}
	if after.Changed {
		return fmt.Errorf("verify lifecycle configuration of s3://%s: the write was accepted but spawn's rules read back differently", p.Bucket)
	}
	if !sameRules(after.Others, p.Others) {
		return fmt.Errorf("verify lifecycle configuration of s3://%s: the bucket's other rules changed during the write; another writer may have raced this one", p.Bucket)
	}
	if p.MinimumObjectSize != "" && after.MinimumObjectSize != p.MinimumObjectSize {
		return fmt.Errorf("verify lifecycle configuration of s3://%s: TransitionDefaultMinimumObjectSize changed from %q to %q", p.Bucket, p.MinimumObjectSize, after.MinimumObjectSize)
	}
	return nil
}

// sameRules compares rule sets by content, ignoring order. S3 can return
// fields the request left unset (an empty Prefix, for instance), so rules are
// compared in a normalised form rather than as the SDK structs.
func sameRules(a, b []s3types.LifecycleRule) bool {
	norm := func(rs []s3types.LifecycleRule) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = normaliseRule(r)
		}
		sort.Strings(out)
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

func normaliseRule(r s3types.LifecycleRule) string {
	type tr struct {
		Days  int32
		Class string
	}
	n := struct {
		ID, Status, Prefix string
		Tags               []string
		MinSize, MaxSize   int64
		Transitions        []tr
		Rest               string
	}{ID: aws.ToString(r.ID), Status: string(r.Status)}
	if f := r.Filter; f != nil {
		n.Prefix = aws.ToString(f.Prefix)
		n.MinSize, n.MaxSize = aws.ToInt64(f.ObjectSizeGreaterThan), aws.ToInt64(f.ObjectSizeLessThan)
		if f.Tag != nil {
			n.Tags = append(n.Tags, aws.ToString(f.Tag.Key)+"="+aws.ToString(f.Tag.Value))
		}
		if a := f.And; a != nil {
			if n.Prefix == "" {
				n.Prefix = aws.ToString(a.Prefix)
			}
			if n.MinSize == 0 {
				n.MinSize = aws.ToInt64(a.ObjectSizeGreaterThan)
			}
			if n.MaxSize == 0 {
				n.MaxSize = aws.ToInt64(a.ObjectSizeLessThan)
			}
			for _, t := range a.Tags {
				n.Tags = append(n.Tags, aws.ToString(t.Key)+"="+aws.ToString(t.Value))
			}
		}
	}
	sort.Strings(n.Tags)
	for _, t := range r.Transitions {
		n.Transitions = append(n.Transitions, tr{aws.ToInt32(t.Days), string(t.StorageClass)})
	}
	sort.Slice(n.Transitions, func(i, j int) bool { return n.Transitions[i].Days < n.Transitions[j].Days })
	// Anything else a rule can carry (expiration, noncurrent-version actions,
	// multipart abort, a deprecated top-level Prefix) is compared verbatim, so
	// a rule that differs only there still counts as different.
	//lint:ignore SA1019 a user's existing rule may still use the deprecated top-level Prefix; it must be compared, not dropped
	legacyPrefix := r.Prefix
	rest, _ := json.Marshal(struct {
		Exp, NcExp, NcTr, Abort any
		Prefix                  *string
	}{r.Expiration, r.NoncurrentVersionExpiration, r.NoncurrentVersionTransitions, r.AbortIncompleteMultipartUpload, legacyPrefix})
	n.Rest = string(rest)
	b, _ := json.Marshal(n)
	return string(b)
}

// The storage classes the rules move objects into, as S3 names them.
const (
	StorageClassIA  = string(s3types.StorageClassStandardIa)
	StorageClassGIR = string(s3types.StorageClassGlacierIr)
)

// DescribeRule renders one of spawn's rules for a person: the class it
// matches and where it moves objects, and when.
func DescribeRule(r s3types.LifecycleRule) string {
	class := ""
	if r.Filter != nil && r.Filter.And != nil {
		for _, t := range r.Filter.And.Tags {
			if aws.ToString(t.Key) == TagClass {
				class = aws.ToString(t.Value)
			}
		}
	}
	var steps []string
	for _, t := range r.Transitions {
		name := string(t.StorageClass)
		switch t.StorageClass {
		case s3types.TransitionStorageClassStandardIa:
			name = "Standard-IA"
		case s3types.TransitionStorageClassGlacierIr:
			name = "Glacier IR"
		}
		steps = append(steps, fmt.Sprintf("%s at %d days", name, aws.ToInt32(t.Days)))
	}
	return fmt.Sprintf("%s=%s -> %s", TagClass, class, strings.Join(steps, ", "))
}
