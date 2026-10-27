package provenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/s3transfer"
)

// Uploader is the slice of the transfer manager used: one call per object, with
// multipart for large files, and tags set in the same request.
type Uploader interface {
	UploadObject(ctx context.Context, in *tm.UploadObjectInput, optFns ...func(*tm.Options)) (*tm.UploadObjectOutput, error)
}

// Annotator writes the provenance annotation after the upload. Annotations
// can't be set at upload time (design: "What is recorded").
type Annotator interface {
	PutObjectAnnotation(ctx context.Context, in *s3.PutObjectAnnotationInput, optFns ...func(*s3.Options)) (*s3.PutObjectAnnotationOutput, error)
}

// Written is one object an output upload created.
type Written struct {
	Bucket, Key, ETag string
	Bytes             int64
	// AnnotateErr is the annotation failure, if any. The object is uploaded and
	// tagged either way; only its provenance detail is missing.
	AnnotateErr error
}

// UploadOutput uploads one task output exactly where `aws s3 cp` would put it
// (ObjectKey), with tags set in the same request, then annotates each object
// with the provenance record.
//
// It replaces the wrapper's `aws s3 cp` for outputs. Tagging at upload is why:
// only an object this call just wrote is ever tagged, so a user's existing
// object under the same prefix is never touched. PutObjectTagging replaces the
// whole tag set, and the task role has no read access to verify an object it
// didn't write. The annotation is bound to the object just written
// (ObjectIfMatch on its ETag), so a concurrent overwrite is refused, not
// annotated.
//
// src is the manifest source: a directory when it ends in "/" (recursive, as
// `aws s3 cp --recursive`), otherwise one file.
//
// Files are uploaded tune.Workers at a time, each with its share of
// tune.Requests part requests and tune.PartSize parts (#1324; see
// UploadTuning). A zero tuning uploads one file at a time with the client's own
// settings. Concurrency changes nothing per object: each is tagged in its own
// upload request and annotated after it, bound to the ETag that upload
// returned, and the result lists the objects written in the walk's stable
// order whatever order they finished in.
//
// An upload error does not stop the others, as `aws s3 cp --recursive` goes on
// past a failed file: every file is attempted, and the failures are returned
// together, each naming its file, after everything else is written. Once ctx is
// done no further file is started. An annotation error is recorded on that
// Written and is not an upload error.
func UploadOutput(ctx context.Context, up Uploader, ann Annotator, src, dst string, tags map[string]string, record []byte, tune UploadTuning) ([]Written, error) {
	bucket, destKey, err := SplitS3URI(dst)
	if err != nil {
		return nil, err
	}
	recursive := strings.HasSuffix(src, "/")
	files, err := localFiles(src, recursive)
	if err != nil {
		return nil, err
	}
	tagging := EncodeTags(tags)
	workers := min(max(1, tune.Workers), len(files))
	var opts []func(*tm.Options)
	if tune.PartSize > 0 {
		opts = append(opts, s3transfer.WithPartSize(tune.PartSize))
	}
	if tune.Requests > 0 {
		opts = append(opts, s3transfer.WithConcurrency(tune.PartConcurrency(workers)))
	}

	results := make([]Written, len(files))
	errs := make([]error, len(files))
	next := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				results[i], errs[i] = uploadAndAnnotate(ctx, up, ann, bucket, ObjectKey(destKey, files[i].rel, recursive), files[i].path, tagging, record, opts)
			}
		}()
	}
	var ctxErr error
	for i := range files {
		// Checked first: select picks at random when both cases are ready.
		if ctx.Err() == nil {
			select {
			case next <- i:
				continue
			case <-ctx.Done():
			}
		}
		ctxErr = fmt.Errorf("upload of %s stopped before %s: %w", src, files[i].path, ctx.Err())
		break
	}
	close(next)
	wg.Wait()

	var out []Written
	var failed []error
	for i := range files {
		if errs[i] != nil {
			failed = append(failed, errs[i])
		} else if results[i].Key != "" {
			out = append(out, results[i])
		}
	}
	if ctxErr != nil {
		failed = append(failed, ctxErr)
	}
	return out, errors.Join(failed...)
}

// uploadAndAnnotate writes one object, tagged, then annotates it with record
// bound to the ETag the upload returned.
func uploadAndAnnotate(ctx context.Context, up Uploader, ann Annotator, bucket, key, path, tagging string, record []byte, opts []func(*tm.Options)) (Written, error) {
	w, err := uploadOne(ctx, up, bucket, key, path, tagging, opts)
	if err != nil {
		return Written{}, fmt.Errorf("upload %s to s3://%s/%s: %w", path, bucket, key, err)
	}
	if len(record) > 0 && ann != nil {
		in := &s3.PutObjectAnnotationInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			AnnotationName:    aws.String(AnnotationName),
			AnnotationPayload: bytes.NewReader(record),
		}
		if w.ETag != "" {
			in.ObjectIfMatch = aws.String(w.ETag)
		}
		if _, aerr := ann.PutObjectAnnotation(ctx, in); aerr != nil {
			w.AnnotateErr = aerr
		}
	}
	return w, nil
}

func uploadOne(ctx context.Context, up Uploader, bucket, key, path, tagging string, opts []func(*tm.Options)) (Written, error) {
	fh, err := os.Open(path) //nolint:gosec // a task output path from the task's own spec
	if err != nil {
		return Written{}, err
	}
	defer func() { _ = fh.Close() }()
	st, err := fh.Stat()
	if err != nil {
		return Written{}, err
	}
	in := &tm.UploadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: fh}
	if tagging != "" {
		in.Tagging = aws.String(tagging)
	}
	res, err := up.UploadObject(ctx, in, opts...)
	if err != nil {
		return Written{}, err
	}
	return Written{Bucket: bucket, Key: key, ETag: aws.ToString(res.ETag), Bytes: st.Size()}, nil
}

type localFile struct{ path, rel string }

// localFiles lists what an output covers: every regular file under a recursive
// source, in a stable order, or the single file. Symlinks are not followed,
// matching `aws s3 cp`'s default. A missing source is an error, as it is for
// `aws s3 cp`.
func localFiles(src string, recursive bool) ([]localFile, error) {
	if !recursive {
		st, err := os.Stat(src)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			return nil, fmt.Errorf("%s is a directory; a recursive output's source must end in /", src)
		}
		return []localFile{{path: src, rel: filepath.Base(src)}}, nil
	}
	root := strings.TrimSuffix(src, "/")
	var files []localFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, localFile{path: p, rel: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}
