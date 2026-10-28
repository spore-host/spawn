package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/instancerole"
)

// SweepIAMRole records the instance profile the CLI IAM flags resolved to for a
// sweep (applyCLIIAMToSweep) and how it was made, so a resume can launch with the
// same profile and re-create it if the idle-role sweep deleted it (#1385).
//
// Before this record, a checkpoint stored neither: resume reloaded the param file,
// which never held the CLI's iam_role, so a resumed member silently got
// spored-instance-profile instead of the role the sweep was launched with.
type SweepIAMRole struct {
	// Profile is the instance profile (and role) name every row without its own
	// iam_role: was launched with.
	Profile string `json:"profile"`
	// Verbatim: the profile came from --instance-profile, so spawn did not
	// create it and has no policy to re-create it from.
	Verbatim bool `json:"verbatim,omitempty"`
	// Config is the IAM configuration CreateOrGetInstanceProfile was called with;
	// nil when Verbatim.
	Config *SweepIAMConfig `json:"config,omitempty"`
	// Flags is the original command-line IAM flags, for messages.
	Flags string `json:"flags,omitempty"`
}

// SweepIAMConfig is aws.IAMRoleConfig with stable JSON names. PolicyFileSHA256
// is the --iam-policy-file content at launch: the role name hashes only the
// file's path, so a changed file would re-create a different policy under the
// same name.
type SweepIAMConfig struct {
	RoleName         string            `json:"role_name,omitempty"`
	Policies         []string          `json:"policies,omitempty"`
	ManagedPolicies  []string          `json:"managed_policies,omitempty"`
	PolicyFile       string            `json:"policy_file,omitempty"`
	PolicyFileSHA256 string            `json:"policy_file_sha256,omitempty"`
	InlinePolicyJSON string            `json:"inline_policy_json,omitempty"`
	TrustServices    []string          `json:"trust_services,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
}

func (c SweepIAMConfig) roleConfig() aws.IAMRoleConfig {
	return aws.IAMRoleConfig{
		RoleName:         c.RoleName,
		Policies:         c.Policies,
		ManagedPolicies:  c.ManagedPolicies,
		PolicyFile:       c.PolicyFile,
		InlinePolicyJSON: c.InlinePolicyJSON,
		TrustServices:    c.TrustServices,
		Tags:             c.Tags,
	}
}

// newSweepIAMRole records profile, created from cfg with the given flags.
func newSweepIAMRole(profile string, cfg aws.IAMRoleConfig, flags string) (*SweepIAMRole, error) {
	rec := &SweepIAMRole{Profile: profile, Flags: flags, Config: &SweepIAMConfig{
		RoleName:         cfg.RoleName,
		Policies:         cfg.Policies,
		ManagedPolicies:  cfg.ManagedPolicies,
		PolicyFile:       cfg.PolicyFile,
		InlinePolicyJSON: cfg.InlinePolicyJSON,
		TrustServices:    cfg.TrustServices,
		Tags:             cfg.Tags,
	}}
	if cfg.PolicyFile != "" {
		sum, err := fileSHA256(cfg.PolicyFile)
		if err != nil {
			return nil, fmt.Errorf("record --iam-policy-file for resume: %w", err)
		}
		rec.Config.PolicyFileSHA256 = sum
	}
	return rec, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the caller's own --iam-policy-file
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// cliIAMFlagsString renders the IAM flags of this invocation, for the record.
func cliIAMFlagsString() string {
	var parts []string
	add := func(flag string, vals ...string) {
		for _, v := range vals {
			if v != "" {
				parts = append(parts, fmt.Sprintf("%s %s", flag, v))
			}
		}
	}
	add("--instance-profile", instanceProfile)
	add("--iam-role", iamRole)
	add("--iam-policy", iamPolicy...)
	add("--iam-managed-policies", iamManagedPolicies...)
	add("--iam-policy-file", iamPolicyFile)
	add("--s3-read", s3ReadBuckets...)
	add("--s3-write", s3WriteBuckets...)
	return strings.Join(parts, " ")
}

// applyRecordedIAM puts a recorded CLI profile back into the param file's
// defaults, as applyCLIIAMToSweep did at launch, so a resumed row gets the same
// profile the original rows got. A row's own iam_role: still wins.
func applyRecordedIAM(rec *SweepIAMRole, pf *ParamFileFormat) {
	if rec == nil || rec.Profile == "" {
		return
	}
	if pf.Defaults == nil {
		pf.Defaults = map[string]interface{}{}
	}
	pf.Defaults["iam_role"] = rec.Profile
}

// resumeProfileEnsurer is the IAM a resume needs: a read-only readiness check,
// and the two calls the original launch used to create a profile.
type resumeProfileEnsurer interface {
	InstanceProfileReady(ctx context.Context, name string) (bool, error)
	CreateOrGetInstanceProfile(ctx context.Context, cfg aws.IAMRoleConfig) (string, error)
	SetupNestedSpawnProfile(ctx context.Context) (string, error)
}

// oldCheckpointIAMFlags names the flags a checkpoint written before #1385 did
// not record.
const oldCheckpointIAMFlags = "--iam-role, --iam-policy, --iam-managed-policies, --iam-policy-file, --s3-read, --s3-write or --instance-profile"

// resumeProfileTarget is one instance profile pending members will launch with.
type resumeProfileTarget struct {
	name   string
	source string        // what named it, for messages
	rec    *SweepIAMRole // how the sweep created it, or nil
}

// spawnCreated reports whether spawn made the profile and could have deleted
// it: a per-task spawn-instance-* role (the idle-role sweep's set), or one the
// sweep's own CLI flags created.
func (t resumeProfileTarget) spawnCreated() bool {
	return instancerole.IsCandidate(t.name) || (t.rec != nil && t.rec.Config != nil)
}

// ensureResumeProfiles makes every spawn-created instance profile the pending
// members name exist again before anything launches (#1385). A profile that is
// still there is reused unchanged: only InstanceProfileReady is called. One that
// is gone is re-created from the sweep's record with the same call and config
// the original launch used, and must come back under the same name. One that is
// gone with no record to rebuild it from refuses the resume, so a member never
// launches with a profile that does not exist or with a different role.
//
// Every check runs before any IAM write, so a refusal changes nothing.
func ensureResumeProfiles(ctx context.Context, e resumeProfileEnsurer, rec *SweepIAMRole, paramFile string, configs []*aws.LaunchConfig) error {
	byName := map[string]resumeProfileTarget{}
	for _, cfg := range configs {
		name := cfg.IamInstanceProfile
		if name == "" {
			continue
		}
		if _, ok := byName[name]; ok {
			continue
		}
		t := resumeProfileTarget{name: name, source: fmt.Sprintf("iam_role: in %s", paramFile)}
		if rec != nil && rec.Profile == name {
			t.rec = rec
			t.source = "the sweep's original IAM flags"
			if rec.Flags != "" {
				t.source = fmt.Sprintf("the sweep's original flags (%s)", rec.Flags)
			}
		}
		byName[name] = t
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	var recreate []resumeProfileTarget
	for _, n := range names {
		t := byName[n]
		if !t.spawnCreated() {
			continue
		}
		ready, err := e.InstanceProfileReady(ctx, n)
		if err != nil {
			return fmt.Errorf("check instance profile %s before resuming: %w", n, err)
		}
		if ready {
			continue
		}
		if err := canRecreate(t); err != nil {
			return err
		}
		recreate = append(recreate, t)
	}

	for _, t := range recreate {
		var got string
		var err error
		if t.rec != nil && t.rec.Config != nil {
			got, err = e.CreateOrGetInstanceProfile(ctx, t.rec.Config.roleConfig())
		} else {
			got, err = e.SetupNestedSpawnProfile(ctx)
		}
		if err != nil {
			return fmt.Errorf("re-create instance profile %s (from %s) before resuming: %w", t.name, t.source, err)
		}
		if got != t.name {
			return fmt.Errorf("re-creating instance profile %s (from %s) produced %s instead; "+
				"nothing was launched, so no member runs with a different role", t.name, t.source, got)
		}
		fmt.Fprintf(os.Stderr, "✓ IAM: re-created instance profile %s from %s (it had been deleted since the sweep launched)\n", t.name, t.source)
	}
	return nil
}

// canRecreate returns nil when the gone profile t can be rebuilt faithfully, and
// otherwise the refusal.
func canRecreate(t resumeProfileTarget) error {
	switch {
	case t.rec != nil && t.rec.Config != nil:
		pfPath := t.rec.Config.PolicyFile
		if pfPath == "" {
			return nil
		}
		sum, err := fileSHA256(pfPath)
		if err != nil || sum != t.rec.Config.PolicyFileSHA256 {
			return fmt.Errorf("instance profile %s no longer exists, and its policy file %s is missing or has changed "+
				"since the sweep launched, so spawn cannot re-create the role with its original policy. "+
				"Nothing was launched. Restore the file, or launch a new sweep with %s", t.name, pfPath, t.rec.Flags)
		}
		return nil
	case t.rec != nil && t.rec.Verbatim && !aws.IsNestedSpawnRole(t.name):
		return fmt.Errorf("instance profile %s no longer exists. It was passed with --instance-profile, so spawn "+
			"has no policy to re-create it from. Nothing was launched. Re-create the profile, or launch a new sweep", t.name)
	case aws.IsNestedSpawnRole(t.name):
		// A nested-spawn role's grants are fixed, so SetupNestedSpawnProfile
		// re-creates it exactly, under the same name for the same grants.
		return nil
	default:
		return fmt.Errorf("instance profile %s (named by %s) no longer exists, and this sweep's checkpoint does not "+
			"record the IAM configuration it was created with, so spawn cannot re-create it with its original policy. "+
			"Nothing was launched. It was most likely deleted as idle by the spawn-instance-* role sweep. "+
			"Launch a new sweep with the IAM flags you used originally (%s), or set iam_role: to a profile that exists",
			t.name, t.source, oldCheckpointIAMFlags)
	}
}

// refuseOldCheckpointRoleDrift refuses to resume a checkpoint written before
// #1385 when the sweep's own instances show it was launched with a profile the
// resumed rows would not get. Such a checkpoint does not record the CLI IAM
// flags, and the param file never held their profile, so without this a resumed
// member silently launched with spored-instance-profile instead.
//
// origProfiles maps a launched parameter-set index to the profile EC2 reports
// for its instance; rows that name their own iam_role: are not compared.
func refuseOldCheckpointRoleDrift(state *SweepState, pf *ParamFileFormat, origProfiles map[int]string) error {
	if state.IAMRecorded {
		return nil
	}
	want := stringParam(pf.Defaults, "iam_role")
	if want == "" {
		want = aws.SporedInstanceProfileName
	}
	idx := make([]int, 0, len(origProfiles))
	for i := range origProfiles {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		got := origProfiles[i]
		if got == "" || i < 0 || i >= len(pf.Params) || stringParam(pf.Params[i], "iam_role") != "" {
			continue
		}
		if got != want {
			return fmt.Errorf("parameter set %d of this sweep was launched with instance profile %s, but resuming "+
				"would launch the pending sets with %s: the checkpoint predates spawn recording the sweep's IAM flags "+
				"(%s), so it cannot reproduce them. Nothing was launched. Set iam_role: %s under defaults: in %s "+
				"to resume with the original profile", i, got, want, oldCheckpointIAMFlags, got, state.ParamFile)
		}
	}
	return nil
}

// detachedIAMRecordPath is where a detached sweep's IAM record lives. A
// detached sweep keeps no local checkpoint, and the orchestrator rewrites its
// DynamoDB record whole, so the record is a local file of its own (#1385).
func detachedIAMRecordPath(sweepID string) (string, error) {
	dir, err := getSweepStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "detached-iam", sweepID+".json"), nil
}

func saveDetachedIAMRecord(sweepID string, rec *SweepIAMRole) error {
	path, err := detachedIAMRecordPath(sweepID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// loadDetachedIAMRecord returns the sweep's record, or nil when there is none.
func loadDetachedIAMRecord(sweepID string) (*SweepIAMRole, error) {
	path, err := detachedIAMRecordPath(sweepID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- under ~/.spawn/sweeps
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec SweepIAMRole
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &rec, nil
}

// resumeProfileError is ensureResumeProfiles refusing, or failing, before a
// resume launched anything.
type resumeProfileError struct{ err error }

func (e *resumeProfileError) Error() string { return e.err.Error() }
func (e *resumeProfileError) Unwrap() error { return e.err }

// ensureDetachedResumeProfile re-creates a detached sweep's CLI IAM profile, from
// the record launchSweepDetached wrote, if it has been deleted since (#1385).
// The orchestrator launches with the profile named in the uploaded params, which
// it cannot create; without a record (a sweep launched before #1385, or from
// another machine) the profile cannot be checked, and that is said.
func ensureDetachedResumeProfile(ctx context.Context, sweepID string, client func(context.Context) (resumeProfileEnsurer, error)) error {
	rec, err := loadDetachedIAMRecord(sweepID)
	if err != nil {
		return fmt.Errorf("read the sweep's IAM record: %w", err)
	}
	if rec == nil {
		fmt.Fprintf(os.Stderr, "ℹ️  No local IAM record for this sweep (launched before spawn recorded one, or from another machine).\n"+
			"   If it used a spawn-instance-* profile that has since been deleted, its launches will fail.\n\n")
		return nil
	}
	e, err := client(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}
	cfg := &aws.LaunchConfig{IamInstanceProfile: rec.Profile}
	return ensureResumeProfiles(ctx, e, rec, "the sweep's uploaded parameters", []*aws.LaunchConfig{cfg})
}
