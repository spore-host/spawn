package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/slurm"
	"gopkg.in/yaml.v3"
)

// parseLaunchArgv runs argv (as `spawn slurm submit` hands it to the child
// process) through the real root command: cobra's own resolution to `launch`,
// launch's own flag set, and launch's own Args validator (launchNameArgs). It
// returns the resolved spore name. Every flag the parse sets is put back
// afterwards, since they are package globals shared with other tests.
func parseLaunchArgv(t *testing.T, argv []string) string {
	t.Helper()
	cmd, rest, err := rootCmd.Find(argv)
	if err != nil {
		t.Fatalf("argv %q does not resolve to a command: %v", argv, err)
	}
	if cmd != launchCmd {
		t.Fatalf("argv %q resolves to %q, not launch", argv, cmd.CommandPath())
	}
	t.Cleanup(func() {
		cmd.Flags().Visit(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	})
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatalf("launch rejects the flags in %q: %v", argv, err)
	}
	positional := cmd.Flags().Args()
	if err := cmd.Args(cmd, positional); err != nil {
		t.Fatalf("launch rejects argv %q: %v", argv, err)
	}
	if len(positional) == 1 {
		return positional[0]
	}
	n, _ := cmd.Flags().GetString("name")
	return n
}

// TestSlurmSubmitArgvIsAcceptedByLaunch is #1217. `spawn slurm submit` re-execs
// `spawn launch --param-file <tmp>` after the user has approved the cost, and
// launch refused it: a spore name is required and submit passed none. The name
// must also be one launch can use, because launch registers it in DNS and sets
// the hostname from it — and a Slurm --job-name is free text.
func TestSlurmSubmitArgvIsAcceptedByLaunch(t *testing.T) {
	dir := t.TempDir()
	jobs := map[string]*slurm.SlurmJob{
		"plain job name":         {JobName: "train", FilePath: filepath.Join(dir, "x.sbatch")},
		"free-text job name":     {JobName: "Align Samples_v2 (run #3)", FilePath: filepath.Join(dir, "x.sbatch")},
		"no job name":            {FilePath: filepath.Join(dir, "My Job.sbatch")},
		"nothing usable":         {JobName: "___", FilePath: filepath.Join(dir, "...sbatch")},
		"overlong job name":      {JobName: strings.Repeat("abc", 40), FilePath: filepath.Join(dir, "x.sbatch")},
		"array with a %N limit":  {JobName: "sweep", FilePath: filepath.Join(dir, "x.sbatch"), Array: &slurm.ArraySpec{Start: 1, End: 4, Step: 1, MaxRunning: 2}},
		"array without a %N cap": {JobName: "sweep", FilePath: filepath.Join(dir, "x.sbatch"), Array: &slurm.ArraySpec{Start: 1, End: 4, Step: 1}},
	}
	for desc, job := range jobs {
		t.Run(desc, func(t *testing.T) {
			paramPath := filepath.Join(dir, "params.yaml")
			got := parseLaunchArgv(t, slurmLaunchArgs(job, paramPath))

			if !isValidDNSName(got) || len(got) > 63 {
				t.Errorf("spore name %q is not a valid DNS label, which launch registers and uses as the hostname", got)
			}
			if paramFile != paramPath {
				t.Errorf("--param-file = %q, want %q", paramFile, paramPath)
			}
			// A detached sweep launches rows with no user data, so the job
			// script would never run (#1323).
			if !noDetach {
				t.Error("the hand-off must launch in the foreground (--no-detach)")
			}
			want := 0
			if job.Array != nil {
				want = job.Array.MaxRunning
			}
			if maxConcurrent != want {
				t.Errorf("--max-concurrent = %d, want the script's %%N limit %d", maxConcurrent, want)
			}
		})
	}
}

