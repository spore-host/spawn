package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The cross-repository tokens spawn's workflows hold (#1602). GitHub shows no
// secret's type or scope to a workflow, so infra/github-tokens/tokens.json
// says what each one must be, and this test holds the file and the workflows
// to it. The owner compares the live tokens with the file (its README).
//
// What it fails on:
//   - a workflow reads a secret, other than GITHUB_TOKEN, that is not listed;
//   - a listed token is a classic PAT, can reach spore-host/spawn, holds a
//     permission beyond contents/pull-requests write and metadata read, or
//     reaches more than one repository;
//   - a token is read anywhere but the step env or with: of the steps its
//     used_by lists (not a job or workflow env:, which every step, third-party
//     actions included, would see), or a listed step no longer reads it;
//   - a workflow that reads one runs on a pull request;
//   - an App token is minted without owner: spore-host, an explicit
//     repositories: list equal to the key's, or explicit permission-* inputs
//     within the key's permissions.

const tokenInventory = "infra/github-tokens/tokens.json"

type ghToken struct {
	Secret        string            `json:"secret"`
	Kind          string            `json:"kind"`
	ResourceOwner string            `json:"resource_owner"`
	Repositories  []string          `json:"repositories"`
	Permissions   map[string]string `json:"permissions"`
	UsedBy        []string          `json:"used_by"`
	Why           string            `json:"why"`
	ReplaceWith   string            `json:"replace_with"`
}

