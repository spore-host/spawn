package taskpool

import (
	"os"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// TestMain redirects the generated pooled job script's LOCAL artifact paths into
// a per-binary temp directory (#642). exec_test.go runs a real generated script
// for task "nf-fail"; with the production default it wrote the same absolute
// /tmp/spawn-completion.json that pkg/taskproto's exec tests read, and `go test
// ./...` runs the two packages in parallel. See pkg/taskproto/main_test.go.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "taskpool-records-")
	if err != nil {
		panic("create temp record dir: " + err.Error())
	}
	restore := taskproto.SetLocalRecordDirForTest(dir)
	code := m.Run()
	restore()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
