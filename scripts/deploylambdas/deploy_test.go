package main

import (
	"context"
	"strings"
	"testing"
)

func TestHappyPathDeploysVerifiesAndStamps(t *testing.T) {
	f := newFixture(t)
	rec, err := f.run(false, nil)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, f.log.String())
	}
	for _, fn := range []string{"fn-a", "fn-b"} {
		r := outcome(t, rec, fn)
		if r.Outcome != outcomeDeployed || r.ConfigDiff != "empty" || r.AfterSha != r.BuiltSha || r.BeforeSha == r.BuiltSha {
			t.Errorf("%s: %+v", fn, r)
		}
		if got := f.aws.fns[fn].tags[versionTag]; got != "0.151.0" {
			t.Errorf("%s: %s = %q", fn, versionTag, got)
		}
		if codeSha256(f.aws.fns[fn].code) != r.BuiltSha {
			t.Errorf("%s: live code is not the build", fn)
		}
	}
	// The SAM function carries the drift note; the shell one does not.
	if !strings.Contains(strings.Join(outcome(t, rec, "fn-b").Notes, " "), "SAM-managed") {
		t.Errorf("fn-b has no SAM drift note: %v", outcome(t, rec, "fn-b").Notes)
	}
	if strings.Contains(strings.Join(outcome(t, rec, "fn-a").Notes, " "), "SAM-managed") {
		t.Errorf("fn-a is shell-deployed but has a SAM note")
	}
	if f.built["lambda/a"] != 1 || f.built["lambda/b"] != 1 {
		t.Errorf("builds: %v", f.built)
	}
}

func TestBuildSubstitutesTheReleaseVersion(t *testing.T) {
	spec, err := parseBuild(newFixture(t).lambdas()[0].Build, "0.151.0")
	if err != nil {
		t.Fatal(err)
	}
	if spec.LDFlags != "-X main.Version=0.151.0" {
		t.Errorf("LDFlags = %q", spec.LDFlags)
	}
}

// Gate: a manual or unchanged component is never deployed. Every status and
// deploy kind the checklist can emit, crossed, and only changed+code-only may
// reach update-function-code.
func TestOnlyChangedCodeOnlyLambdasAreDeployed(t *testing.T) {
	f := newFixture(t)
	base := f.lambdas()[0]
	statuses := []string{"changed", "unchanged", "added", "removed"}
	kinds := []string{"code-only", "manual", "none"}
	for _, st := range statuses {
		for _, k := range kinds {
			f := newFixture(t)
			l := base
			l.Status, l.DeployKind = st, k
			rec, err := f.run(false, f.checklist(l))
			want := st == "changed" && k == "code-only"
			got := f.aws.count("update-function-code", "fn-a")
			if want && (err != nil || got != 1) {
				t.Errorf("%s/%s: want one deploy, got %d (err %v)", st, k, got, err)
			}
			if !want {
				if got != 0 || len(f.aws.mutating()) != 0 || len(f.aws.calls) != 0 {
					t.Errorf("%s/%s: made calls %+v", st, k, f.aws.calls)
				}
				if len(rec.Results) != 0 || len(rec.Skipped) != 1 {
					t.Errorf("%s/%s: results %+v skipped %+v", st, k, rec.Results, rec.Skipped)
				}
				if st != "unchanged" && !strings.HasPrefix(rec.Skipped[0].Reason, "manual, not deployed") {
					t.Errorf("%s/%s: reason %q", st, k, rec.Skipped[0].Reason)
				}
			}
		}
	}
}

func TestChangedDeployInputsAreListedNotDeployed(t *testing.T) {
	f := newFixture(t)
	c := f.checklist()
	c.Inputs = []checklistInput{
		{Path: "lambda/a/scripts/setup-role.sh", Why: "runs aws iam put-role-policy", Status: "changed", DeployKind: "manual"},
		{Path: "lambda/a/scripts/unchanged.sh", Why: "runs aws iam put-role-policy", Status: "unchanged", DeployKind: "none"},
	}
	_, skipped := selectTargets(c)
	if len(skipped) != 1 || skipped[0].What != "lambda/a/scripts/setup-role.sh" || !strings.HasPrefix(skipped[0].Reason, "manual, not deployed") {
		t.Errorf("skipped = %+v", skipped)
	}
}

