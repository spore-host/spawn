package s3transfer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

const (
	tmPath      = "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	managerPath = "github.com/aws/aws-sdk-go-v2/feature/s3/" + "manager"
)

// TestTransferClientsAreBuiltByNew keeps #932's behaviour in one place. A
// transfermanager client built with its own New gets transfermanager's
// defaults (8 MiB parts, a 16 MiB multipart threshold, part-number downloads),
// not the ones spawn's callers were written against. So outside this package
// no Go file in the repository, nested modules included, may call
// transfermanager.New, and none may import the deprecated manager package
// this replaced.
//
// It parses every file and resolves the import's local name, so an import
// alias cannot hide a call and a comment naming either package cannot trip it.
func TestTransferClientsAreBuiltByNew(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}

	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		var tmName string
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch p {
			case managerPath:
				t.Errorf("%s imports the deprecated %s; use pkg/s3transfer (#932)", rel, managerPath)
			case tmPath:
				tmName = "transfermanager"
				if imp.Name != nil {
					tmName = imp.Name.Name
				}
			}
		}
		if tmName == "" || filepath.Dir(path) == self {
			return nil
		}
		full, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(full, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "New" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == tmName {
				t.Errorf("%s calls %s.New; build the client with s3transfer.New so it keeps "+
					"the part size, threshold and download mode callers rely on (#932)", rel, tmName)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("walked only %d Go files under %s; discovery is broken", files, root)
	}
}
