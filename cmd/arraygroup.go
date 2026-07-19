package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

// The `spawn array` group gives job arrays first-class reporting. Unlike sweeps
// (which have a DynamoDB record), array members are discovered by listing EC2 and
// grouping on the spawn:job-array-id / spawn:job-array-name tags — the same tags
// launch writes (pkg/aws/tags.go) and `spawn list --job-array-name` already
// filters on. Members are addressed by array NAME (the human-facing identifier);
// the id tag is the stable grouping key.

var (
	arrayRegion     string
	arrayJSON       bool
	arrayCancelYes  bool
	arrayCancelPend bool
	arrayCollectDir string
	arrayLogsIndex  int
	arrayLogsWhich  string
	arrayLogsLines  int
)

var arrayGroupCmd = &cobra.Command{
	Use:   "array",
	Short: "Manage job arrays (status, collect, cancel)",
	Long: `Manage job arrays launched with --job-array-name / --count.

Members are grouped by their job-array tags, so these work without any
server-side record:

  spawn array status data-proc
  spawn array logs data-proc --index 3
  spawn array collect data-proc ./results
  spawn array cancel data-proc --pending`,
}

// arrayMember is one instance in an array, with the fields status/collect need.
type arrayMember struct {
	Index      int
	InstanceID string
	Name       string
	State      string
	Region     string
	Params     map[string]string
}

