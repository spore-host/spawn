package aws

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/imagebuilder"
	ibtypes "github.com/aws/aws-sdk-go-v2/service/imagebuilder/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// imageBuilderServiceName is the AWS service principal used to create the
// Image Builder service-linked execution role.
const imageBuilderServiceName = "imagebuilder.amazonaws.com"

// Default names for the self-provisioned import infrastructure.
const (
	importInstanceProfileRole = "spawn-imagebuilder-iso-import"
	importInfraConfigName     = "spawn-iso-import"
)

// importInstancePolicies are the managed policies the build instance needs:
// EC2InstanceProfileForImageBuilder lets the instance talk to Image Builder, and
// SSM core lets Image Builder drive it via Systems Manager.
var importInstancePolicies = []string{
	"arn:aws:iam::aws:policy/EC2InstanceProfileForImageBuilder",
	"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
}

const ec2TrustPolicy = `{
  "Version": "2012-10-17",
  "Statement": [{"Effect": "Allow", "Principal": {"Service": "ec2.amazonaws.com"}, "Action": "sts:AssumeRole"}]
}`

// EnsureImportInfrastructureInput configures self-provisioning of the Image
// Builder infrastructure that import-disk-image requires.
type EnsureImportInfrastructureInput struct {
	Region          string
	InstanceType    string   // default m6i.large
	SubnetID        string   // optional
	SecurityGroupID []string // optional
}

// ImportWindowsISOInput configures a Windows-ISO→AMI import via EC2 Image
// Builder's import-disk-image workflow. See:
// https://docs.aws.amazon.com/imagebuilder/latest/userguide/import-iso-disk.html
type ImportWindowsISOInput struct {
	Region                         string // launch region for the import build
	Name                           string // Image Builder image resource name
	SemanticVersion                string // major.minor.patch, e.g. "1.0.0"
	Description                    string // optional
	URI                            string // s3://bucket/key.ISO (uppercase .ISO)
	ExecutionRole                  string // IAM role/ARN; default AWSServiceRoleForImageBuilder
	InfrastructureConfigurationArn string // from the CFN stack output
	ImageIndex                     *int64 // optional: which edition in a multi-edition .wim
	SecureBoot                     *bool  // optional: nil = service default (enabled)
}

// EnsureImageBuilderSLR creates the Image Builder service-linked role
// (AWSServiceRoleForImageBuilder) if it does not already exist and returns its
// full ARN. import-disk-image uses this role as the execution role by default.
// Returning the ARN matters: the SLR's real ARN is under the
// aws-service-role/imagebuilder.amazonaws.com/ path, NOT a bare
// role/AWSServiceRoleForImageBuilder — passing the bare name makes the service
// resolve the wrong ARN and the build fails with "Unable to perform STS Assume
// role". Idempotent: an existing role is not an error.
func (c *Client) EnsureImageBuilderSLR(ctx context.Context) (string, error) {
	iamClient := iam.NewFromConfig(c.cfg)
	out, err := iamClient.CreateServiceLinkedRole(ctx, &iam.CreateServiceLinkedRoleInput{
		AWSServiceName: aws.String(imageBuilderServiceName),
	})
	if err == nil {
		// Wait briefly for the new role to propagate before it's used as an
		// execution role (IAM is eventually consistent).
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
		if out.Role != nil {
			return aws.ToString(out.Role.Arn), nil
		}
	} else {
		// "InvalidInput" with a "has been taken"/"already exists" body is returned
		// when the SLR already exists — treat that as success and look up its ARN.
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidInput" ||
			!(strings.Contains(strings.ToLower(apiErr.ErrorMessage()), "has been taken") ||
				strings.Contains(strings.ToLower(apiErr.ErrorMessage()), "already exists")) {
			return "", fmt.Errorf("create Image Builder service-linked role: %w", err)
		}
	}
	// Look up the existing role's ARN.
	got, gerr := iamClient.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String("AWSServiceRoleForImageBuilder"),
	})
	if gerr != nil {
		return "", fmt.Errorf("get Image Builder service-linked role ARN: %w", gerr)
	}
	return aws.ToString(got.Role.Arn), nil
}

