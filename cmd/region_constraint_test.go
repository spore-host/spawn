package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/regions"
	"github.com/spore-host/spawn/pkg/sweep"
)

// The conversion must carry every field, and must keep doing so when a field is
// added (#811).
//
// Discovered by reflection rather than asserted field by field (rule 4): a test
// listing the five fields it knew about would pass silently when a sixth was
// added to sweep.RegionConstraint and left unmapped, which is a constraint the
// user set being dropped on the floor between validation and filtering.
func TestRegionConstraintConversionCarriesEveryField(t *testing.T) {
	// Every field populated distinctly, so a mis-mapped pair (Include↔Exclude,
	// say) is visible rather than averaging out.
	src := &sweep.RegionConstraint{
		Include:       []string{"us-east-1", "us-west-*"},
		Exclude:       []string{"eu-*"},
		Geographic:    []string{"us"},
		ProximityFrom: "us-east-2",
		CostTier:      "low",
	}

	got := regionConstraintFromSweep(src)
	if got == nil {
		t.Fatal("conversion returned nil for a populated constraint")
	}

	// Compared through their JSON forms rather than field by field.
	//
	// The two structs declare the SAME five json tags — include, exclude,
	// geographic, proximity_from, cost_tier — because they were written as copies
	// of each other, which is the duplication this change removes. That makes the
	// serialized form a faithful comparison of content, and it catches all three
	// failure modes at once: a dropped field (missing key), a swapped pair
	// (mismatched value), and AllowEmpty flipped true (an extra allow_empty key).
	//
	// An earlier version walked the fields with reflect.FieldByName. It worked,
	// but semgrep's unsafe-reflect-by-name rule blocks it — correctly as a general
	// matter, even though the name came from the struct's own compile-time field
	// list here. Reaching for `nosemgrep` would have been the easy move; this is
	// simpler than the version it replaces, so the rule did it a favour.
	var sm, dm map[string]any
	sb, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	db, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(sb, &sm); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(db, &dm); err != nil {
		t.Fatal(err)
	}
	if len(sm) == 0 {
		t.Fatal("the source constraint serialized to nothing — the fixture or the tags have " +
			"changed, and an empty comparison would pass vacuously")
	}
	if !reflect.DeepEqual(sm, dm) {
		t.Errorf("conversion is not faithful:\n  sweep:   %v\n  regions: %v\n"+
			"  A field the user set is validated and persisted but never reaches the filter.", sm, dm)
	}

	// The field COUNT, which the JSON comparison alone would not catch: a field
	// added to sweep.RegionConstraint with `json:"-"` would be invisible above.
	// regions.RegionConstraint carries exactly one field the sweep type does not —
	// AllowEmpty. If that stops being true, this conversion needs revisiting
	// whether or not the new field happens to serialize.
	sn := reflect.TypeOf(sweep.RegionConstraint{}).NumField()
	dn := reflect.TypeOf(regions.RegionConstraint{}).NumField()
	if sn == 0 || dn == 0 {
		t.Fatal("a constraint type reported zero fields — reflection has broken, not the struct")
	}
	if dn != sn+1 {
		t.Errorf("sweep.RegionConstraint has %d fields and regions.RegionConstraint has %d; "+
			"expected exactly one more (AllowEmpty). A field was added to one and not the "+
			"other, so regionConstraintFromSweep is no longer complete.", sn, dn)
	}

	// AllowEmpty must stay false: cmd's own implementation always errored on an
	// empty result, and an empty region set means the sweep has nowhere to launch.
	if got.AllowEmpty {
		t.Error("AllowEmpty is true, so a constraint matching no regions at all would be " +
			"accepted and the sweep would launch nowhere without an error")
	}

	if regionConstraintFromSweep(nil) != nil {
		t.Error("a nil constraint converted to non-nil")
	}
}

// The tested implementation must be the one in use (#811).
//
// This is the invariant that actually broke. pkg/regions held the filtering
// logic and 393 lines of tests for it; cmd/ held a byte-identical copy with no
// tests, and the copy was what ran. Nothing was wrong with either — the fault
// was that the tested one had no caller, so a dead-code pass identified it as
// the thing to delete.
//
// Anchored on the INVOCATION, not the bare name (rule 3): this very comment
// names regions.ApplyConstraints, and a substring search would be satisfied by
// the sentence documenting the gate.
func TestRegionFilteringUsesThePackageWithTheTests(t *testing.T) {
	root := repoRootFromCmd(t)
	cmdDir := filepath.Join(root, "cmd")

	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		t.Fatalf("read cmd/: %v", err)
	}
	var src strings.Builder
	files := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(cmdDir, n)) //nolint:gosec // the repo's own source
		if rerr != nil {
			t.Fatalf("read %s: %v", n, rerr)
		}
		src.Write(b)
		files++
	}
	if files == 0 {
		t.Fatal("found no non-test .go files in cmd/ — the scan has broken rather than the " +
			"package being empty")
	}
	body := src.String()

	for _, call := range []*regexp.Regexp{
		regexp.MustCompile(`regions\.ApplyConstraints\(`),
		regexp.MustCompile(`regions\.ValidateConstraint\(`),
	} {
		if !call.MatchString(body) {
			t.Errorf("no non-test file in cmd/ invokes %s. Region filtering has 393 lines of "+
				"tests in pkg/regions and none anywhere else, so if cmd/ is filtering regions "+
				"some other way, the logic choosing which AWS regions a launch targets is "+
				"running untested — which is exactly how this was found.", call)
		}
	}
}
