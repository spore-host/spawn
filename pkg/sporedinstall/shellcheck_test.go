package sporedinstall

import (
	"testing"

	"github.com/spore-host/spawn/internal/shellcheckgate"
)

// shellcheck on the verified spored installer (#689 part C). It is a fragment
// (a key file plus a bash function), spliced into every Linux bootstrap, the
// app templates, the pipeline stage runner and `spawn upgrade-spored`.
//
// The two halves are only ever spliced together (KeySetupScript sets the
// variables VerifiedInstallFunc reads), so both are checked as the pair every
// caller emits, followed by a call the way the bootstrap makes one.
func TestInstaller_Shellcheck(t *testing.T) {
	script := "#!/bin/bash\n" + KeySetupScript() + VerifiedInstallFunc() +
		"\n" + FuncName + " spored https://example.invalid/a https://example.invalid/b\n"
	shellcheckgate.Check(t, "pkg/sporedinstall.KeySetupScript", script)
	shellcheckgate.Check(t, "pkg/sporedinstall.VerifiedInstallFunc", script)
}
