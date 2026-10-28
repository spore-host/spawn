package cmd

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// SweepMemberRecord is everything a foreground sweep's members were built
// from outside the param file, recorded in the checkpoint so `spawn sweep
// resume` rebuilds each pending member exactly as the launch did (#1400).
//
// A resume reloads the param file, which holds only the rows. Before this
// record it rebuilt every member from an empty config, then from the IAM
// (#1385) and lifetime (#1394) records, so every other launch flag was dropped:
// --pre-stop (the hook that syncs results before termination), --tag/--team,
// the network, storage and purchase flags, the webhooks, --command, the spored
// settings and the bootstrap inputs.
//
// Base is the config every member started from (sweepBaseConfig), so a flag
// added to it later is recorded with no change here. The IAM and lifetime
// records are views of this one: IAM is applyCLIIAMToSweep's record, and the
// lifetime settings are Base's own fields (lifetime).
type SweepMemberRecord struct {
	// Schema is sweepMemberRecordSchema when written; a resume refuses a
	// record from a newer spawn rather than misread it.
	Schema int `json:"schema"`
	// Base is the member base config, with the withheld settings blanked and
	// the per-launch fields (sweepRecordNotRecorded) cleared.
	Base aws.LaunchConfig `json:"base"`
	// NoTimeout is --no-timeout: the 1h idle default was not applied.
	NoTimeout bool `json:"no_timeout,omitempty"`
	// IAM is the profile the CLI IAM flags resolved to; nil when none were
	// passed.
	IAM *SweepIAMRole `json:"iam,omitempty"`
	// Bootstrap is the CLI-side inputs the user-data builder reads.
	Bootstrap SweepBootstrapInputs `json:"bootstrap"`
	// Withheld names each setting the sweep was launched with that is not
	// stored, because it may carry a credential (sweepWithheldSettings). A
	// resume requires each to be passed again.
	Withheld []SweepWithheld `json:"withheld,omitempty"`
}

const sweepMemberRecordSchema = 1

// SweepBootstrapInputs is what the user-data builder reads from the command
// line rather than from the member's config (the flags sweepLaunchFlags marks
// viaUserData): buildUserDataForKey, collectPluginDeclarations,
// launchStorageScript and sweepStorage.member read them as package globals.
//
// A file is recorded by its absolute path and the SHA-256 of its content,
// never the content: a --config file can hold plugin credentials and a user
// data script can hold anything. A resume re-reads the file and refuses one
// that is gone or changed.
type SweepBootstrapInputs struct {
	Plugins []string `json:"plugins,omitempty"`
	// ConfigFile is --config, whose plugins: list the members install.
	ConfigFile       string `json:"config_file,omitempty"`
	ConfigFileSHA256 string `json:"config_file_sha256,omitempty"`
	// UserDataFile is --user-data-file, or the path of --user-data @path.
	UserDataFile       string `json:"user_data_file,omitempty"`
	UserDataFileSHA256 string `json:"user_data_file_sha256,omitempty"`
	// UserData is an inline --user-data script. Never written: it is
	// withheld, and only set in memory on a resume that re-passes it.
	UserData        string `json:"-"`
	EFSMountOptions string `json:"efs_mount_options,omitempty"`
	EFSProfile      string `json:"efs_profile,omitempty"`
	FSxSkipValidate bool   `json:"fsx_skip_validate,omitempty"`
	FSxMountPoint   string `json:"fsx_mount_point,omitempty"`
	// OS is --os, which decides the members' target OS and with it their
	// bootstrap.
	OS string `json:"os,omitempty"`
}

// SweepWithheld is one setting the checkpoint does not store.
type SweepWithheld struct {
	// Flag is the launch flag, without dashes; resume takes the same flag.
	Flag string `json:"flag"`
	// Hint helps the user find the value to pass: a URL's scheme and host,
	// never its path, query or user info, where a token lives.
	Hint string `json:"hint,omitempty"`
}

// withheldSetting is a setting that may carry a credential, and so is never
// written to ~/.spawn (#1400). Each is decided:
//
//   - --spot-webhook-url, --completion-webhook-url: an incoming-webhook URL is
//     itself the credential (Slack, Teams and most queue endpoints authorise a
//     POST by the token in the path or query). Re-deriving is impossible, so
//     the resume must be given it again.
//   - --webhook-correlation: an opaque caller blob echoed in the payload;
//     callers use it to authenticate the callback, so it is treated the same.
//   - --user-data (inline): a script, a common place for an exported token.
//     The @path and --user-data-file forms are recorded by path and hash
//     instead (SweepBootstrapInputs), since the file stays the user's.
//
// Everything else in the base is stored. --pre-stop, --command, the tags and
// the rest are delivered to the instance as EC2 tags readable by anyone with
// ec2:DescribeTags in the account, so they are not a channel for a secret in
// the first place; and the checkpoint is written 0600.
type withheldSetting struct {
	flag string
	get  func(*SweepMemberRecord) string
	set  func(*SweepMemberRecord, string)
	// hint renders SweepWithheld.Hint from the value.
	hint func(string) string
}

