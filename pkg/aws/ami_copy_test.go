package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// fakeCopyEC2 stands in for EC2 in both regions: DescribeImages returns images,
// CopyImage records its input.
type fakeCopyEC2 struct {
	images      []types.Image
	describeIn  []*ec2.DescribeImagesInput
	copyIn      []*ec2.CopyImageInput
	copyErr     error
	copiedImage string
}

func (f *fakeCopyEC2) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	f.describeIn = append(f.describeIn, in)
	return &ec2.DescribeImagesOutput{Images: f.images}, nil
}

func (f *fakeCopyEC2) CopyImage(_ context.Context, in *ec2.CopyImageInput, _ ...func(*ec2.Options)) (*ec2.CopyImageOutput, error) {
	f.copyIn = append(f.copyIn, in)
	if f.copyErr != nil {
		return nil, f.copyErr
	}
	id := f.copiedImage
	if id == "" {
		id = "ami-0f1e2d3c4b5a69788"
	}
	return &ec2.CopyImageOutput{ImageId: aws.String(id)}, nil
}

func sourceImage(tags map[string]string, encrypted bool) types.Image {
	return types.Image{
		ImageId: aws.String("ami-0a1b2c3d4e5f60718"),
		Name:    aws.String("pytorch-2.4"),
		State:   types.ImageStateAvailable,
		Tags:    tagList(tags),
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/xvda"),
			Ebs:        &types.EbsBlockDevice{SnapshotId: aws.String("snap-0src"), Encrypted: aws.Bool(encrypted), VolumeSize: aws.Int32(40)},
		}},
	}
}

