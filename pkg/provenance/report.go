package provenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Phase 2 of #1076: a read-only retention report. It lists what a bucket
// holds, groups it by the run that wrote it, prices its storage, works out what
// the decided tiering rules would do to it, and runs the deletion sweeper's
// check without deleting anything: could the run be proven regenerable today?

// TagRetain is the researcher's keep switch. The writer sets "false" at upload
// and every lifecycle rule requires "false", because a lifecycle filter can't
// negate: "skip retain=true" can't be written, "require retain=false" can
// (design: What is recorded). Setting "true" takes an object out of every rule.
const TagRetain = "spawn:retain"

// Storage prices for the report, per GB-month (GB = 2^30 bytes, as S3 bills).
//
// STATIC, and labelled as such wherever a dollar figure is shown. truffle is
// the suite's pricing authority (#533) and prices EC2 only, so there is no live
// source for these yet. They are the first-tier us-east-1 list prices, read
// from the AWS Price List API (service AmazonS3, productFamily Storage) on
// StoragePricesAsOf. Other regions differ slightly.
var storagePricePerGBMonth = map[string]float64{
	string(s3types.StorageClassStandard):   0.023,
	string(s3types.StorageClassStandardIa): 0.0125,
	string(s3types.StorageClassGlacierIr):  0.004,
}

const (
	StoragePricesAsOf  = "2026-10-17"
	StoragePriceSource = "static us-east-1 first-tier S3 list prices, read from the AWS Price List API on " + StoragePricesAsOf +
		"; static because truffle, the suite's pricing authority, prices EC2 only; other regions differ slightly"
)

// minTransitionBytes: S3 lifecycle doesn't transition objects smaller than this
// by default, and Standard-IA and Glacier IR bill anything smaller as this size.
const minTransitionBytes = 128 * 1024

// Tiering ages, decided by the owner on 2026-10-17 (design, Decisions 3).
const (
	CheapToIA     = 30 * 24 * time.Hour
	CheapToGIR    = 90 * 24 * time.Hour
	ModerateToIA  = 60 * 24 * time.Hour
	gibibyteBytes = 1 << 30
)

var tierRank = map[string]int{
	string(s3types.StorageClassStandard):   0,
	string(s3types.StorageClassStandardIa): 1,
	string(s3types.StorageClassGlacierIr):  2,
}

// MonthlyStorageUSD is what bytes cost per month in storageClass, by the static
// table. ok is false for a class the table doesn't price.
func MonthlyStorageUSD(storageClass string, bytes int64) (usd float64, ok bool) {
	if storageClass == "" {
		storageClass = string(s3types.StorageClassStandard) // ListObjectsV2 omits it for some listings
	}
	p, ok := storagePricePerGBMonth[storageClass]
	if !ok {
		return 0, false
	}
	billable := bytes
	if storageClass != string(s3types.StorageClassStandard) && billable < minTransitionBytes {
		billable = minTransitionBytes
	}
	return p * float64(billable) / gibibyteBytes, true
}

// TierTarget is where the decided lifecycle rules would have put an object by
// now: its class, retain tag, age and size against Decisions 3. target is ""
// when no rule moves it, and why says which condition stopped it.
func TierTarget(class Class, retain string, age time.Duration, bytes int64, current string) (target, why string) {
	switch {
	case retain == "true":
		return "", "spawn:retain=true"
	case retain != "false":
		return "", "no spawn:retain=false tag, so no rule matches (written before v0.133.0)"
	case bytes < minTransitionBytes:
		return "", "under 128 KiB: S3 lifecycle doesn't transition it"
	}
	switch class {
	case ClassCheap:
		switch {
		case age >= CheapToGIR:
			target = string(s3types.StorageClassGlacierIr)
		case age >= CheapToIA:
			target = string(s3types.StorageClassStandardIa)
		default:
			return "", "cheap, younger than 30 days"
		}
	case ClassModerate:
		if age < ModerateToIA {
			return "", "moderate, younger than 60 days"
		}
		target = string(s3types.StorageClassStandardIa)
	case ClassExpensive:
		return "", "expensive: never tiered"
	default:
		return "", "unclassified (rate unknown): never tiered"
	}
	if current == "" {
		current = string(s3types.StorageClassStandard)
	}
	cr, known := tierRank[current]
	if !known {
		return "", "in " + current + ", which the rules don't manage"
	}
	if cr >= tierRank[target] {
		return "", "already in " + current
	}
	return target, ""
}

