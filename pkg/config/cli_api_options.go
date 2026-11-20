package config

import (
	"sync"

	"github.com/aws/smithy-go/middleware"
)

// The spawn CLI's own SDK middleware, added to every config built for the
// caller's account. Empty in any program but the spawn CLI: lagotto, calque
// and spore-host-mcp import spawn's packages, and nothing here may change
// what their configs do.
//
// It is how the CLI's last mile (cmd/lastmile.go, libs#78) learns the
// account the command's own sts:GetCallerIdentity reported, so that it makes
// no STS call of its own. The middleware itself, and everything that reads
// what it records, is in cmd; this is only the seam the loaders below share.
var (
	cliAPIOptionsMu sync.Mutex
	cliAPIOptions   []func(*middleware.Stack) error
)

// SetCLIAPIOptions sets the API options the user-account loaders
// (pkg/aws.NewClientWithRegion, LoadComputeAWSConfig, and
// LoadInfraAWSConfig when infra is the caller's own profile) append to each
// config they build. cmd.Execute calls it once, before any command runs.
// It replaces any earlier set; none clears it.
func SetCLIAPIOptions(fns ...func(*middleware.Stack) error) {
	cliAPIOptionsMu.Lock()
	defer cliAPIOptionsMu.Unlock()
	cliAPIOptions = append([]func(*middleware.Stack) error(nil), fns...)
}

// CLIAPIOptions are the API options SetCLIAPIOptions set, as a fresh slice
// a caller may append to; nil when none were set.
func CLIAPIOptions() []func(*middleware.Stack) error {
	cliAPIOptionsMu.Lock()
	defer cliAPIOptionsMu.Unlock()
	if len(cliAPIOptions) == 0 {
		return nil
	}
	return append([]func(*middleware.Stack) error(nil), cliAPIOptions...)
}
