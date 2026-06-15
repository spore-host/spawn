package aws

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifySnapshotSource(t *testing.T) {
	cases := []struct {
		path  string
		isDir bool
		want  SnapshotSourceKind
	}{
		{"/data/refs", true, SourceDir},
		{"k2_pluspf.tar", false, SourceTar},
		{"k2_pluspf.tar.gz", false, SourceTar},
		{"k2_pluspf.tgz", false, SourceTar},
		{"K2_PLUSPF.TAR.GZ", false, SourceTar}, // case-insensitive
		{"kraken2.raw", false, SourceRawImage},
		{"s3://bucket/img.ext4", false, SourceRawImage},
		{"s3://bucket/db.tar.gz", false, SourceTar},
	}
	for _, c := range cases {
		if got := classifySnapshotSource(c.path, c.isDir); got != c.want {
			t.Errorf("classifySnapshotSource(%q, dir=%v) = %d, want %d", c.path, c.isDir, got, c.want)
		}
	}
}

func TestIsGzipName(t *testing.T) {
	for _, y := range []string{"a.tar.gz", "a.tgz", "A.TGZ", "x.gz"} {
		if !isGzipName(y) {
			t.Errorf("%q should be gzip", y)
		}
	}
	for _, n := range []string{"a.tar", "a.raw", "a.ext4"} {
		if isGzipName(n) {
			t.Errorf("%q should not be gzip", n)
		}
	}
}

func TestTarDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "db", "hash.k2d"), []byte("ref"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := tarDirectory(dir, &buf); err != nil {
		t.Fatalf("tarDirectory: %v", err)
	}

	// The archive should contain the nested file with a relative, slash path.
	tr := tar.NewReader(&buf)
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "db/hash.k2d" {
			found = true
			b, _ := io.ReadAll(tr)
			if string(b) != "ref" {
				t.Errorf("file content = %q, want ref", b)
			}
		}
	}
	if !found {
		t.Error("expected db/hash.k2d in the tar stream")
	}
}

// ext4SuperblockMagic verifies the image at off 0x438 carries the ext4 magic
// 0xEF53 (little-endian 53 EF), proving a real ext4 filesystem was built.
func hasExt4Magic(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 2)
	if _, err := f.ReadAt(buf, 0x438); err != nil {
		t.Fatalf("read superblock magic: %v", err)
	}
	return buf[0] == 0x53 && buf[1] == 0xEF
}

func TestPrependLostFound(t *testing.T) {
	// Original stream has one regular file.
	var srcBuf bytes.Buffer
	tw := tar.NewWriter(&srcBuf)
	body := []byte("hello")
	_ = tw.WriteHeader(&tar.Header{Name: "ref/data.bin", Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()

	combined := prependLostFound(bytes.NewReader(srcBuf.Bytes()))

	tr := tar.NewReader(combined)

	// First entry must be the lost+found directory at 0755.
	h, err := tr.Next()
	if err != nil {
		t.Fatalf("read first entry: %v", err)
	}
	if got := strings.TrimSuffix(h.Name, "/"); got != "lost+found" {
		t.Errorf("first entry = %q, want lost+found", h.Name)
	}
	if h.Typeflag != tar.TypeDir {
		t.Errorf("lost+found typeflag = %d, want dir (%d)", h.Typeflag, tar.TypeDir)
	}
	if h.FileInfo().Mode().Perm() != 0o755 {
		t.Errorf("lost+found mode = %o, want 0755", h.FileInfo().Mode().Perm())
	}

	// Original entry must follow intact.
	h, err = tr.Next()
	if err != nil {
		t.Fatalf("read second entry: %v", err)
	}
	if h.Name != "ref/data.bin" {
		t.Errorf("second entry = %q, want ref/data.bin", h.Name)
	}
	got, _ := io.ReadAll(tr)
	if string(got) != "hello" {
		t.Errorf("second entry body = %q, want hello", got)
	}

	// Stream ends cleanly (no truncation/garbage).
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("expected EOF after entries, got %v", err)
	}
}

func TestPrepareSnapshotImage_DirectoryBuildsExt4(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "db", "hash.k2d"), bytes.Repeat([]byte{0x5}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Client{}
	prepared, err := c.PrepareSnapshotImage(context.Background(), dir, "us-east-1", 64*1024*1024, "")
	if err != nil {
		t.Fatalf("PrepareSnapshotImage(dir): %v", err)
	}
	defer prepared.Cleanup()

	// Drain to a temp file and check the ext4 magic.
	out := filepath.Join(t.TempDir(), "img.ext4")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, prepared.Reader); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if !hasExt4Magic(t, out) {
		t.Error("directory source did not produce a valid ext4 image (missing superblock magic)")
	}
}

func TestPrepareSnapshotImage_TarGzBuildsExt4(t *testing.T) {
	// Build a .tar.gz on disk.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := bytes.Repeat([]byte{0xAB}, 2048)
	_ = tw.WriteHeader(&tar.Header{Name: "ref/data.bin", Mode: 0o644, Size: int64(len(content))})
	_, _ = tw.Write(content)
	_ = tw.Close()

	gzPath := filepath.Join(t.TempDir(), "ref.tar.gz")
	gf, err := os.Create(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(gf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	gw.Close()
	gf.Close()

	c := &Client{}
	prepared, err := c.PrepareSnapshotImage(context.Background(), gzPath, "us-east-1", 64*1024*1024, "")
	if err != nil {
		t.Fatalf("PrepareSnapshotImage(tar.gz): %v", err)
	}
	defer prepared.Cleanup()

	out := filepath.Join(t.TempDir(), "img.ext4")
	f, _ := os.Create(out)
	if _, err := io.Copy(f, prepared.Reader); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if !hasExt4Magic(t, out) {
		t.Error("tar.gz source did not produce a valid ext4 image")
	}
}

func TestPrepareSnapshotImage_RawImagePassthrough(t *testing.T) {
	// A non-archive local file is treated as a raw image and streamed verbatim.
	raw := filepath.Join(t.TempDir(), "disk.raw")
	want := []byte("raw filesystem image bytes")
	if err := os.WriteFile(raw, want, 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Client{}
	prepared, err := c.PrepareSnapshotImage(context.Background(), raw, "us-east-1", 0, "")
	if err != nil {
		t.Fatalf("PrepareSnapshotImage(raw): %v", err)
	}
	defer prepared.Cleanup()

	got, err := io.ReadAll(prepared.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("raw passthrough altered bytes: got %q want %q", got, want)
	}
}

func TestPrepareSnapshotImage_TempDirIsHonored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.bin"), bytes.Repeat([]byte{0x9}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dedicated temp dir; the staged ext4 image must land here, not in /tmp.
	staging := t.TempDir()

	c := &Client{}
	prepared, err := c.PrepareSnapshotImage(context.Background(), dir, "us-east-1", 64*1024*1024, staging)
	if err != nil {
		t.Fatalf("PrepareSnapshotImage(tempDir): %v", err)
	}
	defer prepared.Cleanup()

	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "spawn-ext4-") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the staged ext4 image in %s, contents: %v", staging, entries)
	}
}