// Gate: a live function that is not what the build produces is refused, before
// anything is changed anywhere.
func TestMismatchedLiveFunctionRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"architecture", "Architectures", []any{"arm64"}},
		{"runtime", "Runtime", "provided.al2"},
		{"handler", "Handler", "main"},
		{"package type", "PackageType", "Image"},
		{"update in progress", "LastUpdateStatus", "InProgress"},
		{"other account", "FunctionArn", "arn:aws:lambda:us-east-1:111111111111:function:fn-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.aws.fns["fn-a"].cfg[tc.field] = tc.value
			rec, err := f.run(false, nil)
			if err == nil {
				t.Fatal("deployed a mismatched function")
			}
			if m := f.aws.mutating(); len(m) != 0 {
				t.Errorf("made mutating calls: %+v", m)
			}
			if r := outcome(t, rec, "fn-a"); r.Outcome != outcomeRefused {
				t.Errorf("fn-a outcome %q", r.Outcome)
			}
			if r := outcome(t, rec, "fn-b"); r.Outcome != outcomeNotAttempted {
				t.Errorf("fn-b outcome %q", r.Outcome)
			}
		})
	}
}

// The architecture comes from the checklist's build, not from this tool: the
// same live arm64 function is refused for an amd64 build and accepted for an
// arm64 one.
func TestArchitectureComesFromTheChecklistBuild(t *testing.T) {
	f := newFixture(t)
	l := f.lambdas()[1] // fn-b, live arm64
	l.Build.GOARCH = "amd64"
	l.Build.Command = strings.Replace(l.Build.Command, "GOARCH=arm64", "GOARCH=amd64", 1)
	if _, err := f.run(false, f.checklist(l)); err == nil || !strings.Contains(err.Error(), "Architectures") {
		t.Fatalf("amd64 build onto an arm64 function: err %v", err)
	}
	f = newFixture(t)
	if _, err := f.run(false, f.checklist(f.lambdas()[1])); err != nil {
		t.Fatalf("arm64 build onto an arm64 function: %v", err)
	}
}

