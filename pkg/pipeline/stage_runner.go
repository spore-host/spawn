package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// StageRunner executes a pipeline stage with data handoff
type StageRunner struct {
	pipelineID  string
	stageID     string
	stageIndex  int
	instanceID  string
	region      string
	dataHandler *StageDataHandler
	pipelineDef *Pipeline
	stageDef    *Stage
}

// StageInfo contains information about the current stage from EC2 tags
type StageInfo struct {
	PipelineID    string
	StageID       string
	StageIndex    int
	InstanceIndex int
	S3Bucket      string
	S3Prefix      string
	S3ConfigKey   string
}

// NewStageRunner creates a new stage runner
func NewStageRunner(ctx context.Context) (*StageRunner, error) {
	// Get instance metadata
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	imdsClient := imds.NewFromConfig(cfg)

	// Get instance identity
	idDoc, err := imdsClient.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return nil, fmt.Errorf("get instance identity: %w", err)
	}

	instanceID := idDoc.InstanceID
	region := idDoc.Region

	// Update config with region
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	// Get stage info from EC2 tags
	stageInfo, err := loadStageInfoFromTags(ctx, ec2Client, instanceID)
	if err != nil {
		return nil, fmt.Errorf("load stage info from tags: %w", err)
	}

	log.Printf("Running pipeline stage: pipeline=%s, stage=%s, index=%d",
		stageInfo.PipelineID, stageInfo.StageID, stageInfo.StageIndex)

	// The stage's S3 clients use the bucket's own region. The instance's region
	// is right for EC2 but not for a bucket created elsewhere: a pipeline whose
	// stages span regions shares one data bucket.
	dataCfg := bucketRegionConfig(ctx, cfg, stageInfo.S3Bucket)

	// Create data handler
	dataHandler, err := NewStageDataHandlerWithAWSConfig(ctx,
		stageInfo.S3Bucket,
		stageInfo.S3Prefix,
		stageInfo.PipelineID,
		stageInfo.StageID,
		dataCfg,
	)
	if err != nil {
		return nil, fmt.Errorf("create data handler: %w", err)
	}
	dataHandler.instanceIndex = stageInfo.InstanceIndex

	// Download pipeline definition from S3. From here on a failure is
	// reported as the stage's FAILED marker, since the handler can write it.
	pipelineDef, err := downloadPipelineDefinitionFromS3(ctx, cfg, stageInfo.S3ConfigKey)
	if err != nil {
		err = fmt.Errorf("download pipeline definition: %w", err)
		dataHandler.reportFailure(ctx, err)
		return nil, err
	}

	// Get stage definition
	stageDef := pipelineDef.GetStage(stageInfo.StageID)
	if stageDef == nil {
		err := fmt.Errorf("stage %s not found in pipeline definition", stageInfo.StageID)
		dataHandler.reportFailure(ctx, err)
		return nil, err
	}

	// Generate peer discovery file for network streaming. With the
	// region-bearing config: a bare default config has no region on an
	// instance (#906).
	log.Println("Generating peer discovery file...")
	if err := GeneratePeerDiscoveryFileWithAWSConfig(ctx, stageInfo.PipelineID, stageInfo.StageID,
		stageInfo.StageIndex, stageInfo.InstanceIndex, pipelineDef, cfg); err != nil {
		log.Printf("Warning: Failed to generate peer discovery file: %v", err)
		// Not a fatal error - stage can still run without peer file
	}

	return &StageRunner{
		pipelineID:  stageInfo.PipelineID,
		stageID:     stageInfo.StageID,
		stageIndex:  stageInfo.StageIndex,
		instanceID:  instanceID,
		region:      region,
		dataHandler: dataHandler,
		pipelineDef: pipelineDef,
		stageDef:    stageDef,
	}, nil
}

// Run executes the pipeline stage: download inputs, run the command, upload
// outputs, then write COMPLETE. Any failure writes the FAILED marker instead,
// which the orchestrator reads (StageMarkerKey).
func (r *StageRunner) Run(ctx context.Context) error {
	if err := r.run(ctx); err != nil {
		r.dataHandler.reportFailure(ctx, err)
		return err
	}
	return nil
}

