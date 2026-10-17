package pipeline

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/s3transfer"
)

// StageDataHandler manages S3 data input/output for pipeline stages
type StageDataHandler struct {
	s3Client   *s3.Client
	s3Transfer *tm.Client
	bucket     string
	prefix     string
	pipelineID string
	stageID    string

	// instanceIndex is this instance's index within its stage; its markers
	// are per instance (StageInstanceMarkerKey, #930).
	instanceIndex int

	// pipelineDef is the pipeline definition, for every stage's
	// instance_count: a stage with more than one instance uploads each
	// instance's outputs under its own index (StageInstanceOutputPrefix,
	// #944). nil means every stage has one instance.
	pipelineDef *Pipeline
}

// instanceCount is stageID's instance_count, or 1 when it is not known.
func (h *StageDataHandler) instanceCount(stageID string) int {
	if h.pipelineDef != nil {
		if st := h.pipelineDef.GetStage(stageID); st != nil && st.InstanceCount > 1 {
			return st.InstanceCount
		}
	}
	return 1
}

// NewStageDataHandler creates a new data handler using the default AWS credential chain.
func NewStageDataHandler(ctx context.Context, bucket, prefix, pipelineID, stageID string) (*StageDataHandler, error) {
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return NewStageDataHandlerWithAWSConfig(ctx, bucket, prefix, pipelineID, stageID, cfg)
}

// NewStageDataHandlerWithAWSConfig creates a new data handler with an injected AWS config.
// Use this in tests to point the handler at a Substrate emulator.
func NewStageDataHandlerWithAWSConfig(_ context.Context, bucket, prefix, pipelineID, stageID string, awsCfg aws.Config) (*StageDataHandler, error) {
	s3Client := s3.NewFromConfig(awsCfg)
	return &StageDataHandler{
		s3Client:   s3Client,
		s3Transfer: s3transfer.New(s3Client),
		bucket:     bucket,
		prefix:     prefix,
		pipelineID: pipelineID,
		stageID:    stageID,
	}, nil
}

// DownloadStageInputs downloads inputs from upstream stage(s)
func (h *StageDataHandler) DownloadStageInputs(ctx context.Context, dataInput *DataConfig) error {
	if dataInput == nil || dataInput.Mode != "s3" {
		log.Println("No S3 input configuration, skipping download")
		return nil
	}

	// Determine source stages
	sourceStages := []string{}
	if dataInput.SourceStage != "" {
		sourceStages = append(sourceStages, dataInput.SourceStage)
	}
	sourceStages = append(sourceStages, dataInput.SourceStages...)

	if len(sourceStages) == 0 {
		return fmt.Errorf("no source stages specified for S3 input")
	}

	log.Printf("Downloading inputs from %d source stage(s)", len(sourceStages))

	// Every local path written, and the key it came from: two source stages
	// that both output a.csv would otherwise overwrite each other in dest_path,
	// the last download winning (#944).
	written := map[string]string{}

	// Download from each source stage
	for _, sourceStage := range sourceStages {
		if err := h.downloadFromStage(ctx, sourceStage, dataInput, written); err != nil {
			return fmt.Errorf("download from stage %s: %w", sourceStage, err)
		}
	}

	return nil
}

// InputLocalPath is where a data_input file lands on the instance: dest_path
// (or the working directory) joined with the file's path relative to its source
// stage's output root, which is relPath. That path is the file's path relative
// to the directory part of the data_output pattern that matched it
// (OutputRelPath), prefixed with "<instance-index>/" when the source stage has
// instance_count > 1. So a source's "/data/processed/*" output a.csv arrives as
// "<dest_path>/a.csv", and instance 2's as "<dest_path>/2/a.csv".
func InputLocalPath(destPath, relPath string) string {
	if destPath == "" {
		return filepath.FromSlash(relPath)
	}
	return filepath.Join(destPath, filepath.FromSlash(relPath))
}