// ReportAPI is what building the report reads from the bucket being reported on.
type ReportAPI interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObjectTagging(ctx context.Context, in *s3.GetObjectTaggingInput, optFns ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error)
	GetObjectAnnotation(ctx context.Context, in *s3.GetObjectAnnotationInput, optFns ...func(*s3.Options)) (*s3.GetObjectAnnotationOutput, error)
}

// InputHeader re-reads a recorded input, as the deletion sweeper will.
type InputHeader interface {
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// ReportOptions configures BuildReport.
type ReportOptions struct {
	Bucket, Prefix string
	Now            time.Time
	// Concurrency bounds the per-object GetObjectTagging calls. 0 means 16.
	Concurrency int
	// InputHeaderFor returns a client that can read bucket, in that bucket's
	// region. Inputs live in buckets spawn doesn't own, often in other regions.
	InputHeaderFor func(ctx context.Context, bucket string) InputHeader
}

// ObjectSummary is one object in a run.
type ObjectSummary struct {
	Key          string    `json:"key"`
	Bytes        int64     `json:"bytes"`
	StorageClass string    `json:"storage_class"`
	LastModified time.Time `json:"last_modified"`
	TierTarget   string    `json:"tier_target,omitempty"`
	NotTieredWhy string    `json:"not_tiered_why,omitempty"`
}

// RunSummary is everything one run wrote.
type RunSummary struct {
	RunID       string  `json:"run_id"`
	TaskID      string  `json:"task_id,omitempty"`
	Class       Class   `json:"class,omitempty"`
	RegenUSD    string  `json:"regen_usd,omitempty"`
	RegenHours  string  `json:"regen_hours,omitempty"`
	RateSource  string  `json:"rate_source,omitempty"`
	LowerBound  bool    `json:"rate_is_lower_bound,omitempty"`
	Regenerable bool    `json:"regenerable"`
	Retain      string  `json:"retain,omitempty"`
	Objects     int     `json:"objects"`
	Bytes       int64   `json:"bytes"`
	MonthlyUSD  float64 `json:"monthly_storage_usd"`
	// TieredMonthlyUSD is the monthly cost once the rules have moved what they
	// would have moved by now. Equal to MonthlyUSD when nothing moves.
	TieredMonthlyUSD float64 `json:"tiered_monthly_storage_usd"`
	// Provable is the deletion sweeper's check, run now without deleting.
	Provable  bool            `json:"provable_regenerable"`
	WhyNot    []string        `json:"why_not_provable,omitempty"`
	Unpriced  int64           `json:"unpriced_bytes,omitempty"` // bytes in a storage class the static table doesn't price
	ObjectSet []ObjectSummary `json:"objects_detail"`
}

// Untracked is what a bucket holds that no run's provenance covers:
// completion.json and logs (written by plain `aws s3 cp`), outputs of runs
// before v0.132.1 or uploaded through the fallback, and anything else. It's
// never tiered or deleted.
type Untracked struct {
	Objects    int     `json:"objects"`
	Bytes      int64   `json:"bytes"`
	MonthlyUSD float64 `json:"monthly_storage_usd"`
	Unpriced   int64   `json:"unpriced_bytes,omitempty"`
}

// Report is the whole retention report.
type Report struct {
	Bucket      string       `json:"bucket"`
	Prefix      string       `json:"prefix,omitempty"`
	GeneratedAt time.Time    `json:"generated_at"`
	PriceSource string       `json:"storage_price_source"`
	Runs        []RunSummary `json:"runs"`
	Untracked   Untracked    `json:"untracked"`
	Totals      struct {
		Objects          int     `json:"objects"`
		Bytes            int64   `json:"bytes"`
		MonthlyUSD       float64 `json:"monthly_storage_usd"`
		TieredMonthlyUSD float64 `json:"tiered_monthly_storage_usd"`
		ProvableRuns     int     `json:"provable_runs"`
	} `json:"totals"`
	// Errors are objects whose tags couldn't be read. They're counted nowhere
	// else, so a report with errors is incomplete and says so.
	Errors []string `json:"errors,omitempty"`
}

type listed struct {
	obj  s3types.Object
	tags map[string]string
	err  error
}

// BuildReport reads the bucket and builds the report. It writes nothing.
func BuildReport(ctx context.Context, api ReportAPI, opt ReportOptions) (*Report, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	if opt.Concurrency <= 0 {
		opt.Concurrency = 16
	}
	var objs []s3types.Object
	in := &s3.ListObjectsV2Input{Bucket: aws.String(opt.Bucket)}
	if opt.Prefix != "" {
		in.Prefix = aws.String(opt.Prefix)
	}
	p := s3.NewListObjectsV2Paginator(api, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", opt.Bucket, opt.Prefix, err)
		}
		objs = append(objs, page.Contents...)
	}

	items := make([]listed, len(objs))
	sem := make(chan struct{}, opt.Concurrency)
	var wg sync.WaitGroup
	for i, o := range objs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, o s3types.Object) {
			defer func() { <-sem; wg.Done() }()
			items[i] = listed{obj: o}
			out, err := api.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(opt.Bucket), Key: o.Key})
			if err != nil {
				items[i].err = err
				return
			}
			t := map[string]string{}
			for _, tag := range out.TagSet {
				t[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
			}
			items[i].tags = t
		}(i, o)
	}
	wg.Wait()

	rep := &Report{Bucket: opt.Bucket, Prefix: opt.Prefix, GeneratedAt: opt.Now.UTC(), PriceSource: StoragePriceSource}
	runs := map[string]*RunSummary{}
	firstKey := map[string]string{}
	for _, it := range items {
		key := aws.ToString(it.obj.Key)
		bytes := aws.ToInt64(it.obj.Size)
		sc := string(it.obj.StorageClass)
		if sc == "" {
			sc = string(s3types.StorageClassStandard)
		}
		if it.err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("s3://%s/%s: read tags: %v", opt.Bucket, key, it.err))
			continue
		}
		usd, priced := MonthlyStorageUSD(sc, bytes)
		rep.Totals.Objects++
		rep.Totals.Bytes += bytes
		rep.Totals.MonthlyUSD += usd
		runID := it.tags[TagRunID]
		if runID == "" {
			rep.Untracked.Objects++
			rep.Untracked.Bytes += bytes
			rep.Untracked.MonthlyUSD += usd
			rep.Totals.TieredMonthlyUSD += usd
			if !priced {
				rep.Untracked.Unpriced += bytes
			}
			continue
		}
		r := runs[runID]
		if r == nil {
			r = &RunSummary{RunID: runID, Class: Class(it.tags[TagClass]), RegenUSD: it.tags[TagUSD],
				RegenHours: it.tags[TagHours], Regenerable: it.tags[TagRegenerable] == "true", Retain: it.tags[TagRetain]}
			runs[runID] = r
			firstKey[runID] = key
		}
		lm := aws.ToTime(it.obj.LastModified)
		target, why := TierTarget(r.Class, it.tags[TagRetain], opt.Now.Sub(lm), bytes, sc)
		tiered := usd
		if target != "" {
			tiered, _ = MonthlyStorageUSD(target, bytes)
		}
		r.Objects++
		r.Bytes += bytes
		r.MonthlyUSD += usd
		r.TieredMonthlyUSD += tiered
		rep.Totals.TieredMonthlyUSD += tiered
		if !priced {
			r.Unpriced += bytes
		}
		r.ObjectSet = append(r.ObjectSet, ObjectSummary{Key: key, Bytes: bytes, StorageClass: sc, LastModified: lm.UTC(), TierTarget: target, NotTieredWhy: why})
	}

	for id, r := range runs {
		rec, why := readRecord(ctx, api, opt.Bucket, firstKey[id])
		if rec != nil {
			r.TaskID = rec.TaskID
			r.RateSource, r.LowerBound = rec.Cost.RateSource, rec.Cost.LowerBound
		}
		r.Provable, r.WhyNot = prove(ctx, r, rec, why, opt.InputHeaderFor)
		if r.Provable {
			rep.Totals.ProvableRuns++
		}
		sort.Slice(r.ObjectSet, func(i, j int) bool { return r.ObjectSet[i].Key < r.ObjectSet[j].Key })
		rep.Runs = append(rep.Runs, *r)
	}
	sort.Slice(rep.Runs, func(i, j int) bool {
		if rep.Runs[i].Bytes != rep.Runs[j].Bytes {
			return rep.Runs[i].Bytes > rep.Runs[j].Bytes
		}
		return rep.Runs[i].RunID < rep.Runs[j].RunID
	})
	sort.Strings(rep.Errors)
	return rep, nil
}

