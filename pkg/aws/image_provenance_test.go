package aws

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// #1364: an image spawn creates records the creating spawn's version and the
// source instance's spawn:version, spawn:spored-version, spawn:mpi-enabled and
// spawn:job-array-index, so a later fix can recognise the images made before
// it.

type fakeCreateImage struct{ in []*ec2.CreateImageInput }

func (f *fakeCreateImage) CreateImage(_ context.Context, in *ec2.CreateImageInput, _ ...func(*ec2.Options)) (*ec2.CreateImageOutput, error) {
	f.in = append(f.in, in)
	return &ec2.CreateImageOutput{ImageId: aws.String("ami-0a1b2c3d4e5f60719")}, nil
}

func withCallerVersion(t *testing.T, v string) {
	t.Helper()
	old := CallerVersion
	CallerVersion = v
	t.Cleanup(func() { CallerVersion = old })
}

// createImageSpecs runs createAMI and returns the tags on its image and
// snapshot TagSpecifications, which must agree.
func createImageSpecs(t *testing.T, in CreateAMIInput) map[string]string {
	t.Helper()
	f := &fakeCreateImage{}
	if _, err := createAMI(context.Background(), f, in); err != nil {
		t.Fatalf("createAMI: %v", err)
	}
	if len(f.in) != 1 {
		t.Fatalf("CreateImage calls = %d, want 1", len(f.in))
	}
	got := map[types.ResourceType]map[string]string{}
	for _, s := range f.in[0].TagSpecifications {
		m := map[string]string{}
		for _, tg := range s.Tags {
			m[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
		}
		got[s.ResourceType] = m
	}
	img, snap := got[types.ResourceTypeImage], got[types.ResourceTypeSnapshot]
	if img == nil || snap == nil {
		t.Fatalf("CreateImage TagSpecifications cover %v; want the image and its snapshots", got)
	}
	if fmt.Sprint(img) != fmt.Sprint(snap) {
		t.Errorf("image tags %v differ from snapshot tags %v", img, snap)
	}
	return img
}

// The issue's tag names, written from the issue, not from the constants.
var wantSourceCopies = map[string]string{
	"spawn:version":         "spawn:source-version",
	"spawn:spored-version":  "spawn:source-spored-version",
	"spawn:mpi-enabled":     "spawn:source-mpi-enabled",
	"spawn:job-array-index": "spawn:source-job-array-index",
}

func TestCreateAMI_RecordsTheSourceAndCreatingSpawn(t *testing.T) {
	withCallerVersion(t, "0.148.0")
	src := map[string]string{
		"spawn:version": "0.146.0", "spawn:spored-version": "0.146.1",
		"spawn:mpi-enabled": "true", "spawn:job-array-index": "3",
		"spawn:job-array-id": "ja-1", "Name": "rank-3",
	}
	got := createImageSpecs(t, CreateAMIInput{
		InstanceID: "i-0src", Name: "img", SourceTags: src,
		Tags: map[string]string{"spawn:managed": "true", "stack": "mpi"},
	})
	if got["spawn:image-spawn-version"] != "0.148.0" {
		t.Errorf("spawn:image-spawn-version = %q, want the creating CLI's 0.148.0", got["spawn:image-spawn-version"])
	}
	for from, to := range wantSourceCopies {
		if got[to] != src[from] {
			t.Errorf("%s = %q, want the source's %s %q", to, got[to], from, src[from])
		}
	}
	if got["spawn:managed"] != "true" || got["stack"] != "mpi" {
		t.Errorf("the caller's tags were lost: %v", got)
	}
	for _, k := range []string{"spawn:version", "spawn:job-array-id", "Name"} {
		if _, ok := got[k]; ok {
			t.Errorf("image carries the source's own %s; only the spawn:source-* copies belong on it", k)
		}
	}
}

// A source tag the instance does not have is left off, not written empty:
// absent means "not known", and an empty spawn:source-version would read as
// a version.
func TestCreateAMI_OmitsSourceTagsTheSourceLacks(t *testing.T) {
	withCallerVersion(t, "0.148.0")
	for name, src := range map[string]map[string]string{
		"no tags":      nil,
		"not MPI":      {"spawn:version": "0.146.0"},
		"empty values": {"spawn:version": "", "spawn:mpi-enabled": ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := createImageSpecs(t, CreateAMIInput{InstanceID: "i-0src", Name: "img", SourceTags: src})
			for from, to := range wantSourceCopies {
				v, ok := got[to]
				switch {
				case src[from] == "" && ok:
					t.Errorf("%s = %q written for a source without %s", to, v, from)
				case src[from] != "" && v != src[from]:
					t.Errorf("%s = %q, want %q", to, v, src[from])
				}
			}
			if got["spawn:image-spawn-version"] != "0.148.0" {
				t.Errorf("spawn:image-spawn-version = %q, want 0.148.0 even when the source records nothing", got["spawn:image-spawn-version"])
			}
		})
	}
}

