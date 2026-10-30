package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// releaseTagRe is a spawn release tag. A pre-release or a dev describe names no
// published spored or bootstrap body, so it is refused (#1368).
var releaseTagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// volatileConfig are the get-function-configuration fields a code update changes
// by itself: the code, the update status, and the runtime version, which Lambda
// moves to the latest on a function update under the Auto and FunctionUpdate
// runtime-management modes. Every other field must be identical after the
// deploy; a difference fails it and rolls back.
var volatileConfig = map[string]bool{
	"CodeSha256":                 true,
	"CodeSize":                   true,
	"LastModified":               true,
	"LastUpdateStatus":           true,
	"LastUpdateStatusReason":     true,
	"LastUpdateStatusReasonCode": true,
	"State":                      true,
	"StateReason":                true,
	"StateReasonCode":            true,
	"RevisionId":                 true,
	"RuntimeVersionConfig":       true,
}

const versionTag = "spawn:version"

type deployer struct {
	aws        lambdaCLI
	fetch      fetchFunc
	git        gitFunc
	build      buildFunc
	renderBody renderBodyFunc
	sleep      func(time.Duration)
	log        io.Writer

	repo    string
	region  string
	account string
	tag     string // v0.151.0
	version string // 0.151.0
	dryRun  bool
	workDir string
	bodyURL string // with %s for the version

	pollAttempts int
	pollInterval time.Duration
}

// callLambda is the one way this tool talks to AWS. Keep the subcommand a
// string literal at every call site: the CI-role gate reads them from source.
func (d *deployer) callLambda(ctx context.Context, subcommand string, args ...string) ([]byte, error) {
	return d.aws.Lambda(ctx, subcommand, args...)
}

func (d *deployer) logf(format string, a ...any) {
	if d.log != nil {
		fmt.Fprintf(d.log, format+"\n", a...)
	}
}

// prepared is one function after steps (a) to (c): checked, built and saved.
type prepared struct {
	t          target
	exp        expectation
	arn        string
	snapshot   map[string]any
	beforeSha  string
	prevTag    string
	hadPrevTag bool
	stack      string
	zip        string // path to the built zip
	localSha   string
	saved      string // path to the live code, for rollback
}

func (d *deployer) getConfig(ctx context.Context, fn string) (map[string]any, error) {
	out, err := d.callLambda(ctx, "get-function-configuration", "--function-name", fn)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("get-function-configuration %s: %w", fn, err)
	}
	return m, nil
}

type getFunctionOut struct {
	Configuration struct {
		CodeSha256 string `json:"CodeSha256"`
	} `json:"Configuration"`
	Code struct {
		RepositoryType string `json:"RepositoryType"`
		Location       string `json:"Location"`
	} `json:"Code"`
	Tags map[string]string `json:"Tags"`
}