func specTags(t *testing.T, in *ec2.CopyImageInput, rt types.ResourceType) map[string]string {
	t.Helper()
	for _, s := range in.TagSpecifications {
		if s.ResourceType == rt {
			m := map[string]string{}
			for _, tg := range s.Tags {
				m[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
			}
			return m
		}
	}
	t.Fatalf("CopyImage carried no %s TagSpecification; the copy's %ss would be untagged and outside spawn's inventory", rt, rt)
	return nil
}

func runCopy(t *testing.T, srcTags map[string]string, encrypted bool, in CopyAMIInput) (*fakeCopyEC2, *CopyAMIResult, error) {
	t.Helper()
	src := &fakeCopyEC2{images: []types.Image{sourceImage(srcTags, encrypted)}}
	dst := &fakeCopyEC2{}
	if in.SourceAMI == "" {
		in.SourceAMI = "ami-0a1b2c3d4e5f60718"
	}
	if in.SourceRegion == "" {
		in.SourceRegion = "us-east-1"
	}
	if in.TargetRegion == "" {
		in.TargetRegion = "us-east-2"
	}
	res, err := copyAMI(context.Background(), src, dst, in)
	if len(src.copyIn) != 0 {
		t.Fatalf("CopyImage was called on the SOURCE-region client; it must run in the target region")
	}
	return dst, res, err
}

// The issue's ask: the copy keeps spawn's tags, on the AMI AND on its
// snapshots (CopyImage copies neither by default), plus spawn:copied-from.
func TestCopyAMI_TagsImageAndSnapshotsWithSourceTagsAndCopiedFrom(t *testing.T) {
	srcTags := map[string]string{
		"spawn:managed": "true", "spawn:stack": "pytorch", "spawn:version": "2.4",
		"spawn:base-ami": "ami-0base", "Name": "pytorch", "team": "vision",
	}
	dst, res, err := runCopy(t, srcTags, false, CopyAMIInput{})
	if err != nil {
		t.Fatalf("copyAMI: %v", err)
	}
	if len(dst.copyIn) != 1 {
		t.Fatalf("CopyImage calls in the target region = %d, want 1", len(dst.copyIn))
	}
	in := dst.copyIn[0]
	if aws.ToString(in.SourceRegion) != "us-east-1" || aws.ToString(in.SourceImageId) != "ami-0a1b2c3d4e5f60718" {
		t.Errorf("CopyImage source = %s@%s, want ami-0a1b2c3d4e5f60718@us-east-1", aws.ToString(in.SourceImageId), aws.ToString(in.SourceRegion))
	}
	for _, rt := range []types.ResourceType{types.ResourceTypeImage, types.ResourceTypeSnapshot} {
		got := specTags(t, in, rt)
		for k, v := range srcTags {
			if got[k] != v {
				t.Errorf("%s tag %s = %q, want the source's %q", rt, k, got[k], v)
			}
		}
		if want := "ami-0a1b2c3d4e5f60718@us-east-1"; got[CopiedFromTagKey] != want {
			t.Errorf("%s tag %s = %q, want %q", rt, CopiedFromTagKey, got[CopiedFromTagKey], want)
		}
	}
	if res.AMIID != "ami-0f1e2d3c4b5a69788" || res.TargetRegion != "us-east-2" {
		t.Errorf("result = %s in %s, want ami-0f1e2d3c4b5a69788 in us-east-2", res.AMIID, res.TargetRegion)
	}
	if aws.ToString(in.Name) != "pytorch-2.4" {
		t.Errorf("default name = %q, want the source's", aws.ToString(in.Name))
	}
}

// A copy must not inherit the source's Ec2ImageBuilderArn: DeleteAMI deletes the
// Image Builder image an AMI is tagged with, so deleting the copy would delete
// the SOURCE's. aws:* tags are reserved and would fail the CopyImage call.
func TestCopyAMI_DropsImageBuilderAndReservedTags(t *testing.T) {
	srcTags := map[string]string{
		"spawn:managed":                 "true",
		"Ec2ImageBuilderArn":            "arn:aws:imagebuilder:us-east-1:123456789012:image/x/1.0.0/1",
		"aws:cloudformation:stack-name": "x",
	}
	dst, _, err := runCopy(t, srcTags, false, CopyAMIInput{})
	if err != nil {
		t.Fatalf("copyAMI: %v", err)
	}
	for _, rt := range []types.ResourceType{types.ResourceTypeImage, types.ResourceTypeSnapshot} {
		for k := range specTags(t, dst.copyIn[0], rt) {
			if k == "Ec2ImageBuilderArn" || strings.HasPrefix(k, "aws:") {
				t.Errorf("%s carries %s; it must not be copied", rt, k)
			}
		}
	}
}

func TestCopyAMI_RefusesNonSpawnAMI(t *testing.T) {
	for name, tags := range map[string]map[string]string{
		"no tags":        {},
		"managed=false":  {"spawn:managed": "false", "spawn:stack": "x"},
		"other tag only": {"Name": "someone-elses"},
	} {
		t.Run(name, func(t *testing.T) {
			dst, _, err := runCopy(t, tags, false, CopyAMIInput{})
			if !errors.Is(err, ErrNotSpawnManagedAMI) {
				t.Fatalf("err = %v, want ErrNotSpawnManagedAMI", err)
			}
			if len(dst.copyIn) != 0 {
				t.Fatalf("CopyImage called %d times for a non-spawn AMI, want 0", len(dst.copyIn))
			}
		})
	}
}

// Encryption follows the source; a KMS key can't cross regions, so with no
// --kms-key the copy uses the target's default key (KmsKeyId unset).
func TestCopyAMI_Encryption(t *testing.T) {
	managed := map[string]string{"spawn:managed": "true"}
	cases := []struct {
		name      string
		encrypted bool
		kms       string
		wantEnc   bool
		wantKey   string
	}{
		{"unencrypted source stays unencrypted", false, "", false, ""},
		{"encrypted source uses target default key", true, "", true, ""},
		{"explicit key on encrypted source", true, "alias/research", true, "alias/research"},
		{"explicit key encrypts unencrypted source", false, "alias/research", true, "alias/research"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst, res, err := runCopy(t, managed, c.encrypted, CopyAMIInput{KMSKeyID: c.kms})
			if err != nil {
				t.Fatalf("copyAMI: %v", err)
			}
			in := dst.copyIn[0]
			if aws.ToBool(in.Encrypted) != c.wantEnc || res.Encrypted != c.wantEnc {
				t.Errorf("Encrypted = %v (result %v), want %v", aws.ToBool(in.Encrypted), res.Encrypted, c.wantEnc)
			}
			if aws.ToString(in.KmsKeyId) != c.wantKey {
				t.Errorf("KmsKeyId = %q, want %q", aws.ToString(in.KmsKeyId), c.wantKey)
			}
		})
	}
}

