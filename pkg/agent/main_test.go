package agent

import (
	"testing"

	"github.com/spore-host/spawn/internal/hermetic"
)

// TestMain runs this package's tests hermetically: no ambient credentials, and
// any request that would leave the machine — AWS SDK, the spore.host DNS API,
// IMDS — fails the run and is named (#968).
//
// This package is where it matters most. NewAgent's activation and the monitor
// loop build real DNS, DynamoDB, EC2 and FSx clients from the ambient chain, so
// a test that forgets a fake does not fail on a laptop with a profile set: it
// writes to production. `make check` runs every package under the same
// environment (test/hermetic); this makes a plain `go test ./pkg/agent` safe too.
func TestMain(m *testing.M) { hermetic.Main(m) }
