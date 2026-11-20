package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"
	fp "github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lastmile"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/lastmilespawn"
	"github.com/spore-host/spawn/pkg/lifecyclemark"
	"github.com/spore-host/spawn/pkg/scheduler"
)

// The lazy CLI last mile (libs#78, part of leave-no-trace
// spore-host/spore-host#752). After a command succeeds, the root command's
// post-run hook spends at most lastMileBudget checking, with the caller's own
// credentials, whether spawn left something behind that no backstop will
// remove: a teardown whose holder died (a stale spore-host-lifecycle lease),
// the self-hosted reaper's teardown residue once no spawn lifecycle item is
// left (the jobs' invoke role and schedule group, spawn#1655), and an empty
// spore-host-lifecycle table.
//
// It only reports, and never gets in the way of the command:
//
//   - It changes nothing. libs/lastmile runs in report mode (SPORE_LASTMILE=reap
//     still reports until libs#79), and on top of that the lifecycle client it
//     is given refuses every DynamoDB write and its Deleter refuses Delete
//     (pkg/lastmilespawn).
//   - It writes nothing to stdout, and nothing at all unless --verbose, when
//     its lines go to stderr prefixed "debug: ". A read the caller is not
//     allowed is one such line, never a warning, and the first refusal stops
//     the asking.
//   - It cannot change the exit status: the hook always returns nil, recovers
//     any panic, and runs only after the command itself succeeded (cobra runs
//     no post-run after a failure).
//   - It cannot hold the command past lastMileBudget (+ lastMileSlack): the
//     work runs in a goroutine the hook stops waiting for.
//   - It makes no STS call: the account is the one the command's own
//     GetCallerIdentity reported (callerAccounts, installed by Execute); a
//     command that made none skips it. And between checks (every 6 h per
//     account) it makes no AWS call at all: lastMileMaybeDue reads the
//     rate-limit state first. A check the budget cut off resumes an hour
//     later, not on the next command (lastMileRetryAfter).
//
// It lives here, in cmd, and nowhere in pkg/: lagotto, calque and
// spore-host-mcp import spawn's packages, and the last mile must not run
// inside their processes.
//
// Regions are every region enabled for the account (ec2:DescribeRegions):
// the residue may be judged removable only when no spawn item is left in any
// region spawn can have used, and `spawn footprint --all-regions` reads the
// same set. The cost, once per 6 h per account, is one DescribeRegions and
// about three DynamoDB reads per region; the residue re-check, when no spawn
// item is left anywhere, two IAM reads and one Scheduler read per region; and
// a footprint of every region only when a stale lease is found.

// lastMileSkip are the top-level commands after which the last mile does not
// run, and why. Hidden top-level commands (gen-docs, bootstrap-body,
// awscli-pin, spored-signing-key: release and docs plumbing) are skipped
// too, by lastMileSkipped, without being listed.
var lastMileSkip = map[string]string{
	"completion":                    "shell completion must stay silent and fast",
	cobra.ShellCompRequestCmd:       "shell completion must stay silent and fast",
	cobra.ShellCompNoDescRequestCmd: "shell completion must stay silent and fast",
	"help":                          "help text needs no AWS",
	"version":                       "version needs no AWS",
	"footprint":                     "it is spawn's own inventory",
	"teardown":                      "it is spawn's own teardown",
	"doctor":                        "its report must hold only what its own checks read",
	"cleanup":                       "it is spawn's own cleanup, and says what it found itself",
	"reaper":                        "it deploys, inspects and removes the reaper, whose residue the last mile reports",
	"gen-docs":                      "it builds the command reference offline (make check-docs)",
}

// The account the command's own GetCallerIdentity calls reported. Execute
// adds its middleware to every config built for the caller's account.
var callerAccounts = &lastmilespawn.AccountObserver{}

