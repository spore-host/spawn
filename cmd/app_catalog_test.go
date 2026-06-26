package cmd

import (
	"testing"

	"github.com/spore-host/libs/catalog"
)

// TestCatalogValid runs the libs catalog structural gate from spawn's side too,
// so a stale vendored catalog (a #389-class defect) fails spawn's own CI rather
// than only libs'. Cheap, offline.
func TestCatalogValid(t *testing.T) {
	for _, err := range catalog.Validate() {
		t.Errorf("catalog invalid: %v", err)
	}
}
