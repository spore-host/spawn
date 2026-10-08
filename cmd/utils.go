package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spore-host/spawn/pkg/aws"
)

// ErrInstanceNotFound marks a resolveInstance failure where no instance matches
// a caller-supplied NAME, as distinct from a lookup that failed. Most callers
// (connect, dns, config, extend, …) correctly treat that as an error;
// `terminate` does not, because for a caller whose goal is "ensure this is not
// running", a name that matches nothing already satisfies it (spawn#648).
//
// Deliberately NOT set for an unknown instance ID. An ID is opaque and
// AWS-assigned, so one that matches nothing is a typo rather than an
// already-cleaned-up resource, and silently succeeding there would let someone
// believe they terminated a still-billing instance. `spawn terminate
// i-doesnotexist` therefore still exits non-zero, which test/e2e's negative
// matrix pins.
//
// A sentinel keeps the distinction out of error-string matching.
var ErrInstanceNotFound = errors.New("instance not found")

// notFoundError carries the human message unchanged while matching
// ErrInstanceNotFound under errors.Is. Wrapping with %w instead would append the
// sentinel's text and produce "no instance found with name: x: instance not
// found", so the type exists purely to keep the CLI output clean.
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string        { return e.msg }
func (e *notFoundError) Is(target error) bool { return target == ErrInstanceNotFound }

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

// resolveSSHUser picks the SSH login user for a Linux instance. An explicit
// override (a command's --user flag) wins; otherwise it uses the
// spawn:local-username tag the bootstrap created and installed the SSH key for,
// falling back to ec2-user only for instances launched before that tag existed.
//
// This is the single source of truth for "which user does spawn SSH in as", so
// the interactive `spawn connect` path (cmd/connect.go), resolveSSHTarget
// (cmd/ssh_target.go), and every non-interactive spored-trigger call site
// (extend's reload, config, status, arraygroup) agree and cannot drift.
//
// Hardcoding ec2-user at any of those sites is the #581 bug: on an Ubuntu AMI the
// login user is `ubuntu`, so a hardcoded-ec2-user SSH fails with "Permission
// denied (publickey)". For `spawn extend` that meant the on-box `spored reload`
// silently no-op'd and the instance kept its ORIGINAL TTL — a silent failure on
// a lifecycle-critical operation.
func resolveSSHUser(override string, instance *aws.InstanceInfo) string {
	if override != "" {
		return override
	}
	if u := instance.Tags["spawn:local-username"]; u != "" {
		return u
	}
	return "ec2-user"
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
		// EOF with nothing read: there was no answer, rather than an answer of
		// "no". Those are different and used to be indistinguishable — a script
		// that forgot --yes got `Aborted.`, which reads as a decision somebody
		// made instead of a flag somebody omitted (#737).
		//
		// Deliberately still only a MESSAGE change, and deliberately not gated on
		// stdinIsInteractive: `echo y | spawn terminate …` is a legitimate
		// scripted confirmation, and refusing to read a piped answer would break
		// it. The first version of this fix did exactly that, and the existing
		// confirmYes tests caught it — they feed stdin through an os.Pipe, which
		// is not a character device.
		fmt.Fprintf(os.Stderr, "\n  No answer on stdin — pass --yes to confirm non-interactively.\n")
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
		// Deliberately NOT a notFoundError. An instance ID is opaque and
		// AWS-assigned: you do not guess or reuse one, so an ID that resolves to
		// nothing means the caller is referring to something that never existed
		// here — overwhelmingly a typo. `terminate` must keep failing on that,
		// because exiting 0 would let a user (or a script) believe they stopped a
		// billing instance when they did not. A caller-assigned NAME is different;
		// see below (spawn#648).
		return nil, fmt.Errorf("instance %s not found (must be spawn-managed)", identifier)
	}

	// Handle name matches
	if len(matches) == 0 {
		// A NAME is a handle the caller chose, and its absence after cleanup is the
		// expected steady state — which is why `terminate` treats this as success
		// and an unknown ID as failure (spawn#648).
		return nil, &notFoundError{fmt.Sprintf("no instance found with name: %s", identifier)}
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