// inputPatternSubject is the part of relPath a data_input pattern is matched
// against: the path within the instance's own directory for a multi-instance
// source, so "*.csv" selects every instance's CSVs.
func inputPatternSubject(relPath string, namespaced bool) string {
	if !namespaced {
		return relPath
	}
	if i := strings.Index(relPath, "/"); i >= 0 {
		return relPath[i+1:]
	}
	return relPath
}

func (h *StageDataHandler) downloadFromStage(ctx context.Context, sourceStage string, dataInput *DataConfig, written map[string]string) error {
	// Build S3 prefix for source stage output. A multi-instance source's
	// keys carry the instance index as their first path segment.
	sourcePrefix := StageOutputPrefix(h.prefix, sourceStage)
	namespaced := h.instanceCount(sourceStage) > 1

	log.Printf("Downloading from s3://%s/%s", h.bucket, sourcePrefix)

	// List objects in source stage output
	paginator := s3.NewListObjectsV2Paginator(h.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(h.bucket),
		Prefix: aws.String(sourcePrefix),
	})

	downloadCount := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}

		for _, obj := range page.Contents {
			key := *obj.Key

			relPath := strings.TrimPrefix(key, sourcePrefix)
			if !filepath.IsLocal(filepath.FromSlash(relPath)) {
				return fmt.Errorf("refusing s3://%s/%s: its path %q would land outside dest_path", h.bucket, key, relPath)
			}

			// Skip if pattern specified and doesn't match
			if dataInput.Pattern != "" {
				// path, not filepath: S3 keys are always /-separated. On Windows
				// filepath.Match treats only \ as a separator, so * crossed /
				// and a Windows spore pulled nested objects the pattern
				// excludes (#998).
				matched, err := path.Match(dataInput.Pattern, inputPatternSubject(relPath, namespaced))
				if err != nil {
					return fmt.Errorf("invalid pattern '%s': %w", dataInput.Pattern, err)
				} else if !matched {
					continue
				}
			}

			localPath := InputLocalPath(dataInput.DestPath, relPath)
			if prev, dup := written[localPath]; dup {
				return fmt.Errorf("both s3://%s/%s and s3://%s/%s would be written to %s; "+
					"give the source stages' outputs distinct names", h.bucket, prev, h.bucket, key, localPath)
			}
			written[localPath] = key

			// Create parent directories
			if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
				return fmt.Errorf("create directory: %w", err)
			}

			// Download file
			if err := h.downloadFile(ctx, key, localPath); err != nil {
				return fmt.Errorf("download %s: %w", key, err)
			}

			downloadCount++
			log.Printf("Downloaded: %s -> %s", key, localPath)
		}
	}

	log.Printf("Downloaded %d file(s) from stage %s", downloadCount, sourceStage)
	return nil
}

func (h *StageDataHandler) downloadFile(ctx context.Context, key, localPath string) error {
	// Create local file
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Download from S3
	_, err = h.s3Transfer.DownloadObject(ctx, &tm.DownloadObjectInput{
		Bucket:   aws.String(h.bucket),
		Key:      aws.String(key),
		WriterAt: file,
	})
	if err != nil {
		return fmt.Errorf("s3 download: %w", err)
	}

	return nil
}

