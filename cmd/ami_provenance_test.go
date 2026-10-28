package cmd

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/spore-host/spawn/pkg/aws"
)

// #1364: image provenance, from the command side.

// --tag cannot set a provenance key: spawn refuses it instead of writing it.
func TestParseCreateAMITags_RefusesProvenanceKeys(t *testing.T) {
	keys := aws.ImageProvenanceTagKeys()
	if len(keys) == 0 {
		t.Fatal("aws.ImageProvenanceTagKeys() is empty")
	}
	for _, k := range append(keys, "spawn:source-version") {
		if _, err := parseCreateAMITags([]string{k + "=x"}); err == nil {
			t.Errorf("--tag %s=x accepted; it would forge the image's provenance", k)
		}
	}
	got, err := parseCreateAMITags([]string{"stack=mpi", "spawn:gpu=true"})
	if err != nil || got["stack"] != "mpi" || got["spawn:gpu"] != "true" {
		t.Errorf("ordinary tags: %v, %v", got, err)
	}
}

// Every CreateAMIInput a command builds says what its source instance was, so
// no AMI-creating path silently records nothing. Discovered from the package's
// sources, not listed.
func TestEveryCreateAMIInputCarriesSourceTags(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "CreateAMIInput" {
				return true
			}
			found++
			has := false
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "SourceTags" {
						has = true
					}
				}
			}
			if !has {
				t.Errorf("%s: aws.CreateAMIInput without SourceTags; the image would not record its source (#1364)", f)
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("found no aws.CreateAMIInput literal in package cmd; the discovery is broken")
	}
}

func provenanceAMI() aws.AMIInfo {
	return aws.AMIInfo{
		AMIID: "ami-0abc", Name: "mpi-img", Architecture: "x86_64",
		Provenance: aws.ImageProvenance{SpawnVersion: "0.148.0", SourceVersion: "0.146.0"},
	}
}

// `spawn ami list` shows the source's spawn version in its own column, and
// "-" for an image that does not record one.
func TestAMIListShowsSourceVersion(t *testing.T) {
	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = w.Write([]byte(amiTableHeader))
	writeAMIRow(w, provenanceAMI(), "-", "-", "8GB", "1", "1h", "")
	old := aws.AMIInfo{AMIID: "ami-0old", Name: "old-img", Architecture: "x86_64"}
	writeAMIRow(w, old, "-", "-", "8GB", "1", "1h", "")
	_ = w.Flush()

	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("table:\n%s", b.String())
	}
	// Column headers with a space in them, made one field each.
	fields := func(line string) []string {
		return strings.Fields(strings.NewReplacer("AMI ID", "AMI_ID", "SRC SPAWN", "SRC_SPAWN").Replace(line))
	}
	col := func(line string, i int) string {
		if f := fields(line); i < len(f) {
			return f[i]
		}
		return ""
	}
	idx := -1
	for i, h := range fields(lines[0]) {
		if h == "SRC_SPAWN" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no SRC SPAWN column in %q", lines[0])
	}
	if got := col(lines[1], idx); got != "0.146.0" {
		t.Errorf("SRC SPAWN = %q, want the source version 0.146.0\n%s", got, b.String())
	}
	if got := col(lines[2], idx); got != "-" {
		t.Errorf("SRC SPAWN for an image without provenance = %q, want -\n%s", got, b.String())
	}
}

func TestAMIListJSONIncludesSourceVersion(t *testing.T) {
	var b bytes.Buffer
	if err := writeAMIsJSON(&b, []aws.AMIInfo{provenanceAMI()}); err != nil {
		t.Fatal(err)
	}
	var out []struct {
		SourceVersion string `json:"source_version"`
		Provenance    struct {
			SpawnVersion  string `json:"image_spawn_version"`
			SourceVersion string `json:"source_version"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(b.Bytes(), &out); err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	if len(out) != 1 || out[0].SourceVersion != "0.146.0" || out[0].Provenance.SourceVersion != "0.146.0" ||
		out[0].Provenance.SpawnVersion != "0.148.0" {
		t.Errorf("JSON = %s", b.String())
	}
}
