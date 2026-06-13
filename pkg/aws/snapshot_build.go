package aws

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ebs"
	ebstypes "github.com/aws/aws-sdk-go-v2/service/ebs/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// EBS direct APIs write fixed-size blocks. 512 KiB is the only supported block
// size for StartSnapshot/PutSnapshotBlock.
const ebsBlockSize = 512 * 1024

// BuildSnapshotInput configures an instance-free EBS snapshot build from a raw
// disk/filesystem image (#147 Part A).
type BuildSnapshotInput struct {
	Region      string            // target region
	Description string            // snapshot description
	VolumeSize  int64             // volume size in GiB (the image must fit)
	Tags        map[string]string // tags to set on the snapshot
	// Encrypted requests an encrypted snapshot; KMSKeyARN optionally selects a
	// customer-managed key (else the account default EBS key).
	Encrypted bool
	KMSKeyARN string
}

// BuildSnapshotResult reports the finished snapshot.
type BuildSnapshotResult struct {
	SnapshotID string
	BlockSize  int32
	BlocksPut  int   // non-zero blocks actually uploaded
	BytesRead  int64 // total image bytes read
}

// isZeroBlock reports whether a block is entirely zero bytes. All-zero blocks
// are skipped (sparse upload) — a snapshot reads as zeros for any block never
// written, so skipping them is both correct and far cheaper for a sparse image.
func isZeroBlock(block []byte) bool {
	for _, b := range block {
		if b != 0 {
			return false
		}
	}
	return true
}

// snapshotBlock is one block staged for upload.
type snapshotBlock struct {
	index    int32
	data     []byte
	checksum string // base64 SHA256 of data
}

// planSnapshotBlocks reads an image stream and splits it into the non-zero
// 512 KiB blocks to upload, in index order. The final block is zero-padded to
// the full block size (EBS blocks are fixed-size). It returns the blocks, the
// total bytes read, and the linear-aggregation checksum CompleteSnapshot needs
// (the base64 SHA256 over the concatenation of every block's raw SHA256 digest,
// in ascending block-index order — including zero blocks? no: only the blocks
// actually written, in order). This is pure and fully unit-testable without AWS.
func planSnapshotBlocks(r io.Reader) (blocks []snapshotBlock, bytesRead int64, aggChecksum string, err error) {
	buf := make([]byte, ebsBlockSize)
	var index int32
	// The aggregation hash is taken over the raw (not base64) per-block digests
	// of the blocks we write, in block-index order.
	agg := sha256.New()
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			bytesRead += int64(n)
			// Zero-pad a short final block to the fixed block size.
			block := make([]byte, ebsBlockSize)
			copy(block, buf[:n])
			if !isZeroBlock(block) {
				rawSum := sha256.Sum256(block)
				agg.Write(rawSum[:])
				blocks = append(blocks, snapshotBlock{
					index:    index,
					data:     block,
					checksum: base64.StdEncoding.EncodeToString(rawSum[:]),
				})
			}
			index++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return nil, 0, "", fmt.Errorf("reading image: %w", readErr)
		}
	}
	aggChecksum = base64.StdEncoding.EncodeToString(agg.Sum(nil))
	return blocks, bytesRead, aggChecksum, nil
}