func (r *StageRunner) run(ctx context.Context) error {
	log.Printf("Starting pipeline stage execution: %s", r.stageID)

	startTime := time.Now()

	// Step 1: Download stage inputs
	if r.stageDef.DataInput != nil {
		log.Println("Step 1: Downloading stage inputs...")
		if err := r.dataHandler.DownloadStageInputs(ctx, r.stageDef.DataInput); err != nil {
			return fmt.Errorf("download inputs: %w", err)
		}
		log.Printf("Input download completed in %v", time.Since(startTime))
	} else {
		log.Println("Step 1: No input configuration, skipping download")
	}

	// Step 2: Execute stage command
	log.Printf("Step 2: Executing stage command: %s", r.stageDef.Command)
	commandStartTime := time.Now()

	// Set environment variables from stage definition
	env := os.Environ()
	for key, value := range r.stageDef.Env {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	// Add pipeline-specific env vars
	env = append(env,
		fmt.Sprintf("SPAWN_PIPELINE_ID=%s", r.pipelineID),
		fmt.Sprintf("SPAWN_STAGE_ID=%s", r.stageID),
		fmt.Sprintf("SPAWN_STAGE_INDEX=%d", r.stageIndex),
	)

	// Execute command
	cmd := exec.CommandContext(ctx, "bash", "-c", r.stageDef.Command) // nosemgrep: dangerous-exec-command -- pipeline stage command defined by instance owner
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The working directory is the runner's own, which the stage user data
	// creates and enters (/opt/spawn/pipeline/<pipeline>/<stage>), as the
	// inline bash runner it replaces did. This was a fixed /root.

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command failed: %w", err)
	}

	log.Printf("Command completed successfully in %v", time.Since(commandStartTime))

	// Step 3: Upload stage outputs
	if r.stageDef.DataOutput != nil && r.stageDef.DataOutput.Mode == "s3" {
		log.Println("Step 3: Uploading stage outputs...")
		uploadStartTime := time.Now()

		if err := r.dataHandler.UploadStageOutputs(ctx, r.stageDef.DataOutput); err != nil {
			return fmt.Errorf("upload outputs: %w", err)
		}

		log.Printf("Output upload completed in %v", time.Since(uploadStartTime))
	} else {
		log.Println("Step 3: No S3 output configuration, skipping upload")
	}

	// Step 4: Write completion marker
	log.Println("Step 4: Writing completion marker...")
	if err := r.dataHandler.WriteCompletionMarker(ctx); err != nil {
		return fmt.Errorf("write completion marker: %w", err)
	}

	totalDuration := time.Since(startTime)
	log.Printf("Stage execution completed successfully in %v", totalDuration)

	return nil
}

func loadStageInfoFromTags(ctx context.Context, client *ec2.Client, instanceID string) (*StageInfo, error) {
	output, err := client.DescribeTags(ctx, &ec2.DescribeTagsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("resource-id"),
				Values: []string{instanceID},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe tags: %w", err)
	}

	info := &StageInfo{}
	hasRequiredTags := false

	for _, tag := range output.Tags {
		if tag.Key == nil || tag.Value == nil {
			continue
		}

		switch *tag.Key {
		case tagprefix.Tag("pipeline-id"):
			info.PipelineID = *tag.Value
			hasRequiredTags = true
		case tagprefix.Tag("stage-id"):
			info.StageID = *tag.Value
		case tagprefix.Tag("stage-index"):
			_, _ = fmt.Sscanf(*tag.Value, "%d", &info.StageIndex)
		case tagprefix.Tag("instance-index"):
			_, _ = fmt.Sscanf(*tag.Value, "%d", &info.InstanceIndex)
		case tagprefix.Tag("s3-bucket"):
			info.S3Bucket = *tag.Value
		case tagprefix.Tag("s3-prefix"):
			info.S3Prefix = *tag.Value
		case tagprefix.Tag("s3-config-key"):
			info.S3ConfigKey = *tag.Value
		}
	}

	if !hasRequiredTags {
		return nil, fmt.Errorf("instance is not part of a pipeline (missing %s tag)", tagprefix.Tag("pipeline-id"))
	}

	if info.StageID == "" {
		return nil, fmt.Errorf("missing %s tag", tagprefix.Tag("stage-id"))
	}

	if info.S3ConfigKey == "" {
		return nil, fmt.Errorf("missing %s tag", tagprefix.Tag("s3-config-key"))
	}

	return info, nil
}

