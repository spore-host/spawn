package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

// ---- #944: where data_output files arrive ------------------------------------

// reroot moves an absolute example path under root, so the example's own
// patterns can be run in a temp directory.
func reroot(root, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Join(root, p)
	}
	return p
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func handlerFor(t *testing.T, env *testutil.TestEnv, def *Pipeline, stageID string, index int) *StageDataHandler {
	t.Helper()
	h, err := NewStageDataHandlerWithAWSConfig(context.Background(), testBucket, "pfx", def.PipelineID, stageID, env.AWSConfig)
	if err != nil {
		t.Fatal(err)
	}
	h.instanceIndex = index
	h.pipelineDef = def
	return h
}

func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// examples/pipelines/ml-training.json, run through the real upload and
// download against the emulator, lands files where its README says, which is
// where the example's commands read them: preprocess's "/data/processed/a.csv"
// at train's "/data/input/a.csv" (it arrived as "/data/input/data/processed/a.csv"),
// and each of train's four instances' "/models/best.h5" at evaluate's
// "/models/<index>/best.h5" (they overwrote each other; the last upload won).
func TestMLTrainingExampleDataLandsWhereTheExampleReadsIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: models the Linux stage instance's /data filesystem; the stage runner runs only on Linux spores (#1002)")
	}
	def, err := LoadPipelineFromFile("../../examples/pipelines/ml-training.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for i := range def.Stages {
		st := &def.Stages[i]
		if st.DataOutput != nil {
			for j, p := range st.DataOutput.Paths {
				st.DataOutput.Paths[j] = reroot(root+"/"+st.StageID, p)
			}
		}
		if st.DataInput != nil {
			st.DataInput.DestPath = reroot(root+"/"+st.StageID, st.DataInput.DestPath)
		}
	}
	env := testutil.SubstrateServer(t)
	setupDataBucket(t, env)
	ctx := context.Background()

	pre := def.GetStage("preprocess")
	writeFile(t, root+"/preprocess/data/processed/a.csv", "rows")
	if err := handlerFor(t, env, def, "preprocess", 0).UploadStageOutputs(ctx, pre.DataOutput); err != nil {
		t.Fatal(err)
	}

	train := def.GetStage("train")
	if train.InstanceCount != 4 {
		t.Fatalf("the example's train stage has %d instances; this test is about a multi-instance stage", train.InstanceCount)
	}
	if err := handlerFor(t, env, def, "train", 0).DownloadStageInputs(ctx, train.DataInput); err != nil {
		t.Fatal(err)
	}
	if got := filesUnder(t, root+"/train/data/input"); strings.Join(got, ",") != "a.csv" {
		t.Errorf("train's /data/input holds %v, want [a.csv]", got)
	}
	for i := 0; i < train.InstanceCount; i++ {
		writeFile(t, root+"/train/models/best.h5", "model from instance "+string(rune('0'+i)))
		writeFile(t, root+"/train/logs/training.log", "log")
		if err := handlerFor(t, env, def, "train", i).UploadStageOutputs(ctx, train.DataOutput); err != nil {
			t.Fatal(err)
		}
	}

	eval := def.GetStage("evaluate")
	if err := handlerFor(t, env, def, "evaluate", 0).DownloadStageInputs(ctx, eval.DataInput); err != nil {
		t.Fatal(err)
	}
	got := filesUnder(t, root+"/evaluate/models")
	want := []string{"0/best.h5", "0/training.log", "1/best.h5", "1/training.log", "2/best.h5", "2/training.log", "3/best.h5", "3/training.log"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("evaluate's /models holds %v, want %v", got, want)
	}
	for i := 0; i < 4; i++ {
		if b, _ := os.ReadFile(filepath.Join(root, "evaluate/models", string(rune('0'+i)), "best.h5")); string(b) != "model from instance "+string(rune('0'+i)) {
			t.Errorf("instance %d's model = %q", i, b)
		}
	}
	// A pattern selects within each instance's directory: "*.h5" picks every
	// instance's model and no log.
	only := &DataConfig{Mode: "s3", SourceStage: "train", Pattern: "*.h5", DestPath: root + "/only-models"}
	if err := handlerFor(t, env, def, "evaluate", 0).DownloadStageInputs(ctx, only); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(filesUnder(t, root+"/only-models"), ","); got != "0/best.h5,1/best.h5,2/best.h5,3/best.h5" {
		t.Errorf("pattern *.h5 from a 4-instance stage selected %s", got)
	}

	// The example's evaluate command reads the first instance's model.
	if !strings.Contains(eval.Command, "/models/0/best.h5") {
		t.Errorf("evaluate's command %q does not read a path the layout produces", eval.Command)
	}
}

