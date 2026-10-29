package cmd

import (
	"bytes"
	"testing"

	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// The release workflow verifies spored's signatures against what this prints
// (#1415); it must be exactly the key the installer verifies against.
func TestSporedSigningKeyCommandPrintsTheCompiledKey(t *testing.T) {
	var out bytes.Buffer
	sporedSigningKeyCmd.SetOut(&out)
	t.Cleanup(func() { sporedSigningKeyCmd.SetOut(nil) })
	if err := sporedSigningKeyCmd.RunE(sporedSigningKeyCmd, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != sporedinstall.SigningKeyPEM+"\n" {
		t.Errorf("spawn spored-signing-key printed %q, want sporedinstall.SigningKeyPEM", out.String())
	}
	if !sporedSigningKeyCmd.Hidden {
		t.Error("spored-signing-key is release tooling and should stay hidden")
	}
}