// EnsureImportInfrastructure idempotently creates the IAM instance-profile role
// and the Image Builder infrastructure configuration that import-disk-image
// needs, returning the infrastructure configuration ARN. Safe to call repeatedly:
// existing resources are reused, not recreated. This is what lets
// `spawn image import` run without a separate CloudFormation deploy step.
func (c *Client) EnsureImportInfrastructure(ctx context.Context, in EnsureImportInfrastructureInput) (string, error) {
	cfg := c.cfg.Copy()
	if in.Region != "" {
		cfg.Region = in.Region
	}
	iamClient := iam.NewFromConfig(cfg)
	ibClient := imagebuilder.NewFromConfig(cfg)

	// 1. Instance-profile role (trust ec2.amazonaws.com).
	if err := ensureExists(iamClient.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(importInstanceProfileRole),
		AssumeRolePolicyDocument: aws.String(ec2TrustPolicy),
		Description:              aws.String("spawn: Image Builder ISO-import build instance role"),
	})); err != nil {
		return "", fmt.Errorf("create role %s: %w", importInstanceProfileRole, err)
	}
	for _, arn := range importInstancePolicies {
		if _, err := iamClient.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(importInstanceProfileRole),
			PolicyArn: aws.String(arn),
		}); err != nil {
			return "", fmt.Errorf("attach %s: %w", arn, err)
		}
	}

	// 2. Instance profile of the same name, with the role added.
	if err := ensureExists(iamClient.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(importInstanceProfileRole),
	})); err != nil {
		return "", fmt.Errorf("create instance profile %s: %w", importInstanceProfileRole, err)
	}
	if err := ensureExists(iamClient.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(importInstanceProfileRole),
		RoleName:            aws.String(importInstanceProfileRole),
	})); err != nil {
		// "LimitExceeded" means the role is already attached — treat as success.
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "LimitExceeded" {
			return "", fmt.Errorf("add role to instance profile: %w", err)
		}
	}

	// 3. If an infrastructure configuration with our name exists, reuse it.
	if arn, err := c.findInfraConfigByName(ctx, ibClient, importInfraConfigName); err != nil {
		return "", err
	} else if arn != "" {
		return arn, nil
	}

	// 4. Create the infrastructure configuration. IAM is eventually consistent,
	// so the just-created instance profile may not be visible to Image Builder
	// yet — retry CreateInfrastructureConfiguration briefly on the
	// "not found"/"cannot be assumed" class of errors.
	instanceType := in.InstanceType
	if instanceType == "" {
		instanceType = "m6i.large"
	}
	createInput := &imagebuilder.CreateInfrastructureConfigurationInput{
		Name:                       aws.String(importInfraConfigName),
		Description:                aws.String("spawn: Windows ISO -> AMI import (managed by spawn image import)"),
		InstanceProfileName:        aws.String(importInstanceProfileRole),
		InstanceTypes:              []string{instanceType},
		TerminateInstanceOnFailure: aws.Bool(true),
		ResourceTags:               map[string]string{"spawn:managed": "true"},
	}
	if in.SubnetID != "" {
		createInput.SubnetId = aws.String(in.SubnetID)
	}
	if len(in.SecurityGroupID) > 0 {
		createInput.SecurityGroupIds = in.SecurityGroupID
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		out, err := ibClient.CreateInfrastructureConfiguration(ctx, createInput)
		if err == nil {
			return aws.ToString(out.InfrastructureConfigurationArn), nil
		}
		// Lost the race with another caller — look it up.
		var exists *ibtypes.ResourceAlreadyExistsException
		if errors.As(err, &exists) {
			return c.findInfraConfigByName(ctx, ibClient, importInfraConfigName)
		}
		// IAM propagation delay: instance profile not yet visible.
		msg := strings.ToLower(err.Error())
		if time.Now().Before(deadline) &&
			(strings.Contains(msg, "instance profile") || strings.Contains(msg, "cannot be assumed") || strings.Contains(msg, "not found")) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(10 * time.Second):
				continue
			}
		}
		return "", fmt.Errorf("create infrastructure configuration: %w", err)
	}
}