func downloadPipelineDefinitionFromS3(ctx context.Context, cfg aws.Config, s3Key string) (*Pipeline, error) {
	// Parse S3 key (format: s3://bucket/key)
	parts := splitS3URL(s3Key)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid S3 key format: %s", s3Key)
	}
	bucket, key := parts[0], parts[1]

	// Download from S3, in the bucket's region.
	s3Client := s3.NewFromConfig(bucketRegionConfig(ctx, cfg, bucket))

	result, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get object: %w", err)
	}
	defer func() { _ = result.Body.Close() }()

	// Parse pipeline definition
	var p Pipeline
	if err := json.NewDecoder(result.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode pipeline: %w", err)
	}

	return &p, nil
}

func splitS3URL(s3URL string) []string {
	// Remove s3:// prefix
	path := s3URL
	if len(path) > 5 && path[:5] == "s3://" {
		path = path[5:]
	}

	// Split into bucket and key
	slashIdx := -1
	for i, ch := range path {
		if ch == '/' {
			slashIdx = i
			break
		}
	}

	if slashIdx == -1 {
		return []string{path}
	}

	bucket := path[:slashIdx]
	key := path[slashIdx+1:]

	return []string{bucket, key}
}

// IsPipelineInstance checks if the current instance is part of a pipeline
func IsPipelineInstance(ctx context.Context) (bool, error) {
	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return false, fmt.Errorf("load AWS config: %w", err)
	}

	imdsClient := imds.NewFromConfig(cfg)

	// Get instance identity
	idDoc, err := imdsClient.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return false, fmt.Errorf("get instance identity: %w", err)
	}

	cfg.Region = idDoc.Region
	ec2Client := ec2.NewFromConfig(cfg)

	// Check for pipeline tags
	output, err := ec2Client.DescribeTags(ctx, &ec2.DescribeTagsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("resource-id"),
				Values: []string{idDoc.InstanceID},
			},
			{
				Name:   aws.String("key"),
				Values: []string{tagprefix.Tag("pipeline-id")},
			},
		},
	})
	if err != nil {
		return false, fmt.Errorf("describe tags: %w", err)
	}

	return len(output.Tags) > 0, nil
}

// bucketRegionConfig returns cfg in bucket's region. getBucketRegion is an
// unsigned HeadBucket, so it needs no IAM grant; if it cannot answer, cfg is
// returned unchanged and the call that follows reports the real error.
func bucketRegionConfig(ctx context.Context, cfg aws.Config, bucket string) aws.Config {
	if bucket == "" {
		return cfg
	}
	region := getBucketRegion(ctx, s3.NewFromConfig(cfg), bucket)
	if region == "" || region == cfg.Region {
		return cfg
	}
	out := cfg.Copy()
	out.Region = region
	return out
}

// reportFailure writes the FAILED marker for err, best effort: the stage user
// data writes one too if spored exits non-zero, so a marker that cannot be
// written here is logged, not fatal.
func (h *StageDataHandler) reportFailure(ctx context.Context, cause error) {
	if err := h.WriteFailureMarker(ctx, cause.Error()); err != nil {
		log.Printf("Warning: could not write FAILED marker: %v", err)
	}
}

// getBucketRegion is an unsigned HeadBucket that returns the bucket's
// x-amz-bucket-region header, or "" if the response carried none. It stands in
// for the deprecated feature/s3/manager.GetBucketRegion, which pkg/s3transfer
// replaced (#932). The header is read from error responses too: S3 sends it on
// the 301 for a bucket in another region and on a 403.
func getBucketRegion(ctx context.Context, client *s3.Client, bucket string) string {
	var region string
	capture := middleware.DeserializeMiddlewareFunc("CaptureBucketRegion",
		func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
			out, md, err := next.HandleDeserialize(ctx, in)
			if resp, ok := out.RawResponse.(*smithyhttp.Response); ok && resp != nil {
				region = resp.Header.Get("X-Amz-Bucket-Region")
			}
			return out, md, err
		})
	// The error is deliberately ignored: a 301 or 403 still carries the header,
	// and the capture sits innermost in the deserialize step, beneath the
	// middleware that turns those statuses into errors.
	_, _ = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}, func(o *s3.Options) {
		o.Credentials = nil
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Deserialize.Add(capture, middleware.After)
		})
	})
	return region
}
