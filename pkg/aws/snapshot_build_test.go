package aws

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"testing"
)

// scanSnapshotBlocks is the pure correctness core of the EBS-direct snapshot
// builder (#147): block splitting, sparse-zero skipping, final-block
// zero-padding, and the per-block + linear-aggregation checksums. The EBS
// direct APIs aren't modeled by substrate, so this is where correctness is
// pinned down without AWS. planSnapshotBlocks collects the streamed blocks into
// a slice so these tests can assert on them (the real builder uploads each block
// as it's scanned, never holding them all — #157).
func planSnapshotBlocks(r io.Reader) (blocks []snapshotBlock, bytesRead int64, aggChecksum string, err error) {
	bytesRead, aggChecksum, err = scanSnapshotBlocks(r, func(b snapshotBlock) error {
		// Copy the block: the scanner reuses its read buffer across blocks, and
		// the live uploader copies via bytes.Reader before the buffer is reused;
		// the test must do the same to keep each block's bytes stable.
		cp := make([]byte, len(b.data))
		copy(cp, b.data)
		b.data = cp
		blocks = append(blocks, b)
		return nil
	})
	return blocks, bytesRead, aggChecksum, err
}

func TestPlanSnapshotBlocks_SingleShortBlock(t *testing.T) {
	data := []byte("hello kraken2 database")
	blocks, bytesRead, agg, err := planSnapshotBlocks(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bytesRead != int64(len(data)) {
		t.Errorf("bytesRead = %d, want %d", bytesRead, len(data))
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].index != 0 {
		t.Errorf("block index = %d, want 0", blocks[0].index)
	}
	// The block is zero-padded to the fixed size.
	if len(blocks[0].data) != ebsBlockSize {
		t.Errorf("block size = %d, want %d (zero-padded)", len(blocks[0].data), ebsBlockSize)
	}
	// Checksum is base64 SHA256 of the padded block.
	want := sha256.Sum256(blocks[0].data)
	if blocks[0].checksum != base64.StdEncoding.EncodeToString(want[:]) {
		t.Error("per-block checksum mismatch")
	}
	if agg == "" {
		t.Error("aggregation checksum should be set")
	}
}

func TestPlanSnapshotBlocks_SkipsZeroBlocks(t *testing.T) {
	// Block 0: data, block 1: all-zero, block 2: data.
	img := make([]byte, ebsBlockSize*3)
	copy(img[0:], bytes.Repeat([]byte{0xAB}, 16))
	copy(img[ebsBlockSize*2:], bytes.Repeat([]byte{0xCD}, 16))

	blocks, bytesRead, _, err := planSnapshotBlocks(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bytesRead != int64(len(img)) {
		t.Errorf("bytesRead = %d, want %d", bytesRead, len(img))
	}
	// The all-zero middle block is skipped, but indices reflect position.
	if len(blocks) != 2 {
		t.Fatalf("expected 2 non-zero blocks, got %d", len(blocks))
	}
	if blocks[0].index != 0 {
		t.Errorf("first block index = %d, want 0", blocks[0].index)
	}
	if blocks[1].index != 2 {
		t.Errorf("second block index = %d, want 2 (zero block 1 skipped, index preserved)", blocks[1].index)
	}
}

func TestPlanSnapshotBlocks_AllZeroProducesNoBlocks(t *testing.T) {
	img := make([]byte, ebsBlockSize*2)
	blocks, bytesRead, _, err := planSnapshotBlocks(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) != 0 {
		t.Errorf("all-zero image should produce 0 blocks, got %d", len(blocks))
	}
	if bytesRead != int64(len(img)) {
		t.Errorf("bytesRead = %d, want %d", bytesRead, len(img))
	}
}

func TestPlanSnapshotBlocks_ExactMultipleOfBlockSize(t *testing.T) {
	img := bytes.Repeat([]byte{0x7}, ebsBlockSize*2)
	blocks, _, _, err := planSnapshotBlocks(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 full blocks, got %d", len(blocks))
	}
	for i, b := range blocks {
		if int(b.index) != i {
			t.Errorf("block %d has index %d", i, b.index)
		}
		if len(b.data) != ebsBlockSize {
			t.Errorf("block %d size = %d, want %d", i, len(b.data), ebsBlockSize)
		}
	}
}

func TestPlanSnapshotBlocks_AggregationIsDeterministic(t *testing.T) {
	img := append(bytes.Repeat([]byte{0x1}, ebsBlockSize), bytes.Repeat([]byte{0x2}, 100)...)
	_, _, agg1, _ := planSnapshotBlocks(bytes.NewReader(img))
	_, _, agg2, _ := planSnapshotBlocks(bytes.NewReader(img))
	if agg1 != agg2 {
		t.Error("aggregation checksum must be deterministic for identical input")
	}

	// Linear aggregation = base64 SHA256 over the concatenated raw per-block
	// digests, in index order. Recompute independently to pin the contract.
	blocks, _, agg, _ := planSnapshotBlocks(bytes.NewReader(img))
	h := sha256.New()
	for _, b := range blocks {
		raw := sha256.Sum256(b.data)
		h.Write(raw[:])
	}
	if agg != base64.StdEncoding.EncodeToString(h.Sum(nil)) {
		t.Error("aggregation checksum does not match the linear SHA256-of-digests contract")
	}
}

// TestScanSnapshotBlocks_EmitsInIndexOrderWithoutBuffering verifies the
// streaming contract the parallel uploader relies on (#157): scanSnapshotBlocks
// hands blocks to the callback in ascending index order, computing the
// aggregation as it goes — so uploads can complete out of order — and never
// retains them itself (peak memory is the caller's, not the whole image).
func TestScanSnapshotBlocks_EmitsInIndexOrderWithoutBuffering(t *testing.T) {
	// 5 non-zero blocks, interleaved with a zero block, plus a short tail.
	img := bytes.Join([][]byte{
		bytes.Repeat([]byte{0x1}, ebsBlockSize),
		make([]byte, ebsBlockSize), // zero block (skipped, index still advances)
		bytes.Repeat([]byte{0x2}, ebsBlockSize),
		bytes.Repeat([]byte{0x3}, ebsBlockSize),
		bytes.Repeat([]byte{0x4}, 200), // short tail
	}, nil)

	var indices []int32
	_, agg, err := scanSnapshotBlocks(bytes.NewReader(img), func(b snapshotBlock) error {
		indices = append(indices, b.index)
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := []int32{0, 2, 3, 4} // index 1 (zero) skipped
	if len(indices) != len(want) {
		t.Fatalf("emitted indices %v, want %v", indices, want)
	}
	for i := range want {
		if indices[i] != want[i] {
			t.Errorf("emit order %v, want ascending %v", indices, want)
			break
		}
	}
	if agg == "" {
		t.Error("aggregation checksum should be set")
	}
}

// TestScanSnapshotBlocks_EmitErrorStopsScan ensures an upload-worker failure
// (surfaced as an emit error) halts the scan promptly.
func TestScanSnapshotBlocks_EmitErrorStopsScan(t *testing.T) {
	img := bytes.Repeat([]byte{0x5}, ebsBlockSize*4)
	calls := 0
	_, _, err := scanSnapshotBlocks(bytes.NewReader(img), func(b snapshotBlock) error {
		calls++
		return io.ErrClosedPipe
	})
	if err == nil {
		t.Fatal("expected the emit error to propagate")
	}
	if calls != 1 {
		t.Errorf("scan should stop after the first emit error, got %d calls", calls)
	}
}

func TestIsZeroBlock(t *testing.T) {
	if !isZeroBlock(make([]byte, 1024)) {
		t.Error("all-zero block should be zero")
	}
	b := make([]byte, 1024)
	b[1023] = 1
	if isZeroBlock(b) {
		t.Error("block with a non-zero byte should not be zero")
	}
}

func TestParseS3URI(t *testing.T) {
	bucket, key, err := parseS3URI("s3://my-bucket/path/to/kraken2.raw")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bucket != "my-bucket" || key != "path/to/kraken2.raw" {
		t.Errorf("got bucket=%q key=%q", bucket, key)
	}

	for _, bad := range []string{"s3://only-bucket", "s3://", "s3:///key", "s3://bucket/"} {
		if _, _, err := parseS3URI(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestEBSTags(t *testing.T) {
	if ebsTags(nil) != nil {
		t.Error("nil tags should yield nil")
	}
	out := ebsTags(map[string]string{"spawn:managed": "true"})
	if len(out) != 1 || *out[0].Key != "spawn:managed" || *out[0].Value != "true" {
		t.Errorf("unexpected tags: %+v", out)
	}
}