// findInfraConfigByName returns the ARN of an infrastructure configuration with
// the given name, or "" if none exists.
func (c *Client) findInfraConfigByName(ctx context.Context, ibClient *imagebuilder.Client, name string) (string, error) {
	var token *string
	for {
		out, err := ibClient.ListInfrastructureConfigurations(ctx, &imagebuilder.ListInfrastructureConfigurationsInput{
			NextToken: token,
		})
		if err != nil {
			return "", fmt.Errorf("list infrastructure configurations: %w", err)
		}
		for _, s := range out.InfrastructureConfigurationSummaryList {
			if aws.ToString(s.Name) == name {
				return aws.ToString(s.Arn), nil
			}
		}
		if out.NextToken == nil {
			return "", nil
		}
		token = out.NextToken
	}
}

// ensureExists collapses an IAM "create" call's (output, error) into just error,
// treating EntityAlreadyExists as success. The first return value is ignored.
func ensureExists[T any](_ T, err error) error {
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "EntityAlreadyExists" {
		return nil
	}
	return err
}

// UploadISOToS3 streams a local ISO file to s3://bucket/key using a multipart
// upload (Windows ISOs are ~5-9 GB, well over S3's 5 GB single-PutObject limit).
// import-disk-image requires the object key to end in an uppercase ".ISO"
// extension, so callers should pass such a key; this method enforces it. The
// bucket must already exist (use CreateS3BucketIfNotExists first if needed).
func (c *Client) UploadISOToS3(ctx context.Context, region, bucket, key, localPath string) error {
	if !strings.HasSuffix(key, ".ISO") {
		return fmt.Errorf("s3 key %q must end in an uppercase .ISO extension (import-disk-image requirement)", key)
	}
	f, err := os.Open(localPath) //nolint:gosec // path is an explicit user-supplied ISO
	if err != nil {
		return fmt.Errorf("open ISO %q: %w", localPath, err)
	}
	defer f.Close() //nolint:errcheck

	localSize := int64(-1)
	if fi, statErr := f.Stat(); statErr == nil {
		localSize = fi.Size()
	}

	cfg := c.cfg.Copy()
	cfg.Region = region
	s3Client := s3.NewFromConfig(cfg)

	// Idempotency: if an object with the same key and byte size already exists,
	// skip the (multi-GB, multi-minute) upload. A size match on a content-keyed
	// ISO is a strong-enough signal; the import re-reads from S3 anyway.
	if localSize >= 0 {
		if head, herr := s3Client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}); herr == nil && aws.ToInt64(head.ContentLength) == localSize {
			return nil
		}
	}

	uploader := manager.NewUploader(s3Client, func(u *manager.Uploader) {
		u.PartSize = 64 * 1024 * 1024 // 64 MiB parts
		u.Concurrency = 4
	})
	if _, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   f,
	}); err != nil {
		return fmt.Errorf("upload ISO to s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

// DeleteISOFromS3 removes the staged ISO object. If alsoBucketIfEmpty is true
// and the bucket is now empty, it also deletes the bucket — used to clean up the
// transient managed staging bucket once the AMI is built. Errors deleting the
// bucket are returned, but a still-non-empty bucket is left intact (not an
// error: the user may have put other objects there).
func (c *Client) DeleteISOFromS3(ctx context.Context, region, bucket, key string, alsoBucketIfEmpty bool) error {
	cfg := c.cfg.Copy()
	cfg.Region = region
	s3Client := s3.NewFromConfig(cfg)

	if _, err := s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("delete s3://%s/%s: %w", bucket, key, err)
	}
	if !alsoBucketIfEmpty {
		return nil
	}

	// Only remove the bucket if it's now empty.
	list, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("list s3://%s: %w", bucket, err)
	}
	if aws.ToInt32(list.KeyCount) > 0 {
		return nil // not empty — leave it
	}
	if _, err := s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		return fmt.Errorf("delete bucket %s: %w", bucket, err)
	}
	return nil
}

