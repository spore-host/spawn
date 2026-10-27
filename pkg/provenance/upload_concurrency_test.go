package provenance

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
)

// manyFiles writes n files under dir/out and returns the recursive source.
func manyFiles(t *testing.T, n int) string {
	t.Helper()
	d := t.TempDir()
	for i := range n {
		sub := filepath.Join(d, "out", fmt.Sprintf("d%d", i%3))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%02d.dat", i)), []byte(strings.Repeat("x", i+1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(d, "out") + "/"
}

// gateUploader holds every upload until `want` are in flight at once (or every
// file has started), so the peak it records is how many uploads the code under
// test actually overlaps. Nothing is timed: a serial uploader simply never
// reaches `want`, and the test's context, a hang guard only, ends the wait.
type gateUploader struct {
	mu                    sync.Mutex
	cond                  *sync.Cond
	want, total           int
	inflight, peak, start int
	gen                   int
	opts                  []tm.Options
}

func newGateUploader(want, total int) *gateUploader {
	g := &gateUploader{want: want, total: total}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gateUploader) UploadObject(ctx context.Context, in *tm.UploadObjectInput, optFns ...func(*tm.Options)) (*tm.UploadObjectOutput, error) {
	_, _ = io.Copy(io.Discard, in.Body)
	var o tm.Options
	for _, fn := range optFns {
		fn(&o)
	}
	stop := context.AfterFunc(ctx, func() { g.mu.Lock(); g.cond.Broadcast(); g.mu.Unlock() })
	defer stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.opts = append(g.opts, o)
	g.inflight++
	g.start++
	g.peak = max(g.peak, g.inflight)
	if g.inflight >= g.want || g.start == g.total {
		g.gen++
		g.cond.Broadcast()
	} else {
		for my := g.gen; g.gen == my && ctx.Err() == nil; {
			g.cond.Wait()
		}
	}
	g.inflight--
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &tm.UploadObjectOutput{ETag: aws.String(`"e"`)}, nil
}

// hangGuard bounds a test that would otherwise deadlock on a serial uploader.
// It is never what makes a passing test pass.
func hangGuard(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// #1324: a directory output's files are uploaded tune.Workers at a time, no
// more and no fewer, where it used to be one at a time.
func TestUploadOutputOverlapsFilesUpToWorkers(t *testing.T) {
	const files = 24
	src := manyFiles(t, files)
	for _, workers := range []int{1, 4, 8} {
		g := newGateUploader(workers, files)
		ws, err := UploadOutput(hangGuard(t), g, nil, src, "s3://b/p/", nil, nil, UploadTuning{Workers: workers, Requests: 64, PartSize: 16 * mib})
		if g.peak != workers {
			t.Errorf("workers=%d: at most %d uploads were in flight at once, want exactly %d", workers, g.peak, workers)
		}
		if err != nil || len(ws) != files {
			t.Fatalf("workers=%d: %d written, %v", workers, len(ws), err)
		}
	}
	// More workers than files: every file at once, no idle goroutine waiting.
	g := newGateUploader(3, 3)
	if _, err := UploadOutput(hangGuard(t), g, nil, manyFiles(t, 3), "s3://b/p/", nil, nil, UploadTuning{Workers: 16, Requests: 64}); err != nil || g.peak != 3 {
		t.Errorf("3 files, 16 workers: peak %d (%v), want 3", g.peak, err)
	}
}

// Each upload gets the tuning's part size (and the multipart threshold with
// it, as s3transfer.WithPartSize) and its share of the part requests: one
// file gets them all, many share them.
func TestUploadOutputPassesThePartTuningPerUpload(t *testing.T) {
	tune := UploadTuning{Workers: 4, Requests: 64, PartSize: 16 * mib}
	single := filepath.Join(strings.TrimSuffix(manyFiles(t, 1), "/"), "d0", "f00.dat")
	for name, c := range map[string]struct {
		src      string
		files    int
		wantConc int
	}{
		"one file":  {single, 1, 64},
		"ten files": {manyFiles(t, 10), 10, 16},
	} {
		g := newGateUploader(min(tune.Workers, c.files), c.files)
		if _, err := UploadOutput(hangGuard(t), g, nil, c.src, "s3://b/p/", nil, nil, tune); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(g.opts) != c.files {
			t.Fatalf("%s: %d uploads, want %d", name, len(g.opts), c.files)
		}
		for _, o := range g.opts {
			if o.PartSizeBytes != 16*mib || o.MultipartUploadThreshold != 16*mib || o.Concurrency != c.wantConc {
				t.Errorf("%s: upload options part=%d threshold=%d concurrency=%d, want 16 MiB, 16 MiB, %d",
					name, o.PartSizeBytes, o.MultipartUploadThreshold, o.Concurrency, c.wantConc)
			}
		}
	}
	// A zero tuning leaves the client's own settings alone.
	g := newGateUploader(1, 1)
	if _, err := UploadOutput(hangGuard(t), g, nil, single, "s3://b/p/", nil, nil, UploadTuning{}); err != nil || !reflect.DeepEqual(g.opts, []tm.Options{{}}) {
		t.Errorf("zero tuning set upload options %+v (%v)", g.opts, err)
	}
}

// objectView is everything recorded about one object: its body, tags and
// annotation, and what the annotation was bound to.
type objectView struct{ Body, Tagging, Annotation, Payload, IfMatch string }

func views(f *fakeS3) map[string]objectView {
	out := map[string]objectView{}
	for _, u := range f.uploads {
		k := aws.ToString(u.Key)
		out[k] = objectView{Body: f.bodies[k], Tagging: aws.ToString(u.Tagging)}
	}
	for _, a := range f.annotations {
		k := aws.ToString(a.Key)
		v := out[k]
		v.Annotation, v.Payload, v.IfMatch = aws.ToString(a.AnnotationName), f.payloads[k], aws.ToString(a.ObjectIfMatch)
		out[k] = v
	}
	return out
}

// Concurrency changes nothing about what is written: every object gets the
// same body, tags and annotation, bound to its own ETag, and the result lists
// the objects in the same order as the one-at-a-time upload it replaced.
func TestUploadOutputConcurrentWritesWhatSerialWrote(t *testing.T) {
	src := manyFiles(t, 30)
	tags := map[string]string{TagRunID: "r1", TagClass: "cheap"}
	rec := []byte(`{"schema":1}`)
	serial, concurrent := &fakeS3{}, &fakeS3{}
	ws1, err1 := UploadOutput(context.Background(), serial, serial, src, "s3://bkt/res", tags, rec, UploadTuning{})
	ws2, err2 := UploadOutput(context.Background(), concurrent, concurrent, src, "s3://bkt/res", tags, rec, UploadTuning{Workers: 8, Requests: 64, PartSize: 16 * mib})
	if err1 != nil || err2 != nil || len(ws1) != 30 {
		t.Fatalf("serial %d (%v), concurrent %d (%v)", len(ws1), err1, len(ws2), err2)
	}
	if !reflect.DeepEqual(ws1, ws2) {
		t.Errorf("results differ:\nserial     %+v\nconcurrent %+v", ws1, ws2)
	}
	// The walk's order (sorted by relative path), as the serial loop gave it,
	// not the order uploads happened to finish in.
	for i := 1; i < len(ws2); i++ {
		if ws2[i-1].Key >= ws2[i].Key {
			t.Fatalf("results out of walk order at %d: %s then %s", i, ws2[i-1].Key, ws2[i].Key)
		}
	}
	v1, v2 := views(serial), views(concurrent)
	if !reflect.DeepEqual(v1, v2) {
		t.Errorf("objects differ:\nserial     %+v\nconcurrent %+v", v1, v2)
	}
	for k, v := range v2 {
		if v.IfMatch != `"etag-`+k+`"` || v.Tagging != EncodeTags(tags) || v.Payload != string(rec) {
			t.Errorf("%s: %+v", k, v)
		}
	}
}

// One file failing is reported, naming it, and does not cost the others:
// every other file is still uploaded and annotated (#1324; as `aws s3 cp
// --recursive` goes on past a failed file).
func TestUploadOutputOneFailureKeepsTheRest(t *testing.T) {
	src := manyFiles(t, 9)
	for _, workers := range []int{0, 1, 3} {
		f := &fakeS3{uploadErrOn: "p/d0/f00.dat"} // the first file in the walk
		ws, err := UploadOutput(context.Background(), f, f, src, "s3://bkt/p", nil, []byte("{}"), UploadTuning{Workers: workers})
		if err == nil || !strings.Contains(err.Error(), "f00.dat") || !strings.Contains(err.Error(), "AccessDenied") {
			t.Errorf("workers=%d: error %v, want the failed file named", workers, err)
		}
		if len(ws) != 8 || len(f.uploads) != 8 || len(f.annotations) != 8 {
			t.Errorf("workers=%d: %d written, %d uploaded, %d annotated; want the 8 other files", workers, len(ws), len(f.uploads), len(f.annotations))
		}
		for _, w := range ws {
			if w.Key == "p/d0/f00.dat" {
				t.Errorf("workers=%d: the failed file is listed as written", workers)
			}
		}
	}
}

// A cancelled upload starts no further file and says so.
func TestUploadOutputStopsStartingFilesWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeS3{}
	_, err := UploadOutput(ctx, f, f, manyFiles(t, 5), "s3://bkt/p", nil, nil, UploadTuning{Workers: 2})
	if err == nil || len(f.uploads) != 0 {
		t.Fatalf("a cancelled upload: %d uploaded, %v; want none started and an error", len(f.uploads), err)
	}
}
