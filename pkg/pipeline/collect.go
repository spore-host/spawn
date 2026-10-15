package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// CollectS3API is the S3 surface CollectRunOutputs uses.
type CollectS3API interface {
	s3.ListObjectsV2APIClient
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// CollectOptions selects what CollectRunOutputs downloads, and where to.
type CollectOptions struct {
	RunID     string // "" for the record's (latest) run
	StageID   string // "" for every stage
	OutputDir string
}

// collectRunIDPattern is the shape of a run id (NewRunID). --run is
// joined into an S3 prefix, so it may not carry a "/" or "..".
var collectRunIDPattern = regexp.MustCompile(`^[0-9a-z][0-9a-z-]*$`)

// collectFile is one stage output to download: the object key and its path
// below the output directory, slash-separated and already checked to be local.
type collectFile struct {
	Key   string
	Local string
}

// CollectRunOutputs is `spawn pipeline collect`: it downloads one run's stage
// outputs, the data_output files under <run>/stages/<stage>/output/, to
// <OutputDir>/<stage>/<path below output/>.
//
// It used to list result_s3_bucket, which nothing writes, so every pipeline
// that set it collected nothing; read one page of 1000 keys; download the
// COMPLETE/FAILED markers as results; and write each object to
// <OutputDir>/<key> unchecked, so a key containing ".." wrote outside the
// directory (#989).
func CollectRunOutputs(ctx context.Context, client CollectS3API, state *PipelineState, opts CollectOptions, out io.Writer) error {
	if state.S3Bucket == "" {
		return fmt.Errorf("pipeline %s has no s3_bucket, so it has no stage outputs to collect", state.PipelineID)
	}
	if state.ResultS3Bucket != "" || state.ResultS3Prefix != "" {
		_, _ = fmt.Fprintf(out, "⚠️  result_s3_bucket/result_s3_prefix are deprecated and ignored: nothing writes there. "+
			"Collecting the stage outputs from s3_bucket instead (#989).\n")
	}

	runPrefix := state.DataPrefix()
	runID := state.RunID
	if opts.RunID != "" && opts.RunID != state.RunID {
		if !collectRunIDPattern.MatchString(opts.RunID) {
			return fmt.Errorf("--run %q is not a run id (lower-case letters, digits and '-', as printed at launch)", opts.RunID)
		}
		runPrefix = RunKeyPrefix(state.S3Prefix, opts.RunID)
		runID = opts.RunID
	} else if !state.Status.IsTerminal() {
		_, _ = fmt.Fprintf(out, "⚠️  Run %q is still %s; its outputs may be incomplete.\n", state.RunID, state.Status)
	}
	if runID != "" {
		_, _ = fmt.Fprintf(out, "   Run: %s\n", runID)
	}

	listPrefix := StagesPrefix(runPrefix)
	if opts.StageID != "" {
		listPrefix = StageOutputPrefix(runPrefix, opts.StageID)
	}
	_, _ = fmt.Fprintf(out, "📍 Source: s3://%s/%s\n\n", state.S3Bucket, listPrefix)

	files, err := listCollectFiles(ctx, client, state.S3Bucket, runPrefix, listPrefix, opts.StageID)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		_, _ = fmt.Fprintf(out, "⚠️  No stage outputs found under s3://%s/%s\n", state.S3Bucket, listPrefix)
		if runs, err := ListRunIDs(ctx, client, state.S3Bucket, state.S3Prefix); err == nil && len(runs) > 0 {
			_, _ = fmt.Fprintf(out, "   Runs of this pipeline in s3://%s: %s (select one with --run)\n",
				state.S3Bucket, strings.Join(runs, ", "))
		}
		return nil
	}

	return downloadCollectFiles(ctx, client, state.S3Bucket, files, opts.OutputDir, out)
}

