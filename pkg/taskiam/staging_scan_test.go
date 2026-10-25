package taskiam

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// s3ARNInLiteral finds S3 ARNs (or ARN patterns) inside a string literal.
var s3ARNInLiteral = regexp.MustCompile(`arn:aws:s3:::[^"'\s,\]}]*`)

// fmtVerb is a Printf verb standing in for a caller-supplied value.
var fmtVerb = regexp.MustCompile(`%(\[\d+\])?[sqvd]`)

// TestNoGeneratedPolicyReachesTheStagingBucket is the discovery half of the
// #1274 trust model: no S3 ARN pattern anywhere in spawn's non-test source can
// match the staging bucket or an object in it, other than the task grant's
// own, built from the "arn:aws:s3:::" + bucket concatenation in
// StagedFilesResources. A pattern like spawn-* or spawn-s*-* would hand some
// role read or list on every task's staged wrapper. Values interpolated with a
// Printf verb or concatenation come from callers, and the spec and flag checks
// refuse a staging bucket there (taskproto.Validate, cmd's --s3-read/--s3-write
// checks). Found by walking the source, not listed.
func TestNoGeneratedPolicyReachesTheStagingBucket(t *testing.T) {
	root := filepath.Join("..", "..")
	targets := []string{
		"arn:aws:s3:::spawn-staging-123456789012-us-east-1",
		"arn:aws:s3:::spawn-staging-123456789012-us-east-1/t1/bootstrap-" + strings.Repeat("a", 64) + ".sh",
	}
	fset := token.NewFileSet()
	var arns int
	err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, arn := range s3ARNInLiteral.FindAllString(s, -1) {
				arns++
				pattern := fmtVerb.ReplaceAllString(arn, "caller-value")
				for _, target := range targets {
					if arnGlob(pattern, target) {
						t.Errorf("%s: S3 ARN pattern %q reaches the staging bucket (%s)", fset.Position(lit.Pos()), arn, target)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if arns < 10 {
		t.Fatalf("found %d S3 ARNs in the source; the walk has broken", arns)
	}
}

// arnGlob matches an IAM ARN pattern: * is any run of characters, slashes
// included, and ? any one character.
func arnGlob(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if arnGlob(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && arnGlob(pattern[1:], s[1:])
	}
	return s != "" && s[0] == pattern[0] && arnGlob(pattern[1:], s[1:])
}
