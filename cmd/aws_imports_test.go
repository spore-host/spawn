package cmd

import (
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestNoNewAWSSDKImportsInCmd is a regression guard for the 2026-07-11 audit
// (#326/#327): the cmd package should be a thin CLI layer that delegates AWS
// work to pkg/* stores and clients, not a place that reaches for the AWS SDK
// directly. It can't be "zero SDK imports" today — several commands still
// legitimately build a store's *dynamodb.Client, resolve STS caller identity,
// or drive EC2/S3 for launch/queue paths — so instead this locks the CURRENT
// surface: each cmd file may import only the aws-sdk-go-v2/service/* packages
// listed for it below. A NEW direct service import (or an existing file reaching
// for a new service) fails here, prompting the author to route it through a
// pkg/ package instead — or, if truly warranted, to justify it by extending
// this allowlist in the same PR.
//
// The allowlist shrinks over time as more logic moves into pkg/*; it must never
// silently grow. Trailing service names are the last path segment of
// "github.com/aws/aws-sdk-go-v2/service/<name>".
func TestNoNewAWSSDKImportsInCmd(t *testing.T) {
	// file -> allowed aws-sdk-go-v2/service/* packages (the 2026-07 baseline).
	allow := map[string][]string{
		"alerts.go":              {"dynamodb", "sts"},
		"availability.go":        {"dynamodb"},
		"autoscale_helpers.go":   {"cloudwatch", "dynamodb", "ec2", "lambda", "sqs"},
		"autoscale_status.go":    {"dynamodb", "ec2"},
		"autoscale_lifecycle.go": {"ec2"},
		"bot.go":                 {"dynamodb", "iam", "sts"},
		"burst.go":               {"ec2"},
		"cancel.go":              {"sts"},
		"collect.go":             {"dynamodb", "s3"},
		"completion.go":          {"ec2"},
		"cost.go":                {"dynamodb"},
		"extend.go":              {"sts"},
		"fsx.go":                 {"fsx"},
		"launch_batchqueue.go":   {"sts"},
		"launch_sweep.go":        {"sts"},
		"list-sweeps.go":         {"dynamodb", "sts"},
		"pipeline.go":            {"dynamodb", "lambda", "s3", "sts"},
		"plugin.go":              {"ec2"},
		"queue.go":               {"ec2", "s3"},
		"schedule.go":            {"sts"},
		"team.go":                {"dynamodb", "sts"},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd dir: %v", err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		allowed := make(map[string]bool)
		for _, svc := range allow[name] {
			allowed[svc] = true
		}

		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			const pfx = "github.com/aws/aws-sdk-go-v2/service/"
			if !strings.HasPrefix(path, pfx) {
				continue
			}
			svc := strings.SplitN(strings.TrimPrefix(path, pfx), "/", 2)[0]
			if !allowed[svc] {
				t.Errorf("%s imports aws-sdk-go-v2/service/%s, which is not in the cmd/ allowlist.\n"+
					"cmd/ should delegate AWS work to a pkg/ store/client instead of calling the SDK directly (#326/#327).\n"+
					"If this import is genuinely warranted, add %q to the allowlist for %q in aws_imports_test.go in this PR.",
					name, svc, svc, name)
			}
		}
	}

	// Also flag allowlist entries for files that no longer exist or no longer
	// import the service, so the list shrinks with the code instead of rotting.
	for file, svcs := range allow {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			t.Errorf("allowlist references %q, which no longer exists; remove its entry from aws_imports_test.go", file)
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		imported := make(map[string]bool)
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			const pfx = "github.com/aws/aws-sdk-go-v2/service/"
			if strings.HasPrefix(path, pfx) {
				imported[strings.SplitN(strings.TrimPrefix(path, pfx), "/", 2)[0]] = true
			}
		}
		var stale []string
		for _, svc := range svcs {
			if !imported[svc] {
				stale = append(stale, svc)
			}
		}
		if len(stale) > 0 {
			sort.Strings(stale)
			t.Errorf("%s no longer imports service(s) %v — tighten the allowlist in aws_imports_test.go by removing them",
				file, stale)
		}
	}
}
