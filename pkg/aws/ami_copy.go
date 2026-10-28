package aws

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// CopiedFromTagKey records which AMI a `spawn ami copy` was made from, as
// "<source-ami-id>@<source-region>". It is written on the new AMI and on every
// snapshot backing it.
const CopiedFromTagKey = "spawn:copied-from"

// ErrNotSpawnManagedAMI is returned by CopyAMI for an AMI without
// spawn:managed=true. spawn copies only what it can also list and delete: a copy
// of a foreign AMI would be tagged into spawn's inventory without spawn ever
// having owned the original.
var ErrNotSpawnManagedAMI = errors.New("not a spawn-managed AMI (no spawn:managed=true tag)")

// copyAMIEC2 is the slice of EC2 CopyAMI needs: DescribeImages in the source
// region, CopyImage in the target region.
type copyAMIEC2 interface {
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	CopyImage(ctx context.Context, in *ec2.CopyImageInput, optFns ...func(*ec2.Options)) (*ec2.CopyImageOutput, error)
}

// CopyAMIInput describes one `spawn ami copy`.
type CopyAMIInput struct {
	// SourceAMI is an AMI id (ami-…) or the exact name of an AMI this account
	// owns in SourceRegion.
	SourceAMI    string
	SourceRegion string // empty: the client's configured region
	TargetRegion string
	// Name for the copy. Empty keeps the source AMI's name (names are only
	// unique per region, so this normally works).
	Name string
	// KMSKeyID encrypts the copy with this key, which must exist in
	// TargetRegion. Empty: an encrypted source is re-encrypted with the target
	// region's default EBS key (aws/ebs); an unencrypted source stays
	// unencrypted.
	KMSKeyID string
}

// CopyAMIResult reports what CopyAMI started. The copy is pending when
// returned; WaitForAMICopy waits for it.
type CopyAMIResult struct {
	SourceAMIID  string            `json:"source_ami_id"`
	SourceRegion string            `json:"source_region"`
	AMIID        string            `json:"ami_id"`
	TargetRegion string            `json:"target_region"`
	Name         string            `json:"name"`
	Encrypted    bool              `json:"encrypted"`
	KMSKeyID     string            `json:"kms_key_id,omitempty"` // empty: the target region's default EBS key
	Tags         map[string]string `json:"tags"`                 // applied to the AMI and each of its snapshots
}

// CopyAMI copies a spawn-managed AMI to another region so the copy stays inside
// spawn's inventory: `spawn ami list --region <target>`, `spawn footprint`
// (both find spawn:managed=true) and `spawn ami delete --region <target>`.
//
// CopyImage does not carry tags across by default, and CopyImageTags would tag
// only the AMI, never its snapshots. So the tags are passed as
// TagSpecifications for both the image and its snapshots, which applies them
// atomically with the copy: there is no window in which an untagged copy exists.
//
// The copy is independent of its source. Deleting the source does not delete
// copies, and deleting a copy does not touch the source; its snapshots are new
// snapshots that no other AMI references.
//
// Needs ec2:DescribeImages (source region) and ec2:CopyImage + ec2:CreateTags
// (target region); a denial is returned as an *AccessDeniedError naming the
// action.
func (c *Client) CopyAMI(ctx context.Context, in CopyAMIInput) (*CopyAMIResult, error) {
	if in.SourceRegion == "" {
		in.SourceRegion = c.cfg.Region
	}
	return copyAMI(ctx, c.regionalEC2(in.SourceRegion), c.regionalEC2(in.TargetRegion), in)
}

