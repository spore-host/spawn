package cmd

import (
	"context"
	"fmt"
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
	logsRegion  string
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
pruned on each run.`,
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
		return showTerminatedLog(ctx, client, args[0])
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
	all, err := client.ListInstances(ctx, "", "all")
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
func showTerminatedLog(ctx context.Context, client *aws.Client, identifier string) error {
	gone, when := findRecentlyGone(ctx, client, identifier)
	if gone == nil {
		return fmt.Errorf("no instance found matching %q, alive or recently terminated", identifier)
	}

	fmt.Fprintf(os.Stderr, "%s (%s) is %s", gone.Name, gone.InstanceID, gone.State)
	if reason := strings.TrimSpace(gone.StateTransitionReason); reason != "" {
		fmt.Fprintf(os.Stderr, ": %s", reason)
	}
	fmt.Fprintln(os.Stderr)

	raw, cached, err := consoleOutputCached(ctx, client, gone.Region, gone.InstanceID)
	if err != nil {
		return fmt.Errorf("read console output: %w", err)
	}
	if cached {
		fmt.Fprintf(os.Stderr, "(from the local cache)\n")
	}

	if logsConsole {
		fmt.Print(raw)
		return nil
	}

	body, ok := taskproto.ExtractConsoleLog(raw)
	if ok {
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Println(body)
		return nil
	}

	// No block. Two very different causes, and saying the wrong one sends the
	// user down the wrong path — so distinguish them by how long ago it died
	// rather than reporting a flat "no log".
	if !when.IsZero() && time.Since(when) < 6*time.Minute {
		fmt.Fprintf(os.Stderr,
			"\nNo log yet. The serial console capture takes about five minutes after\n"+
				"termination to appear, and this instance went away %s ago — try again shortly.\n",
			time.Since(when).Round(time.Second))
		return nil
	}
	fmt.Fprintf(os.Stderr,
		"\nNo command log on the console. spored writes one only when the workload\n"+
			"FAILED, so this usually means the job succeeded or never ran a --command.\n"+
			"Use --console to see the raw boot output (%d bytes), which covers\n"+
			"failures that happened before the workload started.\n", len(raw))
	return nil
}

// consoleOutputCached returns a console dump, from ~/.spawn/cache when fresh.
//
// Cached because the fetch is the slow part of a command a user runs repeatedly
// while reading a failure, and because it keeps the diagnostic available if AWS
// eventually drops it. Never cached EMPTY: an empty dump means the capture has
// not populated yet, and caching that would pin the "no log yet" answer for a
// week.
func consoleOutputCached(ctx context.Context, client *aws.Client, region, instanceID string) (string, bool, error) {
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
	if path != "" && raw != "" {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr == nil {
			// Best-effort: an unwritable cache must not fail the read.
			_ = os.WriteFile(path, []byte(raw), 0o600)
		}
	}
	return raw, false, nil
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
	logsCmd.Flags().StringVar(&logsRegion, "region", "", "AWS region (default: resolved as for other commands)")
	rootCmd.AddCommand(logsCmd)
}
