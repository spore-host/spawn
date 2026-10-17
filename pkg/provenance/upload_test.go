package provenance

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type fakeS3 struct {
	uploads     []tm.UploadObjectInput
	bodies      map[string]string
	annotations []s3.PutObjectAnnotationInput
	payloads    map[string]string
	uploadErrOn string
	annotateErr error
}

func (f *fakeS3) UploadObject(_ context.Context, in *tm.UploadObjectInput, _ ...func(*tm.Options)) (*tm.UploadObjectOutput, error) {
	key := aws.ToString(in.Key)
	if key == f.uploadErrOn {
		return nil, errors.New("AccessDenied")
	}
	b, _ := io.ReadAll(in.Body)
	if f.bodies == nil {
		f.bodies = map[string]string{}
	}
	f.bodies[key] = string(b)
	f.uploads = append(f.uploads, *in)
	return &tm.UploadObjectOutput{Key: in.Key, ETag: aws.String(`"etag-` + key + `"`)}, nil
}

func (f *fakeS3) PutObjectAnnotation(_ context.Context, in *s3.PutObjectAnnotationInput, _ ...func(*s3.Options)) (*s3.PutObjectAnnotationOutput, error) {
	b, _ := io.ReadAll(in.AnnotationPayload)
	if f.payloads == nil {
		f.payloads = map[string]string{}
	}
	f.payloads[aws.ToString(in.Key)] = string(b)
	f.annotations = append(f.annotations, *in)
	return &s3.PutObjectAnnotationOutput{}, f.annotateErr
}

func tree(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(d, "out", "sub"), 0o755))
	must(os.WriteFile(filepath.Join(d, "out", "a.txt"), []byte("A"), 0o600))
	must(os.WriteFile(filepath.Join(d, "out", "sub", "b.txt"), []byte("BB"), 0o600))
	must(os.Symlink(filepath.Join(d, "out", "a.txt"), filepath.Join(d, "out", "link.txt")))
	return d
}

func TestUploadOutputRecursiveTagsAtWriteAndAnnotatesWhatItWrote(t *testing.T) {
	d := tree(t)
	f := &fakeS3{}
	tags := map[string]string{TagRunID: "r1", TagClass: "cheap"}
	ws, err := UploadOutput(context.Background(), f, f, filepath.Join(d, "out")+"/", "s3://bkt/results/run1", tags, []byte(`{"schema":1}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"results/run1/a.txt": "A", "results/run1/sub/b.txt": "BB"}
	if len(ws) != 2 || len(f.uploads) != 2 {
		t.Fatalf("wrote %d objects (%v), want exactly the 2 regular files; a symlink is not followed", len(ws), f.bodies)
	}
	for _, u := range f.uploads {
		key := aws.ToString(u.Key)
		if want[key] != f.bodies[key] {
			t.Errorf("uploaded %q with body %q", key, f.bodies[key])
		}
		if aws.ToString(u.Bucket) != "bkt" {
			t.Errorf("bucket = %q", aws.ToString(u.Bucket))
		}
		if aws.ToString(u.Tagging) != EncodeTags(tags) {
			t.Errorf("%s: tags not set at upload: %q", key, aws.ToString(u.Tagging))
		}
	}
	for _, a := range f.annotations {
		key := aws.ToString(a.Key)
		if aws.ToString(a.AnnotationName) != AnnotationName || f.payloads[key] != `{"schema":1}` {
			t.Errorf("%s: annotation %q = %q", key, aws.ToString(a.AnnotationName), f.payloads[key])
		}
		if aws.ToString(a.ObjectIfMatch) != `"etag-`+key+`"` {
			t.Errorf("%s: annotation not bound to the uploaded ETag (ObjectIfMatch=%q)", key, aws.ToString(a.ObjectIfMatch))
		}
	}
}

func TestUploadOutputSingleFileKeysLikeAWSS3Cp(t *testing.T) {
	d := tree(t)
	file := filepath.Join(d, "out", "a.txt")
	for dst, wantKey := range map[string]string{"s3://bkt/f1/named.txt": "f1/named.txt", "s3://bkt/f2/prefix/": "f2/prefix/a.txt"} {
		f := &fakeS3{}
		ws, err := UploadOutput(context.Background(), f, f, file, dst, nil, nil)
		if err != nil || len(ws) != 1 || ws[0].Key != wantKey {
			t.Errorf("%s: wrote %+v (%v), want key %s", dst, ws, err, wantKey)
		}
		if len(f.uploads) == 1 && f.uploads[0].Tagging != nil {
			t.Errorf("%s: no tags requested, but Tagging=%q", dst, aws.ToString(f.uploads[0].Tagging))
		}
		if len(f.annotations) != 0 {
			t.Errorf("%s: annotated with no record", dst)
		}
	}
}

// An upload failure stops and is returned, like a failing `aws s3 cp`; an
// annotation failure is recorded and does not stop the upload.
func TestUploadOutputFailures(t *testing.T) {
	d := tree(t)
	f := &fakeS3{uploadErrOn: "p/sub/b.txt"}
	ws, err := UploadOutput(context.Background(), f, f, filepath.Join(d, "out")+"/", "s3://bkt/p", nil, []byte("{}"))
	if err == nil || len(ws) != 1 {
		t.Fatalf("upload failure: (%d written, %v), want 1 written then the error", len(ws), err)
	}

	g := &fakeS3{annotateErr: errors.New("AccessDenied")}
	ws, err = UploadOutput(context.Background(), g, g, filepath.Join(d, "out")+"/", "s3://bkt/p", nil, []byte("{}"))
	if err != nil || len(ws) != 2 || ws[0].AnnotateErr == nil {
		t.Fatalf("annotation failure: (%+v, %v), want both uploaded with AnnotateErr set", ws, err)
	}

	if _, err := UploadOutput(context.Background(), g, g, filepath.Join(d, "missing.txt"), "s3://bkt/p", nil, nil); err == nil {
		t.Error("a missing source uploaded nothing and reported success")
	}
	if _, err := UploadOutput(context.Background(), g, g, filepath.Join(d, "out"), "s3://bkt/p", nil, nil); err == nil {
		t.Error("a directory without a trailing / was accepted as a single file")
	}
}