// BuildSnapshotFromReader populates a new EBS snapshot directly from a raw
// disk-image stream using the EBS direct APIs — no EC2 instance, no attached
// volume (#147 Part A). The reader must be a raw block image (e.g. a filesystem
// image), not a tarball: the bytes become the snapshot's block device verbatim.
//
// Flow: StartSnapshot → PutSnapshotBlock for each non-zero 512 KiB block →
// CompleteSnapshot with the changed-block count and linear aggregation
// checksum. Then wait (via EC2 DescribeSnapshots) until the snapshot reports
// completed, so the caller can immediately use it with --attach-volume.
func (c *Client) BuildSnapshotFromReader(ctx context.Context, in BuildSnapshotInput, image io.Reader) (*BuildSnapshotResult, error) {
	if in.VolumeSize <= 0 {
		return nil, fmt.Errorf("volume size must be > 0 GiB")
	}
	blocks, bytesRead, aggChecksum, err := planSnapshotBlocks(image)
	if err != nil {
		return nil, err
	}
	if int64(len(blocks))*ebsBlockSize > in.VolumeSize*1024*1024*1024 {
		return nil, fmt.Errorf("image (%d bytes) does not fit in a %d GiB volume", bytesRead, in.VolumeSize)
	}

	cfg, err := c.getRegionalConfig(ctx, in.Region)
	if err != nil {
		return nil, fmt.Errorf("regional config: %w", err)
	}
	ebsClient := ebs.NewFromConfig(cfg)

	start := &ebs.StartSnapshotInput{
		VolumeSize:  aws.Int64(in.VolumeSize),
		Description: strOrNil(in.Description),
		Tags:        ebsTags(in.Tags),
	}
	if in.Encrypted {
		start.Encrypted = aws.Bool(true)
		if in.KMSKeyARN != "" {
			start.KmsKeyArn = aws.String(in.KMSKeyARN)
		}
	}
	started, err := ebsClient.StartSnapshot(ctx, start)
	if err != nil {
		return nil, fmt.Errorf("StartSnapshot: %w", err)
	}
	snapshotID := aws.ToString(started.SnapshotId)

	for _, b := range blocks {
		_, err := ebsClient.PutSnapshotBlock(ctx, &ebs.PutSnapshotBlockInput{
			SnapshotId:        aws.String(snapshotID),
			BlockIndex:        aws.Int32(b.index),
			BlockData:         bytes.NewReader(b.data),
			DataLength:        aws.Int32(ebsBlockSize),
			Checksum:          aws.String(b.checksum),
			ChecksumAlgorithm: ebstypes.ChecksumAlgorithmChecksumAlgorithmSha256,
		})
		if err != nil {
			return nil, fmt.Errorf("PutSnapshotBlock index %d (snapshot %s left incomplete): %w", b.index, snapshotID, err)
		}
	}

	_, err = ebsClient.CompleteSnapshot(ctx, &ebs.CompleteSnapshotInput{
		SnapshotId:                aws.String(snapshotID),
		ChangedBlocksCount:        aws.Int32(int32(len(blocks))),
		Checksum:                  aws.String(aggChecksum),
		ChecksumAggregationMethod: ebstypes.ChecksumAggregationMethodChecksumAggregationLinear,
		ChecksumAlgorithm:         ebstypes.ChecksumAlgorithmChecksumAlgorithmSha256,
	})
	if err != nil {
		return nil, fmt.Errorf("CompleteSnapshot (snapshot %s): %w", snapshotID, err)
	}

	if err := c.waitForSnapshotCompleted(ctx, cfg, snapshotID, 30*time.Minute); err != nil {
		return nil, err
	}

	return &BuildSnapshotResult{
		SnapshotID: snapshotID,
		BlockSize:  ebsBlockSize,
		BlocksPut:  len(blocks),
		BytesRead:  bytesRead,
	}, nil
}

// waitForSnapshotCompleted polls EC2 DescribeSnapshots until the snapshot built
// via the direct APIs reports completed (or errors out). The EBS-direct
// snapshot only becomes attachable once EC2 marks it completed.
func (c *Client) waitForSnapshotCompleted(ctx context.Context, cfg aws.Config, snapshotID string, timeout time.Duration) error {
	ec2Client := ec2.NewFromConfig(cfg)
	waiter := ec2.NewSnapshotCompletedWaiter(ec2Client)
	err := waiter.Wait(ctx, &ec2.DescribeSnapshotsInput{
		SnapshotIds: []string{snapshotID},
	}, timeout)
	if err != nil {
		return fmt.Errorf("waiting for snapshot %s to complete: %w", snapshotID, err)
	}
	return nil
}

func ebsTags(tags map[string]string) []ebstypes.Tag {
	if len(tags) == 0 {
		return nil
	}
	out := make([]ebstypes.Tag, 0, len(tags))
	for k, v := range tags {
		out = append(out, ebstypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// OpenImageSource opens a raw-image source for the snapshot builder. The source
// is either a local file path or an s3://bucket/key URI; the returned
// ReadCloser streams the raw bytes (the caller must Close it). The image is read
// verbatim as the block device — it must be a raw disk/filesystem image, not an
// archive (#147 Part A).
func (c *Client) OpenImageSource(ctx context.Context, source, region string) (io.ReadCloser, error) {
	if strings.HasPrefix(source, "s3://") {
		bucket, key, err := parseS3URI(source)
		if err != nil {
			return nil, err
		}
		cfg, err := c.getRegionalConfig(ctx, region)
		if err != nil {
			return nil, fmt.Errorf("regional config: %w", err)
		}
		s3Client := s3.NewFromConfig(cfg)
		out, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return nil, fmt.Errorf("get s3 object %s: %w", source, err)
		}
		return out.Body, nil
	}
	f, err := os.Open(source) // #nosec G304 -- user-supplied image path is the intended input
	if err != nil {
		return nil, fmt.Errorf("open image %s: %w", source, err)
	}
	return f, nil
}

// parseS3URI splits an s3://bucket/key URI into its bucket and key.
func parseS3URI(uri string) (bucket, key string, err error) {
	rest := strings.TrimPrefix(uri, "s3://")
	i := strings.IndexByte(rest, '/')
	if i < 0 || i == 0 || i == len(rest)-1 {
		return "", "", fmt.Errorf("invalid s3 URI %q: expected s3://bucket/key", uri)
	}
	return rest[:i], rest[i+1:], nil
}
