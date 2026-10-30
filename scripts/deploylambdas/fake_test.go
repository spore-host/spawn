package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFn is one function in the fake account.
type fakeFn struct {
	cfg  map[string]any
	code []byte
	tags map[string]string
	rev  int
}

type call struct {
	sub  string
	args []string
}

// lambdaFunc adapts a function to the lambdaCLI seam.
type lambdaFunc func(ctx context.Context, sub string, args ...string) ([]byte, error)

func (f lambdaFunc) Lambda(ctx context.Context, sub string, args ...string) ([]byte, error) {
	return f(ctx, sub, args...)
}

// fakeLambda is an in-memory Lambda service behind the lambdaCLI seam.
type fakeLambda struct {
	fns   map[string]*fakeFn
	calls []call
	// onUpdate runs after an update-function-code stores the zip; n counts
	// updates of that function so far (1 for the first).
	onUpdate func(fn string, f *fakeFn, n int)
	updates  map[string]int
	failTag  bool
}

const testAccount = "966362334030"

func newFakeFn(name, arch string) *fakeFn {
	f := &fakeFn{code: mustZip([]byte("live-" + name)), tags: map[string]string{versionTag: "0.150.0"}}
	f.cfg = map[string]any{
		"FunctionName":     name,
		"FunctionArn":      fmt.Sprintf("arn:aws:lambda:us-east-1:%s:function:%s", testAccount, name),
		"Runtime":          "provided.al2023",
		"Handler":          "bootstrap",
		"PackageType":      "Zip",
		"Architectures":    []any{arch},
		"MemorySize":       float64(512),
		"Timeout":          float64(900),
		"Role":             "arn:aws:iam::" + testAccount + ":role/r",
		"State":            "Active",
		"LastUpdateStatus": "Successful",
		"LastModified":     "2026-10-01T00:00:00.000+0000",
		"Environment":      map[string]any{"Variables": map[string]any{"K": "V"}},
		"RuntimeVersionConfig": map[string]any{
			"RuntimeVersionArn": "arn:aws:lambda:us-east-1::runtime:1",
		},
	}
	f.sync()
	return f
}

func (f *fakeFn) sync() {
	f.cfg["CodeSha256"] = codeSha256(f.code)
	f.cfg["CodeSize"] = float64(len(f.code))
	f.cfg["RevisionId"] = fmt.Sprintf("rev-%d", f.rev)
}

func mustZip(b []byte) []byte {
	z, err := zipBootstrap(b)
	if err != nil {
		panic(err)
	}
	return z
}

