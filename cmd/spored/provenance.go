package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/buildinfo"
	"github.com/spore-host/spawn/pkg/provenance"
	"github.com/spore-host/spawn/pkg/s3transfer"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// `spored provenance` records what a task output would cost to regenerate, and
// what produced it (#1076, docs/recompute-tag-design.md). The task wrapper calls
// it at three points. Phase 1 is record only: nothing here tiers or deletes.
//
// stage-in and cost never fail the task. A provenance gap only means less is
// known about an output, and less known means kept. upload IS the output upload
// (it replaces `aws s3 cp` so tags are set at write time), so it fails exactly
// as that would.

// State files, under --state.
const (
	provInputsFile = "inputs.json"
	provDigestFile = "image-digest" // written by the wrapper after the pull
	provRecordFile = "record.json"
)

func newProvenanceCmd() *cobra.Command {
	var specPath, stateDir, started, ended string
	cmd := &cobra.Command{
		Use:    "provenance",
		Short:  "Record task output regeneration cost and provenance (called by the task wrapper)",
		Hidden: true,
	}
	stageIn := &cobra.Command{
		Use:   "stage-in",
		Short: "Record each input's ETag and version as it was read",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			ctx := cmd.Context()
			cfg, err := config.LoadDefaultConfig(ctx)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "provenance stage-in: %v\n", err)
				return
			}
			if err := provStageIn(ctx, s3.NewFromConfig(cfg), specPath, stateDir); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "provenance stage-in: %v\n", err)
			}
		},
	}
	costCmd := &cobra.Command{
		Use:   "cost",
		Short: "Build the provenance record and print the run's cost in USD (empty when unknown)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			ctx := cmd.Context()
			cfg, err := config.LoadDefaultConfig(ctx)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "provenance cost: %v\n", err)
				return
			}
			env := instanceFacts{imds: imds.NewFromConfig(cfg), ec2For: func(region string) ec2TagsAPI {
				c := cfg.Copy()
				c.Region = region
				return ec2.NewFromConfig(c)
			}}
			usd, err := provCost(ctx, env, specPath, stateDir, started, ended, buildinfo.Version(Version))
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "provenance cost: %v\n", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), usd)
		},
	}
	upload := &cobra.Command{
		Use:   "upload SOURCE DESTINATION",
		Short: "Upload one task output as `aws s3 cp` would, tagged and annotated",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.LoadDefaultConfig(ctx)
			if err != nil {
				return err
			}
			cli := s3.NewFromConfig(cfg)
			return provUpload(ctx, s3transfer.New(cli), cli, stateDir, args[0], args[1], cmd.ErrOrStderr())
		},
		SilenceUsage: true,
	}
	for _, c := range []*cobra.Command{stageIn, costCmd, upload} {
		c.Flags().StringVar(&stateDir, "state", "", "Provenance state directory")
		_ = c.MarkFlagRequired("state")
	}
	for _, c := range []*cobra.Command{stageIn, costCmd} {
		c.Flags().StringVar(&specPath, "spec", "", "Task spec JSON (env values stripped)")
		_ = c.MarkFlagRequired("spec")
	}
	costCmd.Flags().StringVar(&started, "started", "", "Run start, RFC3339")
	costCmd.Flags().StringVar(&ended, "ended", "", "Run end, RFC3339")
	cmd.AddCommand(stageIn, costCmd, upload)
	return cmd
}

func readSpec(path string) (*taskproto.TaskSpec, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the wrapper's own spec file
	if err != nil {
		return nil, err
	}
	var spec taskproto.TaskSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	return &spec, nil
}

// s3HeadListAPI is what recording inputs needs: read an object's ETag and
// version, or list a prefix. The task role already has s3:GetObject and
// s3:ListBucket on its input buckets.
type s3HeadListAPI interface {
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// inputsRecord is inputs.json. Complete is false when any input could not be
// read. A later deletion sweeper then cannot prove the inputs unchanged, so it
// must never delete those outputs.
type inputsRecord struct {
	Complete bool               `json:"complete"`
	Inputs   []provenance.Input `json:"inputs"`
}

// provStageIn records each s3:// input as the run read it: one object, or every
// object under a prefix source (ending in "/", as `aws s3 cp --recursive`).
func provStageIn(ctx context.Context, api s3HeadListAPI, specPath, stateDir string) error {
	spec, err := readSpec(specPath)
	if err != nil {
		return err
	}
	rec := inputsRecord{Complete: true}
	for _, m := range spec.Inputs {
		bucket, key, err := provenance.SplitS3URI(m.Source)
		if err != nil {
			rec.Complete = false
			continue
		}
		if strings.HasSuffix(m.Source, "/") {
			p := s3.NewListObjectsV2Paginator(api, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(key)})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					rec.Complete = false
					break
				}
				for _, o := range page.Contents {
					rec.Inputs = append(rec.Inputs, provenance.Input{
						URI: "s3://" + bucket + "/" + aws.ToString(o.Key), ETag: aws.ToString(o.ETag), Bytes: aws.ToInt64(o.Size)})
				}
			}
			continue
		}
		h, err := api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			rec.Complete = false
			continue
		}
		rec.Inputs = append(rec.Inputs, provenance.Input{URI: m.Source, ETag: aws.ToString(h.ETag),
			VersionID: aws.ToString(h.VersionId), Bytes: aws.ToInt64(h.ContentLength)})
	}
	return writeJSON(filepath.Join(stateDir, provInputsFile), rec)
}