// Gate: a remote CodeSha256 that is not the build rolls back.
func TestRemoteShaMismatchRollsBack(t *testing.T) {
	f := newFixture(t)
	orig := f.aws.fns["fn-a"].code
	f.aws.onUpdate = func(fn string, fx *fakeFn, n int) {
		if fn == "fn-a" && n == 1 {
			fx.code = mustZip([]byte("something else"))
			fx.sync()
		}
	}
	rec, err := f.run(false, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
	if n := f.aws.count("update-function-code", "fn-a"); n != 2 {
		t.Errorf("fn-a: %d code updates, want the deploy and the rollback", n)
	}
	if string(f.aws.fns["fn-a"].code) != string(orig) {
		t.Error("fn-a is not back on its original code")
	}
	if r := outcome(t, rec, "fn-a"); r.Outcome != outcomeRolledBack || r.AfterSha != codeSha256(orig) {
		t.Errorf("fn-a: %+v", r)
	}
	// Stop at the first failure.
	if f.aws.count("update-function-code", "fn-b") != 0 || outcome(t, rec, "fn-b").Outcome != outcomeNotAttempted {
		t.Errorf("fn-b was touched after fn-a failed: %+v", outcome(t, rec, "fn-b"))
	}
}

// Gate: a configuration change alongside the code update rolls back.
func TestConfigDiffRollsBack(t *testing.T) {
	f := newFixture(t)
	orig := f.aws.fns["fn-a"].code
	f.aws.onUpdate = func(fn string, fx *fakeFn, n int) {
		if fn == "fn-a" && n == 1 {
			fx.cfg["Environment"] = map[string]any{"Variables": map[string]any{}}
		}
	}
	rec, err := f.run(false, nil)
	if err == nil || !strings.Contains(err.Error(), "Environment") {
		t.Fatalf("err = %v", err)
	}
	if n := f.aws.count("update-function-code", "fn-a"); n != 2 {
		t.Errorf("fn-a: %d code updates, want the deploy and the rollback", n)
	}
	if string(f.aws.fns["fn-a"].code) != string(orig) {
		t.Error("fn-a is not back on its original code")
	}
	r := outcome(t, rec, "fn-a")
	if !strings.HasPrefix(r.ConfigDiff, "differs: Environment") {
		t.Errorf("config diff %q", r.ConfigDiff)
	}
	// The environment the fake wiped is not restored by a code rollback, and
	// the record must say so rather than report a clean rollback.
	if r.Outcome != outcomeRollbackFailed {
		t.Errorf("outcome %q", r.Outcome)
	}
	if f.aws.fns["fn-a"].tags[versionTag] != "0.150.0" {
		t.Errorf("stamped despite the failure: %v", f.aws.fns["fn-a"].tags)
	}
}

// A runtime-version move with the update is expected, not a config diff.
func TestRuntimeVersionMoveIsNotAConfigDiff(t *testing.T) {
	f := newFixture(t)
	f.aws.onUpdate = func(_ string, fx *fakeFn, _ int) {
		fx.cfg["RuntimeVersionConfig"] = map[string]any{"RuntimeVersionArn": "arn:aws:lambda:us-east-1::runtime:2"}
	}
	rec, err := f.run(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(outcome(t, rec, "fn-a").Notes, " "), "runtime version moved") {
		t.Errorf("no note: %+v", outcome(t, rec, "fn-a"))
	}
}

func TestFailedStampRollsBackAndRestoresTheTag(t *testing.T) {
	f := newFixture(t)
	orig := f.aws.fns["fn-a"].code
	f.aws.failTag = true
	rec, err := f.run(false, nil)
	if err == nil {
		t.Fatal("no error")
	}
	if string(f.aws.fns["fn-a"].code) != string(orig) {
		t.Error("fn-a not rolled back")
	}
	if r := outcome(t, rec, "fn-a"); r.Outcome != outcomeRollbackFailed {
		// The tag restore also fails (the fake refuses every tag), which is reported.
		t.Errorf("outcome %q: %v", r.Outcome, r.Notes)
	}
}

// Gate: a dry run makes no mutating call, but does everything up to (c).
func TestDryRunMakesNoMutatingCall(t *testing.T) {
	f := newFixture(t)
	rec, err := f.run(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := f.aws.mutating(); len(m) != 0 {
		t.Fatalf("dry run made mutating calls: %+v", m)
	}
	for _, fn := range []string{"fn-a", "fn-b"} {
		r := outcome(t, rec, fn)
		if r.Outcome != outcomeDryRun || r.BuiltSha == "" || r.BeforeSha == "" {
			t.Errorf("%s: %+v", fn, r)
		}
		if f.aws.count("get-function", fn) == 0 || f.aws.count("get-function-configuration", fn) == 0 {
			t.Errorf("%s: dry run skipped the pre-flight or the save", fn)
		}
		notes := strings.Join(r.Notes, " ")
		if !strings.Contains(notes, "would update-function-code") || !strings.Contains(notes, "would stamp spawn:version=0.151.0") {
			t.Errorf("%s: notes %q", fn, notes)
		}
	}
	if f.built["lambda/a"] != 1 || f.built["lambda/b"] != 1 {
		t.Errorf("dry run did not build: %v", f.built)
	}
}

func TestBodyCheckRefusesAnUnpublishedOrDifferentBody(t *testing.T) {
	for name, pub := range map[string][]byte{"missing": nil, "different": []byte("other")} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.pub = pub
			_, err := f.run(true, nil)
			if err == nil || !strings.Contains(err.Error(), "body") {
				t.Fatalf("err = %v", err)
			}
			if len(f.aws.calls) != 0 {
				t.Errorf("checked the function before the body: %+v", f.aws.calls)
			}
		})
	}
}

func TestRefusesAnUncleanOrUntaggedCheckout(t *testing.T) {
	for name, tc := range map[string]struct{ tag, describe, status, head string }{
		"dirty":           {"v0.151.0", "v0.151.0", " M main.go", "headsha"},
		"not at the tag":  {"v0.151.0", "", "", "headsha"},
		"pre-release":     {"v0.151.0-rc1", "v0.151.0-rc1", "", "headsha"},
		"other checklist": {"v0.151.0", "v0.151.0", "", "othersha"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			d := f.deployer(true)
			d.tag = tc.tag
			d.git = func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "describe":
					return tc.describe, nil
				case "status":
					return tc.status, nil
				}
				return tc.head, nil
			}
			if _, err := d.run(context.Background(), f.checklist()); err == nil {
				t.Fatal("no refusal")
			}
			if len(f.aws.calls) != 0 || len(f.built) != 0 {
				t.Errorf("acted on a refused checkout: calls %+v built %v", f.aws.calls, f.built)
			}
		})
	}
}

