package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// GITHUB_TOKEN's scope, per workflow (#1599). A workflow with no `permissions:`
// gets whatever the org or repository default is. That default is "read" for
// spore-host today, but it is a setting, not code: flip it to the legacy
// "read and write" and every job that has no block of its own gets a token that
// can push commits, tags and releases while it runs third-party actions and
// whatever the module graph downloads. So every workflow states its token at
// the top, read-only, and each job that writes grants that write itself.
//
// Four rules, each over every workflow file, discovered:
//  1. a top-level `permissions:` key exists;
//  2. it grants no write (`{}`, `read-all`, or scopes that are each read/none);
//  3. a workflow that runs on pull_request or pull_request_target grants no
//     write anywhere, since a same-repo PR branch runs with the token it names;
//  4. `id-token: write` sits only on a job that consumes an OIDC token: one
//     that assumes an AWS role, signs with cosign, attests provenance, or
//     calls a local reusable workflow some job of which holds it.

type permWorkflow struct {
	path string
	root *yaml.Node // the document's top-level mapping
}

func workflowFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, err := filepath.Glob(filepath.Join(".github", "workflows", pat))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files under .github/workflows: the discovery is broken, not the repo clean")
	}
	sort.Strings(files)
	return files
}

func readPermWorkflow(t *testing.T, path string) permWorkflow {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the repo's own workflows
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		t.Fatalf("%s: not a YAML mapping", path)
	}
	return permWorkflow{path: path, root: doc.Content[0]}
}

// mapValue is the value of key in mapping m, or nil when the key is absent.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// grants is a permissions value as scope -> level. The shorthands read-all and
// write-all become a "*" scope.
func grants(n *yaml.Node) map[string]string {
	out := map[string]string{}
	switch {
	case n == nil:
	case n.Kind == yaml.ScalarNode:
		switch n.Value {
		case "read-all":
			out["*"] = "read"
		case "write-all":
			out["*"] = "write"
		default:
			out["*"] = n.Value
		}
	case n.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out[n.Content[i].Value] = n.Content[i+1].Value
		}
	}
	return out
}

func writeScopes(g map[string]string) []string {
	var w []string
	for scope, level := range g {
		if level != "read" && level != "none" {
			w = append(w, scope+": "+level)
		}
	}
	sort.Strings(w)
	return w
}

// triggers is the set of events in a workflow's `on:` (a scalar, a list or a
// mapping).
func triggers(on *yaml.Node) map[string]bool {
	out := map[string]bool{}
	switch {
	case on == nil:
	case on.Kind == yaml.ScalarNode:
		out[on.Value] = true
	case on.Kind == yaml.SequenceNode:
		for _, c := range on.Content {
			out[c.Value] = true
		}
	case on.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			out[on.Content[i].Value] = true
		}
	}
	return out
}

type permJob struct {
	id    string
	node  *yaml.Node
	perms map[string]string
}

func (w permWorkflow) jobs(t *testing.T) []permJob {
	t.Helper()
	jobs := mapValue(w.root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode || len(jobs.Content) == 0 {
		t.Fatalf("%s: no jobs", w.path)
	}
	var out []permJob
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		j := jobs.Content[i+1]
		out = append(out, permJob{id: jobs.Content[i].Value, node: j, perms: grants(mapValue(j, "permissions"))})
	}
	return out
}

// The actions that exchange the job's OIDC token for something: AWS
// credentials (only with role-to-assume), a Fulcio certificate, an attestation.
var oidcConsumers = regexp.MustCompile(`^(aws-actions/configure-aws-credentials|sigstore/cosign-installer|actions/attest-build-provenance|actions/attest)@`)

func (j permJob) consumesOIDC() bool {
	steps := mapValue(j.node, "steps")
	if steps == nil {
		return false
	}
	for _, s := range steps.Content {
		uses := mapValue(s, "uses")
		if uses == nil || !oidcConsumers.MatchString(uses.Value) {
			continue
		}
		if strings.HasPrefix(uses.Value, "aws-actions/configure-aws-credentials@") && mapValue(mapValue(s, "with"), "role-to-assume") == nil {
			continue // static keys, no OIDC
		}
		return true
	}
	return false
}

func TestEveryWorkflowHasAReadOnlyTopLevelPermissionsBlock(t *testing.T) {
	for _, f := range workflowFiles(t) {
		w := readPermWorkflow(t, f)
		top := mapValue(w.root, "permissions")
		if top == nil {
			t.Errorf("%s has no top-level `permissions:` block, so its GITHUB_TOKEN is the org/repo default; "+
				"add `permissions: contents: read` (or `{}`) at the top and grant writes on the job that needs them (#1599)", f)
			continue
		}
		if w := writeScopes(grants(top)); len(w) > 0 {
			t.Errorf("%s: the top-level permissions grant %v; keep the top read-only and grant writes per job (#1599)", f, w)
		}
	}
}

func TestPullRequestWorkflowsGrantNoWrite(t *testing.T) {
	checked := 0
	for _, f := range workflowFiles(t) {
		w := readPermWorkflow(t, f)
		on := triggers(mapValue(w.root, "on"))
		if !on["pull_request"] && !on["pull_request_target"] {
			continue
		}
		checked++
		if ws := writeScopes(grants(mapValue(w.root, "permissions"))); len(ws) > 0 {
			t.Errorf("%s runs on pull requests and grants %v at the top", f, ws)
		}
		for _, j := range w.jobs(t) {
			if ws := writeScopes(j.perms); len(ws) > 0 {
				t.Errorf("%s runs on pull requests, and job %s grants %v: a PR branch's code would run with it", f, j.id, ws)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no workflow runs on pull_request: the discovery is broken (ci.yml does)")
	}
}

func TestIDTokenWriteOnlyWhereAnOIDCTokenIsUsed(t *testing.T) {
	// Which local reusable workflows hold id-token: write in some job.
	needsOIDC := map[string]bool{}
	files := workflowFiles(t)
	for _, f := range files {
		for _, j := range readPermWorkflow(t, f).jobs(t) {
			if j.perms["id-token"] == "write" {
				needsOIDC["./"+filepath.ToSlash(f)] = true
			}
		}
	}
	holders := 0
	for _, f := range files {
		for _, j := range readPermWorkflow(t, f).jobs(t) {
			if j.perms["id-token"] != "write" && j.perms["*"] != "write" {
				continue
			}
			holders++
			if j.consumesOIDC() {
				continue
			}
			if uses := mapValue(j.node, "uses"); uses != nil && needsOIDC[uses.Value] {
				continue
			}
			t.Errorf("%s job %s holds id-token: write but neither assumes an AWS role, signs with cosign, attests, "+
				"nor calls a workflow that does; drop it (#1599)", f, j.id)
		}
	}
	if holders == 0 {
		t.Fatal("no job holds id-token: write: the discovery is broken (release.yaml's does)")
	}
}