// readRecord reads the run's provenance annotation from one of its objects.
// Every object of a run carries the same record (design: What is recorded).
func readRecord(ctx context.Context, api ReportAPI, bucket, key string) (*Record, string) {
	out, err := api.GetObjectAnnotation(ctx, &s3.GetObjectAnnotationInput{
		Bucket: aws.String(bucket), Key: aws.String(key), AnnotationName: aws.String(AnnotationName)})
	if err != nil {
		return nil, "provenance annotation unreadable: " + errorCode(err)
	}
	defer func() { _ = out.AnnotationPayload.Close() }()
	b, err := io.ReadAll(io.LimitReader(out.AnnotationPayload, 1<<20+1))
	if err != nil {
		return nil, "provenance annotation unreadable: " + err.Error()
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, "provenance annotation is not valid JSON"
	}
	if rec.Schema != SchemaVersion {
		return nil, "provenance annotation has schema " + strconv.Itoa(rec.Schema) + ", this spawn reads " + strconv.Itoa(SchemaVersion)
	}
	return &rec, ""
}

// prove is the deletion sweeper's check, run now and changing nothing. Every
// reason it can't prove regeneration is listed, not only the first, so the
// report says everything that would have to change.
func prove(ctx context.Context, r *RunSummary, rec *Record, recWhy string, headerFor func(context.Context, string) InputHeader) (bool, []string) {
	var why []string
	if !r.Regenerable {
		why = append(why, "not opted in: the task spec didn't set regenerable: true")
	}
	if rec == nil {
		return false, append(why, recWhy)
	}
	if rec.RunID != r.RunID {
		why = append(why, fmt.Sprintf("annotation is for run %s, the object's tag says %s", rec.RunID, r.RunID))
	}
	if !rec.Regenerable && r.Regenerable {
		why = append(why, "the tag says regenerable but the annotation doesn't")
	}
	if rec.Recipe.Image != "" && rec.Recipe.ImageDigest == "" {
		why = append(why, "image "+rec.Recipe.Image+" was not pinned: no digest was recorded, so a rerun may not use the same image")
	}
	if len(rec.Recipe.Command) == 0 {
		why = append(why, "no command recorded")
	}
	for _, in := range rec.Inputs {
		if w := checkInput(ctx, in, headerFor); w != "" {
			why = append(why, w)
		}
	}
	return len(why) == 0, why
}