// Seams. Tests replace them; production uses the values here.
var (
	lastMileRun       = lastmile.Run
	lastMileBudget    = lastmile.DefaultBudget
	lastMileAccount   = callerAccounts.Account
	lastMileStatePath = lastmile.DefaultStatePath
	lastMileNow       = time.Now
	// lastMileConfig is the caller's AWS config, resolved as every command's
	// (--profile, --region and the shared config).
	lastMileConfig = func(ctx context.Context) (sdkaws.Config, error) { return loadAWSConfig(ctx, "") }
	// lastMileRegions lists the regions enabled for the account.
	lastMileRegions = func(ctx context.Context, cfg sdkaws.Config) ([]string, error) {
		return spawnaws.NewClientFromConfig(cfg).GetEnabledRegions(ctx)
	}
	// lastMileTables is the DynamoDB client of each region's lifecycle table.
	lastMileTables = lastmilespawn.RegionalTables
	// lastMileFootprintOf is `spawn footprint --all-regions -o json`.
	lastMileFootprintOf = func(ctx context.Context, cfg sdkaws.Config, account string, regions []string, now time.Time) (*fp.Report, error) {
		return spawnaws.NewClientFromConfig(cfg).AccountFootprint(ctx, account, regions, now), nil
	}
	// lastMileDeleter re-checks a plan and deletes nothing.
	lastMileDeleter = func(cfg sdkaws.Config, account string, now func() time.Time) fp.Deleter {
		return lastmilespawn.NewReportOnlyDeleter(cfg, account, now)
	}
	// lastMileActivity would read spawn's activity signals for lastmile's own
	// idle check (Options.Activity). It is nil: see lastMileVisibility.
	lastMileActivity func(cfg sdkaws.Config, account string) func(ctx context.Context, scope string) (bool, error)
	// lastMileInfraReadable reports whether the caller can read the infra
	// account's activity tables.
	lastMileInfraReadable = func(ctx context.Context) error {
		cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, scheduler.TableRegion)
		if err != nil {
			return err
		}
		return lastmilespawn.InfraSignalsReadableIn(ctx, cfg)
	}
)

// lastMileSlack is how much longer than its budget the hook waits before it
// stops waiting.
const lastMileSlack = 250 * time.Millisecond

// lastMileHook is the root command's PersistentPostRunE. It always returns
// nil.
func lastMileHook(cmd *cobra.Command, _ []string) error {
	runLastMile(cmd)
	return nil
}

// lastMileTop is the top-level command cmd belongs to (`completion` for
// `completion bash`).
func lastMileTop(cmd *cobra.Command) *cobra.Command {
	for cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}
	return cmd
}

// lastMileSkipped reports whether the last mile does not run after cmd: a
// command in lastMileSkip, a subcommand of one, or a hidden top-level
// command (internal plumbing). The experimental chat group (notify) is
// hidden only until opted in, and un-hidden by Execute when it is.
func lastMileSkipped(cmd *cobra.Command) bool {
	top := lastMileTop(cmd)
	if _, skip := lastMileSkip[top.Name()]; skip {
		return true
	}
	return top != cmd.Root() && top.Hidden
}

// runLastMile runs the last mile after cmd, unless cmd is one it skips or
// SPORE_LASTMILE=off, and waits for it at most lastMileBudget+lastMileSlack.
func runLastMile(cmd *cobra.Command) {
	defer func() { _ = recover() }() // nothing here may fail the command
	if lastMileSkipped(cmd) {
		return
	}
	mode := lastmile.ModeFromEnv()
	if mode == lastmile.ModeOff {
		return
	}
	log := &lastMileLog{}
	if spawnVerbose {
		log.w = cmd.ErrOrStderr()
	}
	defer log.close()

	budget := lastMileBudget
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				log.printf("lastmile: internal error (recovered): %v", p)
			}
		}()
		lastMile(ctx, mode, log.printf)
	}()
	timer := time.NewTimer(budget + lastMileSlack)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.printf("lastmile: not finished within its %s budget; not waiting for it", budget)
	}
}

