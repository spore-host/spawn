package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spore-host/spawn/pkg/aws"
)

// newTableWriter returns a tabwriter configured with spawn's standard column
// padding, so table output is consistent across commands. Callers write
// tab-separated rows and must Flush() when done.
func newTableWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// sporedSSHOptions returns the ssh -o options shared by the non-interactive
// spored-exec call sites (status/config/extend/queue): a short-lived,
// throwaway-host-key connection used to run a one-shot `spored` command and
// capture its output. These are deliberately NOT the options used by the
// interactive `spawn connect` path or the launch/plugin paths, which layer on
// ControlMaster / accept-new / BatchMode for their own reasons — do not route
// those through this helper.
func sporedSSHOptions() []string {
	return []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
	}
}

// parseKVTags parses repeated "key=value" flag values into a tag map (#161).
// The value may itself contain '=' (split on the first only). Keys must be
// non-empty and must not use the reserved "spawn:" prefix (those are managed by
// spawn). Returns a fresh map; nil/empty input yields an empty map.
func parseKVTags(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		i := strings.IndexByte(p, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid --tag %q: expected key=value", p)
		}
		key := strings.TrimSpace(p[:i])
		val := p[i+1:]
		if key == "" {
			return nil, fmt.Errorf("invalid --tag %q: empty key", p)
		}
		if strings.HasPrefix(strings.ToLower(key), "spawn:") {
			return nil, fmt.Errorf("invalid --tag %q: the spawn: prefix is reserved", p)
		}
		out[key] = val
	}
	return out, nil
}

// confirmYes is the shared confirmation prompt for destructive commands
// (spawn#40 convention). When skip is true (the command's --yes/-y flag) it
// returns true without prompting. Otherwise it prompts on stderr and returns
// true only on an explicit yes; a read error or non-interactive/piped stdin
// (EOF) reads as "no", so an unattended invocation without --yes aborts rather
// than performing the destructive action silently.
func confirmYes(skip bool, prompt string) bool {
	if skip {
		return true
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// stdinIsInteractive reports whether stdin is a terminal (a character device).
// Used to refuse irreversible prompts (e.g. a Capacity Block purchase) on piped/
// non-interactive stdin rather than reading an EOF as anything but "abort".
func stdinIsInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// confirmTypedPhrase requires the user to type an EXACT phrase (trimmed of
// surrounding whitespace) on stdin to proceed — a stronger gate than y/N, used
// for irreversible high-cost actions like a Capacity Block purchase (#217).
// Returns false on any mismatch, on a read error, or on non-interactive stdin
// (there is no --yes bypass for these gates). The prompt is printed to stderr.
func confirmTypedPhrase(reader *bufio.Reader, prompt, want string) bool {
	fmt.Fprint(os.Stderr, prompt)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	return strings.TrimSpace(line) == want
}

// resolveInstance finds an instance by ID or name
func resolveInstance(ctx context.Context, client *aws.Client, identifier string) (*aws.InstanceInfo, error) {
	fmt.Fprintf(os.Stderr, "Looking up instance %s...\n", identifier)

	instances, err := client.ListInstances(ctx, "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	// Check if identifier is an instance ID (starts with "i-")
	isInstanceID := strings.HasPrefix(identifier, "i-")

	var matches []aws.InstanceInfo
	for _, inst := range instances {
		if isInstanceID {
			// Exact match on instance ID
			if inst.InstanceID == identifier {
				return &inst, nil
			}
		} else {
			// Match on name (case-insensitive)
			if strings.EqualFold(inst.Name, identifier) {
				matches = append(matches, inst)
			}
		}
	}

	if isInstanceID {
		return nil, fmt.Errorf("instance %s not found (must be spawn-managed)", identifier)
	}

	// Handle name matches
	if len(matches) == 0 {
		return nil, fmt.Errorf("no instance found with name: %s", identifier)
	}

	if len(matches) == 1 {
		return &matches[0], nil
	}

	// Multiple matches — prefer running over stopped/terminated (fixes #313).
	// When a cluster is re-launched, old stopped instances share names with
	// the new running ones. Connect/status should target the running instance.
	var running []aws.InstanceInfo
	for _, inst := range matches {
		if inst.State == "running" {
			running = append(running, inst)
		}
	}
	if len(running) == 1 {
		return &running[0], nil
	}

	// Still ambiguous — show only running instances if any, else all
	candidates := running
	if len(candidates) == 0 {
		candidates = matches
	}
	fmt.Fprintf(os.Stderr, "\nMultiple instances found with name '%s':\n\n", identifier)
	for _, inst := range candidates {
		fmt.Fprintf(os.Stderr, "  %s (%s in %s, state: %s)\n",
			inst.InstanceID, inst.InstanceType, inst.Region, inst.State)
	}
	fmt.Fprintf(os.Stderr, "\nPlease use the specific instance ID instead.\n")

	return nil, fmt.Errorf("multiple instances found with name: %s", identifier)
}

// truncate shortens s to maxLen characters, replacing the tail with "..." when
// it would otherwise overflow.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