// UploadStageOutputs uploads outputs to S3
func (h *StageDataHandler) UploadStageOutputs(ctx context.Context, dataOutput *DataConfig) error {
	if dataOutput == nil || dataOutput.Mode != "s3" {
		log.Println("No S3 output configuration, skipping upload")
		return nil
	}

	if len(dataOutput.Paths) == 0 {
		return fmt.Errorf("no output paths specified")
	}

	log.Printf("Uploading outputs for stage %s", h.stageID)

	// Build S3 prefix for this stage's output: this instance's own when the
	// stage has more than one (#944).
	outputPrefix := StageInstanceOutputPrefix(h.prefix, h.stageID, h.instanceCount(h.stageID), h.instanceIndex)

	// Every key this instance uploads, and the file it came from: two paths
	// patterns that map to one key would otherwise overwrite each other.
	uploaded := map[string]string{}

	uploadCount := 0
	for _, pattern := range dataOutput.Paths {
		// Expand glob pattern
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("glob pattern '%s': %w", pattern, err)
		}

		if len(matches) == 0 {
			log.Printf("Warning: no files matched pattern '%s'", pattern)
			continue
		}

		// Upload each matched file
		for _, localPath := range matches {
			// Check if file exists
			info, err := os.Stat(localPath)
			if err != nil {
				if os.IsNotExist(err) {
					log.Printf("Warning: file not found: %s", localPath)
					continue
				}
				return fmt.Errorf("stat %s: %w", localPath, err)
			}

			// Skip directories
			if info.IsDir() {
				log.Printf("Skipping directory: %s", localPath)
				continue
			}

			// The key is the file's path relative to the pattern's directory
			// part, absolute or relative alike. An absolute pattern used to
			// keep the whole path, so "/data/processed/a.csv" arrived
			// downstream as "<dest_path>/data/processed/a.csv" (#944).
			relPath, err := OutputRelPath(pattern, localPath)
			if err != nil {
				return err
			}
			s3Key := outputPrefix + relPath
			if prev, dup := uploaded[s3Key]; dup {
				return fmt.Errorf("data_output: %s and %s would both be uploaded as %s; "+
					"give them distinct names or put them under one pattern", prev, localPath, relPath)
			}
			uploaded[s3Key] = localPath

			// Upload file
			if err := h.uploadFile(ctx, localPath, s3Key); err != nil {
				return fmt.Errorf("upload %s: %w", localPath, err)
			}

			uploadCount++
			log.Printf("Uploaded: %s -> s3://%s/%s", localPath, h.bucket, s3Key)
		}
	}

	log.Printf("Uploaded %d file(s) for stage %s", uploadCount, h.stageID)
	return nil
}