// lastMile is the work: build the options and run lastmile.Run.
func lastMile(ctx context.Context, mode lastmile.Mode, logf func(string, ...any)) {
	account := lastMileAccount()
	switch account {
	case "":
		logf("lastmile: skipped: this command resolved no AWS account, and the last mile makes no STS call of its own")
		return
	case fs.InfraAccountID:
		logf("lastmile: skipped: %s is spore.host's infra account, where no teardown ever runs", account)
		return
	}
	statePath, err := lastMileStatePath()
	if err != nil {
		logf("lastmile: skipped: %v", err)
		return
	}
	if !lastMileMaybeDue(statePath, account, lastMileNow()) {
		logf("lastmile: skipped: rate-limited (checked within the last %s, or cut off by its budget within the last %s)",
			lastmile.DefaultMinInterval, lastMileRetryAfter)
		return
	}
	cfg, err := lastMileConfig(ctx)
	if err != nil {
		logf("lastmile: skipped: load AWS config: %v", err)
		return
	}
	if cfg.Region == "" {
		cfg.Region = lastMileControlRegion
	}
	regions, err := lastMileRegions(ctx, cfg)
	if err != nil {
		logf("lastmile: skipped: list the account's regions: %v", err)
		return
	}
	lc, _, err := lastmilespawn.NewLifecycleReader(lastMileTables(cfg), lastMileNow)
	if err != nil {
		logf("lastmile: skipped: %v", err)
		return
	}
	o := lastMileOptions(cfg, account, regions, lc)
	o.FullVisibility, o.Activity = lastMileVisibility(ctx, cfg, account, logf)
	o.Mode, o.StatePath, o.Logf, o.Now = mode, statePath, logf, lastMileNow
	if deadline, ok := ctx.Deadline(); ok {
		o.Budget = time.Until(deadline)
		if o.Budget <= 0 {
			logf("lastmile: skipped: no budget left")
			return
		}
	}
	res := lastMileRun(ctx, o)
	logf("%s", res.Summary())
}

// lastMileControlRegion is the region holding the global scope's lifecycle
// item, where spawn's creators write it (pkg/lifecyclemark), and the region
// the regions are listed from when none is configured.
const lastMileControlRegion = lifecyclemark.ControlRegion

// lastMileOptions are the lastmile.Options for spawn in account: everything
// but FullVisibility, Activity, the mode, state path, logger, clock and
// budget.
func lastMileOptions(cfg sdkaws.Config, account string, regions []string, lc *lifecycle.Client) lastmile.Options {
	live := &lastMileFootprint{load: func(ctx context.Context) (*fp.Report, error) {
		return lastMileFootprintOf(ctx, cfg, account, regions, lastMileNow())
	}}
	return lastmile.Options{
		Tool:      tags.ToolSpawn,
		Account:   account,
		Regions:   regions,
		Lifecycle: lc,
		Footprint: live.get,
		Deleter:   lastMileDeleter(cfg, account, lastMileNow),
		Patterns:  lastmilespawn.Patterns(),
		Residue:   lastmilespawn.Residue(account, regions),
	}
}

// lastMileVisibility decides FullVisibility and Activity: whether lastmile
// may judge spawn idle itself, in an account with no backstop.
//
// Both conditions must hold. The CLI must read every activity signal the
// reaper's account-idle sweep reads, and the sweeps, pipelines and
// schedules among them live in spore.host's infra account, so the caller has
// to be able to read those tables (spawnconfig.LoadInfraAWSConfig, then a
// read of each). And it needs the sweep's own verdict code to read them
// with, since a second opinion of "is spawn active" is how a teardown
// deletes what one of the two could not see.
//
// That code is not reusable from here: the verdict (accountVerdict and its
// signal readers) is package main of lambda/ttl-reaper, a separate module,
// and it imports calque's footprint for calque's activity, which spawn's own
// module does not. So lastMileActivity is nil and FullVisibility is false,
// and the infra tables are not even read. In report mode that loses
// nothing: lastmile never calls Observe, so without the reaper's sweep no
// scope is ever recorded idle, and FullVisibility could only ever say
// "Observe would start the grace now". Moving the verdict into a pkg/ the
// reaper and the CLI share is the precondition for setting it, with reap
// mode (libs#79).
func lastMileVisibility(ctx context.Context, cfg sdkaws.Config, account string, logf func(string, ...any)) (bool, func(ctx context.Context, scope string) (bool, error)) {
	if lastMileActivity == nil {
		return false, nil
	}
	if err := lastMileInfraReadable(ctx); err != nil {
		logf("lastmile: idle check off: the infra account's sweep, pipeline and schedule tables could not be read, "+
			"so not every activity signal is visible: %v", err)
		return false, nil
	}
	return true, lastMileActivity(cfg, account)
}