// TestSlurmConvertOutputIsAcceptedByLaunch is #1242. `spawn slurm convert`
// wrote each job script under a `script:` key, which launch's param-file
// validation refuses (a passthrough value with newlines), and nothing on the
// instance would have run anyway. This converts real sbatch scripts, feeds the
// file through launch's own parser and validators in the order
// launchParameterSweep runs them, builds every row's launch config and user
// data, and requires the job script to arrive in the user data the instance
// boots with.
func TestSlurmConvertOutputIsAcceptedByLaunch(t *testing.T) {
	dir := t.TempDir()
	pub := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAAtest spawn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plat := &platform.Platform{OS: "linux", HomeDir: dir, SSHPubKeyPath: pub}

	// The body is long enough that the script cannot ride the 256-char
	// spawn:command tag, as a real job script cannot.
	body := "set +e\nmodule load python\n" + strings.Repeat("python3 analyse.py --input data.csv --output out.csv\n", 8) +
		"echo SLURM-BRIDGE-BODY-MARKER\n"
	scripts := map[string]string{
		"single task": "#!/bin/bash\n#SBATCH --job-name=Align_Samples.v2\n#SBATCH --time=01:30:00\n" +
			"#SBATCH --mem=4G\n#SBATCH --cpus-per-task=2\n#SBATCH --chdir=/scratch/run 1\n\n" + body,
		"array with a %N limit": "#!/bin/bash\n#SBATCH --job-name=param-sweep\n#SBATCH --array=0-6:3%2\n" +
			"#SBATCH --time=02:00:00\n#SBATCH --mem=8G\n\n" + body,
	}
	for desc, text := range scripts {
		t.Run(desc, func(t *testing.T) {
			scriptPath := filepath.Join(dir, strings.ReplaceAll(desc, " ", "_")+".sbatch")
			if err := os.WriteFile(scriptPath, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			job, err := slurm.ParseSlurmScript(scriptPath)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			converted, err := slurm.ConvertToSpawn(job)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			out, err := yaml.Marshal(converted)
			if err != nil {
				t.Fatal(err)
			}
			paramPath := filepath.Join(dir, "params.yaml")
			if err := os.WriteFile(paramPath, out, 0o600); err != nil {
				t.Fatal(err)
			}

			// Launch's own parser and validators, in launchParameterSweep's order.
			pf, err := parseParamFile(paramPath)
			if err != nil {
				t.Fatalf("launch cannot parse the converted file: %v", err)
			}
			if err := validateTopLevelParamKeys(paramPath, pf.UnknownTopLevelKeys); err != nil {
				t.Fatalf("launch rejects the converted file: %v", err)
			}
			if err := validateSweepParamKeys(pf); err != nil {
				t.Fatalf("launch rejects the converted file: %v\n%s", err, out)
			}
			if err := validateSweepDurations(pf); err != nil {
				t.Fatalf("launch rejects the converted file: %v", err)
			}
			// slurmLaunchArgs launches it with --no-detach, which demands a
			// bound on every row.
			if unbounded := sweepRowsWithoutBound(pf); len(unbounded) > 0 {
				t.Fatalf("rows with no TTL, refused under --no-detach: %v", unbounded)
			}

			wantRows := job.GetTotalTasks()
			if len(pf.Params) != wantRows {
				t.Fatalf("%d rows, want one per task (%d)", len(pf.Params), wantRows)
			}
			seenNames := map[string]bool{}
			for i, row := range pf.Params {
				cfg, err := buildLaunchConfigFromParams(spawnaws.LaunchConfig{Name: slurm.SporeName(job)},
					pf.Defaults, row, "sweep-1", "sweep", i, len(pf.Params))
				if err != nil {
					t.Fatalf("row %d: launch rejects it: %v", i, err)
				}
				if cfg.TTL == "" {
					t.Errorf("row %d: the script's --time did not become a TTL", i)
				}
				for k := range cfg.Parameters {
					if strings.Contains(cfg.Parameters[k], "SLURM-BRIDGE-BODY-MARKER") {
						t.Errorf("row %d: the script landed in PARAM_%s, which nothing runs", i, k)
					}
				}
				if !isValidDNSName(cfg.Name) || seenNames[cfg.Name] {
					t.Errorf("row %d: name %q is not a distinct valid spore name", i, cfg.Name)
				}
				seenNames[cfg.Name] = true
				if tag, ok := cfg.Tags["spawn:command"]; ok && len(tag) > 256 {
					t.Errorf("row %d: a %d-char spawn:command tag; RunInstances refuses values over 256", i, len(tag))
				}

				cfg.TargetOS = "linux"
				script, err := buildUserData(plat, &cfg, "", nil)
				if err != nil {
					t.Fatalf("row %d: build user data: %v", i, err)
				}
				if err := spawnaws.CheckUserDataSize(encodeUserDataForOS(script, cfg.TargetOS)); err != nil {
					t.Errorf("row %d: %v", i, err)
				}
				if !strings.Contains(script, "SLURM-BRIDGE-BODY-MARKER") {
					t.Errorf("row %d: the job script is not in the user data, so it never runs", i)
				}
				if job.IsArrayJob() {
					want := job.Array.Start + i*job.Array.Step
					if !strings.Contains(script, "export SLURM_ARRAY_TASK_ID="+strconv.Itoa(want)+"\n") {
						t.Errorf("row %d: user data does not carry SLURM_ARRAY_TASK_ID=%d", i, want)
					}
				}
			}
		})
	}
}

// TestSweepRowCommandTheTagCannotCarryIsEmbedded is the launch half of #1242. A
// sweep row's command rode only the spawn:command tag, which EC2 caps at 256
// characters, so any real job script reached RunInstances as an oversized tag.
// One the tag cannot carry is now embedded in the row's user data; a short
// one-liner keeps the tag path it had.
func TestSweepRowCommandTheTagCannotCarryIsEmbedded(t *testing.T) {
	long := "#!/bin/bash\n" + strings.Repeat("echo step\n", 40)
	cases := []struct {
		desc      string
		command   string
		wantEmbed bool
		wantTag   bool
	}{
		{"short one-liner keeps the tag", "python3 train.py", false, true},
		{"over 256 chars", strings.Repeat("x", 257), true, false},
		{"multi-line", "echo a\necho b", true, false},
		{"long job script", long, true, false},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			// The base carries a CLI --command; the row's own command must win
			// (row params > CLI flag).
			cfg, err := buildLaunchConfigFromParams(spawnaws.LaunchConfig{JobArrayCommand: "echo cli"}, nil,
				map[string]interface{}{"command": c.command}, "sweep-1", "sweep", 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			if got := embeddedCommand(&cfg); (got == c.command) != c.wantEmbed || (!c.wantEmbed && got != "") {
				t.Errorf("embedded command = %q, want embedded=%v", got, c.wantEmbed)
			}
			if _, ok := cfg.Tags["spawn:command"]; ok != c.wantTag {
				t.Errorf("spawn:command tag present = %v, want %v", ok, c.wantTag)
			}
			if cfg.JobArrayCommand != c.command {
				t.Errorf("the row's command lost to the CLI --command: got %q", cfg.JobArrayCommand)
			}
		})
	}
}