func argOf(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func (l *fakeLambda) Lambda(_ context.Context, sub string, args ...string) ([]byte, error) {
	l.calls = append(l.calls, call{sub: sub, args: append([]string(nil), args...)})
	name := argOf(args, "--function-name")
	if sub == "tag-resource" {
		arn := argOf(args, "--resource")
		name = arn[strings.LastIndex(arn, ":")+1:]
	}
	f := l.fns[name]
	if f == nil {
		return nil, fmt.Errorf("ResourceNotFoundException: %s", name)
	}
	switch sub {
	case "get-function-configuration":
		return json.Marshal(f.cfg)
	case "get-function":
		return json.Marshal(map[string]any{
			"Configuration": f.cfg,
			"Code":          map[string]any{"RepositoryType": "S3", "Location": "https://fake.invalid/" + name + "?X-Amz-Signature=secret"},
			"Tags":          f.tags,
		})
	case "update-function-code":
		if rev := argOf(args, "--revision-id"); rev != "" && rev != f.cfg["RevisionId"] {
			return nil, fmt.Errorf("PreconditionFailedException")
		}
		zip, err := os.ReadFile(strings.TrimPrefix(argOf(args, "--zip-file"), "fileb://"))
		if err != nil {
			return nil, err
		}
		f.code = zip
		f.rev++
		f.cfg["LastModified"] = fmt.Sprintf("2026-10-30T00:00:%02d.000+0000", f.rev)
		if l.updates == nil {
			l.updates = map[string]int{}
		}
		l.updates[name]++
		f.sync()
		if l.onUpdate != nil {
			l.onUpdate(name, f, l.updates[name])
		}
		return json.Marshal(f.cfg)
	case "tag-resource":
		if l.failTag {
			return nil, fmt.Errorf("AccessDenied")
		}
		for _, kv := range strings.Split(argOf(args, "--tags"), ",") {
			k, v, _ := strings.Cut(kv, "=")
			f.tags[k] = v
		}
		return nil, nil
	}
	return nil, fmt.Errorf("fake: unsupported subcommand %s", sub)
}

// mutating are the calls a dry run must never make: every subcommand that is
// not a read. Derived from the verb, not listed, so a new mutating call is
// counted without editing this.
func (l *fakeLambda) mutating() []call {
	var out []call
	for _, c := range l.calls {
		if !strings.HasPrefix(c.sub, "get-") && !strings.HasPrefix(c.sub, "list-") {
			out = append(out, c)
		}
	}
	return out
}

func (l *fakeLambda) count(sub, fn string) int {
	n := 0
	for _, c := range l.calls {
		if c.sub == sub && (fn == "" || argOf(c.args, "--function-name") == fn || strings.HasSuffix(argOf(c.args, "--resource"), ":"+fn)) {
			n++
		}
	}
	return n
}

// fixture is a fake tag checkout with two Lambdas: lambda/a, shell-deployed
// for amd64, and lambda/b, SAM-deployed for arm64, plus a fake account.
type fixture struct {
	t     *testing.T
	repo  string
	aws   *fakeLambda
	built map[string]int
	body  []byte
	pub   []byte
	log   bytes.Buffer
}

// retiredRuntime is split so the repo's runtime census (make lambda-runtimes),
// which scans every file for runtime names, does not read this fixture as a
// deploy script declaring a retired runtime.
const retiredRuntime = "provided." + "al2"

const shellMechanism = `#!/bin/bash
set -euo pipefail
FUNCTION_NAME="fn-a"
# a comment naming --runtime ` + retiredRuntime + ` must not count
RUNTIME="${SPAWN_LAMBDA_RUNTIME:-provided.al2023}"
SPAWN_VERSION=$(git describe --tags --exact-match)
WANT=$( (cd ../.. && SPORE_NO_UPDATE_CHECK=1 CGO_ENABLED=0 go run . bootstrap-body) | openssl dgst -sha256)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags lambda.norpc -ldflags "-X main.Version=${SPAWN_VERSION}" -o bootstrap .
aws lambda create-function --function-name "$FUNCTION_NAME" --runtime "$RUNTIME" --handler bootstrap --zip-file fileb://function.zip
`

const samMechanism = `AWSTemplateFormatVersion: '2010-09-09'
Transform: AWS::Serverless-2016-10-31
Resources:
  Fn:
    Type: AWS::Serverless::Function
    Properties:
      FunctionName: fn-b
      CodeUri: build/
      Handler: bootstrap
      Runtime: provided.al2023
      Architectures:
        - arm64
`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repo := t.TempDir()
	for p, body := range map[string]string{
		"lambda/a/deploy.sh":     shellMechanism,
		"lambda/b/template.yaml": samMechanism,
		"lambda/b/Makefile":      "build:\n\tGOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o build/bootstrap .\n",
	} {
		full := filepath.Join(repo, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("#!/bin/sh\necho body\n")
	return &fixture{
		t: t, repo: repo, built: map[string]int{}, body: body, pub: body,
		aws: &fakeLambda{fns: map[string]*fakeFn{"fn-a": newFakeFn("fn-a", "x86_64"), "fn-b": newFakeFn("fn-b", "arm64")}},
	}
}

func (f *fixture) lambdas() []checklistLambda {
	return []checklistLambda{
		{ID: "lambda/a", Functions: []string{"fn-a"}, Status: "changed", DeployKind: "code-only",
			Build: checklistBuild{GOOS: "linux", GOARCH: "amd64", Tags: []string{"lambda.norpc"},
				Command: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags lambda.norpc -ldflags "-X main.Version=${SPAWN_VERSION}" -o bootstrap .`},
			Mechanisms: []checklistMechanism{{Path: "lambda/a/deploy.sh", Kind: "shell", Files: []string{"lambda/a/deploy.sh"}}}},
		{ID: "lambda/b", Functions: []string{"fn-b"}, Status: "changed", DeployKind: "code-only",
			Build: checklistBuild{GOOS: "linux", GOARCH: "arm64", Tags: []string{"lambda.norpc"},
				Command: "GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o build/bootstrap ."},
			Mechanisms: []checklistMechanism{{Path: "lambda/b/template.yaml", Kind: "sam", Files: []string{"lambda/b/Makefile", "lambda/b/template.yaml"}}}},
	}
}

func (f *fixture) checklist(ls ...checklistLambda) *checklist {
	if ls == nil {
		ls = f.lambdas()
	}
	return &checklist{Schema: 1, From: "v0.150.0", To: "v0.151.0", FromCommit: "aaa", ToCommit: "headsha", Lambdas: ls}
}

func (f *fixture) deployer(dryRun bool) *deployer {
	return &deployer{
		aws: f.aws,
		fetch: func(_ context.Context, url string) ([]byte, error) {
			if strings.HasPrefix(url, "https://fake.invalid/") {
				fn := strings.TrimPrefix(url, "https://fake.invalid/")
				fn = fn[:strings.IndexByte(fn, '?')]
				return f.aws.fns[fn].code, nil
			}
			if url == "https://body.invalid/0.151.0" {
				if f.pub == nil {
					return nil, fmt.Errorf("404 Not Found")
				}
				return f.pub, nil
			}
			return nil, fmt.Errorf("unexpected fetch %s", url)
		},
		git: func(_ context.Context, args ...string) (string, error) {
			switch args[0] {
			case "describe":
				return "v0.151.0", nil
			case "status":
				return "", nil
			case "rev-parse":
				return "headsha", nil
			}
			return "", fmt.Errorf("unexpected git %v", args)
		},
		build: func(_ context.Context, _ string, pkgDir string, spec buildSpec, _ string) ([]byte, error) {
			f.built[pkgDir]++
			return zipBootstrap([]byte("new-" + pkgDir + "-" + spec.GOARCH + "-" + spec.LDFlags))
		},
		renderBody: func(context.Context, string, string) ([]byte, error) { return f.body, nil },
		sleep:      func(time.Duration) {},
		log:        &f.log,
		repo:       f.repo, region: "us-east-1", account: testAccount, tag: "v0.151.0", version: "0.151.0",
		dryRun: dryRun, workDir: f.t.TempDir(), bodyURL: "https://body.invalid/%s",
		pollAttempts: 3, pollInterval: time.Second,
	}
}

func (f *fixture) run(dryRun bool, c *checklist) (*record, error) {
	if c == nil {
		c = f.checklist()
	}
	return f.deployer(dryRun).run(context.Background(), c)
}

func outcome(t *testing.T, rec *record, fn string) result {
	t.Helper()
	for _, r := range rec.Results {
		if r.Function == fn {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", fn, rec.Results)
	return result{}
}