func (d *deployer) getFunction(ctx context.Context, fn string) (*getFunctionOut, error) {
	out, err := d.callLambda(ctx, "get-function", "--function-name", fn)
	if err != nil {
		return nil, err
	}
	var g getFunctionOut
	if err := json.Unmarshal(out, &g); err != nil {
		return nil, fmt.Errorf("get-function %s: %w", fn, err)
	}
	return &g, nil
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// configDiff names the fields that differ between two configurations, ignoring
// the volatile ones.
func configDiff(before, after map[string]any) []string {
	var keys []string
	seen := map[string]bool{}
	for k := range before {
		seen[k] = true
	}
	for k := range after {
		seen[k] = true
	}
	for k := range seen {
		if volatileConfig[k] {
			continue
		}
		if !reflect.DeepEqual(before[k], after[k]) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// verifyRepo refuses anything but a clean checkout of a release tag that is
// the checklist's --to.
func (d *deployer) verifyRepo(ctx context.Context, c *checklist) error {
	if !releaseTagRe.MatchString(d.tag) {
		return fmt.Errorf("%q is not a spawn release tag (vX.Y.Z); deploy only from a release", d.tag)
	}
	at, err := d.git(ctx, "describe", "--tags", "--exact-match", "--match", "v[0-9]*.[0-9]*.[0-9]*", "HEAD")
	if err != nil || at != d.tag {
		return fmt.Errorf("HEAD is not at tag %s (git describe: %q, %v)", d.tag, at, err)
	}
	if st, err := d.git(ctx, "status", "--porcelain", "--untracked-files=all"); err != nil || st != "" {
		return fmt.Errorf("the working tree is not clean, so the build would not be %s: %q %v", d.tag, st, err)
	}
	head, err := d.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if c.To != d.tag || c.ToCommit != head {
		return fmt.Errorf("the checklist is for %s (%s), but this checkout is %s (%s)", c.To, c.ToCommit, d.tag, head)
	}
	return nil
}

// checkBody is deploy-sweep-orchestrator.sh's pre-flight (#1368): the bootstrap
// body published for this release must be the one this tag compiles in, or the
// members the function launches would retire themselves at boot.
func (d *deployer) checkBody(ctx context.Context) (string, error) {
	want, err := d.renderBody(ctx, d.repo, d.version)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf(d.bodyURL, d.version)
	got, err := d.fetch(ctx, url)
	if err != nil {
		return "", fmt.Errorf("no bootstrap body is published at %s: %w", url, err)
	}
	ws, gs := sha256.Sum256(want), sha256.Sum256(got)
	if ws != gs {
		return "", fmt.Errorf("the body published at %s (sha256 %s) is not the one %s compiles in (%s)", url, hex.EncodeToString(gs[:]), d.tag, hex.EncodeToString(ws[:]))
	}
	return hex.EncodeToString(ws[:]), nil
}

// preflight is step (a) for one function: the live function must already be
// what this build produces, settled, and in the expected account and region.
func (d *deployer) preflight(ctx context.Context, p *prepared) error {
	fn := p.t.Function
	cfg, err := d.getConfig(ctx, fn)
	if err != nil {
		return err
	}
	p.snapshot = cfg
	p.arn = str(cfg, "FunctionArn")
	if want := fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", d.region, d.account, fn); p.arn != want {
		return fmt.Errorf("%s: FunctionArn is %q, expected %q", fn, p.arn, want)
	}
	var problems []string
	check := func(field, got, want string) {
		if got != want {
			problems = append(problems, fmt.Sprintf("%s is %q, the build needs %q", field, got, want))
		}
	}
	check("PackageType", str(cfg, "PackageType"), p.exp.PackageType)
	check("Runtime", str(cfg, "Runtime"), p.exp.Runtime)
	check("Handler", str(cfg, "Handler"), p.exp.Handler)
	var archs []string
	if a, ok := cfg["Architectures"].([]any); ok {
		for _, x := range a {
			s, _ := x.(string)
			archs = append(archs, s)
		}
	}
	check("Architectures", strings.Join(archs, ","), p.exp.Architecture)
	check("State", str(cfg, "State"), "Active")
	check("LastUpdateStatus", str(cfg, "LastUpdateStatus"), "Successful")
	if len(problems) > 0 {
		return fmt.Errorf("%s: refusing a code-only update: %s. Changing these is a configuration deploy, which stays manual", fn, strings.Join(problems, "; "))
	}
	p.beforeSha = str(cfg, "CodeSha256")
	if p.beforeSha == "" {
		return fmt.Errorf("%s: no CodeSha256 in its configuration", fn)
	}
	return nil
}

// save is step (c): download the live code for rollback and prove it is the
// live code.
func (d *deployer) save(ctx context.Context, p *prepared) error {
	fn := p.t.Function
	g, err := d.getFunction(ctx, fn)
	if err != nil {
		return err
	}
	if g.Code.RepositoryType != "S3" || g.Code.Location == "" {
		return fmt.Errorf("%s: get-function gave no downloadable code (RepositoryType %q)", fn, g.Code.RepositoryType)
	}
	if g.Configuration.CodeSha256 != p.beforeSha {
		return fmt.Errorf("%s: code changed during pre-flight (%s, then %s)", fn, p.beforeSha, g.Configuration.CodeSha256)
	}
	p.prevTag, p.hadPrevTag = g.Tags[versionTag]
	p.stack = g.Tags["aws:cloudformation:stack-name"]
	b, err := d.fetch(ctx, g.Code.Location)
	if err != nil {
		return fmt.Errorf("%s: download the live code: %w", fn, err)
	}
	if got := codeSha256(b); got != p.beforeSha {
		return fmt.Errorf("%s: the downloaded live code has CodeSha256 %s, but the function reports %s", fn, got, p.beforeSha)
	}
	p.saved = filepath.Join(d.workDir, fn+".live.zip")
	return os.WriteFile(p.saved, b, 0o600)
}

// waitUpdated polls until the function's last update settles. Counted, not
// timed, so a test drives it through the seam.
func (d *deployer) waitUpdated(ctx context.Context, fn string) (map[string]any, error) {
	for i := 0; i < d.pollAttempts; i++ {
		cfg, err := d.getConfig(ctx, fn)
		if err != nil {
			return nil, err
		}
		switch s := str(cfg, "LastUpdateStatus"); s {
		case "Successful":
			return cfg, nil
		case "Failed":
			return cfg, fmt.Errorf("%s: update failed: %s %s", fn, str(cfg, "LastUpdateStatusReasonCode"), str(cfg, "LastUpdateStatusReason"))
		}
		d.sleep(d.pollInterval)
	}
	return nil, fmt.Errorf("%s: update still in progress after %d checks", fn, d.pollAttempts)
}

// pushCode uploads a zip, waits, and verifies the function reports its sha.
func (d *deployer) pushCode(ctx context.Context, fn, zip, wantSha, revision string) (map[string]any, error) {
	args := []string{"--function-name", fn, "--zip-file", "fileb://" + zip}
	if revision != "" {
		args = append(args, "--revision-id", revision)
	}
	if _, err := d.callLambda(ctx, "update-function-code", args...); err != nil {
		return nil, err
	}
	cfg, err := d.waitUpdated(ctx, fn)
	if err != nil {
		return cfg, err
	}
	if got := str(cfg, "CodeSha256"); got != wantSha {
		return cfg, fmt.Errorf("%s: deployed CodeSha256 %s does not match the zip (%s)", fn, got, wantSha)
	}
	return cfg, nil
}

func (d *deployer) stamp(ctx context.Context, p *prepared, value string) error {
	if _, err := d.callLambda(ctx, "tag-resource", "--resource", p.arn, "--tags", versionTag+"="+value); err != nil {
		return err
	}
	g, err := d.getFunction(ctx, p.t.Function)
	if err != nil {
		return err
	}
	if got := g.Tags[versionTag]; got != value {
		return fmt.Errorf("%s: %s reads %q after stamping %q", p.t.Function, versionTag, got, value)
	}
	return nil
}

// deployOne is steps (d) to (g) for one prepared function.
func (d *deployer) deployOne(ctx context.Context, p *prepared, r *result) error {
	fn := p.t.Function
	// Nothing may have moved since the pre-flight snapshot: the saved code is
	// what rollback restores, and the snapshot is what the config diff is
	// against.
	cur, err := d.getConfig(ctx, fn)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(cur, p.snapshot) {
		diff := configDiff(p.snapshot, cur)
		if str(cur, "CodeSha256") != p.beforeSha {
			diff = append(diff, "CodeSha256")
		}
		r.Outcome = outcomeRefused
		return fmt.Errorf("%s changed since the pre-flight (%s); not touched", fn, strings.Join(diff, ", "))
	}

	touched, stamped := false, false
	fail := func(err error) error {
		r.Error = err.Error()
		if !touched && !stamped {
			r.Outcome = outcomeFailed
			return err
		}
		d.rollback(ctx, p, r, stamped)
		return err
	}

	after := cur
	if p.localSha == p.beforeSha {
		r.Notes = append(r.Notes, "the function already runs this exact build; code not updated")
	} else {
		touched = true
		d.logf("%s: update-function-code (%s -> %s)", fn, p.beforeSha, p.localSha)
		if after, err = d.pushCode(ctx, fn, p.zip, p.localSha, str(p.snapshot, "RevisionId")); err != nil {
			return fail(err)
		}
	}
	r.AfterSha = str(after, "CodeSha256")
	if diff := configDiff(p.snapshot, after); len(diff) > 0 {
		r.ConfigDiff = "differs: " + strings.Join(diff, ", ")
		return fail(fmt.Errorf("%s: configuration changed by the code update: %s", fn, strings.Join(diff, ", ")))
	}
	r.ConfigDiff = "empty"
	if b, a := p.snapshot["RuntimeVersionConfig"], after["RuntimeVersionConfig"]; !reflect.DeepEqual(b, a) {
		r.Notes = append(r.Notes, fmt.Sprintf("runtime version moved with the update (Lambda runtime management): %v -> %v", b, a))
	}
	if p.prevTag != d.version || !p.hadPrevTag {
		stamped = true
		if err := d.stamp(ctx, p, d.version); err != nil {
			return fail(err)
		}
	}
	r.VersionStamp = fmt.Sprintf("%s=%s (verified)", versionTag, d.version)
	r.Outcome = outcomeDeployed
	if !touched {
		r.Outcome = outcomeCurrent
	}
	return nil
}

// rollback restores the saved code (and the previous version stamp) after a
// failure at or after the code update, and verifies it.
func (d *deployer) rollback(ctx context.Context, p *prepared, r *result, stamped bool) {
	fn := p.t.Function
	var notes []string
	ok := true
	cur, err := d.getConfig(ctx, fn)
	switch {
	case err == nil && str(cur, "CodeSha256") == p.beforeSha && str(cur, "LastUpdateStatus") == "Successful":
		notes = append(notes, "code is still the pre-deploy build")
	default:
		d.logf("%s: rolling back to %s", fn, p.beforeSha)
		if _, err := d.waitUpdated(ctx, fn); err != nil {
			// A failed update leaves the function settled at Failed; only an
			// update in progress blocks the rollback, and that is reported.
			if cur, _ := d.getConfig(ctx, fn); str(cur, "LastUpdateStatus") == "InProgress" {
				ok = false
				notes = append(notes, "rollback blocked: "+err.Error())
				break
			}
		}
		after, err := d.pushCode(ctx, fn, p.saved, p.beforeSha, "")
		if err != nil {
			ok = false
			notes = append(notes, "rollback FAILED: "+err.Error())
			break
		}
		r.AfterSha = str(after, "CodeSha256")
		notes = append(notes, "code rolled back, CodeSha256 verified "+p.beforeSha)
		if diff := configDiff(p.snapshot, after); len(diff) > 0 {
			ok = false
			notes = append(notes, "after rollback the configuration still differs: "+strings.Join(diff, ", "))
		}
	}
	if stamped {
		switch {
		case p.hadPrevTag:
			if err := d.stamp(ctx, p, p.prevTag); err != nil {
				ok = false
				notes = append(notes, "restoring "+versionTag+"="+p.prevTag+" FAILED: "+err.Error())
			} else {
				notes = append(notes, versionTag+" restored to "+p.prevTag)
			}
		default:
			notes = append(notes, versionTag+" had no previous value and the role cannot untag; it may read "+d.version)
		}
	}
	r.Notes = append(r.Notes, notes...)
	if ok {
		r.Outcome = outcomeRolledBack
	} else {
		r.Outcome = outcomeRollbackFailed
	}
}

// run is the whole deploy. It always returns a record, also on failure.
func (d *deployer) run(ctx context.Context, c *checklist) (rec *record, err error) {
	rec = &record{From: c.From, To: c.To, DryRun: d.dryRun}
	defer func() {
		if err != nil {
			rec.Error = err.Error()
		}
	}()
	targets, skipped := selectTargets(c)
	rec.Skipped = skipped
	for _, t := range targets {
		rec.Results = append(rec.Results, result{Function: t.Function, Component: t.Lambda.ID, Outcome: outcomeNotAttempted})
	}
	if err := d.verifyRepo(ctx, c); err != nil {
		return rec, err
	}
	if len(targets) == 0 {
		return rec, nil
	}

	// Pre-flight, build and save EVERY function before deploying any, so a
	// refusal on the last one leaves the first untouched.
	preps := make([]*prepared, len(targets))
	built := map[string]*prepared{} // component -> the first function's build
	bodyChecked := false
	for i, t := range targets {
		r := &rec.Results[i]
		p := &prepared{t: t}
		preps[i] = p
		fail := func(err error) (*record, error) {
			r.Outcome, r.Error = outcomeRefused, err.Error()
			return rec, err
		}
		var err error
		if p.exp, err = expect(d.repo, t.Lambda); err != nil {
			return fail(err)
		}
		if p.exp.BodyCheck && !bodyChecked {
			sum, err := d.checkBody(ctx)
			if err != nil {
				return fail(err)
			}
			bodyChecked = true
			rec.Notes = append(rec.Notes, fmt.Sprintf("published bootstrap body for %s verified (sha256 %s)", d.tag, sum))
		}
		if err := d.preflight(ctx, p); err != nil {
			return fail(err)
		}
		r.BeforeSha = p.beforeSha
		if b := built[t.Lambda.ID]; b != nil {
			p.zip, p.localSha = b.zip, b.localSha
		} else {
			spec, err := parseBuild(t.Lambda.Build, d.version)
			if err != nil {
				return fail(fmt.Errorf("%s: %w", t.Lambda.ID, err))
			}
			dir, err := os.MkdirTemp(d.workDir, "build-")
			if err != nil {
				return fail(err)
			}
			zb, err := d.build(ctx, d.repo, t.Lambda.ID, spec, dir)
			if err != nil {
				return fail(err)
			}
			p.zip = filepath.Join(d.workDir, strings.ReplaceAll(t.Lambda.ID, "/", "_")+".zip")
			if err := os.WriteFile(p.zip, zb, 0o600); err != nil {
				return fail(err)
			}
			p.localSha = codeSha256(zb)
			built[t.Lambda.ID] = p
		}
		r.BuiltSha = p.localSha
		if err := d.save(ctx, p); err != nil {
			return fail(err)
		}
		if p.exp.SAM {
			stack := p.stack
			if stack == "" {
				stack = "(no aws:cloudformation:stack-name tag found)"
			}
			r.Notes = append(r.Notes, fmt.Sprintf("SAM-managed, stack %s: after this update the stack's Code and Version parameter still name the previous deploy; that drift is expected and the next `make -C %s deploy` from a release tag reconciles it (docs/infra-account.md)", stack, t.Lambda.ID))
		}
		r.Outcome = outcomePrepared
	}

	if d.dryRun {
		for i, p := range preps {
			r := &rec.Results[i]
			r.Outcome = outcomeDryRun
			if p.localSha == p.beforeSha {
				r.Notes = append(r.Notes, "would not update code: the function already runs this exact build")
			} else {
				r.Notes = append(r.Notes, fmt.Sprintf("would update-function-code %s -> %s", p.beforeSha, p.localSha))
			}
			if p.prevTag != d.version || !p.hadPrevTag {
				prev := p.prevTag
				if !p.hadPrevTag {
					prev = "(none)"
				}
				r.Notes = append(r.Notes, fmt.Sprintf("would stamp %s=%s (now %s)", versionTag, d.version, prev))
			}
			r.VersionStamp = "not stamped (dry run)"
			r.ConfigDiff = "not checked (dry run)"
		}
		return rec, nil
	}

	for i, p := range preps {
		r := &rec.Results[i]
		if err := d.deployOne(ctx, p, r); err != nil {
			if r.Error == "" {
				r.Error = err.Error()
			}
			for j := i + 1; j < len(rec.Results); j++ {
				rec.Results[j].Outcome = outcomeNotAttempted
				rec.Results[j].Notes = append(rec.Results[j].Notes, "not attempted: "+p.t.Function+" failed first")
			}
			return rec, err
		}
	}
	return rec, nil
}
