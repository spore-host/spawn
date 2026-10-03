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
