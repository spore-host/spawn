package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spore-host/spawn/pkg/provenance"
	"github.com/spore-host/spawn/pkg/taskproto"
)

func writeSpec(t *testing.T, dir string, spec taskproto.TaskSpec) string {
	t.Helper()
	b, _ := json.Marshal(spec)
	p := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type fakeHeadList struct{ missing string }

func (f fakeHeadList) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if aws.ToString(in.Key) == f.missing {
		return nil, errors.New("NotFound")
	}
	return &s3.HeadObjectOutput{ETag: aws.String(`"e-` + aws.ToString(in.Key) + `"`), VersionId: aws.String("v1"), ContentLength: aws.Int64(7)}, nil
}

func (f fakeHeadList) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	p := aws.ToString(in.Prefix)
	return &s3.ListObjectsV2Output{Contents: []s3types.Object{
		{Key: aws.String(p + "x.fa"), ETag: aws.String(`"ex"`), Size: aws.Int64(1)},
		{Key: aws.String(p + "y.fa"), ETag: aws.String(`"ey"`), Size: aws.Int64(2)},
	}}, nil
}

func TestProvStageInRecordsWhatWasRead(t *testing.T) {
	dir := t.TempDir()
	spec := writeSpec(t, dir, taskproto.TaskSpec{TaskID: "t", Inputs: []taskproto.Manifest{
		{Source: "s3://in/ref.fa", Destination: "/d/ref.fa"},
		{Source: "s3://in/refs/", Destination: "/d/refs/"},
	}})
	if err := provStageIn(context.Background(), fakeHeadList{}, spec, dir); err != nil {
		t.Fatal(err)
	}
	var rec inputsRecord
	b, _ := os.ReadFile(filepath.Join(dir, provInputsFile))
	_ = json.Unmarshal(b, &rec)
	if !rec.Complete || len(rec.Inputs) != 3 {
		t.Fatalf("inputs = %+v, want 3 (one object + a 2-object prefix), complete", rec)
	}
	if rec.Inputs[0].ETag != `"e-ref.fa"` || rec.Inputs[0].VersionID != "v1" || rec.Inputs[1].URI != "s3://in/refs/x.fa" {
		t.Errorf("inputs = %+v", rec.Inputs)
	}
}

