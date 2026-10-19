package progress

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/libs/i18n"
)

type monitoredConfig struct{ ttl, idle string }

func (c monitoredConfig) GetTTL() string         { return c.ttl }
func (c monitoredConfig) GetIdleTimeout() string { return c.idle }

// TestSuccessSummaryRulesAreBalanced is #1125's gate. The launch summary is a
// title box followed by sections, each closed by a rule. The output ended on
// an unclosed section (the IAM profile line after the last rule) and showed
// two rules back to back when the monitoring section was absent. For every
// combination of optional parts: the summary ends on a rule, no two rules are
// adjacent (blank lines aside), and nothing follows the last rule.
func TestSuccessSummaryRulesAreBalanced(t *testing.T) {
	if err := i18n.Init(i18n.Config{Language: "en"}); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"", "spored-instance-profile"} {
		for _, dnsRecord := range []string{"", "demo.abc123.spore.host"} {
			for _, cfg := range []interface{}{nil, monitoredConfig{ttl: "4h"}, monitoredConfig{ttl: "4h", idle: "30m"}} {
				s := SuccessSummary{
					InstanceID: "i-0123", PublicIP: "1.2.3.4", ConnectCommand: "spawn connect demo",
					InstanceProfile: profile, DNSRecord: dnsRecord,
				}
				if dnsRecord != "" {
					s.DNSConnect = "ssh me@" + dnsRecord
				}
				var buf bytes.Buffer
				renderSuccess(&buf, s, cfg)
				out := buf.String()
				name := "profile=" + profile + " dns=" + dnsRecord
				if cfg != nil {
					name += " monitored"
				}

				var nonBlank []string
				for _, l := range strings.Split(out, "\n") {
					if strings.TrimSpace(l) != "" {
						nonBlank = append(nonBlank, l)
					}
				}
				rules := 0
				for i, l := range nonBlank {
					if l != sectionRule {
						continue
					}
					rules++
					if i > 0 && nonBlank[i-1] == sectionRule {
						t.Errorf("%s: two rules back to back at line %d:\n%s", name, i, out)
					}
				}
				if rules < 2 {
					t.Errorf("%s: %d rules, want one closing each section:\n%s", name, rules, out)
				}
				if last := nonBlank[len(nonBlank)-1]; last != sectionRule {
					t.Errorf("%s: summary ends on %q, an unclosed section:\n%s", name, last, out)
				}
				if profile != "" && !strings.Contains(out, "IAM instance profile: "+profile) {
					t.Errorf("%s: the resolved instance profile is missing (#550):\n%s", name, out)
				}
				if dnsRecord != "" && !strings.Contains(out, dnsRecord) {
					t.Errorf("%s: the DNS record is missing:\n%s", name, out)
				}
			}
		}
	}
}

// TestEveryPreregisteredStepIsStarted: a step defaultProgress pre-registers
// and nothing moves renders as pending forever (#1125's "Installing spawn
// agent" hourglass). Each pre-registered name must be the first argument of a
// .Start or .Skip call (the two ways a step leaves pending) in package cmd, as
// a literal or as i18n.T of a key that renders to it.
func TestEveryPreregisteredStepIsStarted(t *testing.T) {
	if err := i18n.Init(i18n.Config{Language: "en"}); err != nil {
		t.Fatal(err)
	}
	started := map[string]bool{}
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("found no cmd sources (%v)", err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Start" && sel.Sel.Name != "Skip") {
				return true
			}
			switch a := call.Args[0].(type) {
			case *ast.BasicLit:
				if v, err := strconv.Unquote(a.Value); err == nil {
					started[v] = true
				}
			case *ast.CallExpr:
				if s, ok := a.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "T" && len(a.Args) == 1 {
					if lit, ok := a.Args[0].(*ast.BasicLit); ok {
						if k, err := strconv.Unquote(lit.Value); err == nil {
							started[i18n.T(k)] = true
						}
					}
				}
			}
			return true
		})
	}
	steps := defaultProgress().steps
	if len(steps) == 0 {
		t.Fatal("defaultProgress registers no steps")
	}
	for _, st := range steps {
		if !started[st.Name] {
			t.Errorf("pre-registered step %q is never started or skipped by package cmd, so it shows as pending forever", st.Name)
		}
	}
}
