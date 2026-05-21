package pipeline

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// StageDataHandler manages S3 data input/output for pipeline stages
type StageDataHandler struct {
	s3Client     *s3.Client
	s3Uploader   *manager.Uploader
	s3Downloader *manager.Downloader
	bucket       string
	prefix       string
	pipelineID   string
	stageID      string
}

// NewStageDataHandler creates a new data handler using the default AWS credential chain.
func NewStageDataHandler(ctx context.Context, bucket, prefix, pipelineID, stageID string) (*StageDataHandler, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
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
		s3Client:     s3Client,
		s3Uploader:   manager.NewUploader(s3Client),
		s3Downloader: manager.NewDownloader(s3Client),
		bucket:       bucket,
		prefix:       prefix,
		pipelineID:   pipelineID,
		stageID:      stageID,
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

	// Download from each source stage
	for _, sourceStage := range sourceStages {
		if err := h.downloadFromStage(ctx, sourceStage, dataInput); err != nil {
			return fmt.Errorf("download from stage %s: %w", sourceStage, err)
		}
	}

	return nil
}

func (h *StageDataHandler) downloadFromStage(ctx context.Context, sourceStage string, dataInput *DataConfig) error {
	// Build S3 prefix for source stage output
	sourcePrefix := fmt.Sprintf("%s/stages/%s/output/", h.prefix, sourceStage)

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

			// Skip if pattern specified and doesn't match
			if dataInput.Pattern != "" {
				relPath := strings.TrimPrefix(key, sourcePrefix)
				matched, err := filepath.Match(dataInput.Pattern, relPath)
				if err != nil {
					log.Printf("Warning: invalid pattern '%s': %v", dataInput.Pattern, err)
				} else if !matched {
					continue
				}
			}

			// Determine local path
			relPath := strings.TrimPrefix(key, sourcePrefix)
			localPath := relPath
			if dataInput.DestPath != "" {
				localPath = filepath.Join(dataInput.DestPath, relPath)
			}

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
	_, err = h.s3Downloader.Download(ctx, file, &s3.GetObjectInput{
		Bucket: aws.String(h.bucket),
		Key:    aws.String(key),
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

	// Build S3 prefix for this stage's output
	outputPrefix := fmt.Sprintf("%s/stages/%s/output/", h.prefix, h.stageID)

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

			// Determine S3 key
			relPath := filepath.Base(localPath)
			if strings.HasPrefix(pattern, "/") {
				// Absolute path - preserve directory structure
				relPath = strings.TrimPrefix(localPath, "/")
			}
			s3Key := outputPrefix + relPath

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

func (h *StageDataHandler) uploadFile(ctx context.Context, localPath, s3Key string) error {
	// Open local file
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Upload to S3
	_, err = h.s3Uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(h.bucket),
		Key:    aws.String(s3Key),
		Body:   file,
	})
	if err != nil {
		return fmt.Errorf("s3 upload: %w", err)
	}

	return nil
}

// WriteCompletionMarker writes a completion marker to S3
func (h *StageDataHandler) WriteCompletionMarker(ctx context.Context) error {
	markerKey := fmt.Sprintf("%s/stages/%s/COMPLETE", h.prefix, h.stageID)

	log.Printf("Writing completion marker: s3://%s/%s", h.bucket, markerKey)

	// Write empty marker file with metadata
	metadata := map[string]string{
		"pipeline-id":  h.pipelineID,
		"stage-id":     h.stageID,
		"completed-at": fmt.Sprintf("%d", os.Getpid()),
	}

	_, err := h.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(h.bucket),
		Key:      aws.String(markerKey),
		Body:     strings.NewReader(""),
		Metadata: metadata,
	})
	if err != nil {
		return fmt.Errorf("write completion marker: %w", err)
	}

	log.Printf("Completion marker written successfully")
	return nil
}

// CheckCompletionMarker checks if a completion marker exists for a stage
func (h *StageDataHandler) CheckCompletionMarker(ctx context.Context, stageID string) (bool, error) {
	markerKey := fmt.Sprintf("%s/stages/%s/COMPLETE", h.prefix, stageID)

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
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(cfg)
	downloader := manager.NewDownloader(s3Client)

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

			_, err = downloader.Download(ctx, file, &s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
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