// An SDK caller that never set CallerVersion gets no spawn:image-spawn-version
// rather than an invented one (the spawn#515 lesson: a placeholder version is
// worse than none).
func TestCreateAMI_NoCallerVersionNoImageSpawnVersion(t *testing.T) {
	withCallerVersion(t, "")
	got := createImageSpecs(t, CreateAMIInput{InstanceID: "i-0src", Name: "img", Tags: map[string]string{"a": "b"}})
	if v, ok := got["spawn:image-spawn-version"]; ok {
		t.Errorf("spawn:image-spawn-version = %q with no CallerVersion; want it omitted", v)
	}
}

// --tag cannot forge provenance, whether or not the source has the tag.
func TestCreateAMI_UserTagsCannotOverrideProvenance(t *testing.T) {
	withCallerVersion(t, "0.148.0")
	user := map[string]string{"spawn:managed": "true"}
	for _, k := range ImageProvenanceTagKeys() {
		user[k] = "x"
	}
	got := createImageSpecs(t, CreateAMIInput{
		InstanceID: "i-0src", Name: "img", Tags: user,
		SourceTags: map[string]string{"spawn:version": "0.140.0"},
	})
	if got["spawn:source-version"] != "0.140.0" {
		t.Errorf("spawn:source-version = %q, want the source's 0.140.0 over --tag x", got["spawn:source-version"])
	}
	if got["spawn:image-spawn-version"] != "0.148.0" {
		t.Errorf("spawn:image-spawn-version = %q, want 0.148.0 over --tag x", got["spawn:image-spawn-version"])
	}
	for _, k := range []string{"spawn:source-mpi-enabled", "spawn:source-job-array-index", "spawn:source-spored-version"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %q: a --tag value survived for a source without the tag", k, v)
		}
	}
}

// ImageProvenanceTagKeys must cover every key ImageCreateTags writes, or a
// --tag for the missing one would get through.
func TestImageProvenanceTagKeysCoverEverythingWritten(t *testing.T) {
	withCallerVersion(t, "0.148.0")
	all := map[string]string{}
	for from := range wantSourceCopies {
		all[from] = "v"
	}
	written := ImageCreateTags(nil, all)
	if len(written) == 0 {
		t.Fatal("ImageCreateTags wrote nothing")
	}
	reserved := map[string]bool{}
	for _, k := range ImageProvenanceTagKeys() {
		reserved[k] = true
	}
	for k := range written {
		if !reserved[k] || !IsImageProvenanceTagKey(k) {
			t.Errorf("ImageCreateTags writes %s, which ImageProvenanceTagKeys/IsImageProvenanceTagKey do not reserve", k)
		}
	}
}

func TestCreateAMI_RefusesMoreThanFiftyTags(t *testing.T) {
	withCallerVersion(t, "0.148.0")
	user := map[string]string{}
	for i := 0; i < 49; i++ {
		user[fmt.Sprintf("k%02d", i)] = "v"
	}
	f := &fakeCreateImage{}
	_, err := createAMI(context.Background(), f, CreateAMIInput{InstanceID: "i-0src", Name: "img", Tags: user,
		SourceTags: map[string]string{"spawn:version": "0.146.0"}})
	if err == nil || !strings.Contains(err.Error(), "51 tags") {
		t.Fatalf("err = %v, want a refusal naming 51 tags", err)
	}
	if len(f.in) != 0 {
		t.Errorf("CreateImage was called; EC2 would have rejected it")
	}
	// 48 user tags + 2 provenance tags fit exactly.
	delete(user, "k00")
	createImageSpecs(t, CreateAMIInput{InstanceID: "i-0src", Name: "img", Tags: user,
		SourceTags: map[string]string{"spawn:version": "0.146.0"}})
}

