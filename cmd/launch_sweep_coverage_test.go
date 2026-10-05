package cmd

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// sweepFlagClass records, for every launchCmd flag, whether the PARAMETER-SWEEP
// path honours it — and when it does not, why that is acceptable or which issue
// tracks it.
//
// This manifest exists because the sweep path drops flags BY CONSTRUCTION
// (spawn#697): the dispatch builds a two-field LaunchConfig instead of calling
// buildLaunchConfig, and buildLaunchConfigFromParams merges rows onto an empty
// struct. So a flag works on a sweep only if someone hand-wrote a shim for it.
// That has been reported and patched one category at a time — #525 spend
// controls, #539 IAM (twice), #549 DNS, #667 networking (single-path only),
// #673/#674/#675 — and the default remains "dropped".
//
// A flag absent from this map fails the test. That is the point: adding a flag
// to launchCmd should force the question "does this work on a sweep?" to be
// answered once, deliberately, rather than discovered later by someone paying
// for a boot that ignored it.
type sweepFlagClass int

const (
	sweepHonoured sweepFlagClass = iota // reaches sweep rows
	sweepNA                             // not a sweep concept
	sweepKnownGap                       // dropped; tracked, with an issue in the note
)

// sweepFlagCoverage is the manifest. Keep it alphabetical.
var sweepFlagCoverage = map[string]struct {
	class sweepFlagClass
	note  string
}{
	// --- Honoured: read directly on the sweep path or via an applyCLI*ToSweep shim.
	"ami": {sweepHonoured, ""}, "az": {sweepHonoured, ""},
	"budget": {sweepHonoured, ""}, "command": {sweepHonoured, ""},
	"cost-limit": {sweepHonoured, "#525"}, "cost-tier": {sweepHonoured, ""},
	"count": {sweepHonoured, ""}, "detach": {sweepHonoured, ""},
	"estimate-only": {sweepHonoured, ""}, "hibernate": {sweepHonoured, ""},
	"iam-allow-full-access": {sweepHonoured, "#539"}, "iam-managed-policies": {sweepHonoured, "#539"},
	"iam-policy": {sweepHonoured, "#539"}, "iam-policy-file": {sweepHonoured, "#539"},
	"iam-role": {sweepHonoured, "#539"}, "iam-role-tags": {sweepHonoured, "#539"},
	"iam-trust-services": {sweepHonoured, "#539"}, "idle-timeout": {sweepHonoured, "#525"},
	"instance-profile": {sweepHonoured, ""}, "launch-delay": {sweepHonoured, ""},
	"max-concurrent": {sweepHonoured, ""}, "max-concurrent-auto": {sweepHonoured, ""},
	"max-concurrent-per-region": {sweepHonoured, ""}, "mode": {sweepHonoured, ""},
	"name": {sweepHonoured, ""}, "no-detach": {sweepHonoured, ""},
	"no-timeout": {sweepHonoured, ""}, "os": {sweepHonoured, ""},
	"output-id": {sweepHonoured, ""}, "param-file": {sweepHonoured, ""},
	"params": {sweepHonoured, ""}, "proximity-from": {sweepHonoured, ""},
	"region": {sweepHonoured, ""}, "regions-exclude": {sweepHonoured, ""},
	"regions-geographic": {sweepHonoured, ""}, "regions-include": {sweepHonoured, ""},
	"s3-read": {sweepHonoured, "#539"}, "s3-write": {sweepHonoured, "#539"},
	"spot": {sweepHonoured, ""}, "sweep-name": {sweepHonoured, ""},
	"ttl": {sweepHonoured, "#525"}, "volume-size": {sweepHonoured, ""},
	"wait": {sweepHonoured, ""}, "wait-timeout": {sweepHonoured, ""},

	// --- Not applicable: a different launch shape, or meaningless per-row.
	"batch-queue":    {sweepNA, "batch-queue is its own launch path; refused with --param-file"},
	"queue-template": {sweepNA, "see batch-queue"},
	"job-array-name": {sweepNA, "job arrays and sweeps are different shapes"},
	"instance-names": {sweepNA, "job-array naming; rows are named from the sweep index"},
	"min-viable":     {sweepNA, "cohort concept; sweep rows are independent"},
	"mpi":            {sweepNA, "MPI is an all-or-nothing cohort, not a sweep"},
	"mpi-command":    {sweepNA, "see mpi"}, "mpi-processes-per-node": {sweepNA, "see mpi"},
	"skip-mpi-install": {sweepNA, "see mpi"}, "auto-placement-group": {sweepNA, "see mpi"},
	"placement-group": {sweepNA, "see mpi"}, "efa": {sweepNA, "EFA is for MPI cohorts"},
	"interactive":        {sweepNA, "a sweep is non-interactive by definition"},
	"dry-run":            {sweepNA, "explicitly refused on the sweep path (#569)"},
	"print-config":       {sweepNA, "alias of dry-run"},
	"config":             {sweepNA, "plugin declarations file, resolved before dispatch"},
	"template-var":       {sweepNA, "queue-template substitution"},
	"terminate-on-error": {sweepNA, "batch-queue error policy"},
	"cartesian":          {sweepKnownGap, "#674 — parsed and read by nothing at all, so the sweep run COUNT is wrong"},
	"skip-region-check":  {sweepNA, "pre-dispatch validation toggle"},
	"quiet":              {sweepNA, "output verbosity, applied by the printer not the config"},

	// --- Known gaps: should work on a sweep and do not. All #697.
	"active-ports":             {sweepKnownGap, "#697 — idle-detection input; without it idle detection uses defaults"},
	"active-processes":         {sweepKnownGap, "#697 — see active-ports"},
	"allow-cidr":               {sweepKnownGap, "#697 — managed SG ingress CIDR"},
	"allow-cost-limit-overrun": {sweepKnownGap, "#697 — storage-vs-cap override"},
	"attach-volume":            {sweepKnownGap, "#697 — no way to attach a data volume to a sweep row"},
	"capacity-block":           {sweepKnownGap, "#697"},
	"completion-delay":         {sweepKnownGap, "#697"},
	"completion-webhook-url":   {sweepKnownGap, "#697 — off-node completion signal"},
	"dns":                      {sweepKnownGap, "#697"}, "dns-api-endpoint": {sweepKnownGap, "#697"},
	"dns-domain": {sweepKnownGap, "#697"}, "no-dns": {sweepKnownGap, "#549/#697"},
	"efs-id":                {sweepKnownGap, "#697 — a sweep cannot mount EFS AT ALL"},
	"efs-mount-options":     {sweepKnownGap, "#697 — see efs-id"},
	"efs-mount-point":       {sweepKnownGap, "#697 — see efs-id"},
	"efs-profile":           {sweepKnownGap, "#697 — see efs-id"},
	"fsx-create":            {sweepKnownGap, "#697 — a sweep cannot mount FSx AT ALL"},
	"fsx-export-path":       {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-id":                {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-import-path":       {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-lifecycle":         {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-mount-point":       {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-recall":            {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-s3-bucket":         {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-skip-validate":     {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-storage-capacity":  {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-throughput":        {sweepKnownGap, "#697 — see fsx-create"},
	"fsx-ttl":               {sweepKnownGap, "#697 — see fsx-create"},
	"hibernate-on-idle":     {sweepKnownGap, "#697 — has an on_idle/hibernate_on_idle param key"},
	"instance-type":         {sweepKnownGap, "#697 — passed in the minimal config, but rows override; see note in the test"},
	"key-name":              {sweepKnownGap, "#697 — has a key_name param key"},
	"key-pair":              {sweepKnownGap, "#697 — alias of key-name"},
	"nested-virtualization": {sweepKnownGap, "#697"},
	"notify-platform":       {sweepKnownGap, "#697"},
	"on-complete":           {sweepKnownGap, "#697 — has an on_complete param key; the FLAG is dropped"},
	"on-idle":               {sweepKnownGap, "#697"},
	"plugin":                {sweepKnownGap, "#697 — spored plugin declarations"},
	"pre-stop":              {sweepKnownGap, "#697 — DATA LOSS: the hook that syncs results before termination"},
	"pre-stop-timeout":      {sweepKnownGap, "#697 — see pre-stop"},
	"reservation-id":        {sweepKnownGap, "#697"}, "use-reservation": {sweepKnownGap, "#675 — read by nothing anywhere"},
	"security-group":     {sweepKnownGap, "#667/#697 — fixed for single launches, still dropped on sweeps"},
	"security-group-ids": {sweepKnownGap, "#667/#697 — see security-group"},
	"subnet":             {sweepKnownGap, "#667/#697 — see security-group"},
	"subnet-id":          {sweepKnownGap, "#667/#697 — see security-group"},
	"vpc":                {sweepKnownGap, "#673/#697 — read by nothing anywhere"},
	"slack-workspace":    {sweepKnownGap, "#697"},
	"spot-webhook-url":   {sweepKnownGap, "#697 — spot-interruption signal"},
	"strata-formation":   {sweepKnownGap, "#697"}, "strata-profile": {sweepKnownGap, "#697"},
	"strata-registry": {sweepKnownGap, "#697"},
	"tag":             {sweepKnownGap, "#697 — custom tags never reach sweep rows"},
	"team":            {sweepKnownGap, "#697 — team-shared access tagging"},
	"user-data-file":  {sweepKnownGap, "#697"},
	"spot-max-price":  {sweepKnownGap, "#697"},

	// Found by this gate on its first run — seven flags I had not classified,
	// which is the gate working rather than a gap in it.
	"completion-file":     {sweepKnownGap, "#697 — has a completion_file param key; the FLAG is dropped"},
	"session-timeout":     {sweepKnownGap, "#697 — has a session_timeout param key; the FLAG is dropped"},
	"user-data":           {sweepKnownGap, "#697 — has a user_data param key; the FLAG is dropped"},
	"wait-for-running":    {sweepKnownGap, "#697 — no param key either"},
	"wait-for-ssh":        {sweepKnownGap, "#697 — no param key either"},
	"webhook-correlation": {sweepKnownGap, "#697 — companion to the webhook URLs, also dropped"},
	"webhook-timeout":     {sweepKnownGap, "#697 — companion to the webhook URLs, also dropped"},
}

// TestEveryLaunchFlagIsClassifiedForSweeps forces the question "does this work on
// a sweep?" to be answered when a flag is added, rather than discovered later by
// someone paying for a boot that ignored it.
//
// This is the gate for spawn#697. The sweep path drops flags by construction, so
// a new flag's default behaviour is "silently ignored" — six separate issues have
// been filed for instances of that. A missing entry here is a build failure.
func TestEveryLaunchFlagIsClassifiedForSweeps(t *testing.T) {
	flags := launchCmdFlagNames(t)
	if len(flags) < 100 {
		t.Fatalf("only found %d launchCmd flags; the matcher is probably broken, which "+
			"would make this gate pass vacuously", len(flags))
	}

	var unclassified []string
	for _, f := range flags {
		if _, ok := sweepFlagCoverage[f]; !ok {
			unclassified = append(unclassified, f)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("%d launchCmd flag(s) have no sweep classification:\n  --%s\n\n"+
			"The sweep path builds a two-field LaunchConfig and merges rows onto an empty "+
			"struct, so an unclassified flag is SILENTLY DROPPED there (#697). Add it to "+
			"sweepFlagCoverage as honoured, N/A, or a known gap with an issue.",
			len(unclassified), strings.Join(unclassified, "\n  --"))
	}

	// And the reverse: a stale entry for a flag that no longer exists is rot.
	have := map[string]bool{}
	for _, f := range flags {
		have[f] = true
	}
	var stale []string
	for f := range sweepFlagCoverage {
		if !have[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("sweepFlagCoverage lists flags that launchCmd no longer registers: --%s",
			strings.Join(stale, " --"))
	}
}

// TestKnownSweepGapsCiteAnIssue: a gap without a tracking issue is just a bug
// nobody wrote down.
func TestKnownSweepGapsCiteAnIssue(t *testing.T) {
	issue := regexp.MustCompile(`#\d+`)
	for flag, c := range sweepFlagCoverage {
		if c.class != sweepKnownGap {
			continue
		}
		if !issue.MatchString(c.note) {
			t.Errorf("--%s is a known sweep gap with no issue in its note (%q)", flag, c.note)
		}
	}
}

// TestSweepHonouredFlagsReallyAreHonoured keeps the manifest honest in the
// direction that matters: a flag claimed as honoured must actually be referenced
// on the sweep path. Without this, the manifest could drift into wishful
// thinking — which is how #539's second half survived, since the fix listed the
// flags that existed when it was written.
func TestSweepHonouredFlagsReallyAreHonoured(t *testing.T) {
	bound := launchCmdFlagVars(t)
	sweepSrc := sweepPathSource(t)

	var lying []string
	for flag, c := range sweepFlagCoverage {
		if c.class != sweepHonoured {
			continue
		}
		v, ok := bound[flag]
		if !ok {
			continue // covered by the stale check above
		}
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(v) + `\b`).MatchString(sweepSrc) {
			lying = append(lying, fmt.Sprintf("--%s (var %s)", flag, v))
		}
	}
	sort.Strings(lying)
	if len(lying) > 0 {
		t.Errorf("these flags are classified as honoured on the sweep path but their "+
			"variables are never referenced there:\n  %s\n\nEither wire them up or "+
			"reclassify them as a known gap.", strings.Join(lying, "\n  "))
	}
}

// --- helpers ---

var flagBindRE = regexp.MustCompile(`launchCmd\.(?:Persistent)?Flags\(\)\.\w+Var\(&(\w+),\s*"([a-z0-9-]+)"`)

func launchCmdFlagVars(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("launch_flags.go")
	if err != nil {
		t.Fatalf("read launch_flags.go: %v", err)
	}
	out := map[string]string{}
	for _, m := range flagBindRE.FindAllStringSubmatch(string(b), -1) {
		if _, dup := out[m[2]]; !dup {
			out[m[2]] = m[1]
		}
	}
	return out
}

func launchCmdFlagNames(t *testing.T) []string {
	t.Helper()
	m := launchCmdFlagVars(t)
	out := make([]string, 0, len(m))
	for f := range m {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func sweepPathSource(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, f := range []string{"launch_sweep.go", "sweep.go", "sweep_keys.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		sb.Write(b)
	}
	if sb.Len() == 0 {
		t.Fatal("read no sweep-path source; the gate would pass vacuously")
	}
	return sb.String()
}
