package taskproto

import (
	"os"
	"testing"
)

// TestMain redirects the generated scripts' LOCAL artifact paths into a
// per-binary temp directory (#642).
//
// Several tests here exec a generated script and then read the record it wrote.
// With the production default those reads all landed on one absolute path,
// /tmp/spawn-completion.json — which pkg/taskpool's exec tests also write, from
// a separate test binary running in parallel under `go test ./...`. Whichever
// script finished last decided what the other package's test read, so
// TestGenerateWrapper_RunIDKeepsCompletionJSONValid would fail having parsed
// pkg/taskpool's `nf-fail` record. It reproduced 3/3 and passed 3/3 on the same
// commit depending only on test-cache state, which is the worst kind of red:
// it reads as "your change broke the wrapper" on whatever PR happens to lose.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "taskproto-records-")
	if err != nil {
		panic("create temp record dir: " + err.Error())
	}
	restore := SetLocalRecordDirForTest(dir)
	code := m.Run()
	restore()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// legacyPrefix is the results prefix the generators used to DERIVE from a bucket
// name, before spawn#646 made it a caller-resolved parameter. The tests below
// keep naming a bucket because that is what their assertions read, so these three
// shims translate.
func legacyPrefix(bucket, taskID string) string {
	return "s3://" + bucket + "/" + defaultResultsSubPrefix + "/" + taskID
}

// The three shims also absorb the generators' error return (spawn#764). They
// panic rather than taking a *testing.T and calling Fatal, which would mean
// editing every one of their call sites: the only way these can fail is an empty
// ResultsPrefix or RunID, and every caller below passes both, so a failure here
// is a bug in the helper and not a condition under test. The validation itself is
// covered directly in wrapperoptions_test.go.
func mustScript(s string, err error) string {
	if err != nil {
		panic("test helper built invalid WrapperOptions: " + err.Error())
	}
	return s
}

func genWrapper(spec *TaskSpec, bucket, region string, gpu bool, runID string) string {
	return mustScript(GenerateWrapper(spec, legacyOpts(bucket, spec.TaskID, region, runID, gpu)))
}

func genPooled(spec *TaskSpec, bucket, region string, gpu bool, runID string) string {
	return mustScript(GeneratePooledJobScript(spec, legacyOpts(bucket, spec.TaskID, region, runID, gpu)))
}

func genFlush(spec *TaskSpec, bucket, region, runID string) string {
	return mustScript(GenerateFlushScript(spec, legacyOpts(bucket, spec.TaskID, region, runID, false)))
}

func legacyOpts(bucket, taskID, region, runID string, gpu bool) WrapperOptions {
	return WrapperOptions{
		ResultsPrefix: legacyPrefix(bucket, taskID),
		Region:        region,
		RunID:         runID,
		GPU:           gpu,
	}
}
