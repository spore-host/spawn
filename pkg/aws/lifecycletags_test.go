package aws

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// LifecycleTags must always carry the two tags the reaper's sweep depends on.
// spawn:managed is the rail its delete grants are conditioned on; spawn:created
// is what makes the resource ageable. Without the second, a resource is
// permanently uncollectable — the creating path cannot delete it (#752) and the
// reaper will not guess its age.
func TestLifecycleTagsAlwaysCarriesManagedAndCreated(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, purpose string }{
		{"spawn-mpi-x", "mpi-cluster"},
		{"", "mpi"},  // placement groups pass no Name
		{"sg-x", ""}, // some sites set no purpose
		{"", ""},     // neither
	} {
		got := map[string]string{}
		for _, tag := range LifecycleTags(tc.name, tc.purpose, now) {
			got[*tag.Key] = *tag.Value
		}
		if got["spawn:managed"] != "true" {
			t.Errorf("LifecycleTags(%q,%q) has spawn:managed=%q, want true",
				tc.name, tc.purpose, got["spawn:managed"])
		}
		stamp, ok := got[CreatedTagKey]
		if !ok {
			t.Fatalf("LifecycleTags(%q,%q) has no %s — the resource would be uncollectable",
				tc.name, tc.purpose, CreatedTagKey)
		}
		if _, err := time.Parse(time.RFC3339, stamp); err != nil {
			t.Errorf("%s = %q is not RFC3339, so the reaper cannot parse an age: %v",
				CreatedTagKey, stamp, err)
		}
		// Optional tags appear only when supplied; an empty Name is rejected by
		// some AWS APIs and an empty purpose is just noise.
		if tc.name == "" && got["Name"] != "" {
			t.Errorf("wrote an empty Name tag")
		}
		if tc.purpose == "" && got["spawn:purpose"] != "" {
			t.Errorf("wrote an empty spawn:purpose tag")
		}
	}
}

// Every CreateSecurityGroup / CreatePlacementGroup call must tag through
// LifecycleTags. Hand-rolled tag slices are how the creation stamp came to be
// missing from all six sites, and how two of them ended up with no
// spawn:managed tag at all — making them invisible to the reaper even once it
// could age them.
func TestEveryNetResourceCreationIsTagged(t *testing.T) {
	roots := []string{"..", "../../lambda"}
	// Matches a RAW SDK call — the input literal is what distinguishes it. An
	// earlier version matched any `CreateXGroup(ctx` and so flagged
	// pkg/mpicohort/adapter.go's interface SIGNATURE and its delegating call,
	// both of which reach the tagged implementation in this package. A gate that
	// cries wolf on indirection gets disabled.
	creates := regexp.MustCompile(`Create(SecurityGroup|PlacementGroup)\(ctx, &ec2\.Create(SecurityGroup|PlacementGroup)Input\{`)

	checked := 0
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			lines := strings.Split(string(b), "\n")
			for i, line := range lines {
				if !creates.MatchString(line) {
					continue
				}
				// The input literal follows; scan its tag specification.
				window := strings.Join(lines[i:min(i+24, len(lines))], "\n")
				if !strings.Contains(window, "TagSpecifications") {
					t.Errorf("%s:%d creates a network resource with NO TagSpecifications — "+
						"it would be invisible to both the reaper and `spawn orphans`", path, i+1)
					continue
				}
				checked++
				if !strings.Contains(window, "LifecycleTags(") {
					t.Errorf("%s:%d tags a network resource without LifecycleTags — "+
						"it will be missing %s and so permanently uncollectable",
						path, i+1, CreatedTagKey)
				}
			}
			return nil
		})
	}
	if checked == 0 {
		t.Fatal("found no network-resource creation sites — the scan is broken, not the repo")
	}
	t.Logf("checked %d creation site(s)", checked)
}