// `spawn ami copy` keeps the source image's provenance on the copy and its
// snapshots, and does not stamp the copying spawn's version as the image's: a
// copy of an image from before a fix is still an image from before the fix.
func TestCopyAMI_PreservesProvenance(t *testing.T) {
	withCallerVersion(t, "0.150.0")
	prov := map[string]string{
		"spawn:image-spawn-version":    "0.148.0",
		"spawn:source-version":         "0.146.0",
		"spawn:source-spored-version":  "0.146.1",
		"spawn:source-mpi-enabled":     "true",
		"spawn:source-job-array-index": "2",
	}
	src := map[string]string{"spawn:managed": "true"}
	for k, v := range prov {
		src[k] = v
	}
	dst, _, err := runCopy(t, src, false, CopyAMIInput{})
	if err != nil {
		t.Fatalf("copyAMI: %v", err)
	}
	for _, rt := range []types.ResourceType{types.ResourceTypeImage, types.ResourceTypeSnapshot} {
		got := specTags(t, dst.copyIn[0], rt)
		for k, v := range prov {
			if got[k] != v {
				t.Errorf("copy %s tag %s = %q, want the source image's %q", rt, k, got[k], v)
			}
		}
	}

	dst, _, err = runCopy(t, map[string]string{"spawn:managed": "true"}, false, CopyAMIInput{})
	if err != nil {
		t.Fatalf("copyAMI: %v", err)
	}
	for _, rt := range []types.ResourceType{types.ResourceTypeImage, types.ResourceTypeSnapshot} {
		if v, ok := specTags(t, dst.copyIn[0], rt)["spawn:image-spawn-version"]; ok {
			t.Errorf("copy of an image without provenance got %s spawn:image-spawn-version=%q", rt, v)
		}
	}
}

func TestCopyAMI_RefusesMoreThanFiftyTags(t *testing.T) {
	src := map[string]string{"spawn:managed": "true"}
	for i := 0; i < 49; i++ {
		src[fmt.Sprintf("k%02d", i)] = "v"
	}
	dst, _, err := runCopy(t, src, false, CopyAMIInput{})
	if err == nil || !strings.Contains(err.Error(), "51 tags") {
		t.Fatalf("err = %v, want a refusal naming 51 tags (50 + spawn:copied-from)", err)
	}
	if len(dst.copyIn) != 0 {
		t.Errorf("CopyImage was called; EC2 would have rejected it")
	}
}

// What CreateAMI writes, `spawn ami list` reads back.
func TestImageProvenanceRoundTripsThroughListAMIs(t *testing.T) {
	withCallerVersion(t, "0.148.0")
	tags := ImageCreateTags(nil, map[string]string{
		"spawn:version": "0.146.0", "spawn:spored-version": "0.146.1",
		"spawn:mpi-enabled": "true", "spawn:job-array-index": "0",
	})
	p := amiInfoFromImage(types.Image{ImageId: aws.String("ami-0a1b2c3d4e5f6071a"), Tags: tagList(tags)}).Provenance
	want := ImageProvenance{SpawnVersion: "0.148.0", SourceVersion: "0.146.0", SourceSporedVersion: "0.146.1",
		SourceMPIEnabled: "true", SourceJobArrayIndex: "0"}
	if p != want {
		t.Errorf("Provenance = %+v, want %+v", p, want)
	}
	if !p.Recorded() {
		t.Error("Recorded() = false for an image with provenance")
	}
	if (ImageProvenance{}).Recorded() {
		t.Error("Recorded() = true for an image without provenance")
	}
}

func TestWindowsImportRecordsImageSpawnVersion(t *testing.T) {
	withCallerVersion(t, "0.148.0")
	for _, tg := range windowsImportTags() {
		if aws.ToString(tg.Key) == "spawn:image-spawn-version" {
			if aws.ToString(tg.Value) != "0.148.0" {
				t.Errorf("spawn:image-spawn-version = %q", aws.ToString(tg.Value))
			}
			return
		}
	}
	t.Error("an imported Windows AMI carries no spawn:image-spawn-version")
}