// fanout-fanin.json: each process-chunk-N stage selects its chunks with a
// relative pattern, and the aggregate stage gathers three stages' outputs into
// one dest_path without collision.
func TestFanoutExamplePatternsSelectTheirChunks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: models the Linux stage instance's /data filesystem; the stage runner runs only on Linux spores (#1002)")
	}
	def, err := LoadPipelineFromFile("../../examples/pipelines/fanout-fanin.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for i := range def.Stages {
		st := &def.Stages[i]
		if st.DataOutput != nil {
			for j, p := range st.DataOutput.Paths {
				st.DataOutput.Paths[j] = reroot(root+"/"+st.StageID, p)
			}
		}
		if st.DataInput != nil {
			st.DataInput.DestPath = reroot(root+"/"+st.StageID, st.DataInput.DestPath)
		}
	}
	env := testutil.SubstrateServer(t)
	setupDataBucket(t, env)
	ctx := context.Background()

	for _, c := range []string{"chunk-1-a", "chunk-1-b", "chunk-2-a", "chunk-3-a"} {
		writeFile(t, root+"/split/data/chunks/"+c, c)
	}
	if err := handlerFor(t, env, def, "split", 0).UploadStageOutputs(ctx, def.GetStage("split").DataOutput); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]string{"1": "chunk-1-a,chunk-1-b", "2": "chunk-2-a", "3": "chunk-3-a"} {
		id := "process-chunk-" + n
		st := def.GetStage(id)
		if err := handlerFor(t, env, def, id, 0).DownloadStageInputs(ctx, st.DataInput); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(filesUnder(t, root+"/"+id+"/input"), ","); got != want {
			t.Errorf("%s received %s, want %s", id, got, want)
		}
		writeFile(t, root+"/"+id+"/output/chunk-"+n+"-result.csv", n)
		if err := handlerFor(t, env, def, id, 0).UploadStageOutputs(ctx, st.DataOutput); err != nil {
			t.Fatal(err)
		}
	}
	agg := def.GetStage("aggregate")
	if err := handlerFor(t, env, def, "aggregate", 0).DownloadStageInputs(ctx, agg.DataInput); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(filesUnder(t, root+"/aggregate/data/processed"), ","); got != "chunk-1-result.csv,chunk-2-result.csv,chunk-3-result.csv" {
		t.Errorf("aggregate received %s", got)
	}
}

// Two of one instance's outputs that map to the same key fail the upload
// rather than one silently replacing the other.
func TestUploadRefusesTwoFilesWithOneKey(t *testing.T) {
	env := testutil.SubstrateServer(t)
	setupDataBucket(t, env)
	root := t.TempDir()
	writeFile(t, root+"/a/x.csv", "a")
	writeFile(t, root+"/b/x.csv", "b")
	h := handlerFor(t, env, &Pipeline{PipelineID: "p"}, "s", 0)
	err := h.UploadStageOutputs(context.Background(), &DataConfig{Mode: "s3", Paths: []string{root + "/a/*", root + "/b/*"}})
	if err == nil || !strings.Contains(err.Error(), "x.csv") {
		t.Errorf("err = %v, want a collision on x.csv", err)
	}
}

// Two source stages delivering the same name to one dest_path fail the
// download rather than the second overwriting the first.
func TestDownloadRefusesTwoSourcesWritingOnePath(t *testing.T) {
	env := testutil.SubstrateServer(t)
	setupDataBucket(t, env)
	ctx := context.Background()
	for _, src := range []string{"left", "right"} {
		if _, err := env.S3Client().PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(testBucket), Key: aws.String(StageOutputPrefix("pfx", src) + "out.csv"), Body: strings.NewReader(src),
		}); err != nil {
			t.Fatal(err)
		}
	}
	h := handlerFor(t, env, &Pipeline{PipelineID: "p"}, "join", 0)
	err := h.DownloadStageInputs(ctx, &DataConfig{Mode: "s3", SourceStages: []string{"left", "right"}, DestPath: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "out.csv") {
		t.Errorf("err = %v, want a collision on out.csv", err)
	}
}

// A key that would climb out of dest_path is refused.
func TestDownloadRefusesAKeyOutsideDestPath(t *testing.T) {
	env := testutil.SubstrateServer(t)
	setupDataBucket(t, env)
	ctx := context.Background()
	if _, err := env.S3Client().PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(testBucket), Key: aws.String(StageOutputPrefix("pfx", "up") + "../../escape"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	h := handlerFor(t, env, &Pipeline{PipelineID: "p"}, "down", 0)
	if err := h.DownloadStageInputs(ctx, &DataConfig{Mode: "s3", SourceStage: "up", DestPath: filepath.Join(dest, "in")}); err == nil {
		t.Error("a key with ../ was downloaded")
	}
	if _, err := os.Stat(filepath.Join(dest, "escape")); err == nil {
		t.Error("a file was written outside dest_path")
	}
}

// An absolute data_input pattern can never match, and validate says so.
func TestValidateRejectsAnAbsoluteInputPattern(t *testing.T) {
	ids := map[string]bool{"split": true}
	d := &DataConfig{Mode: "s3", SourceStage: "split", Pattern: "/data/chunks/chunk-1-*"}
	if err := d.ValidateInput(ids); err == nil {
		t.Error("an absolute pattern validated")
	}
	d.Pattern = "chunk-1-*"
	if err := d.ValidateInput(ids); err != nil {
		t.Errorf("relative pattern: %v", err)
	}
}
