// Package s3transfer builds the S3 transfer manager that spawn's uploads and
// downloads go through, configured to behave as the Uploader and Downloader of
// the deprecated aws-sdk-go-v2 s3 "manager" package it replaced (#932).
//
// feature/s3/transfermanager is not a drop-in rename. Its defaults differ from
// manager's, and three of the four callers run inside spored on instances,
// where a change shows up only on real hardware. So New starts from manager's
// values instead of transfermanager's:
//
//   - part size 5 MiB (transfermanager: 8 MiB), for uploads and ranged downloads;
//   - multipart threshold equal to the part size, so a body smaller than one
//     part is a single PutObject and anything larger is multipart, as with
//     manager (transfermanager: a fixed 16 MiB threshold);
//   - 5 concurrent parts (the same in both);
//   - downloads as ranged GETs of one part size each (transfermanager: a
//     part-number GET per uploaded part, which follows the object's own part
//     layout rather than ours).
//
// Checksums need no option. Both libraries default to the CRC32 that
// RequestChecksumCalculationWhenSupported asks for on upload, and both ask for
// response checksum validation on download.
package s3transfer

import (
	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	tmtypes "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager/types"
)

const (
	// DefaultPartSize is the old manager package's DefaultUploadPartSize and
	// DefaultDownloadPartSize. It is also S3's minimum part size.
	DefaultPartSize int64 = 5 * 1024 * 1024
	// DefaultConcurrency is the old manager package's DefaultUploadConcurrency
	// and DefaultDownloadConcurrency.
	DefaultConcurrency = 5
)

// New returns a transfer manager client for s3Client with the old manager
// package's defaults. optFns run afterwards; use WithPartSize to change the
// part size, because it moves the multipart threshold with it.
func New(s3Client tm.S3APIClient, optFns ...func(*tm.Options)) *tm.Client {
	base := func(o *tm.Options) {
		o.PartSizeBytes = DefaultPartSize
		o.MultipartUploadThreshold = DefaultPartSize
		o.Concurrency = DefaultConcurrency
		o.GetObjectType = tmtypes.GetObjectRanges
	}
	return tm.New(s3Client, append([]func(*tm.Options){base}, optFns...)...)
}

// WithPartSize sets the part size and the multipart threshold together, as
// manager's single PartSize did: a body smaller than one part is uploaded with
// a single PutObject.
func WithPartSize(n int64) func(*tm.Options) {
	return func(o *tm.Options) {
		o.PartSizeBytes = n
		o.MultipartUploadThreshold = n
	}
}

// WithConcurrency sets how many parts are transferred at once.
func WithConcurrency(n int) func(*tm.Options) {
	return func(o *tm.Options) { o.Concurrency = n }
}
