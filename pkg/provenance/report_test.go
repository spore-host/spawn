package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const day = 24 * time.Hour

// The tiering ages are the owner's Decisions 3, read from the design rather
// than from the code: cheap to Standard-IA at 30 days and Glacier IR at 90,
// moderate to Standard-IA at 60, expensive and unclassified never, and nothing
// without spawn:retain=false.
func TestTierTargetFollowsTheDecidedRules(t *testing.T) {
	const big = 1 << 20
	for _, c := range []struct {
		name    string
		class   Class
		retain  string
		age     time.Duration
		bytes   int64
		current string
		want    string
	}{
		{"cheap 29d", ClassCheap, "false", 29 * day, big, "", ""},
		{"cheap 30d", ClassCheap, "false", 30 * day, big, "", "STANDARD_IA"},
		{"cheap 89d", ClassCheap, "false", 89 * day, big, "STANDARD", "STANDARD_IA"},
		{"cheap 90d", ClassCheap, "false", 90 * day, big, "", "GLACIER_IR"},
		{"cheap 90d already IA", ClassCheap, "false", 90 * day, big, "STANDARD_IA", "GLACIER_IR"},
		{"cheap 90d already GIR", ClassCheap, "false", 90 * day, big, "GLACIER_IR", ""},
		{"moderate 59d", ClassModerate, "false", 59 * day, big, "", ""},
		{"moderate 60d", ClassModerate, "false", 60 * day, big, "", "STANDARD_IA"},
		{"moderate 400d", ClassModerate, "false", 400 * day, big, "", "STANDARD_IA"},
		{"expensive", ClassExpensive, "false", 400 * day, big, "", ""},
		{"unclassified", "", "false", 400 * day, big, "", ""},
		{"retain=true", ClassCheap, "true", 400 * day, big, "", ""},
		{"no retain tag", ClassCheap, "", 400 * day, big, "", ""},
		{"under 128 KiB", ClassCheap, "false", 400 * day, 128*1024 - 1, "", ""},
		{"exactly 128 KiB", ClassCheap, "false", 400 * day, 128 * 1024, "", "GLACIER_IR"},
		{"unmanaged class", ClassCheap, "false", 400 * day, big, "DEEP_ARCHIVE", ""},
	} {
		got, why := TierTarget(c.class, c.retain, c.age, c.bytes, c.current)
		if got != c.want {
			t.Errorf("%s: target %q (why %q), want %q", c.name, got, why, c.want)
		}
		if got == "" && why == "" {
			t.Errorf("%s: no target and no reason; every untiered object must say why", c.name)
		}
	}
}

func TestMonthlyStorageUSD(t *testing.T) {
	if usd, ok := MonthlyStorageUSD("STANDARD", 1<<30); !ok || math.Abs(usd-0.023) > 1e-12 {
		t.Errorf("1 GiB Standard = %v %v, want $0.023", usd, ok)
	}
	if usd, _ := MonthlyStorageUSD("", 1<<30); math.Abs(usd-0.023) > 1e-12 {
		t.Errorf("an omitted storage class must price as Standard, got %v", usd)
	}
	// Standard-IA and Glacier IR bill anything under 128 KiB as 128 KiB.
	small, _ := MonthlyStorageUSD("STANDARD_IA", 1)
	if want := 0.0125 * 128 * 1024 / (1 << 30); math.Abs(small-want) > 1e-15 {
		t.Errorf("1 byte in IA = %v, want the 128 KiB minimum %v", small, want)
	}
	if _, ok := MonthlyStorageUSD("DEEP_ARCHIVE", 1); ok {
		t.Error("a class the static table doesn't price must report !ok, not a guess")
	}
	// The label must say static, why, and when (feedback: static pricing must be labelled).
	for _, want := range []string{"static", "truffle", StoragePricesAsOf, "Price List API"} {
		if !strings.Contains(StoragePriceSource, want) {
			t.Errorf("StoragePriceSource %q doesn't say %q", StoragePriceSource, want)
		}
	}
}

// --- a fake bucket ---

type fakeObj struct {
	key     string
	bytes   int64
	age     time.Duration
	class   string
	tags    map[string]string
	tagErr  error
	record  *Record
	annoErr error
}

type fakeBucket struct {
	now  time.Time
	objs []fakeObj
}

func (f *fakeBucket) find(key string) *fakeObj {
	for i := range f.objs {
		if f.objs[i].key == key {
			return &f.objs[i]
		}
	}
	return nil
}

func (f *fakeBucket) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{}
	for _, o := range f.objs {
		if !strings.HasPrefix(o.key, aws.ToString(in.Prefix)) {
			continue
		}
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(o.key), Size: aws.Int64(o.bytes),
			StorageClass: s3types.ObjectStorageClass(o.class), LastModified: aws.Time(f.now.Add(-o.age))})
	}
	return out, nil
}

