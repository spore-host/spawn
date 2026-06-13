package aws

import (
	"archive/tar"
	"bufio"
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
// is built compactly (sized to content, capped at maxBytes) into a temp file;
// Cleanup removes it.
func (c *Client) PrepareSnapshotImage(ctx context.Context, source, region string, maxBytes int64) (*PreparedImage, error) {
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

	img, err := os.CreateTemp("", "spawn-ext4-*.img")
	if err != nil {
		_ = closeSrc()
		return nil, fmt.Errorf("create temp image: %w", err)
	}
	cleanup := func() {
		img.Close()
		os.Remove(img.Name())
	}

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

// isGzipName reports whether a path looks gzip-compressed by extension.
func isGzipName(path string) bool {
	l := strings.ToLower(path)
	return strings.HasSuffix(l, ".gz") || strings.HasSuffix(l, ".tgz")
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
