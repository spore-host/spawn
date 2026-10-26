package aws

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

// The spored instance role must not be able to write the tags the reaper acts on.
//
// Why this is not already covered: every existing gate on these policies asks
// whether a grant is present and whether it is tag-scoped. The defect here was a
// grant that is present, correctly tag-scoped, and still lets one spore take root
// on another — because ec2:CreateTags on Resource "*" conditioned on
// ec2:ResourceTag/spawn:managed=true permits retagging any OTHER managed instance,
// and the reaper feeds spawn:pre-stop to SSM AWS-RunShellScript as root
// (GHSA-8q3p-qcv8-2pr4).
//
// All three gates run against both policy documents, because the dynamic baseline
// and the static DCV copy have diverged on exactly this statement before.

// policyDocuments returns both spored policies, parsed. Keyed by a description
// that names the code path that writes each, so a failure says which one.
func policyDocuments(t *testing.T) map[string]map[string]interface{} {
	t.Helper()

	out := map[string]map[string]interface{}{}

	var dcv map[string]interface{}
	if err := json.Unmarshal([]byte(sporedDCVRolePolicy), &dcv); err != nil {
		t.Fatalf("sporedDCVRolePolicy is not valid JSON: %v", err)
	}
	out["static DCV/no-flags role (SetupSporedIAMRole)"] = dcv

	c := &Client{}
	raw, err := json.Marshal(c.buildInlinePolicy(nil))
	if err != nil {
		t.Fatalf("marshal buildInlinePolicy: %v", err)
	}
	var dyn map[string]interface{}
	if err := json.Unmarshal(raw, &dyn); err != nil {
		t.Fatalf("buildInlinePolicy is not valid JSON: %v", err)
	}
	out["dynamic baseline (buildInlinePolicy)"] = dyn

	if len(out) != 2 {
		t.Fatal("expected two policy documents; the gate is checking fewer than it claims")
	}
	return out
}