func (f *fakeBucket) GetObjectTagging(_ context.Context, in *s3.GetObjectTaggingInput, _ ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error) {
	o := f.find(aws.ToString(in.Key))
	if o.tagErr != nil {
		return nil, o.tagErr
	}
	out := &s3.GetObjectTaggingOutput{}
	for k, v := range o.tags {
		out.TagSet = append(out.TagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

func (f *fakeBucket) GetObjectAnnotation(_ context.Context, in *s3.GetObjectAnnotationInput, _ ...func(*s3.Options)) (*s3.GetObjectAnnotationOutput, error) {
	o := f.find(aws.ToString(in.Key))
	if aws.ToString(in.AnnotationName) != AnnotationName {
		return nil, &smithy.GenericAPIError{Code: "NoSuchAnnotation"}
	}
	if o.annoErr != nil {
		return nil, o.annoErr
	}
	if o.record == nil {
		return nil, &smithy.GenericAPIError{Code: "NoSuchAnnotation"}
	}
	b, _ := json.Marshal(o.record)
	return &s3.GetObjectAnnotationOutput{AnnotationPayload: io.NopCloser(bytes.NewReader(b))}, nil
}

// fakeInputs answers HeadObject for recorded inputs: an ETag, or an error.
type fakeInputs struct {
	etag  map[string]string
	err   map[string]error
	asked []string
}

func (f *fakeInputs) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	uri := "s3://" + aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	f.asked = append(f.asked, uri)
	if err := f.err[uri]; err != nil {
		return nil, err
	}
	e, ok := f.etag[uri]
	if !ok {
		return nil, &s3types.NotFound{}
	}
	return &s3.HeadObjectOutput{ETag: aws.String(e)}, nil
}

func outTags(run string, class Class, regen bool) map[string]string {
	t := map[string]string{TagRunID: run, TagClass: string(class), TagRetain: "false", TagUSD: "0.0100", TagHours: "0.1000"}
	if regen {
		t[TagRegenerable] = "true"
	}
	return t
}

func rec(run string, regen bool, inputs ...Input) *Record {
	return &Record{Schema: SchemaVersion, TaskID: "t-" + run, RunID: run, Regenerable: regen,
		Recipe: Recipe{Command: []string{"true"}}, Inputs: inputs,
		Cost: Cost{RatePerHour: 0.1, RateSource: RateSourceOnDemandLive, USD: 0.01, ComputeSeconds: 360}}
}

func build(t *testing.T, b *fakeBucket, ins *fakeInputs) *Report {
	t.Helper()
	rep, err := BuildReport(context.Background(), b, ReportOptions{Bucket: "bkt", Now: b.now,
		InputHeaderFor: func(context.Context, string) InputHeader { return ins }})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func runByID(t *testing.T, rep *Report, id string) RunSummary {
	t.Helper()
	for _, r := range rep.Runs {
		if r.RunID == id {
			return r
		}
	}
	t.Fatalf("run %s not in report", id)
	return RunSummary{}
}

func whyHas(r RunSummary, s string) bool {
	for _, w := range r.WhyNot {
		if strings.Contains(w, s) {
			return true
		}
	}
	return false
}

func TestReportProvesOnlyWhatTheSweeperCouldVerifyNow(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	same := Input{URI: "s3://in/ref.fa", ETag: `"aaa"`}
	b := &fakeBucket{now: now, objs: []fakeObj{
		{key: "out/ok/a", bytes: 1 << 30, age: 100 * day, tags: outTags("ok", ClassCheap, true), record: rec("ok", true, same)},
		{key: "out/ok/b", bytes: 1 << 30, age: 100 * day, tags: outTags("ok", ClassCheap, true), record: rec("ok", true, same)},
		{key: "out/changed/a", bytes: 10, tags: outTags("changed", ClassCheap, true), record: rec("changed", true, Input{URI: "s3://in/moved.fa", ETag: `"old"`})},
		{key: "out/optout/a", bytes: 10, tags: outTags("optout", ClassCheap, false), record: rec("optout", false, Input{URI: "s3://in/gone.fa", ETag: `"g"`})},
		{key: "out/denied/a", bytes: 10, tags: outTags("denied", ClassModerate, true), record: rec("denied", true, Input{URI: "s3://private/x", ETag: `"x"`})},
		{key: "out/noanno/a", bytes: 10, tags: outTags("noanno", ClassCheap, true)},
		{key: "out/unpinned/a", bytes: 10, tags: outTags("unpinned", ClassCheap, true), record: func() *Record {
			r := rec("unpinned", true)
			r.Recipe.Image = "bwa:latest"
			return r
		}()},
		{key: "tasks/t/completion.json", bytes: 300, tags: map[string]string{}},
		{key: "out/broken", bytes: 999, tagErr: &smithy.GenericAPIError{Code: "AccessDenied"}},
	}}
	ins := &fakeInputs{
		etag: map[string]string{"s3://in/ref.fa": `"aaa"`, "s3://in/moved.fa": `"new"`},
		err:  map[string]error{"s3://private/x": &smithy.GenericAPIError{Code: "AccessDenied"}},
	}
	rep := build(t, b, ins)

	ok := runByID(t, rep, "ok")
	if !ok.Provable || len(ok.WhyNot) != 0 {
		t.Errorf("opted in, annotation readable, input unchanged: want provable, got %v %v", ok.Provable, ok.WhyNot)
	}
	if ok.Objects != 2 || ok.Bytes != 2<<30 || ok.TaskID != "t-ok" || ok.RateSource != RateSourceOnDemandLive {
		t.Errorf("ok run summary = %+v", ok)
	}
	// 2 GiB of cheap, 100 days old: Glacier IR by now.
	if math.Abs(ok.MonthlyUSD-0.046) > 1e-9 || math.Abs(ok.TieredMonthlyUSD-0.008) > 1e-9 {
		t.Errorf("ok run: monthly %v tiered %v, want 0.046 -> 0.008", ok.MonthlyUSD, ok.TieredMonthlyUSD)
	}
	if !whyHas(runByID(t, rep, "changed"), "changed") {
		t.Errorf("an input with a new ETag must block proof: %v", runByID(t, rep, "changed").WhyNot)
	}
	opt := runByID(t, rep, "optout")
	if opt.Provable || !whyHas(opt, "not opted in") || !whyHas(opt, "gone") {
		t.Errorf("not opted in AND input gone: both reasons must be listed, got %v", opt.WhyNot)
	}
	den := runByID(t, rep, "denied")
	if den.Provable || !whyHas(den, "unreadable (AccessDenied)") {
		t.Errorf("an input the caller can't read is unproven, never assumed unchanged: %v", den.WhyNot)
	}
	if na := runByID(t, rep, "noanno"); na.Provable || !whyHas(na, "annotation unreadable") {
		t.Errorf("no annotation: %v", na.WhyNot)
	}
	if up := runByID(t, rep, "unpinned"); up.Provable || !whyHas(up, "not pinned") {
		t.Errorf("an image with no recorded digest can't be proven: %v", up.WhyNot)
	}
	if rep.Totals.ProvableRuns != 1 {
		t.Errorf("provable runs = %d, want 1", rep.Totals.ProvableRuns)
	}
	// completion.json has no run-id: untracked, kept.
	if rep.Untracked.Objects != 1 || rep.Untracked.Bytes != 300 {
		t.Errorf("untracked = %+v, want completion.json only", rep.Untracked)
	}
	// An object whose tags couldn't be read is reported as an error and counted
	// nowhere else, so an incomplete report says it's incomplete.
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "out/broken") {
		t.Errorf("errors = %v", rep.Errors)
	}
	if rep.Totals.Objects != 8 {
		t.Errorf("total objects = %d, want 8 (the unreadable one excluded)", rep.Totals.Objects)
	}
	if !strings.Contains(rep.PriceSource, "static") {
		t.Errorf("price source unlabelled: %q", rep.PriceSource)
	}
}

// The sweeper's check must re-read the input at the recorded version, not
// whatever is current: a versioned input overwritten since keeps its old
// version, which is what the run read.
func TestReportChecksTheRecordedVersion(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	var gotVersion string
	b := &fakeBucket{now: now, objs: []fakeObj{
		{key: "o", bytes: 1, tags: outTags("v", ClassCheap, true), record: rec("v", true, Input{URI: "s3://in/x", ETag: `"e"`, VersionID: "v1"})},
	}}
	h := headerFunc(func(in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		gotVersion = aws.ToString(in.VersionId)
		return &s3.HeadObjectOutput{ETag: aws.String(`"e"`)}, nil
	})
	rep, err := BuildReport(context.Background(), b, ReportOptions{Bucket: "bkt", Now: now,
		InputHeaderFor: func(context.Context, string) InputHeader { return h }})
	if err != nil {
		t.Fatal(err)
	}
	if gotVersion != "v1" {
		t.Errorf("HeadObject VersionId = %q, want the recorded v1", gotVersion)
	}
	if !rep.Runs[0].Provable {
		t.Errorf("why not: %v", rep.Runs[0].WhyNot)
	}
}

type headerFunc func(*s3.HeadObjectInput) (*s3.HeadObjectOutput, error)

func (f headerFunc) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return f(in)
}

func TestReportFailsWhenTheListFails(t *testing.T) {
	_, err := BuildReport(context.Background(), &failingList{}, ReportOptions{Bucket: "bkt"})
	if err == nil || !strings.Contains(err.Error(), "list s3://bkt") {
		t.Fatalf("a failed listing must fail the report, not print an empty one: %v", err)
	}
}

type failingList struct{ fakeBucket }

func (*failingList) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return nil, errors.New("AccessDenied")
}
