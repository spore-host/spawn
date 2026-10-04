// EC2 instance and resource tag construction.

package aws

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// propagatableSnapshotTags filters a snapshot's tags down to the ones worth
// copying onto a volume created from it (#161): everything EXCEPT the snapshot's
// own Name and any spawn:* baseline, so the volume isn't stamped with the
// snapshot's identity (which would confuse Cost Explorer / cleanup tooling).
func propagatableSnapshotTags(snapTags []types.Tag) []types.Tag {
	var out []types.Tag
	for _, t := range snapTags {
		k := aws.ToString(t.Key)
		if k == "Name" || strings.HasPrefix(strings.ToLower(k), "spawn:") {
			continue
		}
		out = append(out, types.Tag{Key: t.Key, Value: t.Value})
	}
	return out
}

func buildTags(config LaunchConfig, accountID, userARN, accountNameSlug string) []types.Tag {
	// buildTags assembles the spawn: metadata tag set in sections:
	//   1. Base identity (always present): managed/root/created-by/version/account/iam-user.
	//   2. Naming & DNS: Name, account-name alias, dns-name.
	//   3. Lifecycle: launch-time, ttl(+deadline), idle-timeout, on-complete, pre-stop, hibernation.
	//   4. Notifications & activity signals: slack/notify-*, active-ports/processes.
	//   5. Storage: FSx/EFS mount metadata.
	//   6. Job-array / sweep: array id/name/index and per-run parameters.
	//   7. User-supplied custom tags (appended last).

	// --- Section 1: base identity (always present) ---
	// Convert account ID to base36 for DNS namespace
	accountBase36 := intToBase36(accountID)

	tags := []types.Tag{
		{Key: aws.String("spawn:managed"), Value: aws.String("true")},
		{Key: aws.String("spawn:root"), Value: aws.String("true")},
		{Key: aws.String("spawn:created-by"), Value: aws.String("spawn")},
		{Key: aws.String("spawn:account-id"), Value: aws.String(accountID)},
		{Key: aws.String("spawn:account-base36"), Value: aws.String(accountBase36)},
		{Key: aws.String("spawn:iam-user"), Value: aws.String(userARN)}, // Per-user isolation
	}

	// spawn:version records the ACTUAL launching spawn's version (config.SpawnVersion,
	// set by the CLI from cmd.version()/buildinfo — pkg/aws can't import cmd to
	// resolve it itself). Previously hard-coded to the literal "0.1.0" regardless
	// of the real version, on every instance spawn has ever launched (spawn#515) —
	// worse than an absent tag, since it looks like an answer to "was this
	// instance launched by a spawn that predates fix X?" and gives the wrong one.
	// Omitted (not written as a false placeholder) when the caller doesn't supply
	// it, e.g. an older SDK integration.
	if config.SpawnVersion != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:version"), Value: aws.String(config.SpawnVersion)})
	}

	// Friendly account-name DNS segment, when the account has one and it
	// slugifies to a valid DNS label (#121). base36 stays canonical (it's always
	// valid and unique); the name is an alias the DNS updater can prefer for a
	// legible FQDN — {name}.{account-name}.spore.host instead of {name}.{base36}.
	if accountNameSlug != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:account-name"), Value: aws.String(accountNameSlug)})
	}

	// --- Section 2: naming & DNS ---
	if config.Name != "" {
		tags = append(tags, types.Tag{Key: aws.String("Name"), Value: aws.String(config.Name)})
	}

	// --- Section 3: lifecycle ---
	// Record the absolute launch time once — survives stop/wake cycles.
	launchTime := time.Now().UTC().Format(time.RFC3339)
	tags = append(tags, types.Tag{Key: aws.String("spawn:launch-time"), Value: aws.String(launchTime)})

	// Target OS — lets `spawn connect` choose the Windows (SSM + password) vs
	// Linux (SSH) path without re-describing the AMI, and documents the OS for
	// the reaper/dashboard.
	if config.TargetOS != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:os"), Value: aws.String(config.TargetOS)})
	}

	if config.TTL != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:ttl"), Value: aws.String(config.TTL)})
		// Compute the absolute deadline once at launch; spored uses this across stop/wake cycles
		// so that TTL is always relative to original launch time, never reset.
		if d, err := time.ParseDuration(config.TTL); err == nil {
			deadline := time.Now().Add(d).UTC().Format(time.RFC3339)
			tags = append(tags, types.Tag{Key: aws.String("spawn:ttl-deadline"), Value: aws.String(deadline)})
		}
	}

	if config.DNSName != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:dns-name"), Value: aws.String(config.DNSName)})
	}

	// --- Section 4: notifications & activity signals ---
	if config.SlackWorkspaceID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:slack-workspace-id"), Value: aws.String(config.SlackWorkspaceID)})
	}
	if config.NotifyURL != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:notify-url"), Value: aws.String(config.NotifyURL)})
	}
	if config.NotifyCommand != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:notify-command"), Value: aws.String(config.NotifyCommand)})
	}
	if config.NotifyPlatform != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:notify-platform"), Value: aws.String(config.NotifyPlatform)})
	}
	if config.ActivePortsRaw != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:active-ports"), Value: aws.String(config.ActivePortsRaw)})
	}
	if config.ActiveProcessesRaw != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:active-processes"), Value: aws.String(config.ActiveProcessesRaw)})
	}
	// The command rides the spawn:command tag only when it fits EC2's 256-char
	// tag-value cap. A longer --command is delivered via user-data instead (the
	// bootstrap embeds it and prefers the embedded file over this tag), so we must
	// NOT write an oversized tag here — that fails RunInstances outright (#214/#246).
	// Short commands keep the tag for observability + the parameter-sweep path.
	if config.JobArrayCommand != "" && len(config.JobArrayCommand) <= 256 {
		tags = append(tags, types.Tag{Key: aws.String("spawn:command"), Value: aws.String(config.JobArrayCommand)})
	}
	if config.DCVSessionID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:dcv-session-id"), Value: aws.String(config.DCVSessionID)})
	}
	if config.AppName != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:app-name"), Value: aws.String(config.AppName)})
	}
	// Web-UI app mode (#590): spored probes ReadyPort + fronts it with a TLS proxy.
	if config.AppMode != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:app-mode"), Value: aws.String(config.AppMode)})
	}
	if config.ReadyPort > 0 {
		tags = append(tags, types.Tag{Key: aws.String("spawn:ready-port"), Value: aws.String(strconv.Itoa(config.ReadyPort))})
	}
	if config.ReadyHealthPath != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:ready-path"), Value: aws.String(config.ReadyHealthPath)})
	}
	if config.WebAuth != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:web-auth"), Value: aws.String(config.WebAuth)})
	}

	// --- Section 5: storage ---
	// Storage filesystem tags — written so instance scripts can auto-mount
	// without needing the filesystem ID hardcoded (fixes #314).
	if config.FSxLustreID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-id"), Value: aws.String(config.FSxLustreID)})
		mp := config.FSxMountPoint
		if mp == "" {
			mp = "/fsx"
		}
		tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-mount-point"), Value: aws.String(mp)})
		if config.FSxMountName != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-mount-name"), Value: aws.String(config.FSxMountName)})
		}
	}
	// Ephemeral async FSx (#194): the filesystem is still CREATING at launch, so
	// instead of spawn:fsx-id we tag spawn:fsx-pending + the mount point and the
	// import/export paths. spored polls until AVAILABLE, sets up the DRA, mounts,
	// then flips the tag to spawn:fsx-id (reaper refcount, #192).
	if config.FSxPending != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-pending"), Value: aws.String(config.FSxPending)})
		mp := config.FSxMountPoint
		if mp == "" {
			mp = "/fsx"
		}
		tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-mount-point"), Value: aws.String(mp)})
		if config.FSxImportPath != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-s3-import-path"), Value: aws.String(config.FSxImportPath)})
		}
		if config.FSxExportPath != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-s3-export-path"), Value: aws.String(config.FSxExportPath)})
		}
	}
	if config.EFSID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:efs-id"), Value: aws.String(config.EFSID)})
		mp := config.EFSMountPoint
		if mp == "" {
			mp = "/efs"
		}
		tags = append(tags, types.Tag{Key: aws.String("spawn:efs-mount-point"), Value: aws.String(mp)})
	}

	if config.IdleTimeout != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:idle-timeout"), Value: aws.String(config.IdleTimeout)})
	}

	if config.HibernateOnIdle {
		tags = append(tags, types.Tag{Key: aws.String("spawn:hibernate-on-idle"), Value: aws.String("true")})
	}

	// Completion signal settings. spawn:completion-file is gated on OnComplete
	// being set too, not just CompletionFile being non-empty: --completion-file
	// has a non-empty flag default ("/tmp/SPAWN_COMPLETE", cmd/launch_flags.go),
	// so a launch that never asked for --on-complete previously still got a
	// spawn:completion-file tag — a watch with no action attached. That reads,
	// to an operator or anything summarizing tags, as "this instance has a
	// completion path" when touching the file does nothing (spawn#515). The
	// pair is atomic: a completion file is meaningless without an action.
	if config.OnComplete != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:on-complete"), Value: aws.String(config.OnComplete)})
		if config.CompletionFile != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:completion-file"), Value: aws.String(config.CompletionFile)})
		}
	}

	if config.CompletionDelay != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:completion-delay"), Value: aws.String(config.CompletionDelay)})
	}

	// Pre-stop hook
	if config.PreStop != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:pre-stop"), Value: aws.String(config.PreStop)})
		if config.PreStopTimeout != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:pre-stop-timeout"), Value: aws.String(config.PreStopTimeout)})
		}
	}

	// Spot-interruption webhook (#228): only tagged when a URL is set (opt-in).
	// The correlation blob and timeout are companions, meaningful only with a URL.
	if config.SpotInterruptionWebhookURL != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:spot-webhook-url"), Value: aws.String(config.SpotInterruptionWebhookURL)})
		if config.WebhookCorrelation != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:webhook-correlation"), Value: aws.String(config.WebhookCorrelation)})
		}
		if config.WebhookTimeout != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:webhook-timeout"), Value: aws.String(config.WebhookTimeout)})
		}
	}

	// Completion webhook (#497): only tagged when a URL is set (opt-in). Shares
	// spawn:webhook-correlation/spawn:webhook-timeout with the spot webhook above.
	if config.CompletionWebhookURL != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:completion-webhook-url"), Value: aws.String(config.CompletionWebhookURL)})
		if config.WebhookCorrelation != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:webhook-correlation"), Value: aws.String(config.WebhookCorrelation)})
		}
		if config.WebhookTimeout != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:webhook-timeout"), Value: aws.String(config.WebhookTimeout)})
		}
	}

	// Record the instance's primary user so spored can run the pre-stop hook as
	// that user rather than root (#63). Tagged whenever known.
	if config.Username != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:local-username"), Value: aws.String(config.Username)})
	}

	// Always tag the on-demand price — used by spored for effective cost calculation.
	if config.PricePerHour > 0 {
		tags = append(tags, types.Tag{Key: aws.String("spawn:price-per-hour"), Value: aws.String(fmt.Sprintf("%.6f", config.PricePerHour))})
	}
	if config.CostLimit > 0 {
		tags = append(tags, types.Tag{Key: aws.String("spawn:cost-limit"), Value: aws.String(fmt.Sprintf("%.4f", config.CostLimit))})
	}

	// Session management
	if config.SessionTimeout != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:session-timeout"), Value: aws.String(config.SessionTimeout)})
	}

	// Job array tags
	// --- Section 6: job-array / sweep ---
	if config.JobArrayID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-id"), Value: aws.String(config.JobArrayID)})
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-name"), Value: aws.String(config.JobArrayName)})
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-size"), Value: aws.String(fmt.Sprintf("%d", config.JobArraySize))})
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-index"), Value: aws.String(fmt.Sprintf("%d", config.JobArrayIndex))})
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-created"), Value: aws.String(time.Now().Format(time.RFC3339))})
	}

	// Parameter sweep tags
	if config.SweepID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:sweep-id"), Value: aws.String(config.SweepID)})
		tags = append(tags, types.Tag{Key: aws.String("spawn:sweep-name"), Value: aws.String(config.SweepName)})
		tags = append(tags, types.Tag{Key: aws.String("spawn:sweep-size"), Value: aws.String(fmt.Sprintf("%d", config.SweepSize))})
		tags = append(tags, types.Tag{Key: aws.String("spawn:sweep-index"), Value: aws.String(fmt.Sprintf("%d", config.SweepIndex))})
	}

	// Dedupe everything the sections above appended, keeping the first
	// occurrence. EC2 rejects the entire RunInstances call on a repeated key
	// ("Duplicate tag key '<k>' specified"), and the spot-interruption and
	// completion webhook blocks both emit spawn:webhook-correlation and
	// spawn:webhook-timeout — so setting both URLs failed the launch with two
	// flags. Done as a pass here rather than by threading appendTagUnique through
	// every section, so a future section cannot forget to use it.
	tags = dedupeTags(tags)

	// --- Section 7: user-supplied custom tags ---
	// Sorted so the set is deterministic, and deduped against spawn's own keys:
	// the --tag help states the spawn: prefix is reserved, so a collision is a
	// caller error rather than an override.
	for _, k := range sortedKeys(config.Tags) {
		tags = appendTagUnique(tags, k, config.Tags[k])
	}

	// --- Section 8: sweep parameter tags, LAST and budgeted (#477) ---
	//
	// AWS caps a resource at 50 tags and FAILS RunInstances outright when
	// exceeded — it does not truncate. The old code capped parameters at 35 with
	// a comment claiming that "stays under AWS 50-tag limit", but counted only
	// the parameters and ignored the ~45 tags the sections above had already
	// appended; a maximal launch with 60 parameters emitted 81 tags.
	//
	// Emitted last so the cap can be computed against what the launch actually
	// used, and in SORTED order so the surviving subset is deterministic. The old
	// loop ranged over the map directly, so which parameters survived depended on
	// Go's randomised iteration order — two members of one sweep could record
	// different points in the parameter space.
	//
	// Parameters are the only droppable class here: spawn:* lifecycle tags are
	// spored's contract (dropping spawn:ttl would disable TTL enforcement and
	// bill open-endedly), and --tag is an explicit request. spawn:param:* is a
	// convenience record of the sweep point, and the sweep manifest remains
	// authoritative — so a truncated tag set beats a launch that cannot start.
	if config.SweepID != "" && len(config.Parameters) > 0 {
		budget := awsMaxTagsPerResource - len(tags)
		if budget < 0 {
			budget = 0
		}
		keys := sortedKeys(config.Parameters)
		emitted := 0
		for _, k := range keys {
			if emitted >= budget {
				break
			}
			before := len(tags)
			tags = appendTagUnique(tags, "spawn:param:"+k, config.Parameters[k])
			if len(tags) > before {
				emitted++
			}
		}
		if dropped := len(keys) - emitted; dropped > 0 {
			fmt.Fprintf(os.Stderr,
				"⚠️  %d of %d sweep parameters were not tagged: the launch already uses %d of "+
					"AWS's %d tags per resource.\n   The instance still runs with the full "+
					"parameter set in its environment; only the spawn:param:* record is "+
					"truncated (first %d by name kept).\n",
				dropped, len(keys), len(tags)-emitted, awsMaxTagsPerResource, emitted)
		}
	}

	return tags
}