func readTokenInventory(t *testing.T) []ghToken {
	t.Helper()
	b, err := os.ReadFile(tokenInventory)
	if err != nil {
		t.Fatal(err)
	}
	var inv struct {
		About  string    `json:"_about"`
		Tokens []ghToken `json:"tokens"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&inv); err != nil {
		t.Fatalf("%s: %v", tokenInventory, err)
	}
	if len(inv.Tokens) == 0 {
		t.Fatalf("%s lists no tokens", tokenInventory)
	}
	return inv.Tokens
}

// The permissions a cross-repository token may hold: pushing a commit or
// opening a PR in its one target, and the metadata read GitHub adds to every
// fine-grained token. Never workflows, actions, administration or secrets.
var allowedTokenPerms = map[string]map[string]bool{
	"contents":      {"read": true, "write": true},
	"pull-requests": {"read": true, "write": true},
	"metadata":      {"read": true},
}

func TestTokenInventoryIsLeastPrivilege(t *testing.T) {
	seen := map[string]bool{}
	for _, tok := range readTokenInventory(t) {
		if seen[tok.Secret] {
			t.Errorf("%s listed twice", tok.Secret)
		}
		seen[tok.Secret] = true
		if tok.Kind != "fine-grained-pat" && tok.Kind != "github-app" {
			t.Errorf("%s: kind %q; a classic PAT's repo scope reaches every repository its owner can write, spawn included", tok.Secret, tok.Kind)
		}
		if tok.ResourceOwner != "spore-host" {
			t.Errorf("%s: resource_owner %q, want spore-host", tok.Secret, tok.ResourceOwner)
		}
		// A PAT reaches its repositories itself, so it gets one. An App key
		// reaches its installation; what each run gets is the token minted
		// from it, which TestAppTokensAreMintedNoWiderThanTheInventory holds
		// to this list. GoReleaser needs the tap and the bucket in one
		// process, so two single-repository tokens would isolate nothing.
		if len(tok.Repositories) == 0 || (tok.Kind != "github-app" && len(tok.Repositories) != 1) {
			t.Errorf("%s: repositories %v; scope each token to its one target", tok.Secret, tok.Repositories)
		}
		for _, r := range tok.Repositories {
			if strings.EqualFold(r, "spore-host/spawn") {
				t.Errorf("%s can reach spore-host/spawn: a token that writes spawn can push a v* tag and so reach the release role's kms:Sign", tok.Secret)
			}
		}
		for p, level := range tok.Permissions {
			if !allowedTokenPerms[p][level] {
				t.Errorf("%s: permission %s: %s is not one a cross-repository token here may hold", tok.Secret, p, level)
			}
		}
		if len(tok.UsedBy) == 0 || strings.TrimSpace(tok.Why) == "" || strings.TrimSpace(tok.ReplaceWith) == "" {
			t.Errorf("%s: used_by, why and replace_with are required", tok.Secret)
		}
	}
}

var secretRef = regexp.MustCompile(`\$\{\{\s*secrets\.([A-Za-z0-9_]+)\s*\}\}`)

type tokStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	If   string            `yaml:"if"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type tokJob struct {
	Env     map[string]string `yaml:"env"`
	With    map[string]any    `yaml:"with"`
	Secrets any               `yaml:"secrets"`
	Steps   []tokStep         `yaml:"steps"`
}

type tokWorkflow struct {
	On   yaml.Node         `yaml:"on"`
	Env  map[string]string `yaml:"env"`
	Jobs map[string]tokJob `yaml:"jobs"`
}

func secretsIn(vals ...map[string]string) []string {
	var out []string
	for _, m := range vals {
		for _, v := range m {
			for _, s := range secretRef.FindAllStringSubmatch(v, -1) {
				if s[1] != "GITHUB_TOKEN" {
					out = append(out, s[1])
				}
			}
		}
	}
	return out
}

func TestWorkflowsReadTokensOnlyWhereTheInventorySays(t *testing.T) {
	inv := map[string]ghToken{}
	for _, tok := range readTokenInventory(t) {
		inv[tok.Secret] = tok
	}
	files, err := filepath.Glob(".github/workflows/*.y*ml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	sort.Strings(files)
	found := map[string]bool{} // "secret|file:job:step" actually read
	all := 0
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // the repo's own workflows
		if err != nil {
			t.Fatal(err)
		}
		// Every reference in the file, wherever it is, so one the structured
		// walk below does not reach is still counted.
		raw := map[string]int{}
		for _, m := range secretRef.FindAllStringSubmatch(string(b), -1) {
			if m[1] != "GITHUB_TOKEN" {
				raw[m[1]]++
				all++
			}
		}
		var w tokWorkflow
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		base := filepath.Base(f)
		for _, s := range secretsIn(w.Env) {
			t.Errorf("%s: secrets.%s in the workflow env, where every step of every job sees it", f, s)
		}
		walked := map[string]int{}
		for id, j := range w.Jobs {
			for _, s := range secretsIn(j.Env) {
				t.Errorf("%s job %s: secrets.%s in the job env, where every step sees it; move it to the step that uses it", f, id, s)
			}
			if j.Secrets != nil {
				t.Errorf("%s job %s passes secrets to a reusable workflow; list what it passes in %s and extend this test", f, id, tokenInventory)
			}
			for _, st := range j.Steps {
				for _, s := range secretsIn(st.Env, st.With) {
					walked[s]++
					where := base + ":" + id + ":" + st.Name
					found[s+"|"+where] = true
					tok, ok := inv[s]
					if !ok {
						t.Errorf("%s: %s reads secrets.%s, which is not in %s: add its kind, target repository and permissions there (#1602)", f, where, s, tokenInventory)
						continue
					}
					listed := false
					for _, u := range tok.UsedBy {
						listed = listed || u == where
					}
					if !listed {
						t.Errorf("%s: %s reads secrets.%s, but %s lists it only for %v", f, where, s, tokenInventory, tok.UsedBy)
					}
				}
			}
		}
		for s, n := range raw {
			if walked[s] != n {
				t.Errorf("%s: secrets.%s appears %d times, %d of them in a step env or with:; the rest are somewhere this test cannot vouch for", f, s, n, walked[s])
			}
		}
		if len(raw) > 0 {
			for i := 0; i < len(w.On.Content); i += 2 {
				if strings.HasPrefix(w.On.Content[i].Value, "pull_request") {
					t.Errorf("%s reads cross-repository tokens and runs on %s", f, w.On.Content[i].Value)
				}
			}
			if w.On.Kind != yaml.MappingNode {
				t.Errorf("%s reads cross-repository tokens; write its on: as a mapping so this test can read the triggers", f)
			}
		}
	}
	if all == 0 {
		t.Fatal("no workflow reads a secret other than GITHUB_TOKEN: the discovery is broken (release.yaml reads two)")
	}
	for _, tok := range inv {
		for _, u := range tok.UsedBy {
			if !found[tok.Secret+"|"+u] {
				t.Errorf("%s: %s lists %s, which does not read it (renamed step, or a token no longer used?)", tok.Secret, tokenInventory, u)
			}
		}
	}
}

// An App key reaches its whole installation, so the token each step mints
// from it is what bounds a run: owner and repositories must be explicit (left
// unset, create-github-app-token mints for the current repository, which is
// spawn), equal to the inventory's, and the permissions explicit and within
// it (left unset, the token gets everything the installation has).
func TestAppTokensAreMintedNoWiderThanTheInventory(t *testing.T) {
	inv := map[string]ghToken{}
	for _, tok := range readTokenInventory(t) {
		inv[tok.Secret] = tok
	}
	files, err := filepath.Glob(".github/workflows/*.y*ml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	minted := 0
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // the repo's own workflows
		if err != nil {
			t.Fatal(err)
		}
		var w tokWorkflow
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for id, j := range w.Jobs {
			for _, st := range j.Steps {
				if !strings.HasPrefix(st.Uses, "actions/create-github-app-token@") {
					continue
				}
				minted++
				where := filepath.Base(f) + ":" + id + ":" + st.Name
				keys := secretsIn(map[string]string{"private-key": st.With["private-key"]})
				if len(keys) != 1 {
					t.Errorf("%s: private-key is not one secrets.X reference", where)
					continue
				}
				tok, ok := inv[keys[0]]
				if !ok || tok.Kind != "github-app" {
					t.Errorf("%s: secrets.%s is not a github-app key in %s", where, keys[0], tokenInventory)
					continue
				}
				if st.With["owner"] != "spore-host" {
					t.Errorf("%s: owner %q, want spore-host", where, st.With["owner"])
				}
				var got []string
				for _, r := range strings.FieldsFunc(st.With["repositories"], func(c rune) bool { return c == ',' || c == '\n' || c == ' ' }) {
					got = append(got, "spore-host/"+r)
				}
				want := append([]string(nil), tok.Repositories...)
				sort.Strings(got)
				sort.Strings(want)
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s: mints for %v, want exactly %v", where, got, want)
				}
				perms := 0
				for k, level := range st.With {
					p, isPerm := strings.CutPrefix(k, "permission-")
					if !isPerm {
						continue
					}
					perms++
					if tok.Permissions[p] != level && !(level == "read" && tok.Permissions[p] == "write") {
						t.Errorf("%s: %s: %s is beyond the key's %v", where, k, level, tok.Permissions)
					}
				}
				if perms == 0 {
					t.Errorf("%s: no permission-* input, so the token gets every permission the installation has", where)
				}
			}
		}
	}
	if minted == 0 {
		t.Fatal("no workflow mints an App token: the discovery is broken (release.yaml mints one)")
	}
}