// listCollectFiles lists every stage output under listPrefix, all pages, and
// maps each to its local path, <stage>/<path below output/>. Markers and
// anything else that is not a data_output file are skipped. A key whose path
// would leave the output directory or its stage's directory in it, or two keys
// that map to one local path, fail the whole listing before anything is
// written.
func listCollectFiles(ctx context.Context, client CollectS3API, bucket, runPrefix, listPrefix, stageFilter string) ([]collectFile, error) {
	var files []collectFile
	var refused []string
	seen := map[string]string{}

	pager := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(listPrefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", bucket, listPrefix, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if strings.HasSuffix(key, "/") {
				continue // a "directory" placeholder
			}
			stageID, rel, ok := ParseStageOutputKey(runPrefix, key)
			if !ok || (stageFilter != "" && stageID != stageFilter) {
				continue // a COMPLETE/FAILED marker, or not a stage output
			}
			// Both parts are checked on their own, as data_input checks a
			// key against dest_path (#944): "s/output/../t/x" is inside the
			// output directory but would write into stage t's files.
			if !filepath.IsLocal(stageID) || !filepath.IsLocal(filepath.FromSlash(rel)) {
				refused = append(refused, key)
				continue
			}
			clean := path.Clean(stageID + "/" + rel)
			if prev, dup := seen[clean]; dup {
				return nil, fmt.Errorf("both s3://%s/%s and s3://%s/%s would be written to %s",
					bucket, prev, bucket, key, filepath.FromSlash(clean))
			}
			seen[clean] = key
			files = append(files, collectFile{Key: key, Local: clean})
		}
	}
	if len(refused) > 0 {
		return nil, fmt.Errorf("refusing to collect: %d object key(s) would be written outside the output directory "+
			"(or outside their stage's directory in it), "+
			"so nothing was downloaded: s3://%s/%s", len(refused), bucket, strings.Join(refused, ", s3://"+bucket+"/"))
	}
	return files, nil
}

// downloadCollectFiles writes files below outputDir through an os.Root, so a
// path cannot leave the directory even through a symlink already inside it.
// A failed download no longer reports success: every file is attempted, and
// the failures are returned.
func downloadCollectFiles(ctx context.Context, client CollectS3API, bucket string, files []collectFile, outputDir string, out io.Writer) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	root, err := os.OpenRoot(outputDir)
	if err != nil {
		return fmt.Errorf("open output directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	_, _ = fmt.Fprintf(out, "📥 Downloading %d files...\n", len(files))
	var failed []error
	for _, f := range files {
		if err := downloadCollectFile(ctx, client, bucket, f, root); err != nil {
			_, _ = fmt.Fprintf(out, "   ✗ %s: %v\n", f.Local, err)
			failed = append(failed, fmt.Errorf("s3://%s/%s: %w", bucket, f.Key, err))
			continue
		}
		_, _ = fmt.Fprintf(out, "   ✓ %s\n", f.Local)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d files failed to download: %w", len(failed), len(files), errors.Join(failed...))
	}
	_, _ = fmt.Fprintf(out, "\n✅ Downloaded %d files to %s\n", len(files), outputDir)
	return nil
}

func downloadCollectFile(ctx context.Context, client CollectS3API, bucket string, f collectFile, root *os.Root) error {
	local := filepath.FromSlash(f.Local)
	if err := root.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	obj, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(f.Key)})
	if err != nil {
		return err
	}
	defer func() { _ = obj.Body.Close() }()
	file, err := root.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, obj.Body); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// ListRunIDs lists the run ids that have data under s3_prefix, oldest first
// (run ids begin with their launch time), for the hint when the collected run
// has no outputs and for `spawn pipeline status` (#1014). It lists only the
// common prefixes under <s3_prefix>/runs/, one request per 1000 runs. The
// result is non-nil, so a JSON consumer can tell "no runs" from "not listed".
func ListRunIDs(ctx context.Context, client s3.ListObjectsV2APIClient, bucket, s3Prefix string) ([]string, error) {
	runsPrefix := strings.TrimSuffix(RunKeyPrefix(s3Prefix, "x"), "x")
	ids := []string{}
	pager := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(bucket),
		Prefix:    aws.String(runsPrefix),
		Delimiter: aws.String("/"),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cp := range page.CommonPrefixes {
			ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(aws.ToString(cp.Prefix), runsPrefix), "/"))
		}
	}
	sort.Strings(ids)
	return ids, nil
}