// ImportWindowsISO starts an Image Builder import-disk-image workflow that
// converts a Windows 11 ISO (already in S3) into an AMI. It returns the image
// build version ARN to poll with WaitForImage. The output AMI has the AWS guest
// components (ENA/NVMe/PCISerial/EC2WinUtil drivers, EC2Launch v2, SSM agent,
// Defender) pre-staged by the workflow — no Packer/qemu/provisioning needed.
func (c *Client) ImportWindowsISO(ctx context.Context, in ImportWindowsISOInput) (string, error) {
	cfg := c.cfg.Copy()
	if in.Region != "" {
		cfg.Region = in.Region
	}
	ibClient := imagebuilder.NewFromConfig(cfg)

	execRole := in.ExecutionRole
	if execRole == "" {
		execRole = "AWSServiceRoleForImageBuilder"
	}

	input := &imagebuilder.ImportDiskImageInput{
		Name:                           aws.String(in.Name),
		SemanticVersion:                aws.String(in.SemanticVersion),
		Platform:                       aws.String("Windows"),
		OsVersion:                      aws.String("Microsoft Windows 11"),
		Uri:                            aws.String(in.URI),
		ExecutionRole:                  aws.String(execRole),
		InfrastructureConfigurationArn: aws.String(in.InfrastructureConfigurationArn),
	}
	if in.Description != "" {
		input.Description = aws.String(in.Description)
	}
	if in.ImageIndex != nil {
		input.WindowsConfiguration = &ibtypes.WindowsConfiguration{ImageIndex: in.ImageIndex}
	}
	if in.SecureBoot != nil {
		input.RegisterImageOptions = &ibtypes.RegisterImageOptions{SecureBootEnabled: in.SecureBoot}
	}

	out, err := ibClient.ImportDiskImage(ctx, input)
	if err != nil {
		return "", fmt.Errorf("import-disk-image: %w", err)
	}
	return aws.ToString(out.ImageBuildVersionArn), nil
}

// ErrWaitTimeout is returned by WaitForImage when the build is still in
// progress at the timeout. It's distinct from a build FAILURE so callers (and
// scripts, via a dedicated exit code) can tell "still building" from "failed".
var ErrWaitTimeout = errors.New("timed out waiting for image build")

