package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/costmodel"
)

// costedInstance carries every component the cap counts: compute, EBS, one
// auto-assigned public IPv4 and one Elastic IP, with 2h of compute over 10h
// since launch (the rest stopped).
func costedInstance(launch time.Time, state string) *aws.InstanceInfo {
	return &aws.InstanceInfo{
		InstanceID: "i-acct", InstanceType: "m7i.large", State: state, Region: "us-east-1",
		LaunchTime: launch, PublicIP: "52.0.0.1",
		Tags: map[string]string{
			"spawn:launch-time":        launch.UTC().Format(time.RFC3339),
			"spawn:price-per-hour":     "1.000000",
			"spawn:ebs-hourly-cost":    "0.100000",
			"spawn:public-ipv4-count":  "1",
			"spawn:elastic-ipv4-count": "1",
			"spawn:compute-seconds":    "7200",
			"spawn:cost-limit":         "100",
		},
	}
}

// TestCostDisplaysAndExtendShareTheEnforcedAccounting: `spawn extend`'s
// decideCostLimit, the not-running `spawn status` and `spawn cost` all report
// the figure pkg/costmodel.Accrue gives over the enforced footprint — the
// function spored terminates on (pkg/agent accumulatedComputeCost) — so what
// is shown is what is enforced (#51). Each figure includes public IPv4.
func TestCostDisplaysAndExtendShareTheEnforcedAccounting(t *testing.T) {
	launch := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := launch.Add(10 * time.Hour)
	inst := costedInstance(launch, "stopped")
	fp := costmodel.Footprint{ComputePerHour: 1.0, EBSPerHour: 0.1, PublicIPv4: 1, ElasticIPv4: 1}
	want := costmodel.Accrue(fp, 2*time.Hour, 10*time.Hour)
	ipv4 := 0.005 * (2 + 10) // auto over compute, Elastic over the wall clock
	if math.Abs(want.PublicIPv4-ipv4) > 1e-9 {
		t.Fatalf("fixture: IPv4 accrual $%.4f, want $%.4f", want.PublicIPv4, ipv4)
	}

	t.Run("stopped status", func(t *testing.T) {
		r := buildEC2StatusReport(inst, tagStatusRate(inst), now)
		if math.Abs(r.CostLimitUsed-want.Total) > 1e-9 {
			t.Errorf("CostLimitUsed = %.4f, enforced = %.4f", r.CostLimitUsed, want.Total)
		}
		if math.Abs(r.CumulativeCost-want.Total) > 1e-9 || math.Abs(r.IPv4Cost-want.PublicIPv4) > 1e-9 {
			t.Errorf("Cumulative = %.4f (IPv4 %.4f), enforced = %.4f (IPv4 %.4f)", r.CumulativeCost, r.IPv4Cost, want.Total, want.PublicIPv4)
		}
		// While stopped: EBS + the Elastic IP keep billing, not the auto-assigned address.
		if math.Abs(r.StoppedPerHour-(0.1+0.005)) > 1e-9 {
			t.Errorf("stopped rate = %.4f, want EBS + one Elastic IP = 0.105", r.StoppedPerHour)
		}
	})

	t.Run("spawn cost", func(t *testing.T) {
		out := singleInstanceCostReport(inst, now)
		line := fmt.Sprintf("Est. total:      ~$%.2f", want.Total)
		if !strings.Contains(out, line) || !strings.Contains(out, "Public IPv4:") {
			t.Errorf("spawn cost does not show the enforced total %q with its IPv4 line:\n%s", line, out)
		}
	})

	t.Run("extend", func(t *testing.T) {
		deadline := now.Add(5 * time.Hour)
		inst.Tags["spawn:cost-limit"] = "5" // binds, so PerHour and FiresAt are set
		d := decideCostLimit(inst, deadline, now, 0, false)
		soFar := computeSoFar(inst, launch, now)
		need := costmodel.Accrue(fp, soFar+5*time.Hour, deadline.Sub(launch)).Total
		if math.Abs(d.Needed-need) > 1e-9 {
			t.Errorf("Needed = %.4f, enforced accounting = %.4f", d.Needed, need)
		}
		if math.Abs(d.PerHour-fp.RunningPerHour()) > 1e-9 {
			t.Errorf("PerHour = %.4f, want every running component %.4f", d.PerHour, fp.RunningPerHour())
		}
	})

	t.Run("billable resources", func(t *testing.T) {
		var row *billableResource
		for _, r := range billableResources(inst) {
			if r.Kind == "ipv4" {
				r := r
				row = &r
			}
		}
		if row == nil || !row.Capped {
			t.Fatalf("no capped public IPv4 row: %+v", billableResources(inst))
		}
	})
}

// TestNoCostRateTagReadOutsideTheModel discovers every Go file in the spawn
// module and fails on any that names a cost-footprint tag other than through
// pkg/costmodel (#51): a second reader is how a display and the enforcement
// drift (#959). The model owns the names; pkg/provider is the one writer and
// parser of spored's config; everything else uses costmodel.FromTags.
func TestNoCostRateTagReadOutsideTheModel(t *testing.T) {
	root := ".."
	suffixes := []string{costmodel.TagEBSHourlyCost, costmodel.TagPublicIPv4, costmodel.TagElasticIPv4}
	consts := map[string]bool{"TagEBSHourlyCost": true, "TagPublicIPv4": true, "TagElasticIPv4": true}
	fset := token.NewFileSet()
	files, decls := 0, 0
	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "vendor" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		files++
		slash := filepath.ToSlash(path)
		inModel := strings.Contains(slash, "pkg/costmodel/")
		inProvider := strings.Contains(slash, "pkg/provider/")
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				v, _ := strconv.Unquote(x.Value)
				for _, s := range suffixes {
					// The tag name as a value — a map key, a tag write — not
					// prose that mentions it (help text, a label).
					if v == s || strings.HasSuffix(v, ":"+s) && !strings.ContainsAny(v, " \n") {
						if inModel {
							decls++
						} else {
							bad = append(bad, fmt.Sprintf("%s: literal %q", fset.Position(x.Pos()), v))
						}
					}
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "costmodel" && consts[x.Sel.Name] && !inProvider {
					bad = append(bad, fmt.Sprintf("%s: costmodel.%s read outside pkg/provider", fset.Position(x.Pos()), x.Sel.Name))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 || decls == 0 {
		t.Fatalf("discovery found %d files and %d tag declarations in pkg/costmodel: the walk is broken, not the repo clean", files, decls)
	}
	for _, b := range bad {
		t.Errorf("%s — read cost-footprint tags through costmodel.FromTags (or enforcedFootprint), not directly", b)
	}
}