func copyAMI(ctx context.Context, src, dst copyAMIEC2, in CopyAMIInput) (*CopyAMIResult, error) {
	if in.TargetRegion == "" {
		return nil, fmt.Errorf("a target region is required")
	}
	if in.SourceRegion == "" {
		return nil, fmt.Errorf("a source region is required")
	}
	if in.TargetRegion == in.SourceRegion {
		return nil, fmt.Errorf("the AMI is already in %s; --to-region must be a different region", in.SourceRegion)
	}

	img, err := resolveSourceAMI(ctx, src, in.SourceAMI, in.SourceRegion)
	if err != nil {
		return nil, err
	}
	srcID := aws.ToString(img.ImageId)
	srcTags := map[string]string{}
	for _, t := range img.Tags {
		srcTags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	if srcTags["spawn:managed"] != "true" {
		return nil, fmt.Errorf("refusing to copy %s (%s): %w. spawn ami copy only copies AMIs spawn created, "+
			"so the copy can be listed and deleted by spawn; copy other AMIs with 'aws ec2 copy-image'",
			srcID, aws.ToString(img.Name), ErrNotSpawnManagedAMI)
	}

	encrypted := in.KMSKeyID != ""
	for _, bdm := range img.BlockDeviceMappings {
		if bdm.Ebs != nil && aws.ToBool(bdm.Ebs.Encrypted) {
			encrypted = true
		}
	}

	name := in.Name
	if name == "" {
		name = aws.ToString(img.Name)
	}
	tags := copiedAMITags(srcTags, srcID, in.SourceRegion)
	if err := checkImageTagCount("copy "+srcID, tags); err != nil {
		return nil, err
	}

	copyIn := &ec2.CopyImageInput{
		Name:          aws.String(name),
		SourceImageId: aws.String(srcID),
		SourceRegion:  aws.String(in.SourceRegion),
		Description:   img.Description,
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeImage, Tags: tagList(tags)},
			{ResourceType: types.ResourceTypeSnapshot, Tags: tagList(tags)},
		},
	}
	if encrypted {
		copyIn.Encrypted = aws.Bool(true)
		if in.KMSKeyID != "" {
			copyIn.KmsKeyId = aws.String(in.KMSKeyID)
		}
	}
	out, err := dst.CopyImage(ctx, copyIn)
	if err != nil {
		if d := denied("ec2:CopyImage and ec2:CreateTags in "+in.TargetRegion, err); d != nil {
			return nil, d
		}
		return nil, fmt.Errorf("copy %s from %s to %s: %w", srcID, in.SourceRegion, in.TargetRegion, err)
	}
	return &CopyAMIResult{
		SourceAMIID:  srcID,
		SourceRegion: in.SourceRegion,
		AMIID:        aws.ToString(out.ImageId),
		TargetRegion: in.TargetRegion,
		Name:         name,
		Encrypted:    encrypted,
		KMSKeyID:     in.KMSKeyID,
		Tags:         tags,
	}, nil
}

// resolveSourceAMI finds the source image by id, or by exact name among the
// AMIs this account owns.
func resolveSourceAMI(ctx context.Context, src copyAMIEC2, ref, region string) (types.Image, error) {
	if ref == "" {
		return types.Image{}, fmt.Errorf("an AMI id or name is required")
	}
	in := &ec2.DescribeImagesInput{}
	if strings.HasPrefix(ref, "ami-") {
		in.ImageIds = []string{ref}
	} else {
		in.Owners = []string{"self"}
		in.Filters = []types.Filter{{Name: aws.String("name"), Values: []string{ref}}}
	}
	out, err := src.DescribeImages(ctx, in)
	if err != nil {
		if d := denied("ec2:DescribeImages in "+region, err); d != nil {
			return types.Image{}, d
		}
		return types.Image{}, fmt.Errorf("describe AMI %s in %s: %w", ref, region, err)
	}
	switch len(out.Images) {
	case 0:
		return types.Image{}, fmt.Errorf("AMI %s not found in %s", ref, region)
	case 1:
		return out.Images[0], nil
	default:
		ids := make([]string, 0, len(out.Images))
		for _, i := range out.Images {
			ids = append(ids, aws.ToString(i.ImageId))
		}
		sort.Strings(ids)
		return types.Image{}, fmt.Errorf("%d AMIs named %q in %s (%s); pass the AMI id", len(ids), ref, region, strings.Join(ids, ", "))
	}
}

