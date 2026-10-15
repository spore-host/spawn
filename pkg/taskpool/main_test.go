package taskpool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// TestMain redirects the generated pooled job script's LOCAL artifact paths into
// a per-binary temp directory (#642). exec_test.go runs a real generated script
// for task "nf-fail"; with the production default it wrote the same absolute
// /tmp/spawn-completion.json that pkg/taskproto's exec tests read, and `go test
// ./...` runs the two packages in parallel. See pkg/taskproto/main_test.go.
//
// It also puts a stand-in `aws` first on PATH (#968). The job scripts these tests
// run upload their completion records with `aws s3 cp`, and the real CLI used
// the developer's ambient credentials: every `make check` sent S3 PutObject calls
// for tasks/nf-*/completion.json and .exitcode. The uploads are best-effort — the
// script does not `set -e` — so a stand-in that fails, as the real one does
// without credentials in CI, leaves every assertion's meaning unchanged.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "taskpool-records-")
	if err != nil {
		panic("create temp record dir: " + err.Error())
	}
	restore := taskproto.SetLocalRecordDirForTest(dir)

	binDir, err := os.MkdirTemp("", "taskpool-bin-")
	if err != nil {
		panic("create stub bin dir: " + err.Error())
	}
	stub := "#!/bin/sh\necho 'aws (test stand-in, #968): no AWS in unit tests' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "aws"), []byte(stub), 0o755); err != nil {
		panic("write aws stub: " + err.Error())
	}
	_ = os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	code := m.Run()
	restore()
	_ = os.RemoveAll(dir)
	_ = os.RemoveAll(binDir)
	os.Exit(code)
}