func TestChangedSincePreflightIsNotTouched(t *testing.T) {
	f := newFixture(t)
	d := f.deployer(false)
	// Move fn-a between the pre-flight (on the first read) and the deploy.
	reads := 0
	inner := f.aws
	d.aws = lambdaFunc(func(ctx context.Context, sub string, args ...string) ([]byte, error) {
		if sub == "get-function" && argOf(args, "--function-name") == "fn-a" {
			reads++
			if reads == 1 {
				defer func() { inner.fns["fn-a"].cfg["MemorySize"] = float64(1024) }()
			}
		}
		return inner.Lambda(ctx, sub, args...)
	})
	rec, err := d.run(context.Background(), f.checklist())
	if err == nil || !strings.Contains(err.Error(), "MemorySize") {
		t.Fatalf("err = %v", err)
	}
	if len(inner.mutating()) != 0 {
		t.Errorf("touched a function that moved: %+v", inner.mutating())
	}
	if outcome(t, rec, "fn-a").Outcome != outcomeRefused {
		t.Errorf("%+v", outcome(t, rec, "fn-a"))
	}
}

func TestAlreadyCurrentOnlyStamps(t *testing.T) {
	f := newFixture(t)
	d := f.deployer(false)
	d.build = func(context.Context, string, string, buildSpec, string) ([]byte, error) {
		return f.aws.fns["fn-a"].code, nil
	}
	rec, err := d.run(context.Background(), f.checklist(f.lambdas()[0]))
	if err != nil {
		t.Fatal(err)
	}
	if f.aws.count("update-function-code", "") != 0 || outcome(t, rec, "fn-a").Outcome != outcomeCurrent {
		t.Errorf("%+v", rec.Results)
	}
	if f.aws.fns["fn-a"].tags[versionTag] != "0.151.0" {
		t.Error("not stamped")
	}
}

func TestPresignedURLIsNotLogged(t *testing.T) {
	if got := redact("https://x/y?X-Amz-Signature=secret"); strings.Contains(got, "secret") {
		t.Errorf("redact = %q", got)
	}
}

func TestParseBuildRefusesWhatItCannotReproduce(t *testing.T) {
	for _, cmd := range []string{
		"GOOS=linux GOARCH=amd64 go build -trimpath -o bootstrap .",
		"GOOS=linux GOARCH=amd64 GOAMD64=v3 go build -o bootstrap .",
		"GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -o bootstrap .",
		"GOOS=linux GOARCH=amd64 go build -o bootstrap main.go",
		`GOOS=linux GOARCH=amd64 go build -ldflags "-X main.V=${OTHER}" -o bootstrap .`,
	} {
		if _, err := parseBuild(checklistBuild{GOOS: "linux", GOARCH: "amd64", Command: cmd}, "0.151.0"); err == nil {
			t.Errorf("accepted %q", cmd)
		}
	}
	spec, err := parseBuild(checklistBuild{GOOS: "linux", GOARCH: "amd64", Command: `GOOS=linux GOARCH=amd64 go build -o bootstrap -ldflags="-s -w" .`}, "0.151.0")
	if err != nil || spec.LDFlags != "-s -w" {
		t.Errorf("-ldflags=\"-s -w\": %+v %v", spec, err)
	}
}

func TestRecordRendersEveryResult(t *testing.T) {
	f := newFixture(t)
	c := f.checklist()
	c.Lambdas = append(c.Lambdas, checklistLambda{ID: "lambda/c", Functions: []string{"fn-c"}, Status: "unchanged", DeployKind: "none"})
	rec, err := f.run(true, c)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	rec.writeMarkdown(&b)
	for _, want := range []string{"DRY RUN", "`fn-a`", "`fn-b`", "lambda/c (fn-c)", "unchanged, no deploy"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("record has no %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "secret") {
		t.Error("record leaks the presigned URL")
	}
}
