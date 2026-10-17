package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// #1080: RetireSpotReplacement must hand RetireReplacement THIS instance's id,
// or the self-terminate either targets nothing (refused, so the replacement is
// only stopped) or targets something else. Gated structurally: the provider holds
// a concrete *ec2.Client and Substrate does not implement
// CancelSpotInstanceRequests, so the call cannot be driven end to end here. The
// behaviour behind it is covered in pkg/spotrequest, and was proven on real AWS.
func TestRetireSpotReplacementPassesItsOwnInstanceID(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "ec2.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "RetireSpotReplacement" {
			return true
		}
		ast.Inspect(fd.Body, func(m ast.Node) bool {
			c, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "RetireReplacement" || len(c.Args) != 5 {
				return true
			}
			id, ok := c.Args[3].(*ast.SelectorExpr) // p.identity.InstanceID
			if ok && id.Sel.Name == "InstanceID" {
				if inner, ok := id.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "identity" {
					found = true
				}
			}
			return true
		})
		return false
	})
	if !found {
		t.Error("RetireSpotReplacement does not pass p.identity.InstanceID as the instance to terminate")
	}
}
