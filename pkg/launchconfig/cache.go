package launchconfig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// CachePath is where spored caches the object on the root volume, and where a
// detached sweep member's user data seeds it: /etc/spawn/launch-config.json on
// Linux, %PROGRAMDATA%\spored\launch-config.json on Windows. A variable so
// tests can point it elsewhere.
var CachePath = defaultCachePath()

// LinuxCachePath is CachePath on Linux, which the Linux bootstrap writes.
const LinuxCachePath = "/etc/spawn/launch-config.json"

func defaultCachePath() string {
	if runtime.GOOS != "windows" {
		return LinuxCachePath
	}
	base := os.Getenv("PROGRAMDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "spored", "launch-config.json")
}

// Source is where [Load] found the object.
type Source string

const (
	// FromCache: the cached copy on the root volume, verified.
	FromCache Source = "cache"
	// FromS3: fetched from the staging bucket and verified (and cached, unless
	// the write failed).
	FromS3 Source = "s3"
)

// GetObjectAPI is the S3 call spored makes. *s3.Client satisfies it.
type GetObjectAPI interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// maxObjectBytes bounds what a fetch reads. A real object is well under a
// kilobyte; anything this size is not one.
const maxObjectBytes = 256 << 10

// Load returns the launch config object an instance's spawn:config tag names,
// verified against sha. It reads the cached copy first; a cache that is
// missing, unreadable or does not hash to sha (tampered, corrupted, or
// inherited from an image of another instance) is not used, and the object is
// fetched from s3://<bucket>/<Key(sha)> with api, as account (the expected
// bucket owner), verified, and written to the cache for the next boot. A cache
// write failure is returned in cacheErr and does not fail the load.
func Load(ctx context.Context, api GetObjectAPI, bucket, account, sha, cachePath string) (doc *Document, unknown []string, src Source, cacheErr, err error) {
	if !ValidSHA(sha) {
		return nil, nil, "", nil, fmt.Errorf("spawn:config %q is not a sha256", sha)
	}
	if body, rerr := os.ReadFile(cachePath); rerr == nil {
		if d, u, derr := Decode(body, sha); derr == nil {
			return d, u, FromCache, nil, nil
		}
		// Not ours, or not intact: never read it, and do not keep it.
		_ = os.Remove(cachePath)
	}
	if api == nil {
		return nil, nil, "", nil, errors.New("no cached launch config object, and no S3 client to fetch it with")
	}
	body, err := Fetch(ctx, api, bucket, account, sha)
	if err != nil {
		return nil, nil, "", nil, err
	}
	d, u, err := Decode(body, sha)
	if err != nil {
		return nil, nil, "", nil, err
	}
	return d, u, FromS3, WriteCache(cachePath, body), nil
}

// Fetch reads the object for sha from the staging bucket. It does not verify
// it; [Decode] does.
func Fetch(ctx context.Context, api GetObjectAPI, bucket, account, sha string) ([]byte, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(Key(sha))}
	if account != "" {
		in.ExpectedBucketOwner = aws.String(account)
	}
	out, err := api.GetObject(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("fetch launch config object s3://%s/%s: %w", bucket, Key(sha), err)
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(out.Body, maxObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read launch config object s3://%s/%s: %w", bucket, Key(sha), err)
	}
	if len(body) > maxObjectBytes {
		return nil, fmt.Errorf("launch config object s3://%s/%s is over %d bytes; refusing it", bucket, Key(sha), maxObjectBytes)
	}
	return body, nil
}

// WriteCache writes body to path, mode 0600, atomically: a crash mid-write
// leaves the old file or none, never a partial one (which would fail its hash
// and be fetched again anyway).
func WriteCache(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cache launch config object: %w", err)
	}
	f, err := os.CreateTemp(dir, ".launch-config-*")
	if err != nil {
		return fmt.Errorf("cache launch config object: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	// Windows has no mode bits beyond read-only; the directory's ACL
	// (%PROGRAMDATA%\spored, LocalSystem's) is what protects it there.
	if err := f.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = f.Close()
		return fmt.Errorf("cache launch config object: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("cache launch config object: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cache launch config object: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("cache launch config object: %w", err)
	}
	return nil
}

// DiscardCache removes a cached object that no spawn:config tag names: the
// instance was launched from an image of one that had it. Missing is fine.
func DiscardCache(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
