package cmd

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/spore-host/spawn/internal/treewalk"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
)

// #1400: a resume rebuilt each pending member from a config holding only the
// IAM (#1385) and lifetime (#1394) records, so every other launch flag was
// dropped: --pre-stop (outputs lost at termination), --tag/--team, network,
// storage, the webhooks, the spored settings, the bootstrap inputs. A
// checkpoint now records the whole member base (SweepMemberRecord), and a
// resume builds each member with the launch's own builder from it.

// carriedSweepFlags is every launch flag sweepLaunchFlags says reaches a
// sweep's members: through their config, or through the user-data builder.
func carriedSweepFlags(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, n := range launchFlagNames(t) {
		if r, ok := sweepLaunchFlags[n]; ok && (r.kind == sweepViaConfig || r.kind == sweepViaUserData) {
			out = append(out, n)
		}
	}
	if len(out) < 40 {
		t.Fatalf("found only %d carried sweep flags; the discovery is broken", len(out))
	}
	return out
}

// resetLaunchFlags puts every launch flag back to its default, unset: the
// process a resume runs in, which never parsed them. Under keepLaunchGlobals.
func resetLaunchFlags(t *testing.T) {
	t.Helper()
	launchCmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch v := f.Value.(type) {
		case pflag.SliceValue:
			_ = v.Replace(nil)
		default:
			if f.Value.Type() != "stringToString" {
				if err := f.Value.Set(f.DefValue); err != nil {
					t.Fatalf("reset --%s: %v", f.Name, err)
				}
			}
		}
		f.Changed = false
	})
}

// withResumeWithheld sets resume's re-pass flags for one test.
func withResumeWithheld(t *testing.T, vals map[string]string) {
	t.Helper()
	prev := resumeWithheldValues()
	t.Cleanup(func() {
		for f, v := range prev {
			*resumeWithheld[f] = v
		}
	})
	for f := range resumeWithheld {
		*resumeWithheld[f] = vals[f]
	}
}

// recordParamFile is a two-row sweep whose defaults: set a ttl and a volume
// size, so the CLI's precedence over them is exercised, and whose second row
// sets its own.
func recordParamFile() *ParamFileFormat {
	return &ParamFileFormat{
		Defaults: map[string]interface{}{"ttl": "3h", "volume_size": 25},
		Params:   []map[string]interface{}{{"trial": "a"}, {"trial": "b", "ttl": "30m", "volume_size": 50}},
	}
}

// launchedSweep builds a foreground sweep's members from the launch flags as
// they are now set, the way launchParameterSweep does (its defaults folding,
// foregroundSweepMembers, then the user data), and the checkpoint record it
// writes. The AMI and target OS are fixed, as AWS would resolve them.
func launchedSweep(t *testing.T, plat *platform.Platform, pf *ParamFileFormat) ([]*aws.LaunchConfig, *SweepMemberRecord) {
	t.Helper()
	base, err := sweepBaseConfig(context.Background())
	if err != nil {
		t.Fatalf("sweepBaseConfig: %v", err)
	}
	pf = copyParamFile(pf)
	seedSweepDefaults(pf.Defaults, *base)
	applyCLISpendControlsToSweep(pf)
	applyCLIVolumeSizeToSweep(pf)
	rec, err := newSweepMemberRecord(base, noTimeout, nil)
	if err != nil {
		t.Fatalf("newSweepMemberRecord: %v", err)
	}
	cfgs, err := foregroundMembers(t, plat, *base, pf, &fakeStorageLookups{})
	if err != nil {
		t.Fatalf("build the launched members: %v", err)
	}
	return cfgs, rec
}