func TestCopyAMI_ResolvesNameAmongOwnAMIs(t *testing.T) {
	src := &fakeCopyEC2{images: []types.Image{sourceImage(map[string]string{"spawn:managed": "true"}, false)}}
	dst := &fakeCopyEC2{}
	if _, err := copyAMI(context.Background(), src, dst, CopyAMIInput{
		SourceAMI: "pytorch-2.4", SourceRegion: "us-east-1", TargetRegion: "us-east-2",
	}); err != nil {
		t.Fatalf("copyAMI: %v", err)
	}
	in := src.describeIn[0]
	if len(in.ImageIds) != 0 || len(in.Owners) != 1 || in.Owners[0] != "self" ||
		len(in.Filters) != 1 || aws.ToString(in.Filters[0].Name) != "name" || in.Filters[0].Values[0] != "pytorch-2.4" {
		t.Errorf("name lookup = %+v, want owner self + name filter", in)
	}
}

func TestCopyAMI_SameRegionRefused(t *testing.T) {
	_, _, err := runCopy(t, map[string]string{"spawn:managed": "true"}, false, CopyAMIInput{SourceRegion: "us-east-2", TargetRegion: "us-east-2"})
	if err == nil {
		t.Fatal("copying into the source region succeeded; want an error")
	}
}

func TestCopyAMI_DeniedIsAccessDeniedError(t *testing.T) {
	src := &fakeCopyEC2{images: []types.Image{sourceImage(map[string]string{"spawn:managed": "true"}, false)}}
	dst := &fakeCopyEC2{copyErr: &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "no"}}
	_, err := copyAMI(context.Background(), src, dst, CopyAMIInput{SourceAMI: "ami-0a1b2c3d4e5f60718", SourceRegion: "us-east-1", TargetRegion: "us-east-2"})
	var ade *AccessDeniedError
	if !errors.As(err, &ade) || !strings.Contains(ade.Action, "ec2:CopyImage") {
		t.Fatalf("err = %v, want an AccessDeniedError naming ec2:CopyImage", err)
	}
}

// The copy is in spawn's inventory: what the copy is tagged with satisfies both
// definitions of "spawn-managed" — the tag query `spawn footprint` / orphans /
// cleanup run (managedResourceTagFilters), for the AMI and for its snapshots,
// and the predicate `spawn ami list` applies to the image as ListAMIs reads it.
// The source carries no stack tag, so list can only see the copy through
// spawn:managed.
func TestCopiedAMIIsDiscoveredByListAndFootprint(t *testing.T) {
	dst, res, err := runCopy(t, map[string]string{"spawn:managed": "true", "team": "vision"}, false, CopyAMIInput{})
	if err != nil {
		t.Fatalf("copyAMI: %v", err)
	}
	filters := managedResourceTagFilters()
	if len(filters) == 0 {
		t.Fatal("managedResourceTagFilters is empty; discovery has no definition to check against")
	}
	for _, rt := range []types.ResourceType{types.ResourceTypeImage, types.ResourceTypeSnapshot} {
		tags := specTags(t, dst.copyIn[0], rt)
		for _, f := range filters {
			v, ok := tags[aws.ToString(f.Key)]
			match := ok && len(f.Values) == 0
			for _, want := range f.Values {
				match = match || (ok && v == want)
			}
			if !match {
				t.Errorf("copied %s tags %v do not satisfy discovery filter %s=%v; footprint/cleanup would not see it", rt, tags, aws.ToString(f.Key), f.Values)
			}
		}
	}

	img := types.Image{
		ImageId: aws.String(res.AMIID),
		Name:    aws.String(res.Name),
		Tags:    specTagsList(t, dst.copyIn[0], types.ResourceTypeImage),
	}
	if !IsSpawnManagedAMI(amiInfoFromImage(img)) {
		t.Error("spawn ami list would not show the copy")
	}
}