var sweepWithheldSettings = []withheldSetting{
	{
		flag: "spot-webhook-url",
		get:  func(r *SweepMemberRecord) string { return r.Base.SpotInterruptionWebhookURL },
		set:  func(r *SweepMemberRecord, v string) { r.Base.SpotInterruptionWebhookURL = v },
		hint: urlHint,
	},
	{
		flag: "completion-webhook-url",
		get:  func(r *SweepMemberRecord) string { return r.Base.CompletionWebhookURL },
		set:  func(r *SweepMemberRecord, v string) { r.Base.CompletionWebhookURL = v },
		hint: urlHint,
	},
	{
		flag: "webhook-correlation",
		get:  func(r *SweepMemberRecord) string { return r.Base.WebhookCorrelation },
		set:  func(r *SweepMemberRecord, v string) { r.Base.WebhookCorrelation = v },
	},
	{
		flag: "user-data",
		get:  func(r *SweepMemberRecord) string { return r.Bootstrap.UserData },
		set:  func(r *SweepMemberRecord, v string) { r.Bootstrap.UserData = v },
	},
}

func withheldSettingFor(flag string) (withheldSetting, bool) {
	for _, w := range sweepWithheldSettings {
		if w.flag == flag {
			return w, true
		}
	}
	return withheldSetting{}, false
}

// urlHint is a URL's scheme and host, or "" for anything that does not parse.
func urlHint(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// newSweepMemberRecord records base, the config every member was built from,
// with this invocation's bootstrap inputs and IAM record, for the checkpoint.
func newSweepMemberRecord(base *aws.LaunchConfig, noTimeoutSet bool, iam *SweepIAMRole) (*SweepMemberRecord, error) {
	boot, err := currentSweepBootstrapInputs()
	if err != nil {
		return nil, err
	}
	rec := &SweepMemberRecord{Schema: sweepMemberRecordSchema, Base: *base, NoTimeout: noTimeoutSet, IAM: iam, Bootstrap: boot}
	// Rendered per member, and stamped by the resuming spawn itself.
	rec.Base.UserData, rec.Base.SpawnVersion = "", ""
	for _, w := range sweepWithheldSettings {
		v := w.get(rec)
		if v == "" {
			continue
		}
		hint := ""
		if w.hint != nil {
			hint = w.hint(v)
		}
		rec.Withheld = append(rec.Withheld, SweepWithheld{Flag: w.flag, Hint: hint})
		w.set(rec, "")
	}
	return rec, nil
}

// currentSweepBootstrapInputs reads this invocation's bootstrap flags.
func currentSweepBootstrapInputs() (SweepBootstrapInputs, error) {
	in := SweepBootstrapInputs{
		Plugins: slices.Clone(launchPlugins), EFSMountOptions: efsMountOptions, EFSProfile: efsProfile,
		FSxSkipValidate: fsxSkipValidate, FSxMountPoint: fsxMountPoint, OS: osFlag,
	}
	var err error
	if launchConfigFile != "" {
		if in.ConfigFile, in.ConfigFileSHA256, err = recordFile(launchConfigFile, "--config"); err != nil {
			return in, err
		}
	}
	// resolveCustomUserData's order: --user-data-file wins over --user-data.
	switch {
	case userDataFile != "":
		in.UserDataFile, in.UserDataFileSHA256, err = recordFile(userDataFile, "--user-data-file")
	case strings.HasPrefix(userData, "@"):
		in.UserDataFile, in.UserDataFileSHA256, err = recordFile(userData[1:], "--user-data @path")
	default:
		in.UserData = userData
	}
	return in, err
}

func recordFile(path, flag string) (string, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("record %s for resume: %w", flag, err)
	}
	sum, err := fileSHA256(abs)
	if err != nil {
		return "", "", fmt.Errorf("record %s for resume: %w", flag, err)
	}
	return abs, sum, nil
}

// lifetime is the record's lifetime settings, the #1394 record's view of it.
func (r *SweepMemberRecord) lifetime() *SweepLifetime {
	return newSweepLifetime(&r.Base, r.NoTimeout)
}