// lastMileFootprint is spawn's footprint of every region, read at most once
// per run.
//
// A mutex, not a sync.Once: the IAM inventory's call graph (internal/
// iamcoverage) merges every function passed to sync.Once.Do, so a Once here
// would charge the footprint's reads to every command that uses one.
type lastMileFootprint struct {
	load func(ctx context.Context) (*fp.Report, error)
	mu   sync.Mutex
	done bool
	rep  *fp.Report
	err  error
}

func (l *lastMileFootprint) get(ctx context.Context) (*fp.Report, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.done {
		l.done = true
		l.rep, l.err = l.load(ctx)
	}
	return l.rep, l.err
}

// lastMileRetryAfter is how long after a run the budget cut off the next one
// waits. lastmile.Run alone would retry on the very next invocation, so an
// account whose check never fits the budget would hold every command for the
// whole budget; this bounds that to one wait an hour.
const lastMileRetryAfter = time.Hour

// lastMileMaybeDue reports whether lastmile.Run could have anything to check
// for spawn in account, from the rate-limit state file alone, so that a run
// that is not due costs no AWS call (not even the DescribeRegions Regions
// needs). It reads the format libs/lastmile documents (version 1).
//
// It is not due within lastMileRetryAfter of a run the budget cut off (the
// scope it was in is marked incomplete, with the attempt's time). Otherwise
// it is due unless the global scope, which every run checks last, was checked
// to the end within the minimum interval; a region that falls due before it
// waits at most until then. Anything it cannot read is due, and lastmile.Run
// makes the exact decision.
func lastMileMaybeDue(path, account string, now time.Time) bool {
	f, err := os.Open(path) // #nosec G304 -- the caller's own lastmile state file
	if err != nil {
		return true
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return true
	}
	var st struct {
		Version int `json:"version"`
		Runs    map[string]struct {
			LastRun     time.Time `json:"last_run"`
			LastAttempt time.Time `json:"last_attempt"`
			Incomplete  string    `json:"incomplete"`
		} `json:"runs"`
	}
	if json.Unmarshal(b, &st) != nil || st.Version != 1 {
		return true
	}
	prefix := string(tags.ToolSpawn) + "/" + account + "/"
	for key, rec := range st.Runs {
		if strings.HasPrefix(key, prefix) && rec.Incomplete != "" &&
			!rec.LastAttempt.After(now) && now.Sub(rec.LastAttempt) < lastMileRetryAfter {
			return false
		}
	}
	rec, ok := st.Runs[prefix+lifecycle.ScopeGlobal]
	if !ok || rec.Incomplete != "" || rec.LastRun.IsZero() || rec.LastRun.After(now) {
		return true
	}
	return now.Sub(rec.LastRun) >= lastmile.DefaultMinInterval
}

// lastMileLog writes the last mile's lines to w (stderr under --verbose, else
// nowhere), each prefixed "debug: ". After close it drops them, so a run the
// hook stopped waiting for cannot write once the command has moved on.
type lastMileLog struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (l *lastMileLog) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil || l.closed {
		return
	}
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	if strings.Contains(msg, lastmilespawn.NotAsked) {
		return // the first refusal was logged; the rest repeat it
	}
	if lastmilespawn.NotPermitted(msg) {
		// What the caller's identity does not grant is expected: a debug
		// note, not a warning.
		msg = strings.Replace(msg, "lastmile: warning: ", "lastmile: not permitted, skipped: ", 1)
	}
	_, _ = fmt.Fprintf(l.w, "debug: %s\n", msg)
}

func (l *lastMileLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
}
