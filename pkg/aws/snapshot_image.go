package aws

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Microsoft/hcsshim/ext4/tar2ext4"
)

// SnapshotSourceKind classifies what `--from` points at, which decides whether
// spawn streams raw bytes straight into the snapshot or first builds an ext4
// filesystem image from the input (#147 Part B).
type SnapshotSourceKind int

const (
	// SourceRawImage is a raw disk/filesystem image — its bytes ARE the block
	// device, streamed verbatim into the snapshot (Part A).
	SourceRawImage SnapshotSourceKind = iota
	// SourceDir is a local directory — tarred in-process, then converted to ext4.
	SourceDir
	// SourceTar is a (possibly gzipped) tar archive — converted to ext4.
	SourceTar
)

// PreparedImage is a ready-to-snapshot raw image: an open reader over the image
// bytes plus a Close that releases any temp file the conversion produced.
type PreparedImage struct {
	Reader io.ReadCloser
	// Cleanup removes any temp artifacts; always call it when done.
	Cleanup func()
}

// classifySnapshotSource decides how to treat a local `--from` path:
//   - a directory          → SourceDir
//   - *.tar / *.tar.gz/.tgz → SourceTar
//   - anything else         → SourceRawImage (the user supplied a real image)
//
// s3:// sources are always treated by extension only (we don't stat them) and
// fall here as raw/tar; the caller passes the extension-bearing key.
func classifySnapshotSource(path string, isDir bool) SnapshotSourceKind {
	if isDir {
		return SourceDir
	}
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".tar"),
		strings.HasSuffix(lower, ".tar.gz"),
		strings.HasSuffix(lower, ".tgz"):
		return SourceTar
	default:
		return SourceRawImage
	}
}

// PrepareSnapshotImage turns a `--from` source into a raw ext4-image reader the
// snapshot builder can stream, staying fully instance-free and cross-platform
// (pure Go — no mkfs, no EC2 bake box) (#147 Part B).
//
//   - raw image   → opened and returned as-is (Part A behavior).
//   - directory   → tarred in-process and converted to an ext4 image.
//   - tar/tar.gz  → (gunzipped if needed and) converted to an ext4 image.
//
// maxBytes caps the ext4 filesystem at the target volume size. The ext4 image
// is built compactly (sized to content, capped at maxBytes) into a temp file in
// tempDir (empty = the system temp dir); Cleanup removes it. tempDir lets the
// caller stage the (potentially large) image on a roomier volume than /tmp.
func (c *Client) PrepareSnapshotImage(ctx context.Context, source, region string, maxBytes int64, tempDir string) (*PreparedImage, error) {
	// Local directory?
	isDir := false
	if !strings.HasPrefix(source, "s3://") {
		if fi, err := os.Stat(source); err == nil {
			isDir = fi.IsDir()
		}
	}
	kind := classifySnapshotSource(source, isDir)

	if kind == SourceRawImage {
		rc, err := c.OpenImageSource(ctx, source, region)
		if err != nil {
			return nil, err
		}
		return &PreparedImage{Reader: rc, Cleanup: func() {}}, nil
	}

	// Build a tar stream for the source, then convert tar → ext4.
	var tarReader io.Reader
	var closeSrc func() error

	switch kind {
	case SourceDir:
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(tarDirectory(source, pw))
		}()
		tarReader = pr
		closeSrc = func() error { return pr.Close() }
	case SourceTar:
		rc, err := c.OpenImageSource(ctx, source, region)
		if err != nil {
			return nil, err
		}
		// Transparently gunzip a .tar.gz/.tgz.
		if isGzipName(source) {
			gz, err := gzip.NewReader(bufio.NewReader(rc))
			if err != nil {
				rc.Close()
				return nil, fmt.Errorf("gunzip %s: %w", source, err)
			}
			tarReader = gz
			closeSrc = func() error { gz.Close(); return rc.Close() }
		} else {
			tarReader = rc
			closeSrc = rc.Close
		}
	}

	img, err := os.CreateTemp(tempDir, "spawn-ext4-*.img")
	if err != nil {
		_ = closeSrc()
		return nil, fmt.Errorf("create temp image in %q: %w", tempDirOrDefault(tempDir), err)
	}
	cleanup := func() {
		img.Close()
		os.Remove(img.Name())
	}

	// Override the ext4 lost+found directory mode. The tar2ext4 writer creates
	// lost+found as root-owned 0700 (hcsshim compactext4), which makes a tool
	// that walks the volume (e.g. MetaPhlAn's `find -L <db>`) emit a harmless but
	// noisy "Permission denied" on lost+found. Prepending a lost+found dir entry
	// at 0755 re-sets the mode (the writer allows re-creating it as a directory).
	tarReader = prependLostFound(tarReader)

	opts := []tar2ext4.Option{}
	if maxBytes > 0 {
		opts = append(opts, tar2ext4.MaximumDiskSize(maxBytes))
	}
	if err := tar2ext4.Convert(tarReader, img, opts...); err != nil {
		_ = closeSrc()
		cleanup()
		return nil, fmt.Errorf("build ext4 image from %s: %w", source, err)
	}
	if err := closeSrc(); err != nil {
		cleanup()
		return nil, fmt.Errorf("read source %s: %w", source, err)
	}
	if _, err := img.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, fmt.Errorf("rewind image: %w", err)
	}

	return &PreparedImage{Reader: img, Cleanup: cleanup}, nil
}

// tempDirOrDefault renders tempDir for error messages (empty → the OS default).
func tempDirOrDefault(tempDir string) string {
	if tempDir == "" {
		return os.TempDir()
	}
	return tempDir
}

// isGzipName reports whether a path looks gzip-compressed by extension.
func isGzipName(path string) bool {
	l := strings.ToLower(path)
	return strings.HasSuffix(l, ".gz") || strings.HasSuffix(l, ".tgz")
}

// prependLostFound returns a reader that emits a single `lost+found` directory
// tar entry (mode 0755) followed by the original tar stream. tar2ext4 processes
// entries in order and re-applies the mode when a directory entry repeats, so
// this overrides the writer's default root-only 0700 lost+found without
// re-muxing the (possibly very large) source archive.
//
// Only a tar HEADER is written for the prepended entry — Flush (not Close) is
// used so no end-of-archive zero blocks are emitted before the real stream.
func prependLostFound(src io.Reader) io.Reader {
	var hdrBuf bytes.Buffer
	tw := tar.NewWriter(&hdrBuf)
	// A directory entry: trailing slash + Typeflag dir. Size 0, so the header is
	// self-contained and immediately followed by the next entry.
	_ = tw.WriteHeader(&tar.Header{
		Name:     "lost+found/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	})
	_ = tw.Flush() // header only — do NOT Close (that appends the archive trailer)
	return io.MultiReader(&hdrBuf, src)
}

// tarDirectory writes a tar stream of dir's contents (recursively, with paths
// relative to dir) to w. Regular files, directories, and symlinks are included.
func tarDirectory(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// tar wants forward slashes regardless of host OS.
		name := filepath.ToSlash(rel)

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path) // #nosec G304 -- walking a user-specified source tree is the intent
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