// awsMaxTagsPerResource is AWS's hard per-resource tag cap. Exceeding it makes
// RunInstances fail outright — EC2 does not silently truncate — and the same
// slice is applied to both the instance and its volumes.
const awsMaxTagsPerResource = 50

// appendTagUnique appends key=value unless key is already present, keeping the
// FIRST occurrence.
//
// Duplicate keys are not merely untidy: EC2 rejects the whole RunInstances call
// with "Duplicate tag key '<k>' specified". The spot-interruption and completion
// webhook blocks both emitted spawn:webhook-correlation and
// spawn:webhook-timeout, so setting both URLs failed the launch with just two
// flags — no tag-limit arithmetic required. Both blocks read the same config
// fields, so first-wins and last-wins are identical in value here; first-wins
// keeps the output order stable.
func appendTagUnique(tags []types.Tag, key, value string) []types.Tag {
	for _, t := range tags {
		if t.Key != nil && *t.Key == key {
			return tags
		}
	}
	return append(tags, types.Tag{Key: aws.String(key), Value: aws.String(value)})
}

// sortedKeys returns m's keys in sorted order, so anything derived from a Go map
// is deterministic rather than dependent on randomised iteration order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// managedResourceTags builds the standard spawn:managed tag set for a secondary
// AWS resource (key pair, IAM role, table, …) so cleanup can find and attribute
// it. Mirrors the per-instance baseline (spawn:managed/created-by/account/
// iam-user) plus spawn:created-at, and merges any extra tags. Identity lookup is
// best-effort — a resource still gets spawn:managed=true even if STS is slow.
func (c *Client) managedResourceTags(ctx context.Context, extra map[string]string) []types.Tag {
	out := map[string]string{
		"spawn:managed":    "true",
		"spawn:created-by": "spawn",
		"spawn:created-at": time.Now().UTC().Format(time.RFC3339),
	}
	if accountID, userARN, err := c.GetCallerIdentityInfo(ctx); err == nil {
		out["spawn:account-id"] = accountID
		out["spawn:iam-user"] = userARN
	}
	for k, v := range extra {
		out[k] = v
	}
	tags := make([]types.Tag, 0, len(out))
	for k, v := range out {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return tags
}

// AccountBase36 converts an AWS account ID to its base36 form, the segment used
// in spawn's instance FQDNs ({name}.{account-base36}.spore.host). Exported
// wrapper over the internal helper for callers outside this package.
func AccountBase36(accountID string) string { return intToBase36(accountID) }

// intToBase36 converts a numeric string (AWS account ID) to base36
// Example: "942542972736" -> "c0zxr0ao"
func intToBase36(accountID string) string {
	// Parse account ID as integer
	num, err := strconv.ParseUint(accountID, 10, 64)
	if err != nil {
		// Fallback: return account ID as-is if parsing fails
		return accountID
	}

	// Convert to base36 (lowercase)
	return strconv.FormatUint(num, 36)
}

// slugifyDNSLabel converts an account name into a single DNS label safe for use
// as the FQDN segment {name}.{label}.spore.host, or "" if it can't produce a
// valid label. Rules (RFC 1035 label): lowercase; [a-z0-9-]; collapse runs of
// other chars to a single hyphen; no leading/trailing hyphen; 1–63 chars.
func slugifyDNSLabel(name string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			// Map any other char (space, _, ., etc.) to a single hyphen.
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 63 {
		slug = strings.Trim(slug[:63], "-")
	}
	return slug
}

// dedupeTags removes repeated keys, keeping the first occurrence and preserving
// order. See appendTagUnique for why a duplicate key is launch-breaking.
func dedupeTags(tags []types.Tag) []types.Tag {
	seen := make(map[string]bool, len(tags))
	out := make([]types.Tag, 0, len(tags))
	for _, t := range tags {
		if t.Key == nil {
			continue
		}
		if seen[*t.Key] {
			continue
		}
		seen[*t.Key] = true
		out = append(out, t)
	}
	return out
}