// ec2TagsAPI reads the instance's own tags. The spored baseline policy grants
// ec2:DescribeTags. Instance metadata tags are not enabled, so IMDS can't serve them.
type ec2TagsAPI interface {
	DescribeTags(ctx context.Context, in *ec2.DescribeTagsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeTagsOutput, error)
}

type imdsAPI interface {
	GetMetadata(ctx context.Context, in *imds.GetMetadataInput, optFns ...func(*imds.Options)) (*imds.GetMetadataOutput, error)
}

// instanceFacts is what the cost step reads about the instance it runs on.
type instanceFacts struct {
	imds   imdsAPI
	ec2For func(region string) ec2TagsAPI
}

func (f instanceFacts) meta(ctx context.Context, path string) string {
	out, err := f.imds.GetMetadata(ctx, &imds.GetMetadataInput{Path: path})
	if err != nil {
		return ""
	}
	defer func() { _ = out.Content.Close() }()
	b, _ := io.ReadAll(out.Content)
	return strings.TrimSpace(string(b))
}

func (f instanceFacts) tags(ctx context.Context, region, instanceID string) (map[string]string, error) {
	out := map[string]string{}
	p := ec2.NewDescribeTagsPaginator(f.ec2For(region), &ec2.DescribeTagsInput{
		Filters: []ec2types.Filter{{Name: aws.String("resource-id"), Values: []string{instanceID}}}})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, t := range page.Tags {
			out[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	return out, nil
}

// provCost builds the record (recipe, inputs, cost, class), writes it for
// upload, and returns the USD figure for completion.json's cost_estimate:
// formatted, or "" when the rate is unknown, so the wrapper omits the field
// rather than writing a guess.
func provCost(ctx context.Context, f instanceFacts, specPath, stateDir, started, ended, sporedVersion string) (string, error) {
	spec, err := readSpec(specPath)
	if err != nil {
		return "", err
	}
	start, err1 := time.Parse(time.RFC3339, started)
	end, err2 := time.Parse(time.RFC3339, ended)
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("bad --started/--ended (%q, %q)", started, ended)
	}

	instanceID := f.meta(ctx, "instance-id")
	region := f.meta(ctx, "placement/region")
	tags := map[string]string{}
	if instanceID != "" && region != "" {
		tags, _ = f.tags(ctx, region, instanceID) // a tag-read failure leaves the rate unknown: kept, not cheap
	}
	rate, source, lower := provenance.RateFromTags(tags)

	var envNames []string
	for k := range spec.Env {
		envNames = append(envNames, k)
	}
	sort.Strings(envNames)
	digest, _ := os.ReadFile(filepath.Join(stateDir, provDigestFile)) //nolint:gosec // the wrapper's own state file
	var in inputsRecord
	if b, err := os.ReadFile(filepath.Join(stateDir, provInputsFile)); err == nil { //nolint:gosec // same
		_ = json.Unmarshal(b, &in)
	}

	rec := provenance.Record{
		Schema: provenance.SchemaVersion, TaskID: spec.TaskID, RunID: os.Getenv("SPAWN_RUN_ID"),
		Regenerable: spec.Regenerable && in.Complete,
		Recipe: provenance.Recipe{
			Command: spec.Command, EnvNames: envNames, Image: spec.Container,
			ImageDigest: strings.TrimSpace(string(digest)),
			AMI:         f.meta(ctx, "ami-id"), InstanceType: f.meta(ctx, "instance-type"),
			SpawnVersion: tags["spawn:version"], SporedVersion: sporedVersion,
		},
		Inputs: in.Inputs,
		Cost:   provenance.NewCost(start, end, rate, source, lower),
	}
	if c, ok := provenance.Classify(rec.Cost); ok {
		rec.Class = c
	}
	if err := writeJSON(filepath.Join(stateDir, provRecordFile), rec); err != nil {
		return "", err
	}
	if !rec.Cost.RateKnown() {
		return "", nil
	}
	return strconv.FormatFloat(rec.Cost.USD, 'f', 6, 64), nil
}

// provUpload uploads one output, tagged and annotated from record.json. With no
// record (the cost step failed or never ran) it still uploads, untagged, exactly
// as `aws s3 cp` did: provenance must never cost a researcher their output.
func provUpload(ctx context.Context, up provenance.Uploader, ann provenance.Annotator, stateDir, src, dst string, errw io.Writer) error {
	var tags map[string]string
	var payload []byte
	if b, err := os.ReadFile(filepath.Join(stateDir, provRecordFile)); err == nil { //nolint:gosec // the wrapper's own state file
		var rec provenance.Record
		if json.Unmarshal(b, &rec) == nil {
			tags, payload = rec.Tags(), b
		}
	}
	written, err := provenance.UploadOutput(ctx, up, ann, src, dst, tags, payload)
	for _, w := range written {
		if w.AnnotateErr != nil {
			fmt.Fprintf(errw, "provenance: s3://%s/%s uploaded and tagged; annotation failed: %v\n", w.Bucket, w.Key, w.AnnotateErr)
		}
	}
	return err
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