// globRoot is the directory part of a data_output pattern: every path segment
// before the first one containing a glob metacharacter, or, for a pattern with
// none (a literal file), its directory.
func globRoot(pattern string) string {
	clean := filepath.Clean(pattern)
	segs := strings.Split(clean, string(filepath.Separator))
	for i, seg := range segs {
		if strings.ContainsAny(seg, `*?[\`) {
			root := strings.Join(segs[:i], string(filepath.Separator))
			switch {
			case root == "" && strings.HasPrefix(clean, string(filepath.Separator)):
				// A leading separator, not filepath.IsAbs: "/data/..." is an
				// instance path, and is not absolute on a Windows host (#998).
				return string(filepath.Separator)
			case root == "":
				return "."
			}
			return root
		}
	}
	return filepath.Dir(clean)
}

// OutputRelPath is the key, below the stage's output prefix, of a file that the
// data_output pattern matched: its slash-separated path relative to the
// pattern's directory part (globRoot). "/data/processed/*" uploads
// "/data/processed/a.csv" as "a.csv", "out/*/x.csv" uploads "out/b/x.csv" as
// "b/x.csv", and the literal "/logs/training.log" as "training.log".
func OutputRelPath(pattern, match string) (string, error) {
	rel, err := filepath.Rel(globRoot(pattern), filepath.Clean(match))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("data_output: %s is not under the directory of its pattern %q", match, pattern)
	}
	return filepath.ToSlash(rel), nil
}

func (h *StageDataHandler) uploadFile(ctx context.Context, localPath, s3Key string) error {
	// Open local file
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Upload to S3
	_, err = h.s3Transfer.UploadObject(ctx, &tm.UploadObjectInput{
		Bucket: aws.String(h.bucket),
		Key:    aws.String(s3Key),
		Body:   file,
	})
	if err != nil {
		return fmt.Errorf("s3 upload: %w", err)
	}

	return nil
}

// WriteCompletionMarker writes this instance's COMPLETE marker, at the key the
// orchestrator reads (StageInstanceMarkerKey). Written only after the outputs are
// uploaded, so a downstream stage never starts before its inputs exist.
func (h *StageDataHandler) WriteCompletionMarker(ctx context.Context) error {
	return h.writeMarker(ctx, MarkerComplete, "success")
}

// WriteFailureMarker writes this instance's FAILED marker, at the key the
// orchestrator reads. The orchestrator fails the stage on it, so the runner
// writes it itself rather than leaving the stage user data to guess.
func (h *StageDataHandler) WriteFailureMarker(ctx context.Context, reason string) error {
	return h.writeMarker(ctx, MarkerFailed, reason)
}

func (h *StageDataHandler) writeMarker(ctx context.Context, marker, body string) error {
	markerKey := StageInstanceMarkerKey(h.prefix, h.stageID, h.instanceIndex, marker)

	log.Printf("Writing %s marker: s3://%s/%s", marker, h.bucket, markerKey)

	metadata := map[string]string{
		"pipeline-id": h.pipelineID,
		"stage-id":    h.stageID,
		"instance":    fmt.Sprintf("%d", h.instanceIndex),
		"written-at":  time.Now().UTC().Format(time.RFC3339),
	}

	_, err := h.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(h.bucket),
		Key:      aws.String(markerKey),
		Body:     strings.NewReader(body),
		Metadata: metadata,
	})
	if err != nil {
		return fmt.Errorf("write %s marker: %w", marker, err)
	}

	log.Printf("%s marker written", marker)
	return nil
}

// CheckCompletionMarker checks whether the COMPLETE marker for stageID at this
// handler's instance index exists.
func (h *StageDataHandler) CheckCompletionMarker(ctx context.Context, stageID string) (bool, error) {
	markerKey := StageInstanceMarkerKey(h.prefix, stageID, h.instanceIndex, MarkerComplete)

	_, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.bucket),
		Key:    aws.String(markerKey),
	})
	if err != nil {
		// Check if error is "not found"
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("check marker: %w", err)
	}

	return true, nil
}

// DownloadResultsToLocal downloads all pipeline results to a local directory
func DownloadResultsToLocal(ctx context.Context, bucket, prefix, pipelineID, localDir string) error {
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(cfg)
	downloader := s3transfer.New(s3Client)

	// List all stage outputs
	resultsPrefix := fmt.Sprintf("%s/stages/", prefix)
	log.Printf("Downloading results from s3://%s/%s", bucket, resultsPrefix)

	paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(resultsPrefix),
	})

	downloadCount := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}

		for _, obj := range page.Contents {
			key := *obj.Key

			// Skip completion markers
			if strings.HasSuffix(key, "/COMPLETE") {
				continue
			}

			// Determine local path (preserve directory structure)
			relPath := strings.TrimPrefix(key, resultsPrefix)
			localPath := filepath.Join(localDir, relPath)

			// Create parent directories
			if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
				return fmt.Errorf("create directory: %w", err)
			}

			// Download file
			file, err := os.Create(localPath)
			if err != nil {
				return fmt.Errorf("create file %s: %w", localPath, err)
			}

			_, err = downloader.DownloadObject(ctx, &tm.DownloadObjectInput{
				Bucket:   aws.String(bucket),
				Key:      aws.String(key),
				WriterAt: file,
			})
			_ = file.Close()

			if err != nil {
				return fmt.Errorf("download %s: %w", key, err)
			}

			downloadCount++
			log.Printf("Downloaded: %s -> %s", key, localPath)
		}
	}

	log.Printf("Downloaded %d result file(s) to %s", downloadCount, localDir)
	return nil
}

// CopyFile copies a file from src to dst
func CopyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = sourceFile.Close() }()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = destFile.Close() }()

	_, err = io.Copy(destFile, sourceFile)
	return err
}