// checkInput re-reads one recorded input. Anything but "still there, same
// ETag (and version)" is a reason: an input the caller can't read is unproven,
// never assumed unchanged.
func checkInput(ctx context.Context, in Input, headerFor func(context.Context, string) InputHeader) string {
	bucket, key, err := SplitS3URI(in.URI)
	if err != nil || key == "" {
		return "input " + in.URI + ": not an s3:// object URI"
	}
	if headerFor == nil {
		return "input " + in.URI + ": not checked"
	}
	hin := &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if in.VersionID != "" {
		hin.VersionId = aws.String(in.VersionID)
	}
	h, err := headerFor(ctx, bucket).HeadObject(ctx, hin)
	if err != nil {
		var nf *s3types.NotFound
		if errors.As(err, &nf) || errorCode(err) == "NotFound" || errorCode(err) == "NoSuchKey" {
			return "input " + in.URI + ": gone"
		}
		return "input " + in.URI + ": unreadable (" + errorCode(err) + ")"
	}
	if in.ETag != "" && aws.ToString(h.ETag) != in.ETag {
		return fmt.Sprintf("input %s: changed (ETag %s, was %s)", in.URI, aws.ToString(h.ETag), in.ETag)
	}
	if in.ETag == "" && in.VersionID == "" {
		return "input " + in.URI + ": no ETag or version was recorded, so a change can't be detected"
	}
	return ""
}

func errorCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return err.Error()
}