// install points the user-data builder's globals at the recorded inputs, as
// they were set when the sweep launched, and returns a function putting them
// back. A resume's process never parsed the launch flags, so without this its
// members would get no plugins, no --user-data and default mount options.
func (in SweepBootstrapInputs) install() func() {
	pu, puf, pc, pp := userData, userDataFile, launchConfigFile, launchPlugins
	pmo, pep, pfs, pfm, pos := efsMountOptions, efsProfile, fsxSkipValidate, fsxMountPoint, osFlag
	userData, userDataFile, launchConfigFile, launchPlugins = in.UserData, in.UserDataFile, in.ConfigFile, slices.Clone(in.Plugins)
	efsMountOptions, efsProfile, fsxSkipValidate, fsxMountPoint, osFlag = in.EFSMountOptions, in.EFSProfile, in.FSxSkipValidate, in.FSxMountPoint, in.OS
	return func() {
		userData, userDataFile, launchConfigFile, launchPlugins = pu, puf, pc, pp
		efsMountOptions, efsProfile, fsxSkipValidate, fsxMountPoint, osFlag = pmo, pep, pfs, pfm, pos
	}
}

// restoreMemberRecord is the checkpoint's record ready to build members from:
// each withheld setting re-passed (passed maps a flag to the resume's value),
// and each recorded file still there and unchanged. It refuses, before any AWS
// call, a record it cannot reproduce. A nil record (an old checkpoint) returns
// nil, and refuses a re-passed setting it has no place for.
func restoreMemberRecord(rec *SweepMemberRecord, passed map[string]string, sweepID string) (*SweepMemberRecord, error) {
	var given []string
	for f, v := range passed {
		if v != "" {
			given = append(given, "--"+f)
		}
	}
	sort.Strings(given)
	if rec == nil {
		if len(given) > 0 {
			return nil, fmt.Errorf("%s re-pass a setting the checkpoint withheld, but this checkpoint predates spawn "+
				"recording a sweep's launch flags (#1400), so its members never had them. Nothing was launched", strings.Join(given, ", "))
		}
		return nil, nil
	}
	if rec.Schema > sweepMemberRecordSchema {
		return nil, fmt.Errorf("this sweep's checkpoint was written by a newer spawn (record schema %d, this spawn reads %d); "+
			"resume it with that spawn. Nothing was launched", rec.Schema, sweepMemberRecordSchema)
	}
	out := *rec
	out.Withheld = nil
	var missing []string
	want := map[string]bool{}
	for _, w := range rec.Withheld {
		want[w.Flag] = true
		s, ok := withheldSettingFor(w.Flag)
		if !ok {
			return nil, fmt.Errorf("this sweep's checkpoint withholds --%s, which this spawn does not know; "+
				"resume it with the spawn that launched it. Nothing was launched", w.Flag)
		}
		v := passed[w.Flag]
		if v == "" {
			value := "<value>"
			if w.Hint != "" {
				value = fmt.Sprintf("<the %s/... URL>", w.Hint)
			}
			missing = append(missing, fmt.Sprintf("--%s %s", w.Flag, value))
			continue
		}
		s.set(&out, v)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("this sweep was launched with settings spawn does not store in its checkpoint, because they "+
			"may carry a token. Pass them again, as you did at launch: spawn sweep resume %s %s. Nothing was launched",
			sweepID, strings.Join(missing, " "))
	}
	var extra []string
	for _, g := range given {
		if !want[strings.TrimPrefix(g, "--")] {
			extra = append(extra, g)
		}
	}
	if len(extra) > 0 {
		return nil, fmt.Errorf("%s: this sweep was not launched with it, and a resume relaunches members as they were "+
			"launched. Nothing was launched", strings.Join(extra, ", "))
	}
	for _, f := range []struct{ flag, path, sum string }{
		{"--config", rec.Bootstrap.ConfigFile, rec.Bootstrap.ConfigFileSHA256},
		{"--user-data-file", rec.Bootstrap.UserDataFile, rec.Bootstrap.UserDataFileSHA256},
	} {
		if f.path == "" {
			continue
		}
		if sum, err := fileSHA256(f.path); err != nil || sum != f.sum {
			return nil, fmt.Errorf("the sweep was launched with %s %s, which is missing or has changed since, so its "+
				"members cannot be rebuilt as they were launched. Nothing was launched. Restore the file, or launch a new sweep",
				f.flag, f.path)
		}
	}
	return &out, nil
}

// iamRecord is the checkpoint's IAM record: the full record's, else the one a
// #1385 checkpoint wrote.
func (s *SweepState) iamRecord() *SweepIAMRole {
	if s.Member != nil {
		return s.Member.IAM
	}
	return s.IAMRole
}

// iamRecorded reports whether the checkpoint records the CLI IAM flags at all.
func (s *SweepState) iamRecorded() bool {
	return s.Member != nil || s.IAMRecorded
}

// lifetimeRecord is the checkpoint's lifetime settings: the full record's,
// else the one a #1394 checkpoint wrote, else nil.
func (s *SweepState) lifetimeRecord() *SweepLifetime {
	if s.Member != nil {
		return s.Member.lifetime()
	}
	return s.Lifetime
}