// WaitForImage polls GetImage on an image build version ARN until the image
// reaches AVAILABLE (returning the output AMI id), FAILED (returning the failure
// reason), or the timeout elapses (returning ErrWaitTimeout, build still
// running). progressCb, if non-nil, is called with each observed status string.
func (c *Client) WaitForImage(ctx context.Context, region, imageBuildVersionArn string, timeout time.Duration, progressCb func(status string)) (string, error) {
	cfg := c.cfg.Copy()
	if region != "" {
		cfg.Region = region
	}
	ibClient := imagebuilder.NewFromConfig(cfg)

	deadline := time.Now().Add(timeout)
	var last string
	for {
		out, err := ibClient.GetImage(ctx, &imagebuilder.GetImageInput{
			ImageBuildVersionArn: aws.String(imageBuildVersionArn),
		})
		if err == nil && out.Image != nil && out.Image.State != nil {
			status := string(out.Image.State.Status)
			if progressCb != nil && status != last {
				progressCb(status)
				last = status
			}
			switch out.Image.State.Status {
			case ibtypes.ImageStatusAvailable:
				if out.Image.OutputResources != nil && len(out.Image.OutputResources.Amis) > 0 {
					return aws.ToString(out.Image.OutputResources.Amis[0].Image), nil
				}
				return "", fmt.Errorf("image %s is AVAILABLE but reported no output AMI", imageBuildVersionArn)
			case ibtypes.ImageStatusFailed, ibtypes.ImageStatusCancelled, ibtypes.ImageStatusDeprecated, ibtypes.ImageStatusDeleted:
				return "", fmt.Errorf("image build %s: %s — %s", imageBuildVersionArn,
					out.Image.State.Status, aws.ToString(out.Image.State.Reason))
			}
			// PENDING / BUILDING / TESTING / etc. → keep polling.
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: %s (last status %q)", ErrWaitTimeout, imageBuildVersionArn, last)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

// ImageStatus is a one-shot snapshot of an Image Builder image build.
type ImageStatus struct {
	Status string // PENDING/BUILDING/TESTING/AVAILABLE/FAILED/...
	Reason string // failure reason, if any
	AMI    string // output AMI id, once AVAILABLE
}

// GetImageStatus returns the current state of an image build version (one call,
// no polling) — backs `spawn image status`.
func (c *Client) GetImageStatus(ctx context.Context, region, imageBuildVersionArn string) (*ImageStatus, error) {
	cfg := c.cfg.Copy()
	if region != "" {
		cfg.Region = region
	}
	ibClient := imagebuilder.NewFromConfig(cfg)
	out, err := ibClient.GetImage(ctx, &imagebuilder.GetImageInput{
		ImageBuildVersionArn: aws.String(imageBuildVersionArn),
	})
	if err != nil {
		return nil, fmt.Errorf("get image %s: %w", imageBuildVersionArn, err)
	}
	st := &ImageStatus{}
	if out.Image != nil && out.Image.State != nil {
		st.Status = string(out.Image.State.Status)
		st.Reason = aws.ToString(out.Image.State.Reason)
	}
	if out.Image != nil && out.Image.OutputResources != nil && len(out.Image.OutputResources.Amis) > 0 {
		st.AMI = aws.ToString(out.Image.OutputResources.Amis[0].Image)
	}
	return st, nil
}

// TagAMIWindows tags an imported AMI with the spawn metadata so it's discoverable
// via `spawn ami list` and treated as Windows by connect/launch. The Image
// Builder output AMI already registers with Platform=windows (so IsWindowsAMI
// works), but the tags make detection explicit and let the AMI show up in
// listings filterable by source/arch. x64 because import-disk-image is x64-only.
func (c *Client) TagAMIWindows(ctx context.Context, region, amiID string) error {
	cfg := c.cfg.Copy()
	if region != "" {
		cfg.Region = region
	}
	ec2Client := ec2.NewFromConfig(cfg)
	_, err := ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{amiID},
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:os"), Value: aws.String("windows")},
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:source"), Value: aws.String("iso-import")},
			{Key: aws.String("spawn:arch"), Value: aws.String("x86_64")},
		},
	})
	if err != nil {
		return fmt.Errorf("tag AMI %s: %w", amiID, err)
	}
	return nil
}

// TagAMIWindowsWarm tags a "warm" AMI — one built from a seed launched off an
// imported base AMI, after first boot finished (#98). Same Windows/managed/arch
// tags as TagAMIWindows, but spawn:source=iso-import-warm and a
// spawn:warm-parent pointer to the base AMI for lineage.
func (c *Client) TagAMIWindowsWarm(ctx context.Context, region, amiID, parentAMI string) error {
	cfg := c.cfg.Copy()
	if region != "" {
		cfg.Region = region
	}
	ec2Client := ec2.NewFromConfig(cfg)
	_, err := ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{amiID},
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:os"), Value: aws.String("windows")},
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:source"), Value: aws.String("iso-import-warm")},
			{Key: aws.String("spawn:arch"), Value: aws.String("x86_64")},
			{Key: aws.String("spawn:warm-parent"), Value: aws.String(parentAMI)},
		},
	})
	if err != nil {
		return fmt.Errorf("tag warm AMI %s: %w", amiID, err)
	}
	return nil
}