// copiedAMITags is the tag set a copy carries: the source's own tags, plus
// spawn:copied-from, minus two kinds that would be wrong on the copy.
//
// The source's provenance tags (ImageProvenanceTagKeys, #1364) are kept as
// they are: the copy has the same disk, so it was made by the same spawn from
// the same instance. The copying spawn's version is deliberately NOT written
// as spawn:image-spawn-version, which would make a copy of an image from
// before a fix look as if it were made after it.
//
//   - aws:* tags are reserved; EC2 rejects them in a TagSpecification.
//   - Ec2ImageBuilderArn names the source's Image Builder image, in the source
//     region. DeleteAMI deletes the Image Builder resource an AMI carries that
//     tag for, so a copy that inherited it would, when deleted, try to delete
//     the SOURCE's Image Builder image.
func copiedAMITags(src map[string]string, srcID, srcRegion string) map[string]string {
	out := make(map[string]string, len(src)+1)
	for k, v := range src {
		if strings.HasPrefix(k, "aws:") || k == "Ec2ImageBuilderArn" {
			continue
		}
		out[k] = v
	}
	out[CopiedFromTagKey] = srcID + "@" + srcRegion
	return out
}

func tagList(m map[string]string) []types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tags := make([]types.Tag, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return tags
}

// AMIWaitOptions bounds WaitForAMICopy.
type AMIWaitOptions struct {
	// Interval between DescribeImages polls. Zero: 15s.
	Interval time.Duration
	// MaxPolls is the most DescribeImages calls made before giving up. Zero:
	// 240 (an hour at the default interval).
	MaxPolls int
	// Progress, if set, is called after every poll with the image state.
	Progress func(state string, poll, maxPolls int)
}

// ErrAMIWaitExhausted is returned when MaxPolls is reached before the AMI is
// available. The copy itself keeps going; only the wait stopped.
var ErrAMIWaitExhausted = errors.New("AMI still not available")

// WaitForAMICopy polls an AMI in region until it is available, fails, or
// MaxPolls is reached. Unlike WaitForAMI it reports progress and treats a
// briefly-invisible new AMI (InvalidAMIID.NotFound right after CopyImage) as
// still pending.
func (c *Client) WaitForAMICopy(ctx context.Context, region, amiID string, opts AMIWaitOptions) error {
	return waitAMIAvailable(ctx, c.regionalEC2(region), amiID, opts, sleepCtx)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type describeImagesAPI interface {
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
}

func waitAMIAvailable(ctx context.Context, api describeImagesAPI, amiID string, opts AMIWaitOptions, sleep func(context.Context, time.Duration) error) error {
	interval := opts.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	maxPolls := opts.MaxPolls
	if maxPolls <= 0 {
		maxPolls = 240
	}
	state := "pending"
	for poll := 1; poll <= maxPolls; poll++ {
		out, err := api.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{amiID}})
		switch {
		case err != nil && isAMINotFound(err):
			state = "pending"
		case err != nil:
			return fmt.Errorf("describe AMI %s: %w", amiID, err)
		case len(out.Images) == 0:
			state = "pending"
		default:
			img := out.Images[0]
			state = string(img.State)
			switch img.State {
			case types.ImageStateAvailable:
				if opts.Progress != nil {
					opts.Progress(state, poll, maxPolls)
				}
				return nil
			case types.ImageStateFailed, types.ImageStateError, types.ImageStateInvalid,
				types.ImageStateDeregistered, types.ImageStateDisabled:
				reason := ""
				if img.StateReason != nil {
					reason = ": " + aws.ToString(img.StateReason.Message)
				}
				return fmt.Errorf("AMI %s is %s%s", amiID, state, reason)
			}
		}
		if opts.Progress != nil {
			opts.Progress(state, poll, maxPolls)
		}
		if poll == maxPolls {
			break
		}
		if err := sleep(ctx, interval); err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: %s is %s after %d polls", ErrAMIWaitExhausted, amiID, state, maxPolls)
}

func isAMINotFound(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "InvalidAMIID.NotFound"
}