// checkpointThrough writes rec in a checkpoint and reads it back through the
// file, as a real resume does.
func checkpointThrough(t *testing.T, rec *SweepMemberRecord, total int) *SweepState {
	t.Helper()
	if err := saveSweepState(&SweepState{SweepID: "sw-1", SweepName: "sw", ParamFile: "/data/p.yaml", TotalParams: total, LaunchDelay: "0s", Member: rec}); err != nil {
		t.Fatal(err)
	}
	state, err := loadSweepState("sw-1")
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// resumedMembers rebuilds the pending members of state from pf as runResume
// does: restore the record, the IAM and lifetime records into the defaults,
// the members through the plan, the AMI and target OS fixed as for the
// launched ones, and the user data with the recorded bootstrap installed.
func resumedMembers(t *testing.T, plat *platform.Platform, state *SweepState, pf *ParamFileFormat, pending []int) ([]*aws.LaunchConfig, error) {
	t.Helper()
	pf = copyParamFile(pf)
	member, err := restoreMemberRecord(state.Member, resumeWithheldValues(), state.SweepID)
	if err != nil {
		return nil, err
	}
	applyRecordedIAM(state.iamRecord(), pf)
	plan, err := planResumeLifetime(state.lifetimeRecord(), member, pf, resumeLifetimeFlags{})
	if err != nil {
		return nil, err
	}
	cfgs, err := plan.members(pf, state, pending)
	if err != nil {
		return nil, err
	}
	for _, c := range cfgs {
		if c.AMI == "" {
			c.AMI = "ami-0123456789abcdef0"
		}
		if c.TargetOS == "" {
			c.TargetOS = "linux"
		}
	}
	defer plan.installBootstrap()()
	if err := renderForegroundSweepUserData(context.Background(), plat, cfgs, newSweepStorage(&fakeStorageLookups{}, io.Discard)); err != nil {
		return nil, err
	}
	return cfgs, nil
}

// setSweepFlag sets launch flag name to a value its validation accepts,
// writing a file for the flags that name one, and returns the value.
func setSweepFlag(t *testing.T, dir, name string) string {
	t.Helper()
	v := sampleFlagValue(t, name)
	if name == "user-data-file" || name == "config" {
		v = filepath.Join(dir, name+".txt")
		body := "echo from-a-file\n"
		if name == "config" {
			body = "plugins:\n  - ref: rstudio-server\n"
		}
		if err := os.WriteFile(v, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setLaunchFlag(t, name, v)
	return v
}

// TestResumedMemberIsTheLaunchedMember is the resume-equals-launch gate. For
// every flag sweepLaunchFlags says a sweep carries to its members, discovered
// from the cobra command (so a new carried flag is covered with no change
// here): launch a two-row sweep with the flag, write its checkpoint, then in a
// process with no launch flags set resume every row, and require each resumed
// member's config (what aws.Client.Launch turns into RunInstances: launch
// settings, tags and the fields it reads directly) and user data to equal the
// launched member's. A withheld setting is passed again, as the resume
// requires.
func TestResumedMemberIsTheLaunchedMember(t *testing.T) {
	for _, n := range carriedSweepFlags(t) {
		t.Run(n, func(t *testing.T) {
			keepLaunchGlobals(t)
			dir := isolateAWSEnv(t)
			setTestHome(t, dir)
			stubTeamName(t)
			plat := testSweepPlat(t)
			for k, v := range map[string]string{"instance-type": "m7i.large", "region": "us-east-1"} {
				setLaunchFlag(t, k, v)
			}
			set := map[string]string{}
			for _, p := range flagPrereqs[n] {
				set[p] = setSweepFlag(t, dir, p)
			}
			before, _ := launchedSweep(t, plat, recordParamFile())
			set[n] = setSweepFlag(t, dir, n)
			launched, rec := launchedSweep(t, plat, recordParamFile())
			if reflect.DeepEqual(scripts(t, before), scripts(t, launched)) && reflect.DeepEqual(configsOnly(before), configsOnly(launched)) {
				t.Fatalf("--%s changes nothing a launched member gets; the gate would pass vacuously", n)
			}
			state := checkpointThrough(t, rec, 2)

			resetLaunchFlags(t) // the resume's process
			passed := map[string]string{}
			for _, w := range sweepWithheldSettings {
				if v, ok := set[w.flag]; ok {
					passed[w.flag] = v
				}
			}
			withResumeWithheld(t, passed)
			resumed, err := resumedMembers(t, plat, state, recordParamFile(), []int{0, 1})
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			for i := range launched {
				if got, want := configsOnly(resumed)[i], configsOnly(launched)[i]; !reflect.DeepEqual(got, want) {
					t.Errorf("member %d resumed with a different config than it launched with; fields %v differ",
						i, changedConfigFields(got, want))
				}
				if got, want := scripts(t, resumed)[i], scripts(t, launched)[i]; got != want {
					t.Errorf("member %d resumed with different user data than it launched with", i)
				}
			}
		})
	}
}

func configsOnly(cfgs []*aws.LaunchConfig) []aws.LaunchConfig {
	out := make([]aws.LaunchConfig, len(cfgs))
	for i, c := range cfgs {
		out[i] = *c
		out[i].UserData = ""
	}
	return out
}

func scripts(t *testing.T, cfgs []*aws.LaunchConfig) []string {
	t.Helper()
	out := make([]string, len(cfgs))
	for i, c := range cfgs {
		out[i] = string(decodeUserDataOnce(t, c.UserData))
	}
	return out
}

// The two settings #1400 names, end to end: a sweep launched with --pre-stop
// (the hook that syncs results before termination) and --tag resumes its
// pending members with both, on the config the rolling queue launches and in
// the instance tags.
func TestResume_KeepsPreStopAndTag(t *testing.T) {
	keepLaunchGlobals(t)
	dir := isolateAWSEnv(t)
	setTestHome(t, dir)
	fastRollingQueue(t)
	plat := testSweepPlat(t)
	setLaunchFlag(t, "instance-type", "m7i.large")
	setLaunchFlag(t, "region", "us-east-1")
	setLaunchFlag(t, "pre-stop", "aws s3 sync /out s3://b/")
	setLaunchFlag(t, "tag", "project=x")
	_, rec := launchedSweep(t, plat, recordParamFile())
	state := checkpointThrough(t, rec, 2)

	resetLaunchFlags(t)
	withResumeWithheld(t, nil)
	cfgs, err := resumedMembers(t, plat, state, recordParamFile(), []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	l := &capturingLauncher{}
	if _, _, n, err := relaunchPending(context.Background(), l, state, cfgs, []int{0, 1}, 2, unverifiedGuard(), &fakeProfileIAM{exists: map[string]bool{}}); err != nil || n != 2 {
		t.Fatalf("relaunchPending: n=%d err=%v", n, err)
	}
	for _, c := range l.configs {
		tags, err := testMemberTagList(c)
		if err != nil {
			t.Fatal(err)
		}
		m := tagsAsMap(tags)
		if c.PreStop != "aws s3 sync /out s3://b/" || m["spawn:pre-stop"] != c.PreStop {
			t.Errorf("member %d resumed with pre-stop %q (tag %q), want the sweep's --pre-stop", c.SweepIndex, c.PreStop, m["spawn:pre-stop"])
		}
		if c.Tags["project"] != "x" || m["project"] != "x" {
			t.Errorf("member %d resumed without the sweep's --tag project=x (config %v, tag %q)", c.SweepIndex, c.Tags, m["project"])
		}
	}
}

// secretLikeFlag is a launch flag name that suggests a credential.
var secretLikeFlag = regexp.MustCompile(`url|webhook|token|secret|password|credential|correlation|auth`)

// notSecret are the secret-looking carried flags that are not secrets.
var notSecret = map[string]string{
	"webhook-timeout": "a duration bounding the POST",
}

// The secret gate. Every setting that may carry a credential is withheld from
// the checkpoint: sentinels passed to each withheld flag, an inline
// --user-data, a --config file and a --user-data-file appear nowhere under
// ~/.spawn after a sweep's checkpoint is written, and the checkpoint is 0600.
// Every carried flag whose name looks like a credential is decided, and every
// withheld one is a carried flag that both resume spellings take again.
func TestCheckpointHoldsNoSecretInPlaintext(t *testing.T) {
	carried := carriedSweepFlags(t)
	for _, n := range carried {
		if !secretLikeFlag.MatchString(n) {
			continue
		}
		if _, ok := withheldSettingFor(n); !ok && notSecret[n] == "" {
			t.Errorf("--%s looks like it may carry a credential: withhold it (sweepWithheldSettings) or say why not (notSecret)", n)
		}
	}
	if len(sweepWithheldSettings) == 0 {
		t.Fatal("no withheld settings; the gate would pass vacuously")
	}
	for _, w := range sweepWithheldSettings {
		if !slices.Contains(carried, w.flag) {
			t.Errorf("withheld --%s is not a flag a sweep carries", w.flag)
		}
		for _, c := range []*pflagCmd{{"resume", resumeCmd.Flags()}, {"sweep resume", sweepResumeCmd.Flags()}} {
			if c.fs.Lookup(w.flag) == nil {
				t.Errorf("spawn %s has no --%s to pass the withheld setting again", c.name, w.flag)
			}
		}
	}

	for _, form := range []string{"inline", "file"} {
		t.Run(form, func(t *testing.T) {
			keepLaunchGlobals(t)
			dir := isolateAWSEnv(t)
			home := filepath.Join(dir, "home")
			setTestHome(t, home)
			stubTeamName(t)
			fastRollingQueue(t)
			setLaunchFlag(t, "instance-type", "m7i.large")
			var sentinels []string
			for _, w := range sweepWithheldSettings {
				s := "SENTINEL-" + strings.ToUpper(w.flag) + "-9f3a"
				v := s
				if strings.HasSuffix(w.flag, "-url") {
					v = "https://hooks.example.org/services/" + s
				}
				setLaunchFlag(t, w.flag, v)
				sentinels = append(sentinels, s)
			}
			cfgFile := filepath.Join(dir, "launch.yaml")
			if err := os.WriteFile(cfgFile, []byte("plugins:\n  - ref: tailscale\n    config:\n      auth_key: SENTINEL-CONFIG-9f3a\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			setLaunchFlag(t, "config", cfgFile)
			sentinels = append(sentinels, "SENTINEL-CONFIG-9f3a")
			if form == "file" {
				udFile := filepath.Join(dir, "ud.sh")
				if err := os.WriteFile(udFile, []byte("export TOKEN=SENTINEL-UDFILE-9f3a\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				setLaunchFlag(t, "user-data-file", udFile)
				sentinels = append(sentinels, "SENTINEL-UDFILE-9f3a")
			}
			base, err := sweepBaseConfig(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			rec, err := newSweepMemberRecord(base, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			f := newFakeFleetSweep(0)
			g := newFleetGuard("sweep", io.Discard, f, sporedUp(), false)
			if err := runSweepLaunch(context.Background(), f, g, sweepRun{
				sweepID: "sec", name: "sec", totalParams: 2, maxConcurrent: 1, launchDelay: "0s",
				configs: sweepConfigs("us-east-1", "us-east-1"), record: rec,
			}); err != nil {
				t.Fatal(err)
			}
			found := 0
			root := filepath.Join(home, ".spawn")
			err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					if treewalk.Vanished(root, p, err) {
						return nil
					}
					return err
				}
				if d.IsDir() {
					return nil
				}
				found++
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				for _, s := range sentinels {
					if strings.Contains(string(b), s) {
						t.Errorf("%s holds %s in plaintext", p, s)
					}
				}
				if fi, _ := d.Info(); runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
					t.Errorf("%s is mode %v; the checkpoint holds the sweep's launch flags and must be 0600", p, fi.Mode().Perm())
				}
				return nil
			})
			if err != nil || found == 0 {
				t.Fatalf("walked %d files under ~/.spawn (err %v); no checkpoint was written", found, err)
			}
			// And the record says what was withheld, so a resume can ask.
			state, err := loadSweepState("sec")
			if err != nil || state.Member == nil {
				t.Fatalf("checkpoint: %v %+v", err, state)
			}
			var got []string
			for _, w := range state.Member.Withheld {
				got = append(got, w.Flag)
			}
			want := []string{"completion-webhook-url", "spot-webhook-url", "webhook-correlation", "user-data"}
			if form == "file" {
				want = want[:3] // --user-data-file wins over --user-data, as at launch
			}
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("withheld %v, want %v", got, want)
			}
		})
	}
}

type pflagCmd struct {
	name string
	fs   *pflag.FlagSet
}

// A resume of a sweep launched with a withheld setting is refused, before any
// AWS call, until it is passed again, naming it and the host it pointed at;
// one passed that the sweep never had is refused too; and a --config file
// changed since the launch refuses the resume.
func TestResume_RefusesWhatItCannotReproduce(t *testing.T) {
	keepLaunchGlobals(t)
	dir := isolateAWSEnv(t)
	setTestHome(t, dir)
	stubTeamName(t)
	setLaunchFlag(t, "instance-type", "m7i.large")
	setLaunchFlag(t, "completion-webhook-url", "https://hooks.example.org/services/T0/B0/xyz")
	cfgFile := setSweepFlag(t, dir, "config")
	plat := testSweepPlat(t)
	_, rec := launchedSweep(t, plat, recordParamFile())
	state := checkpointThrough(t, rec, 2)
	resetLaunchFlags(t)

	withResumeWithheld(t, nil)
	_, err := resumedMembers(t, plat, state, recordParamFile(), []int{0})
	if err == nil || !strings.Contains(err.Error(), "--completion-webhook-url <the https://hooks.example.org/... URL>") ||
		strings.Contains(err.Error(), "xyz") || !strings.Contains(err.Error(), "Nothing was launched") {
		t.Fatalf("err = %v, want a refusal naming --completion-webhook-url and its host only", err)
	}
	withResumeWithheld(t, map[string]string{"completion-webhook-url": "https://hooks.example.org/services/T0/B0/xyz", "spot-webhook-url": "https://x.example.org/"})
	if _, err := resumedMembers(t, plat, state, recordParamFile(), []int{0}); err == nil || !strings.Contains(err.Error(), "--spot-webhook-url") {
		t.Fatalf("err = %v, want a refusal of a --spot-webhook-url the sweep never had", err)
	}
	withResumeWithheld(t, map[string]string{"completion-webhook-url": "https://hooks.example.org/services/T0/B0/xyz"})
	if _, err := resumedMembers(t, plat, state, recordParamFile(), []int{0}); err != nil {
		t.Fatalf("with the webhook passed again: %v", err)
	}
	if err := os.WriteFile(cfgFile, []byte("plugins:\n  - ref: other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resumedMembers(t, plat, state, recordParamFile(), []int{0}); err == nil || !strings.Contains(err.Error(), "--config "+cfgFile) {
		t.Fatalf("err = %v, want a refusal naming the changed --config", err)
	}
	state.Member.Schema = sweepMemberRecordSchema + 1
	if _, err := restoreMemberRecord(state.Member, nil, "sw-1"); err == nil || !strings.Contains(err.Error(), "newer spawn") {
		t.Fatalf("err = %v, want a refusal of a newer record", err)
	}
}

// Old checkpoints: one written before #1400 (with #1394's lifetime record, or
// with none) still resumes with the #1394 behaviour, and the resume says which
// settings it cannot restore; one with the full record says nothing. A setting
// re-passed to an old checkpoint is refused: it never had a place for it.
func TestResume_OldCheckpointResumesWithTheNotice(t *testing.T) {
	setTestHome(t, t.TempDir())
	withResumeFlags(t, resumeLifetimeFlags{})
	withResumeWithheld(t, nil)
	withLifetimeFlags(t, "", "", 0, "", false)
	for name, rec := range map[string]*SweepLifetime{"1394": {TTL: "4h"}, "1385": nil} {
		state := checkpointedState(t, "old-"+name, rec)
		configs, err := resumeConfigs(t, state, lifetimeParamFile(), []int{0, 2})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantTTL, wantIdle := "", "1h"
		if rec != nil {
			wantTTL, wantIdle = "4h", ""
		}
		if c := configs[0]; c.TTL != wantTTL || c.IdleTimeout != wantIdle || configs[1].TTL != "1h" {
			t.Errorf("%s: member 0 ttl=%q idle=%q, member 2 ttl=%q; want the #1394 behaviour", name, c.TTL, c.IdleTimeout, configs[1].TTL)
		}
		out := captureStderr(t, func() { noteOldCheckpointSettings(state) })
		if !strings.Contains(out, "#1400") || !strings.Contains(out, "--pre-stop") || !strings.Contains(out, "--tag") {
			t.Errorf("%s: notice = %q, want it to list the settings it cannot restore", name, out)
		}
	}
	if out := captureStderr(t, func() { noteOldCheckpointSettings(&SweepState{Member: &SweepMemberRecord{}}) }); out != "" {
		t.Errorf("a checkpoint with the full record printed %q", out)
	}
	withResumeWithheld(t, map[string]string{"user-data": "echo hi"})
	if _, err := restoreMemberRecord(nil, resumeWithheldValues(), "old"); err == nil || !strings.Contains(err.Error(), "--user-data") {
		t.Errorf("err = %v, want a refusal of --user-data on an old checkpoint", err)
	}
}

// The notice names every carried flag an old checkpoint cannot restore,
// discovered from sweepLaunchFlags, so a new carried flag must be placed.
func TestOldCheckpointNoticeNamesEveryLostFlag(t *testing.T) {
	notice := oldCheckpointNotice("sw")
	carried := carriedSweepFlags(t)
	for _, n := range carried {
		if oldCheckpointRestored[n] {
			continue
		}
		if oldCheckpointLostCategory[n] == "" {
			t.Errorf("--%s reaches a sweep's members and an old checkpoint cannot restore it: give it a category in oldCheckpointLostCategory", n)
		}
		if !regexp.MustCompile(`(^|\s)--` + regexp.QuoteMeta(n) + `(\s|$)`).MatchString(notice) {
			t.Errorf("the old-checkpoint notice does not name --%s", n)
		}
	}
	for n := range oldCheckpointLostCategory {
		if !slices.Contains(carried, n) || oldCheckpointRestored[n] {
			t.Errorf("oldCheckpointLostCategory names --%s, which is not a carried flag an old checkpoint loses", n)
		}
	}
	for n := range oldCheckpointRestored {
		if !slices.Contains(carried, n) {
			t.Errorf("oldCheckpointRestored names --%s, which is not a carried flag", n)
		}
	}
}

// The record round-trips: every LaunchConfig field, found by reflection, comes
// back from the checkpoint file as it was recorded, except the withheld ones
// (blanked) and the two a launch renders itself. A field whose type JSON
// cannot carry fails here.
func TestMemberRecordRoundTripsEveryLaunchConfigField(t *testing.T) {
	setTestHome(t, t.TempDir())
	keepLaunchGlobals(t)
	resetLaunchFlags(t)
	withheld := map[string]bool{"SpotInterruptionWebhookURL": true, "CompletionWebhookURL": true, "WebhookCorrelation": true}
	notRecorded := map[string]bool{"UserData": true, "SpawnVersion": true}
	var base aws.LaunchConfig
	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		if !sampleField(reflect.ValueOf(&base).Elem().Field(i), typ.Field(i).Name) {
			t.Fatalf("cannot sample LaunchConfig.%s (%s)", typ.Field(i).Name, typ.Field(i).Type)
		}
	}
	rec, err := newSweepMemberRecord(&base, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := checkpointThrough(t, rec, 1).Member.Base
	gv, bv := reflect.ValueOf(got), reflect.ValueOf(base)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		g, b := gv.Field(i).Interface(), bv.Field(i).Interface()
		switch {
		case withheld[name] || notRecorded[name]:
			if !gv.Field(i).IsZero() {
				t.Errorf("LaunchConfig.%s was written to the checkpoint", name)
			}
		case !reflect.DeepEqual(g, b):
			t.Errorf("LaunchConfig.%s does not survive the checkpoint: wrote %#v, read %#v", name, b, g)
		}
	}
	fields := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		fields[typ.Field(i).Name] = true
	}
	for name := range withheld {
		if !fields[name] {
			t.Errorf("withheld field %s no longer exists", name)
		}
	}
	// The withheld fields are exactly sweepWithheldSettings' config ones.
	probe := &SweepMemberRecord{}
	for _, w := range sweepWithheldSettings {
		w.set(probe, "x")
	}
	for i := 0; i < typ.NumField(); i++ {
		if name := typ.Field(i).Name; !reflect.ValueOf(probe.Base).Field(i).IsZero() != withheld[name] {
			t.Errorf("LaunchConfig.%s: sweepWithheldSettings and this gate disagree on whether it is withheld", name)
		}
	}
}

// Detached resume (#1400's fifth point): a detached sweep's members are
// launched by the orchestrator from the params uploaded at launch, and a
// resume only re-invokes it. So for every carried flag, discovered as above,
// the uploaded params must carry it (they differ with and without it) unless
// the detached launch refuses it outright; and resumeSweepDetached must not
// rebuild, re-render or re-upload anything, and the orchestrator must read
// the launch's params on every invocation.
func TestDetachedResumeRelaunchesFromParamsCarryingEveryFlag(t *testing.T) {
	pfOf := func(t *testing.T) (string, error) {
		t.Helper()
		base, err := sweepBaseConfig(context.Background())
		if err != nil {
			return "", err
		}
		pf := &ParamFileFormat{Defaults: map[string]interface{}{}, Params: []map[string]interface{}{{"trial": "a"}, {"trial": "b"}}}
		seedSweepDefaults(pf.Defaults, *base)
		applyCLISpendControlsToSweep(pf)
		applyCLIVolumeSizeToSweep(pf)
		// The launch keys, with the AMI AWS would resolve fixed when --ami is
		// not set (the client is then never asked).
		if base.AMI == "" {
			pf.Defaults["ami"] = "ami-0123456789abcdef0"
		}
		if err := seedDetachedLaunchKeys(context.Background(), nil, pf, base, "us-east-1"); err != nil {
			return "", err
		}
		boot := testDetachedBootstrap(t)
		boot.plat = testSweepPlat(t) // holds the --key-name key, as the launching machine does
		if err := seedDetachedBootstrap(pf, *base, "sw-1", "sw", boot); err != nil {
			return "", err
		}
		b, err := json.Marshal(pf)
		return string(b), err
	}
	carriedOnDetached := 0
	for _, n := range carriedSweepFlags(t) {
		t.Run(n, func(t *testing.T) {
			keepLaunchGlobals(t)
			dir := isolateAWSEnv(t)
			setTestHome(t, dir)
			stubTeamName(t)
			for k, v := range map[string]string{"instance-type": "m7i.large", "region": "us-east-1", "ttl": "1h"} {
				setLaunchFlag(t, k, v)
			}
			for _, p := range flagPrereqs[n] {
				setSweepFlag(t, dir, p)
			}
			plat := testSweepPlat(t)
			fgBefore, _ := launchedSweep(t, plat, recordParamFile())
			before, err := pfOf(t)
			if err != nil {
				t.Fatalf("baseline: %v", err)
			}
			setSweepFlag(t, dir, n)
			fgAfter, _ := launchedSweep(t, plat, recordParamFile())
			if reflect.DeepEqual(renderedMembers(t, fgBefore), renderedMembers(t, fgAfter)) {
				t.Skipf("--%s's sample value renders as the default does (a launched member's tags, settings and user data are the same)", n)
			}
			after, err := pfOf(t)
			if err != nil {
				t.Skipf("a detached launch refuses --%s (%v), so there is nothing to resume", n, err)
			}
			if before == after {
				t.Errorf("--%s is carried to a sweep's members, but the params uploaded for the orchestrator are the same "+
					"with and without it: a detached resume would relaunch without it", n)
			}
			carriedOnDetached++
		})
	}
	if carriedOnDetached < 30 {
		t.Errorf("only %d carried flags reached the detached params; the gate is not exercising the detached path", carriedOnDetached)
	}

	calls := funcCalls(t, "resume.go")
	for _, c := range calls["resumeSweepDetached"] {
		switch c {
		case "UploadParamsToS3", "seedDetachedBootstrap", "buildLaunchConfigFromParams", "sweepMemberConfig", "restoreMemberRecord", "parseParamFile":
			t.Errorf("resumeSweepDetached calls %s: a detached resume must relaunch from the params uploaded at launch", c)
		}
	}
	if !slices.Contains(calls["resumeSweepDetached"], "InvokeSweepOrchestrator") {
		t.Error("resumeSweepDetached does not re-invoke the orchestrator")
	}
	orch, err := os.ReadFile(filepath.Join("..", "lambda", "sweep-orchestrator", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s*params, err := downloadParams\(ctx, state\.S3ParamsKey, `).Match(orch) {
		t.Error("the orchestrator no longer reads the launch's uploaded params (state.S3ParamsKey) on each invocation")
	}
}

// renderedMembers is what members are launched with: the fields
// aws.Client.Launch reads itself (foregroundDispositions' direct ones), their
// tags, launch settings and user data.
func renderedMembers(t *testing.T, cfgs []*aws.LaunchConfig) []string {
	t.Helper()
	var out []string
	for _, c := range cfgs {
		direct := []interface{}{c.InstanceType, c.Region, c.AMI, c.KeyName, c.IamInstanceProfile, c.EFAInterfaces}
		b, err := json.Marshal([]interface{}{direct, gateTags(t, *c), aws.NewLaunchSettings(*c, "", 0)})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b), string(decodeUserDataOnce(t, c.UserData)))
	}
	return out
}

// funcCalls maps each function in file to the names it calls, in source order.
func funcCalls(t *testing.T, file string) map[string][]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string][]string{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				switch fn := c.Fun.(type) {
				case *ast.Ident:
					calls[fd.Name.Name] = append(calls[fd.Name.Name], fn.Name)
				case *ast.SelectorExpr:
					calls[fd.Name.Name] = append(calls[fd.Name.Name], fn.Sel.Name)
				}
			}
			return true
		})
	}
	return calls
}

// runResume reaches the mechanism the gates above drive: it restores the
// record before planning, plans with it, installs the recorded bootstrap
// before resolving the members' OS and rendering their user data through the
// launch's renderer, and refuses re-passed settings before a detached resume.
func TestRunResumeBuildsMembersFromTheRecord(t *testing.T) {
	calls := funcCalls(t, "resume.go")["runResume"]
	idx := func(name string) int { return slices.Index(calls, name) }
	order := []string{"restoreMemberRecord", "planResumeLifetime", "members", "installBootstrap", "resolveTargetOS", "renderForegroundSweepUserData", "relaunchPending"}
	for i := 1; i < len(order); i++ {
		if a, b := idx(order[i-1]), idx(order[i]); a < 0 || b < 0 || a > b {
			t.Errorf("runResume: want %s before %s (%d, %d)", order[i-1], order[i], a, b)
		}
	}
	if a, b := idx("refuseDetachedWithheldFlags"), idx("resumeSweepDetached"); a < 0 || b < 0 || a > b {
		t.Errorf("runResume: want refuseDetachedWithheldFlags before resumeSweepDetached (%d, %d)", a, b)
	}
	src, err := os.ReadFile("resume.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s*member, err := restoreMemberRecord\(state\.Member, resumeWithheldValues\(\), state\.SweepID\)$`).Match(src) ||
		!regexp.MustCompile(`(?m)^\s*lifetime, err := planResumeLifetime\(state\.lifetimeRecord\(\), member, paramFormat, resumeLifetimeFlagValues\(\)\)$`).Match(src) {
		t.Error("runResume does not plan its members from the checkpoint's restored record")
	}
	if !regexp.MustCompile(`(?m)^\s*record, err := newSweepMemberRecord\(baseConfig, noTimeout, iamRecord\)$`).Match(mustRead(t, "launch_sweep.go")) ||
		!regexp.MustCompile(`(?m)^\s*record: record,$`).Match(mustRead(t, "launch_sweep.go")) {
		t.Error("launchParameterSweep does not record its member base in the checkpoint")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