func statements(t *testing.T, doc map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := doc["Statement"].([]interface{})
	if !ok || len(raw) == 0 {
		t.Fatal("policy has no Statement list")
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for _, s := range raw {
		m, ok := s.(map[string]interface{})
		if !ok {
			t.Fatalf("statement is not an object: %#v", s)
		}
		out = append(out, m)
	}
	return out
}

func stringsOf(v interface{}) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Both policies must explicitly Deny tag writes to the reaper's control keys.
func TestBothSporedPoliciesDenyTheReaperControlTagKeys(t *testing.T) {
	if len(reaperControlTagKeys) == 0 {
		t.Fatal("reaperControlTagKeys is empty — the deny statements would deny nothing")
	}

	for name, doc := range policyDocuments(t) {
		t.Run(name, func(t *testing.T) {
			found := false
			for _, st := range statements(t, doc) {
				if st["Effect"] != "Deny" {
					continue
				}
				cond, _ := st["Condition"].(map[string]interface{})
				anyVal, _ := cond["ForAnyValue:StringEquals"].(map[string]interface{})
				if anyVal == nil {
					// ForAllValues on an Allow would only deny a request made up
					// entirely of control keys; appending one legitimate tag would
					// defeat it. Name the trap here so a "simplification" has to
					// read it.
					if cond["ForAllValues:StringNotEquals"] != nil {
						t.Error("the deny uses ForAllValues:StringNotEquals; on a Deny that fires only " +
							"when EVERY requested key is a control key, so adding one innocuous tag " +
							"to the request defeats it. Use ForAnyValue:StringEquals.")
					}
					continue
				}
				keys := stringsOf(anyVal["aws:TagKeys"])
				if len(keys) == 0 {
					continue
				}
				found = true

				acts := stringsOf(st["Action"])
				for _, want := range []string{"ec2:CreateTags", "ec2:DeleteTags"} {
					if !containsStr(acts, want) {
						t.Errorf("the control-tag deny does not cover %s (actions: %v). DeleteTags "+
							"matters as much as CreateTags: deleting a victim's "+
							"spawn:local-username is what used to make its hook run as root", want, acts)
					}
				}

				for _, want := range reaperControlTagKeys {
					if !containsStr(keys, want) {
						t.Errorf("%s is in reaperControlTagKeys but not denied by this policy (denied: %v)",
							want, keys)
					}
				}
				for _, got := range keys {
					if !containsStr(reaperControlTagKeys, got) {
						t.Errorf("this policy denies %q, which is not in reaperControlTagKeys — the two "+
							"policies are drifting again", got)
					}
				}
			}
			if !found {
				t.Error("no Deny statement conditioned on aws:TagKeys. Without it, ec2:CreateTags on " +
					"Resource \"*\" lets any spore rewrite another managed instance's spawn:pre-stop, " +
					"which the reaper runs as root via SSM (GHSA-8q3p-qcv8-2pr4)")
			}
		})
	}
}

// Nothing running on the instance may write a key we deny.
//
// This is the gate that caught the mistake: the obvious control-key list includes
// spawn:ttl-deadline and spawn:last-heartbeat, and spored writes BOTH — the
// heartbeat every cycle and the deadline on the `spored ttl set` path. Denying
// them would have broken idle detection and `spored ttl` while reading as a
// security improvement. Tag tampering with the deadline is contained by the
// unconditional max-age ceiling instead (#76), which is the right tool precisely
// because that key has a legitimate in-instance writer.
//
// Discovery, not a list (CLAUDE.md rule 4): it greps the in-instance source for
// every literal tag key handed to CreateTags/DeleteTags, and fails if it finds
// none at all.
func TestNoDeniedTagKeyHasAnInInstanceWriter(t *testing.T) {
	root := repoRootFromPkgAWS(t)

	// Everything that runs ON the instance under the spored role.
	sources := []string{
		filepath.Join(root, "pkg", "agent"),
		filepath.Join(root, "cmd", "spored"),
	}

	written := map[string]string{} // key -> where
	keyLit := regexp.MustCompile(`aws\.String\("(spawn:[a-z0-9-]+)"\)`)
	for _, dir := range sources {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				if treewalk.Vanished(dir, path, err) {
					return nil
				}
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // walking a fixed repo subtree
			if err != nil {
				return err
			}
			for _, m := range keyLit.FindAllStringSubmatch(string(b), -1) {
				rel, _ := filepath.Rel(root, path)
				if _, seen := written[m[1]]; !seen {
					written[m[1]] = rel
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(written) == 0 {
		t.Fatal("found no spawn: tag keys in pkg/agent or cmd/spored. Discovery has broken — it has " +
			"not proven the instance writes nothing")
	}
	// Sanity-check discovery against a key we know is there, so a regex that
	// silently stops matching cannot pass this test.
	if _, ok := written["spawn:last-heartbeat"]; !ok {
		t.Fatalf("discovery did not find spawn:last-heartbeat, which pkg/agent writes every heartbeat "+
			"cycle — the extraction is broken. Found: %v", written)
	}

	for _, denied := range reaperControlTagKeys {
		if where, conflict := written[denied]; conflict {
			t.Errorf("%s is denied to the instance role but %s writes it — the deny will break spored "+
				"at runtime, and an explicit Deny cannot be worked around", denied, where)
		}
	}
}

// The keys the reaper EXECUTES must be denied, not merely the ones it reads.
//
// A floor, not an inventory: the deny list may legitimately be longer, but if
// either of these two leaves it, the advisory's finding is reopened. They are
// named explicitly because they are the two that reach a root shell — pre-stop is
// the command, local-username is the privilege drop that stops it being root.
func TestTheExecutedTagKeysAreDenied(t *testing.T) {
	for _, must := range []string{"spawn:pre-stop", "spawn:local-username"} {
		if !containsStr(reaperControlTagKeys, must) {
			t.Errorf("%s is not denied to the instance role. The reaper passes spawn:pre-stop to SSM "+
				"AWS-RunShellScript (root) and uses spawn:local-username to drop privilege, so an "+
				"instance able to write either has root on every managed instance in the account",
				must)
		}
	}
}

// The tags the reaper uses to BOUND an instance's lifetime must be denied too
// (GHSA-c93q-4jh7-6pvj). The deadline itself is legitimately writable from the
// instance, which is acceptable only because the max-age ceiling caps it and
// the scan finds every managed instance. So whatever the ceiling measures age
// from, and whatever the scan filters on, must be out of the instance's reach.
//
// Discovery, not a list (rule 4): it reads lambda/ttl-reaper for the tag keys
// handed to launchTime() and to the EC2 instance-scan filters, and fails if it
// finds none, or misses the two it is known to use.
func TestTheLifetimeBoundingTagKeysAreDenied(t *testing.T) {
	root := repoRootFromPkgAWS(t)
	dir := filepath.Join(root, "lambda", "ttl-reaper")

	ageKey := regexp.MustCompile(`launchTime\(tags\[tagprefix\.Tag\("([a-z0-9-]+)"\)\],\s*(?:inst\.LaunchTime|apiLaunch)\)`)
	scanKey := regexp.MustCompile(`tagprefix\.FilterTag\("([a-z0-9-]+)"\)`)

	found := map[string]string{} // spawn:key -> why
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // fixed repo subtree
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ageKey.FindAllStringSubmatch(string(b), -1) {
			found["spawn:"+m[1]] = name + ": the max-age ceiling measures age from it"
		}
		for _, m := range scanKey.FindAllStringSubmatch(string(b), -1) {
			found["spawn:"+m[1]] = name + ": the instance scan filters on it"
		}
	}

	if len(found) == 0 {
		t.Fatal("found no lifetime-bounding tag keys in lambda/ttl-reaper. Discovery has broken — " +
			"it has not proven the reaper's inputs are protected")
	}
	for _, sanity := range []string{"spawn:launch-time", "spawn:managed"} {
		if _, ok := found[sanity]; !ok {
			t.Fatalf("discovery did not find %s, which the reaper is known to use — the extraction "+
				"is broken. Found: %v", sanity, found)
		}
	}
	for key, why := range found {
		if !containsStr(reaperControlTagKeys, key) {
			t.Errorf("%s is writable by the instance role, but %s. An instance that rewrites or "+
				"deletes it escapes the reaper; add it to reaperControlTagKeys", key, why)
		}
	}
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// repoRootFromPkgAWS walks up from the package directory to the module root.
func repoRootFromPkgAWS(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "cmd", "spored")); err == nil {
				return dir
			}
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repo root from pkg/aws")
	return ""
}
