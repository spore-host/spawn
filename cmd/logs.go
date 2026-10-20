package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

var (
	logsWhich   string
	logsLines   int
	logsConsole bool
	logsNoCache bool
)

// consoleCacheTTL is how long a fetched console dump is kept on disk.
//
// Seven days, pruned on every invocation. Long enough that a Friday-evening
// failure is still readable on Monday; short enough that ~/.spawn/cache cannot
// grow without bound. Pruning on invocation rather than on a timer means no
// daemon and no new moving part.
//
// The existing precedent (pkg/plugin/indexcache.go) uses 6h, which is right for
// a package index that goes stale. A diagnostic does not go stale — it describes
// a failure that already happened — and 6h would expire it before AWS does:
// measured, the console output of instances DescribeInstances has entirely
// forgotten was still served more than twelve hours later. A cache that expires
// before its source is worse than no cache, because it teaches you not to trust
// it.
const consoleCacheTTL = 7 * 24 * time.Hour

var logsCmd = &cobra.Command{
	Use:   "logs <name-or-instance-id>",
	Short: "Show a spore's command or spored log, including after it has terminated",
	Long: `Fetch a spore's log.

While the instance is alive this tails the log directly, over SSH when a local key
resolves and over SSM otherwise — the same path 'spawn array logs' uses.

Once the instance is GONE, that is impossible: the log died with it. spored
writes the last 50 lines of a FAILED job's command log to the serial console
before terminating (#736), and this reads it back out, so "why did my job fail?"
is answerable after the fact without knowing that get-console-output exists.

The serial console capture is not immediate — allow about five minutes after
termination. Fetched dumps are cached under ~/.spawn/cache/console for 7 days and
pruned on each run. Once EC2 has stopped listing the instance, the cache is
still found by name or instance ID.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if logsLines <= 0 {
			return fmt.Errorf("--lines must be positive")
		}
		logPath, err := arrayLogPath(logsWhich)
		if err != nil {
			return err
		}

		client, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}

		pruneConsoleCache()

		// A live instance has the real log; prefer it. resolveInstance's default
		// state filter excludes terminated and shutting-down, so a hit here means
		// genuinely reachable.
		instance, err := resolveInstance(ctx, client, args[0])
		if err == nil {
			out, rerr := runArrayMemberCommand(ctx, client, instance,
				fmt.Sprintf("tail -n %d %s", logsLines, logPath))
			if rerr != nil {
				return fmt.Errorf("fetch log from %s: %w", instance.InstanceID, rerr)
			}
			fmt.Print(out)
			return nil
		}

		// Gone. Fall back to the console, which outlives the instance.
		return showTerminatedLog(ctx, client, args[0], os.Stdout, os.Stderr)
	},
}

// findRecentlyGone locates a terminated or shutting-down instance and, when EC2
// reports one, its termination time.
//
// The time matters for exactly one decision: whether an absent log means "the job
// did not fail" or "the console capture has not populated yet". Those send the
// user in opposite directions, and the capture takes ~4-5 minutes — so an
// immediate read is indistinguishable from a missing log without it.
//
// EC2 embeds the moment in StateTransitionReason, e.g.
// "User initiated (2026-10-08 16:48:02 GMT)". It is not always present, so a
// zero time means "do not guess" rather than "just now".
func findRecentlyGone(ctx context.Context, client *aws.Client, identifier string) (*aws.InstanceInfo, time.Time) {
	all, err := listInstancesForLookup(ctx, client, lookupRegion(), "all")
	if err != nil {
		return nil, time.Time{}
	}
	best := selectRecentlyGone(all, identifier)
	if best == nil {
		return nil, time.Time{}
	}
	return best, parseTransitionTime(best.StateTransitionReason)
}

// parseTransitionTime pulls the timestamp out of a StateTransitionReason.
//
// Format per EC2: "<reason> (YYYY-MM-DD HH:MM:SS GMT)". Returns the zero time on
// anything unexpected — a wrong time here would produce a confidently wrong
// "try again shortly", which is worse than no hint at all.
func parseTransitionTime(reason string) time.Time {
	open := strings.LastIndex(reason, "(")
	close := strings.LastIndex(reason, ")")
	if open < 0 || close < open {
		return time.Time{}
	}
	inner := strings.TrimSpace(reason[open+1 : close])
	inner = strings.TrimSuffix(inner, " GMT")
	t, err := time.Parse("2006-01-02 15:04:05", inner)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// showTerminatedLog reads a terminated instance's console output and prints the
// command-log block spored wrote before it died.
//
// When EC2 no longer lists the instance at all, the local cache is consulted by
// ID or name before giving up (#1160). That is the case the cache exists for —
// keeping the diagnostic after AWS forgets the instance — so it must not sit
// behind a lookup that requires AWS to remember it.
func showTerminatedLog(ctx context.Context, client *aws.Client, identifier string, stdout, stderr io.Writer) error {
	gone, when := findRecentlyGone(ctx, client, identifier)
	if gone == nil {
		if !logsNoCache {
			if id, raw, ok := cachedConsoleFor(identifier); ok {
				fmt.Fprintf(stderr, "EC2 no longer lists %s; showing the console output cached for %s\n(from the local cache)\n", identifier, id)
				printConsoleLog(stdout, stderr, raw, time.Time{})
				return nil
			}
		}
		return fmt.Errorf("no instance found matching %q, alive or recently terminated, and nothing cached for it under ~/.spawn/cache/console", identifier)
	}

	fmt.Fprintf(stderr, "%s (%s) is %s", gone.Name, gone.InstanceID, gone.State)
	if reason := strings.TrimSpace(gone.StateTransitionReason); reason != "" {
		fmt.Fprintf(stderr, ": %s", reason)
	}
	fmt.Fprintln(stderr)

	raw, cached, err := consoleOutputCached(ctx, client, gone.Region, gone.InstanceID, gone.Name)
	if err != nil {
		return consoleReadError(err)
	}
	if cached {
		fmt.Fprintf(stderr, "(from the local cache)\n")
	}
	printConsoleLog(stdout, stderr, raw, when)
	return nil
}

// consoleReadError wraps a console-output failure, naming the permission when it
// is a denial: ec2:GetConsoleOutput is not in the minimal IAM baseline (the site
// lists it as an optional addition), so "access denied" is the expected failure
// for a least-privilege user and should say what to add (#1160).
func consoleReadError(err error) error {
	if aws.IsAccessDenied(err) {
		return fmt.Errorf("read console output: %w\n\nReading a terminated spore's log needs ec2:GetConsoleOutput, which the minimal spawn IAM policy does not include — add it to read logs after termination", err)
	}
	return fmt.Errorf("read console output: %w", err)
}

// printConsoleLog prints the command-log block of a console dump (or the whole
// dump under --console). when is the termination time, zero when unknown.
func printConsoleLog(stdout, stderr io.Writer, raw string, when time.Time) {
	if logsConsole {
		fmt.Fprint(stdout, raw)
		return
	}

	body, ok := taskproto.ExtractConsoleLog(raw)
	if ok {
		fmt.Fprintf(stderr, "\n")
		fmt.Fprintln(stdout, body)
		return
	}

	// No block. Two very different causes, and saying the wrong one sends the
	// user down the wrong path — so distinguish them by how long ago it died
	// rather than reporting a flat "no log".
	if !when.IsZero() && time.Since(when) < 6*time.Minute {
		fmt.Fprintf(stderr,
			"\nNo log yet. The serial console capture takes about five minutes after\n"+
				"termination to appear, and this instance went away %s ago — try again shortly.\n",
			time.Since(when).Round(time.Second))
		return
	}
	fmt.Fprintf(stderr,
		"\nNo command log on the console. spored writes one only when the workload\n"+
			"FAILED, so this usually means the job succeeded or never ran a --command.\n"+
			"Use --console to see the raw boot output (%d bytes), which covers\n"+
			"failures that happened before the workload started.\n", len(raw))
}

// consoleOutputCached returns a console dump, from ~/.spawn/cache when fresh.
//
// Cached because the fetch is the slow part of a command a user runs repeatedly
// while reading a failure, and because it keeps the diagnostic available if AWS
// eventually drops it. Never cached EMPTY: an empty dump means the capture has
// not populated yet, and caching that would pin the "no log yet" answer for a
// week.
func consoleOutputCached(ctx context.Context, client *aws.Client, region, instanceID, name string) (string, bool, error) {
	path := consoleCachePath(instanceID)
	if !logsNoCache && path != "" {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b), true, nil
		}
	}
	raw, err := client.GetConsoleOutput(ctx, region, instanceID)
	if err != nil {
		return "", false, err
	}
	if raw != "" {
		writeConsoleCache(instanceID, name, raw)
	}
	return raw, false, nil
}

// writeConsoleCache stores a dump as <id>.log, and the instance's name beside it
// as <id>.name so a later `spawn logs <name>` can find it once EC2 has forgotten
// the instance (#1160). Best-effort: an unwritable cache must not fail the read.
func writeConsoleCache(instanceID, name, raw string) {
	path := consoleCachePath(instanceID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		return
	}
	if name != "" {
		_ = os.WriteFile(strings.TrimSuffix(path, ".log")+".name", []byte(name), 0o600)
	}
}

// cachedConsoleFor finds a cached dump by instance ID, or by the name recorded
// beside it — the newest dump when several instances shared the name.
func cachedConsoleFor(identifier string) (instanceID, raw string, ok bool) {
	if strings.HasPrefix(identifier, "i-") {
		path := consoleCachePath(identifier)
		if path == "" {
			return "", "", false
		}
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 { //nolint:gosec // path under ~/.spawn/cache, ID from the caller
			return identifier, string(b), true
		}
		return "", "", false
	}
	dir := filepath.Dir(consoleCachePath("x"))
	if dir == "." {
		return "", "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", false
	}
	var newest time.Time
	for _, e := range entries {
		id, isName := strings.CutSuffix(e.Name(), ".name")
		if !isName {
			continue
		}
		n, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // entry of our own cache dir
		if err != nil || !strings.EqualFold(strings.TrimSpace(string(n)), identifier) {
			continue
		}
		logPath := filepath.Join(dir, id+".log")
		info, err := os.Stat(logPath)
		if err != nil || info.Size() == 0 || (ok && !info.ModTime().After(newest)) {
			continue
		}
		b, err := os.ReadFile(logPath) //nolint:gosec // entry of our own cache dir
		if err != nil {
			continue
		}
		instanceID, raw, ok, newest = id, string(b), true, info.ModTime()
	}
	return instanceID, raw, ok
}

func consoleCachePath(instanceID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".spawn", "cache", "console", instanceID+".log")
}

// pruneConsoleCache deletes cached dumps older than consoleCacheTTL.
//
// On every invocation, so the cache cannot outlive its own policy without a
// daemon. "Leave no trace" applies to the user's disk too: an unexpiring pile of
// console dumps in ~/.spawn is the same un-reaped-footprint problem this project
// treats as a design flaw elsewhere, just not billed to anyone.
func pruneConsoleCache() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".spawn", "cache", "console")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-consoleCacheTTL)
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

func init() {
	logsCmd.Flags().StringVar(&logsWhich, "which", "command", "Which log: command or spored")
	logsCmd.Flags().IntVar(&logsLines, "lines", 100, "Lines to tail from a LIVE instance (the console block is fixed at what spored wrote)")
	logsCmd.Flags().BoolVar(&logsConsole, "console", false, "Print the whole serial console dump, not just the command-log block")
	logsCmd.Flags().BoolVar(&logsNoCache, "no-cache", false, "Ignore the local cache and re-fetch from AWS")
	// No local --region here. One was registered and never read, which SHADOWED
	// the persistent root --region: `spawn logs x --region us-west-2` bound to the
	// dead local flag, so the value never reached SetSharedFlags and the global
	// mechanism never saw it. There was therefore no working --region on this
	// command at all (#807). Deleting the local flag is the fix — the persistent
	// one then applies, as it does everywhere else.
	rootCmd.AddCommand(logsCmd)
}