// What a checkpoint written before #1400 cannot restore, by category: every
// flag that reaches a sweep's members (sweepLaunchFlags' viaConfig and
// viaUserData), except those the #1394 lifetime record restores
// (oldCheckpointRestored). TestOldCheckpointNoticeNamesEveryLostFlag fails on
// a carried flag with no category.
var oldCheckpointLostCategory = map[string]string{
	"instance-type": catShape, "ami": catShape, "az": catShape, "region": catShape, "volume-size": catShape,
	"hibernate": catShape, "nested-virtualization": catShape, "key-name": catShape, "key-pair": catShape,
	"spot": catPurchase, "spot-max-price": catPurchase, "reservation-id": catPurchase, "capacity-block": catPurchase,
	"security-group": catNetwork, "security-group-ids": catNetwork, "subnet": catNetwork, "subnet-id": catNetwork,
	"efa": catNetwork, "efa-interfaces": catNetwork, "placement-group": catNetwork,
	"efs-id": catStorage, "efs-mount-point": catStorage, "efs-mount-options": catStorage, "efs-profile": catStorage,
	"fsx-id": catStorage, "fsx-mount-point": catStorage, "fsx-skip-validate": catStorage, "attach-volume": catStorage,
	"tag": catTags, "team": catTags, "name": catTags, "dns": catTags, "no-dns": catTags,
	"pre-stop": catWorkload, "pre-stop-timeout": catWorkload, "command": catWorkload,
	"user-data": catWorkload, "user-data-file": catWorkload, "plugin": catWorkload, "config": catWorkload,
	"spot-webhook-url": catNotify, "completion-webhook-url": catNotify, "webhook-correlation": catNotify,
	"webhook-timeout": catNotify, "slack-workspace": catNotify, "notify-platform": catNotify,
	"active-ports": catSpored, "active-processes": catSpored, "metrics": catSpored, "metrics-port": catSpored,
	"metrics-bind": catSpored, "metrics-path": catSpored, "tracing": catSpored, "tracing-exporter": catSpored,
	"tracing-sampling": catSpored,
}

const (
	catShape    = "instance shape and image"
	catPurchase = "purchase"
	catNetwork  = "network"
	catStorage  = "storage"
	catTags     = "tags, team and name"
	catWorkload = "workload and bootstrap"
	catNotify   = "webhooks and notifications"
	catSpored   = "spored activity, metrics and tracing"
)

// oldCheckpointRestored are the carried flags a #1394 checkpoint's lifetime
// record restores.
var oldCheckpointRestored = map[string]bool{
	"ttl": true, "idle-timeout": true, "cost-limit": true, "on-idle": true, "hibernate-on-idle": true,
	"on-complete": true, "completion-file": true, "completion-delay": true, "session-timeout": true,
	"idle-cpu-threshold": true,
}

// oldCheckpointNotice is the notice for a checkpoint with no full record: the
// launch flags its members may have had and will not get.
func oldCheckpointNotice(sweepID string) string {
	byCat := map[string][]string{}
	for f, c := range oldCheckpointLostCategory {
		byCat[c] = append(byCat[c], "--"+f)
	}
	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️  This checkpoint predates spawn recording a sweep's launch flags (#1400). Relaunched members get\n"+
		"   only the param file's rows and the recorded IAM and lifetime settings. If the sweep was launched with\n"+
		"   any of these, its pending members will NOT have them:\n")
	for _, c := range cats {
		sort.Strings(byCat[c])
		fmt.Fprintf(&b, "     %s: %s\n", c, strings.Join(byCat[c], " "))
	}
	fmt.Fprintf(&b, "   To keep them, stop here and launch the sweep's remaining rows as a new sweep with its original flags,\n"+
		"   instead of resuming %s.\n\n", sweepID)
	return b.String()
}

// noteOldCheckpointSettings prints oldCheckpointNotice for a checkpoint with
// no full record.
func noteOldCheckpointSettings(state *SweepState) {
	if state.Member != nil {
		return
	}
	fmt.Fprint(os.Stderr, oldCheckpointNotice(state.SweepID))
}

// refuseDetachedWithheldFlags: a detached sweep's members are launched by the
// orchestrator from the params uploaded at launch, whose rendered tags and
// user data already carry every setting, the withheld ones included; a resume
// neither needs them nor can change them.
func refuseDetachedWithheldFlags(passed map[string]string) error {
	var given []string
	for f, v := range passed {
		if v != "" {
			given = append(given, "--"+f)
		}
	}
	if len(given) == 0 {
		return nil
	}
	sort.Strings(given)
	return fmt.Errorf("%s not supported with --detach: the orchestrator relaunches with the settings uploaded when "+
		"the sweep was launched, which already carry them and which a resume cannot change", strings.Join(given, ", "))
}