// An input that can't be read makes the record incomplete. A later deletion
// sweeper then cannot prove the inputs unchanged, so the output must not be
// marked regenerable, whatever the spec says.
func TestAnUnreadableInputMakesTheOutputNonRegenerable(t *testing.T) {
	dir := t.TempDir()
	spec := writeSpec(t, dir, taskproto.TaskSpec{TaskID: "t", Regenerable: true, Command: []string{"x"},
		Inputs: []taskproto.Manifest{{Source: "s3://in/gone.fa", Destination: "/d/gone.fa"}}})
	if err := provStageIn(context.Background(), fakeHeadList{missing: "gone.fa"}, spec, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := provCost(context.Background(), fakeFacts(nil), spec, dir, "2026-10-17T12:00:00Z", "2026-10-17T12:30:00Z", "0.0"); err != nil {
		t.Fatal(err)
	}
	var rec provenance.Record
	b, _ := os.ReadFile(filepath.Join(dir, provRecordFile))
	_ = json.Unmarshal(b, &rec)
	if rec.Regenerable {
		t.Error("the spec opted in, but an input was unreadable: the record must not claim regenerable")
	}
}

type fakeIMDS map[string]string

func (f fakeIMDS) GetMetadata(_ context.Context, in *imds.GetMetadataInput, _ ...func(*imds.Options)) (*imds.GetMetadataOutput, error) {
	v, ok := f[in.Path]
	if !ok {
		return nil, errors.New("404")
	}
	return &imds.GetMetadataOutput{Content: io.NopCloser(strings.NewReader(v))}, nil
}

type fakeTags map[string]string

func (f fakeTags) DescribeTags(_ context.Context, _ *ec2.DescribeTagsInput, _ ...func(*ec2.Options)) (*ec2.DescribeTagsOutput, error) {
	var out []ec2types.TagDescription
	for k, v := range f {
		out = append(out, ec2types.TagDescription{Key: aws.String(k), Value: aws.String(v)})
	}
	return &ec2.DescribeTagsOutput{Tags: out}, nil
}

func fakeFacts(tags map[string]string) instanceFacts {
	return instanceFacts{
		imds:   fakeIMDS{"instance-id": "i-1", "placement/region": "us-east-1", "instance-type": "m7i.large", "ami-id": "ami-1"},
		ec2For: func(string) ec2TagsAPI { return fakeTags(tags) },
	}
}

func TestProvCostBuildsTheRecordAndPrintsUSD(t *testing.T) {
	dir := t.TempDir()
	spec := writeSpec(t, dir, taskproto.TaskSpec{TaskID: "align", Command: []string{"bwa", "mem"}, Container: "bwa:latest",
		Regenerable: true, Env: map[string]string{"B": "", "A": ""}})
	_ = os.WriteFile(filepath.Join(dir, provDigestFile), []byte("bwa@sha256:abc\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, provInputsFile), []byte(`{"complete":true,"inputs":[]}`), 0o600)
	t.Setenv("SPAWN_RUN_ID", "run-9")

	usd, err := provCost(context.Background(),
		fakeFacts(map[string]string{"spawn:price-per-hour": "0.100000", "spawn:price-source": "static", "spawn:version": "0.132.0"}),
		spec, dir, "2026-10-17T12:00:00Z", "2026-10-17T12:30:00Z", "0.132.0")
	if err != nil || usd != "0.050000" {
		t.Fatalf("usd = %q (%v), want 0.050000 (30 min at $0.10/h)", usd, err)
	}
	var rec provenance.Record
	b, _ := os.ReadFile(filepath.Join(dir, provRecordFile))
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.RunID != "run-9" || rec.TaskID != "align" || !rec.Regenerable || rec.Class != provenance.ClassCheap {
		t.Errorf("record identity/class = %+v", rec)
	}
	if rec.Cost.RateSource != provenance.RateSourceOnDemandStatic {
		t.Errorf("rate source = %q, want the static label carried through", rec.Cost.RateSource)
	}
	r := rec.Recipe
	if r.ImageDigest != "bwa@sha256:abc" || r.InstanceType != "m7i.large" || r.AMI != "ami-1" || r.SpawnVersion != "0.132.0" ||
		strings.Join(r.EnvNames, ",") != "A,B" {
		t.Errorf("recipe = %+v", r)
	}
}

func TestProvCostWithNoRateIsEmptyAndUnclassified(t *testing.T) {
	dir := t.TempDir()
	spec := writeSpec(t, dir, taskproto.TaskSpec{TaskID: "t", Command: []string{"x"}})
	usd, err := provCost(context.Background(), fakeFacts(map[string]string{}), spec, dir, "2026-10-17T12:00:00Z", "2026-10-17T13:00:00Z", "0")
	if err != nil || usd != "" {
		t.Fatalf("usd = %q (%v), want empty: an unknown cost must not reach completion.json as a number", usd, err)
	}
	var rec provenance.Record
	b, _ := os.ReadFile(filepath.Join(dir, provRecordFile))
	_ = json.Unmarshal(b, &rec)
	if rec.Class != "" {
		t.Errorf("class = %q with no rate", rec.Class)
	}
}

type recUploader struct{ tagging []string }

func (r *recUploader) UploadObject(_ context.Context, in *tm.UploadObjectInput, _ ...func(*tm.Options)) (*tm.UploadObjectOutput, error) {
	_, _ = io.Copy(io.Discard, in.Body)
	r.tagging = append(r.tagging, aws.ToString(in.Tagging))
	return &tm.UploadObjectOutput{ETag: aws.String(`"e"`)}, nil
}

type noAnnotate struct{}

func (noAnnotate) PutObjectAnnotation(context.Context, *s3.PutObjectAnnotationInput, ...func(*s3.Options)) (*s3.PutObjectAnnotationOutput, error) {
	return nil, errors.New("AccessDenied")
}

// With no record, the output is still uploaded, untagged: provenance must never
// cost a researcher their output. With one, it is tagged. An annotation failure
// is reported and does not fail the upload.
func TestProvUpload(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "o.txt")
	_ = os.WriteFile(f, []byte("x"), 0o600)

	up := &recUploader{}
	if err := provUpload(context.Background(), up, noAnnotate{}, dir, f, "s3://b/o.txt", provenance.UploadTuning{}, io.Discard); err != nil || len(up.tagging) != 1 || up.tagging[0] != "" {
		t.Fatalf("no record: err=%v tagging=%q, want an untagged upload", err, up.tagging)
	}

	rec := provenance.Record{RunID: "r", Cost: provenance.NewCost(t0(), t0().Add(1800e9), 0.1, provenance.RateSourceSpot, false)}
	b, _ := json.Marshal(rec)
	_ = os.WriteFile(filepath.Join(dir, provRecordFile), b, 0o600)
	up = &recUploader{}
	var errw bytes.Buffer
	if err := provUpload(context.Background(), up, noAnnotate{}, dir, f, "s3://b/o.txt", provenance.UploadTuning{}, &errw); err != nil {
		t.Fatalf("an annotation failure failed the upload: %v", err)
	}
	if len(up.tagging) != 1 || !strings.Contains(up.tagging[0], "spawn%3Aregen-class=cheap") {
		t.Errorf("tagging = %q, want the class from the record", up.tagging)
	}
	if !strings.Contains(errw.String(), "annotation failed") {
		t.Errorf("the annotation failure was not reported: %q", errw.String())
	}
}

func t0() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }

// #1231: a run whose container had the host Docker socket says so in its
// recipe, since the engine's own containers are not in it; one without does not.
func TestProvCostRecordsTheDockerSocket(t *testing.T) {
	for _, sock := range []bool{true, false} {
		dir := t.TempDir()
		spec := writeSpec(t, dir, taskproto.TaskSpec{TaskID: "wdl", Command: []string{"cromwell"}, Container: "cromwell:86",
			ContainerDockerSocket: sock})
		if _, err := provCost(context.Background(), fakeFacts(map[string]string{}), spec, dir,
			"2026-10-17T12:00:00Z", "2026-10-17T12:30:00Z", "0"); err != nil {
			t.Fatal(err)
		}
		var rec provenance.Record
		b, _ := os.ReadFile(filepath.Join(dir, provRecordFile))
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Recipe.DockerSocket != sock {
			t.Errorf("container_docker_socket=%v: recipe docker_socket = %v", sock, rec.Recipe.DockerSocket)
		}
	}
}

// #1324: the upload is sized from the box's instance type (through the
// wrapper's NIC-class table), vCPUs and MemTotal; whatever can't be read is
// unknown, not guessed.
func TestStageOutTuningReadsTheBox(t *testing.T) {
	dir := t.TempDir()
	mi := filepath.Join(dir, "meminfo")
	_ = os.WriteFile(mi, []byte("MemFree:  100 kB\nMemTotal:       16384000 kB\nSwapTotal: 0 kB\n"), 0o600)
	if got := memTotalKiB(mi); got != 16384000 {
		t.Fatalf("MemTotal = %d", got)
	}
	if got := memTotalKiB(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("missing meminfo: %d, want 0", got)
	}
	ctx := context.Background()
	got := stageOutTuning(ctx, instanceFacts{imds: fakeIMDS{"instance-type": "c7gn.16xlarge"}}, 64, mi)
	if want := provenance.TuneUpload(taskproto.NICTargetGbps("c7gn.16xlarge"), 64, 16384000); got != want || want == provenance.TuneUpload(0, 64, 16384000) {
		t.Errorf("c7gn.16xlarge: %+v, want %+v (its NIC class, not the unknown row)", got, want)
	}
	if got, want := stageOutTuning(ctx, instanceFacts{imds: fakeIMDS{}}, 4, filepath.Join(dir, "missing")), provenance.TuneUpload(0, 4, 0); got != want {
		t.Errorf("no IMDS, no meminfo: %+v, want %+v", got, want)
	}
}