// findArrayMembers lists instances (optionally in one region) and returns those
// whose job-array-name matches, sorted by index. The requested size comes from
// the spawn:job-array-size tag (the count the array was launched with), which is
// stamped identically on every member.
func findArrayMembers(ctx context.Context, name, region string) (members []arrayMember, requestedSize int, err error) {
	client, err := aws.NewClient(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("init AWS client: %w", err)
	}
	instances, err := client.ListInstances(ctx, region, "")
	if err != nil {
		return nil, 0, fmt.Errorf("list instances: %w", err)
	}
	for _, in := range instances {
		if in.JobArrayName != name {
			continue
		}
		idx, _ := strconv.Atoi(in.JobArrayIndex)
		if in.JobArraySize != "" {
			if sz, e := strconv.Atoi(in.JobArraySize); e == nil && sz > requestedSize {
				requestedSize = sz
			}
		}
		members = append(members, arrayMember{
			Index:      idx,
			InstanceID: in.InstanceID,
			Name:       in.Name,
			State:      in.State,
			Region:     in.Region,
			Params:     in.Parameters,
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Index < members[j].Index })
	return members, requestedSize, nil
}

// missingIndexes returns the requested indexes (0..size-1) that have no live
// member — the sparse-index gap a --min-viable partial launch leaves behind.
func missingIndexes(members []arrayMember, size int) []int {
	present := make(map[int]bool, len(members))
	for _, m := range members {
		present[m.Index] = true
	}
	var missing []int
	for i := 0; i < size; i++ {
		if !present[i] {
			missing = append(missing, i)
		}
	}
	return missing
}

// memberByIndex returns the member at the given index, or ok=false when no live
// member holds that index (a --min-viable gap, or already terminated). Pure so
// the logs index-selection is unit-testable without AWS.
func memberByIndex(members []arrayMember, index int) (arrayMember, bool) {
	for _, m := range members {
		if m.Index == index {
			return m, true
		}
	}
	return arrayMember{}, false
}

// arrayLogPath maps the --which selector to the on-instance log path. The paths
// are the same constants `spawn task diagnose` points at (cmd/task.go). Returns
// an error for an unknown selector so the flag is validated in one place.
func arrayLogPath(which string) (string, error) {
	switch which {
	case "command", "":
		return commandLogRemotePath, nil
	case "spored":
		return sporedLogRemotePath, nil
	default:
		return "", fmt.Errorf("invalid --which %q: want \"command\" or \"spored\"", which)
	}
}

// ── array logs ───────────────────────────────────────────────────────────────

var arrayLogsCmd = &cobra.Command{
	Use:   "logs <array-name>",
	Short: "Tail a member's command or spored log (by --index)",
	Long: `Fetch the tail of one array member's log.

Selects the member by --index (the sparse job-array index, as shown by
'spawn array status'), then reads /var/log/spawn-command.log (default) or
/var/log/spored.log (--which spored). Uses the instance's SSH key when one is
on disk, else falls back to SSM (keyless/lagotto-launched members).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		logPath, err := arrayLogPath(arrayLogsWhich)
		if err != nil {
			return err
		}
		if arrayLogsLines <= 0 {
			return fmt.Errorf("--lines must be positive")
		}

		members, size, err := findArrayMembers(ctx, args[0], arrayRegion)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return fmt.Errorf("no instances found for job array %q (try --region, or the array may be fully terminated)", args[0])
		}
		member, ok := memberByIndex(members, arrayLogsIndex)
		if !ok {
			return fmt.Errorf("index %d has no live member in job array %q (requested size %d) — run 'spawn array status %s' to see which indexes launched",
				arrayLogsIndex, args[0], size, args[0])
		}

		client, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		instance, err := resolveInstance(ctx, client, member.InstanceID)
		if err != nil {
			return err
		}

		remoteCmd := fmt.Sprintf("tail -n %d %s", arrayLogsLines, logPath)
		out, err := runArrayMemberCommand(ctx, client, instance, remoteCmd)
		if err != nil {
			return fmt.Errorf("fetch index %d log: %w", arrayLogsIndex, err)
		}
		fmt.Print(out)
		return nil
	},
}

// runArrayMemberCommand runs a one-shot shell command on an array member and
// returns its combined output. It reuses the exact SSH-key-or-SSM branch the
// status path uses (cmd/status.go): when a local SSH key resolves it runs over
// SSH (sudo, ec2-user@publicIP); otherwise — a keyless, SSM-only member as
// lagotto/cohort launches leave — it runs over SSM RunShellScript, where the
// agent already runs as root so `sudo` is unnecessary (#222).
func runArrayMemberCommand(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo, remoteCmd string) (string, error) {
	keyPath, keyErr := findSSHKey(instance.KeyName)
	if keyErr != nil {
		res, err := client.RunShellScript(ctx, instance.Region, instance.InstanceID, remoteCmd, 60*time.Second)
		if err != nil {
			return "", fmt.Errorf("run over SSM (member is keyless; SSM required): %w", err)
		}
		out := res.Stdout
		if res.Stderr != "" {
			out += res.Stderr
		}
		return out, nil
	}

	sshArgs := append([]string{"-i", keyPath}, sporedSSHOptions()...)
	sshArgs = append(sshArgs, fmt.Sprintf("ec2-user@%s", instance.PublicIP), "sudo "+remoteCmd+" 2>&1")
	output, err := exec.CommandContext(ctx, "ssh", sshArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh: %w\nOutput: %s", err, string(output))
	}
	return string(output), nil
}

// ── array status ─────────────────────────────────────────────────────────────

var arrayStatusCmd = &cobra.Command{
	Use:   "status <array-name>",
	Short: "Show a job array's members, requested vs launched, and missing indexes",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		members, size, err := findArrayMembers(ctx, args[0], arrayRegion)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return fmt.Errorf("no instances found for job array %q (try --region, or the array may be fully terminated)", args[0])
		}
		missing := missingIndexes(members, size)

		if arrayJSON || getOutputFormat() == "json" {
			return json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"name":            args[0],
				"requested":       size,
				"launched":        len(members),
				"missing_indexes": missing,
				"members":         members,
			})
		}

		fmt.Printf("Job array: %s\n", args[0])
		fmt.Printf("Launched %d of %d requested member(s).\n", len(members), size)
		w := newTableWriter(os.Stdout)
		_, _ = fmt.Fprintln(w, "INDEX\tNAME\tINSTANCE\tSTATE\tREGION")
		for _, m := range members {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", m.Index, m.Name, m.InstanceID, m.State, m.Region)
		}
		_ = w.Flush()
		if len(missing) > 0 {
			fmt.Printf("\n⚠ Missing indexes (never launched or already terminated): %v\n", missing)
			fmt.Printf("  {total} stays %d for every member, so a shard scheme assuming a dense\n", size)
			fmt.Printf("  0..%d range will skip these. See job-arrays docs.\n", size-1)
		}
		return nil
	},
}

// ── array collect ──────────────────────────────────────────────────────────

var arrayCollectCmd = &cobra.Command{
	Use:   "collect <array-name> [dest-dir]",
	Short: "Report where each member's results are (per-index)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		members, size, err := findArrayMembers(ctx, args[0], arrayRegion)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return fmt.Errorf("no instances found for job array %q", args[0])
		}
		dest := arrayCollectDir
		if len(args) == 2 {
			dest = args[1]
		}
		// Array members write to wherever their own --command sends output (there
		// is no server-side results manifest as sweeps have). Report the per-index
		// members so the caller can pull from their known S3 layout; note any gaps.
		fmt.Printf("Job array %s: %d of %d members present.\n", args[0], len(members), size)
		if dest != "" {
			fmt.Printf("Destination: %s\n", dest)
		}
		for _, m := range members {
			fmt.Printf("  index %d → %s (%s)\n", m.Index, m.Name, m.State)
		}
		if missing := missingIndexes(members, size); len(missing) > 0 {
			fmt.Printf("⚠ No results for missing indexes: %v\n", missing)
		}
		return nil
	},
}

// ── array cancel ─────────────────────────────────────────────────────────────

var arrayCancelCmd = &cobra.Command{
	Use:   "cancel <array-name>",
	Short: "Terminate a job array's instances",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		members, _, err := findArrayMembers(ctx, args[0], arrayRegion)
		if err != nil {
			return err
		}
		// --pending keeps running work and only reaps members that are not doing
		// useful compute (stopped/stopping) — mirrors "cancel the queued remainder".
		targets := members
		if arrayCancelPend {
			targets = targets[:0]
			for _, m := range members {
				switch m.State {
				case "running", "pending":
					// leave active members alone
				default:
					targets = append(targets, m)
				}
			}
		}
		if len(targets) == 0 {
			fmt.Println("Nothing to cancel.")
			return nil
		}

		fmt.Printf("Will terminate %d instance(s) in job array %q:\n", len(targets), args[0])
		for _, m := range targets {
			fmt.Printf("  index %d  %s  %s (%s)\n", m.Index, m.Name, m.InstanceID, m.State)
		}
		if !confirmYes(arrayCancelYes, "Terminate these instances?") {
			fmt.Println("Aborted.")
			return nil
		}

		client, err := aws.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("init AWS client: %w", err)
		}
		var failed int
		for _, m := range targets {
			if err := client.Terminate(ctx, m.Region, m.InstanceID); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ terminate %s (index %d) failed: %v\n", m.InstanceID, m.Index, err)
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d terminations failed", failed, len(targets))
		}
		fmt.Printf("Terminated %d instance(s).\n", len(targets))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(arrayGroupCmd)
	arrayGroupCmd.AddCommand(arrayStatusCmd, arrayLogsCmd, arrayCollectCmd, arrayCancelCmd)

	for _, sub := range []*cobra.Command{arrayStatusCmd, arrayLogsCmd, arrayCollectCmd, arrayCancelCmd} {
		sub.Flags().StringVar(&arrayRegion, "region", "", "Region to search (default: all regions)")
	}
	arrayStatusCmd.Flags().BoolVar(&arrayJSON, "json", false, "Output as JSON")
	_ = arrayStatusCmd.Flags().MarkDeprecated("json", "use --output json instead")
	arrayLogsCmd.Flags().IntVar(&arrayLogsIndex, "index", 0, "Array member index to fetch logs for")
	_ = arrayLogsCmd.MarkFlagRequired("index")
	arrayLogsCmd.Flags().StringVar(&arrayLogsWhich, "which", "command", "Which log to tail: \"command\" or \"spored\"")
	arrayLogsCmd.Flags().IntVar(&arrayLogsLines, "lines", 200, "Number of trailing lines to show")
	arrayCollectCmd.Flags().StringVar(&arrayCollectDir, "output-dir", "", "Destination directory hint for results")
	arrayCancelCmd.Flags().BoolVarP(&arrayCancelYes, "yes", "y", false, "Skip the confirmation prompt")
	arrayCancelCmd.Flags().BoolVar(&arrayCancelPend, "pending", false, "Only terminate members that are not actively running")
}