func specTagsList(t *testing.T, in *ec2.CopyImageInput, rt types.ResourceType) []types.Tag {
	return tagList(specTags(t, in, rt))
}

// fakeWaitEC2 returns states in order, repeating the last.
type fakeWaitEC2 struct {
	states []any // types.ImageState or error
	calls  int
}

func (f *fakeWaitEC2) DescribeImages(_ context.Context, _ *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	i := f.calls
	if i >= len(f.states) {
		i = len(f.states) - 1
	}
	f.calls++
	if f.calls > 1000 {
		// An unbounded wait would spin forever against this fake; end it with
		// an error the caller's assertions will reject.
		return nil, errors.New("fake: poll cap exceeded")
	}
	switch s := f.states[i].(type) {
	case error:
		return nil, s
	case types.ImageState:
		return &ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String("ami-0f1e2d3c4b5a69788"), State: s}}}, nil
	}
	return &ec2.DescribeImagesOutput{}, nil
}

func countingSleep(n *int) func(context.Context, time.Duration) error {
	return func(context.Context, time.Duration) error { *n++; return nil }
}

// --wait is bounded: a copy that never becomes available costs exactly MaxPolls
// DescribeImages calls, then ErrAMIWaitExhausted. Counted through the seam, not
// timed.
func TestWaitAMIAvailable_BoundedByMaxPolls(t *testing.T) {
	api := &fakeWaitEC2{states: []any{types.ImageStatePending}}
	sleeps, progress := 0, 0
	err := waitAMIAvailable(context.Background(), api, "ami-0f1e2d3c4b5a69788",
		AMIWaitOptions{Interval: time.Second, MaxPolls: 5, Progress: func(string, int, int) { progress++ }},
		countingSleep(&sleeps))
	if !errors.Is(err, ErrAMIWaitExhausted) {
		t.Fatalf("err = %v, want ErrAMIWaitExhausted", err)
	}
	if api.calls != 5 || sleeps != 4 || progress != 5 {
		t.Errorf("polls=%d sleeps=%d progress=%d, want 5/4/5", api.calls, sleeps, progress)
	}
}

func TestWaitAMIAvailable_Outcomes(t *testing.T) {
	notFound := &smithy.GenericAPIError{Code: "InvalidAMIID.NotFound"}
	cases := []struct {
		name      string
		states    []any
		wantErr   bool
		wantCalls int
	}{
		{"available after pending and not-yet-visible", []any{notFound, types.ImageStatePending, types.ImageStateAvailable}, false, 3},
		{"failed stops at once", []any{types.ImageStatePending, types.ImageStateFailed}, true, 2},
		{"other describe error stops", []any{&smithy.GenericAPIError{Code: "Throttling"}}, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &fakeWaitEC2{states: c.states}
			sleeps := 0
			err := waitAMIAvailable(context.Background(), api, "ami-0f1e2d3c4b5a69788", AMIWaitOptions{MaxPolls: 10}, countingSleep(&sleeps))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if errors.Is(err, ErrAMIWaitExhausted) {
				t.Fatalf("err = %v; a terminal state must not be reported as a timeout", err)
			}
			if api.calls != c.wantCalls {
				t.Errorf("polls = %d, want %d", api.calls, c.wantCalls)
			}
		})
	}
}
