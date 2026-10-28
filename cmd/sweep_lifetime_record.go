package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// SweepLifetime is the lifetime and enforcement settings a sweep's members took
// from the command line, recorded in the checkpoint so `spawn sweep resume`
// launches its pending members with the same bounds (#1394).
//
// Resume rebuilds each member from the param file, which never held these: the
// CLI --ttl, --idle-timeout and --cost-limit were folded into the in-memory
// defaults only, and --no-timeout, --on-idle and the completion flags rode the
// launch's base config. Without the record a resumed member ran with no TTL,
// no idle timeout and no cost limit.
type SweepLifetime struct {
	TTL             string  `json:"ttl,omitempty"`
	IdleTimeout     string  `json:"idle_timeout,omitempty"`
	CostLimit       float64 `json:"cost_limit,omitempty"`
	HibernateOnIdle bool    `json:"hibernate_on_idle,omitempty"`
	// NoTimeout is --no-timeout: the 1h idle default was not applied.
	NoTimeout       bool    `json:"no_timeout,omitempty"`
	OnComplete      string  `json:"on_complete,omitempty"`
	CompletionFile  string  `json:"completion_file,omitempty"`
	CompletionDelay string  `json:"completion_delay,omitempty"`
	SessionTimeout  string  `json:"session_timeout,omitempty"`
	IdleCPUPercent  float64 `json:"idle_cpu_percent,omitempty"`
}

// newSweepLifetime records the lifetime settings of base, the CLI config every
// member of a sweep starts from (sweepBaseConfig), and --no-timeout.
func newSweepLifetime(base *aws.LaunchConfig, noTimeoutSet bool) *SweepLifetime {
	return &SweepLifetime{
		TTL: base.TTL, IdleTimeout: base.IdleTimeout, CostLimit: base.CostLimit,
		HibernateOnIdle: base.HibernateOnIdle, NoTimeout: noTimeoutSet,
		OnComplete: base.OnComplete, CompletionFile: base.CompletionFile,
		CompletionDelay: base.CompletionDelay, SessionTimeout: base.SessionTimeout,
		IdleCPUPercent: base.IdleCPUPercent,
	}
}

// launchBase is the base config a resumed member starts from: the recorded
// settings, where the original launch had the whole CLI config.
func (l SweepLifetime) launchBase() aws.LaunchConfig {
	cfg := aws.LaunchConfig{
		Tags: map[string]string{},
		TTL:  l.TTL, IdleTimeout: l.IdleTimeout, CostLimit: l.CostLimit,
		HibernateOnIdle: l.HibernateOnIdle, OnComplete: l.OnComplete,
		CompletionFile: l.CompletionFile, CompletionDelay: l.CompletionDelay,
		SessionTimeout: l.SessionTimeout, IdleCPUPercent: l.IdleCPUPercent,
	}
	// As buildLaunchConfig does: a hibernating idle or completion action needs
	// an instance launched for hibernation (#949).
	if cfg.HibernationRequested() {
		cfg.Hibernate = true
	}
	return cfg
}

// resumeLifetimeFlags are resume's own --ttl, --idle-timeout and --cost-limit,
// which replace the recorded CLI values. A row's own value still wins, as it
// did over the original flags.
type resumeLifetimeFlags struct {
	ttl, idleTimeout string
	costLimit        float64
}

func (f resumeLifetimeFlags) any() bool {
	return f.ttl != "" || f.idleTimeout != "" || f.costLimit > 0
}

func (f resumeLifetimeFlags) validate() error {
	if f.ttl != "" {
		if _, err := sweepDurationValue("--ttl", f.ttl); err != nil {
			return err
		}
	}
	if f.idleTimeout != "" {
		if _, err := sweepDurationValue("--idle-timeout", f.idleTimeout); err != nil {
			return err
		}
	}
	if f.costLimit < 0 {
		return fmt.Errorf("--cost-limit must not be negative, got %v", f.costLimit)
	}
	return nil
}

func (f resumeLifetimeFlags) over(l SweepLifetime) SweepLifetime {
	if f.ttl != "" {
		l.TTL = f.ttl
	}
	if f.idleTimeout != "" {
		l.IdleTimeout = f.idleTimeout
	}
	if f.costLimit > 0 {
		l.CostLimit = f.costLimit
	}
	return l
}

// foldSpendControls writes ttl, idle timeout and cost limit into a sweep's
// defaults, where they beat the file's own defaults: and lose to a row's own
// value (#525). Shared by the launch (applyCLISpendControlsToSweep) and the
// resume, so a resumed member resolves its bounds as the original did.
func foldSpendControls(defaults map[string]interface{}, ttlV, idleV string, cost float64) {
	if ttlV != "" {
		defaults["ttl"] = ttlV
	}
	if idleV != "" {
		defaults["idle_timeout"] = idleV
	}
	if cost > 0 {
		defaults["cost_limit"] = cost
	}
}

// resumeLifetime is how a resume bounds its pending members.
type resumeLifetime struct {
	// eff is what the members launch with: the record, or nothing for an old
	// checkpoint, with resume's flags over it.
	eff SweepLifetime
	// orig is the checkpoint's record, nil for one written before #1394. A
	// member may never end up with a weaker bound than it gives.
	orig *SweepLifetime
	// fileDefaults is the param file's defaults: before anything was folded in.
	fileDefaults map[string]interface{}
	// record is the checkpoint's full record, its withheld settings re-passed
	// (restoreMemberRecord); nil for a checkpoint written before #1400, whose
	// members start from the lifetime record alone.
	record *SweepMemberRecord
}

// planResumeLifetime resolves a resume's lifetime settings from the checkpoint
// record and resume's flags, and folds them into pf's defaults (so the
// nested-role TTL check sees what the original launch saw). It must run after
// applyRecordedIAM, which also writes pf's defaults. member is the full record
// (#1400), or nil for an older checkpoint; rec is the lifetime record either
// way (SweepState.lifetimeRecord).
func planResumeLifetime(rec *SweepLifetime, member *SweepMemberRecord, pf *ParamFileFormat, flags resumeLifetimeFlags) (*resumeLifetime, error) {
	if err := flags.validate(); err != nil {
		return nil, err
	}
	p := &resumeLifetime{orig: rec, fileDefaults: map[string]interface{}{}, record: member}
	if rec != nil {
		p.eff = *rec
	}
	p.eff = flags.over(p.eff)
	for k, v := range pf.Defaults {
		p.fileDefaults[k] = v
	}
	pf.Defaults = p.defaults(p.eff)
	return p, nil
}

func (p *resumeLifetime) defaults(l SweepLifetime) map[string]interface{} {
	d := make(map[string]interface{}, len(p.fileDefaults)+3)
	for k, v := range p.fileDefaults {
		d[k] = v
	}
	// What the launch folded in from its base (#697, #544), with the same
	// functions.
	if p.record != nil {
		seedSweepDefaults(d, p.record.Base)
		foldVolumeSize(d, p.record.Base.RootVolumeSizeGiB)
	}
	foldSpendControls(d, l.TTL, l.IdleTimeout, l.CostLimit)
	return d
}

// base is the config a pending member starts from, with lifetime l: the
// recorded base (#1400), else, for an older checkpoint, the lifetime record
// alone.
func (p *resumeLifetime) base(l SweepLifetime) aws.LaunchConfig {
	if p.record == nil {
		return l.launchBase()
	}
	b := p.record.Base
	b.TTL, b.IdleTimeout, b.CostLimit = l.TTL, l.IdleTimeout, l.CostLimit
	return b
}

// installBootstrap points the user-data builder at the recorded bootstrap
// inputs (SweepBootstrapInputs.install); a no-op for an older checkpoint.
func (p *resumeLifetime) installBootstrap() func() {
	if p.record == nil {
		return func() {}
	}
	return p.record.Bootstrap.install()
}

// member builds pending member idx as the original launch did, with the
// launch's own builder (sweepMemberConfig): the recorded base config, the
// folded defaults, the row, and the 1h idle default for a member with no ttl
// and no idle timeout unless --no-timeout was passed.
func (p *resumeLifetime) member(l SweepLifetime, row map[string]interface{}, state *SweepState, idx int) (aws.LaunchConfig, error) {
	cfg, _, err := sweepMemberConfig(p.base(l), p.defaults(l), row, state.SweepID, state.SweepName, idx, state.TotalParams, l.NoTimeout)
	return cfg, err
}

// members builds the pending members' configs, refusing the resume when one
// would launch weaker than the original or with no bound at all.
func (p *resumeLifetime) members(pf *ParamFileFormat, state *SweepState, pending []int) ([]*aws.LaunchConfig, error) {
	out := make([]*aws.LaunchConfig, 0, len(pending))
	for _, idx := range pending {
		cfg, err := p.member(p.eff, pf.Params[idx], state, idx)
		if err != nil {
			return nil, err
		}
		if p.orig != nil {
			orig, err := p.member(*p.orig, pf.Params[idx], state, idx)
			if err != nil {
				return nil, err
			}
			if err := notWeaker(orig, cfg); err != nil {
				return nil, fmt.Errorf("parameter set %d: %w; nothing was launched", idx, err)
			}
		}
		if err := requireResumeBound(&cfg, idx); err != nil {
			return nil, err
		}
		out = append(out, &cfg)
	}
	return out, nil
}

// notWeaker reports whether got bounds a member less than orig, the bound the
// original launch gave it: a TTL, idle timeout or cost limit dropped or raised.
func notWeaker(orig, got aws.LaunchConfig) error {
	var weaker []string
	longer := func(name, o, g, flag string) {
		if o == "" {
			return
		}
		if g == "" {
			weaker = append(weaker, fmt.Sprintf("it would lose its %s of %s (pass %s %s to keep it)", name, o, flag, o))
			return
		}
		od, oerr := parseDuration(o)
		gd, gerr := parseDuration(g)
		if oerr == nil && gerr == nil && gd > od {
			weaker = append(weaker, fmt.Sprintf("its %s would rise from %s to %s", name, o, g))
		}
	}
	longer("TTL", orig.TTL, got.TTL, "--ttl")
	longer("idle timeout", orig.IdleTimeout, got.IdleTimeout, "--idle-timeout")
	if orig.CostLimit > 0 && (got.CostLimit <= 0 || got.CostLimit > orig.CostLimit) {
		weaker = append(weaker, fmt.Sprintf("its cost limit would rise from $%.2f", orig.CostLimit))
	}
	if len(weaker) > 0 {
		return fmt.Errorf("a resume may not loosen the bounds the sweep was launched with: %s", strings.Join(weaker, "; "))
	}
	return nil
}

// requireResumeBound refuses a resumed member with no TTL, no idle timeout and
// no cost limit: nothing would ever end it but the out-of-band reaper.
func requireResumeBound(cfg *aws.LaunchConfig, idx int) error {
	if cfg.TTL != "" || cfg.IdleTimeout != "" || cfg.CostLimit > 0 {
		return nil
	}
	return fmt.Errorf("parameter set %d would be relaunched with no TTL, no idle timeout and no cost limit "+
		"(the sweep was launched with --no-timeout, and its row sets none); nothing was launched. "+
		"Give the members a bound: pass --ttl 4h (or --idle-timeout / --cost-limit) to `spawn sweep resume %s`", idx, cfgSweepID(cfg))
}

func cfgSweepID(cfg *aws.LaunchConfig) string {
	if cfg.SweepID != "" {
		return cfg.SweepID
	}
	return "<sweep-id>"
}

// refuseUnboundedResume is relaunchPending's backstop: no config reaches the
// rolling queue without a bound.
func refuseUnboundedResume(configs []*aws.LaunchConfig, pending []int) error {
	for i, cfg := range configs {
		idx := i
		if i < len(pending) {
			idx = pending[i]
		}
		if err := requireResumeBound(cfg, idx); err != nil {
			return err
		}
	}
	return nil
}

// noteOldCheckpointLifetime tells the user an old checkpoint's members get the
// launch defaults, not the flags the sweep was launched with.
func noteOldCheckpointLifetime(state *SweepState, flags resumeLifetimeFlags) {
	if state.lifetimeRecord() != nil || flags.any() {
		return
	}
	fmt.Fprintf(os.Stderr, "⚠️  This checkpoint predates spawn recording a sweep's --ttl/--idle-timeout/--cost-limit (#1394).\n"+
		"   Relaunched members get the launch defaults: their row's own bounds, else a 1h idle timeout.\n"+
		"   To bound them otherwise, pass --ttl <duration> (or --idle-timeout / --cost-limit) to `spawn sweep resume %s`.\n\n", state.SweepID)
}

// refuseDetachedLifetimeFlags: a detached sweep's members are launched by the
// orchestrator from the params uploaded at launch, whose rendered tags and
// user data already carry the sweep's bounds; a resume cannot change them.
func refuseDetachedLifetimeFlags(f resumeLifetimeFlags) error {
	if !f.any() {
		return nil
	}
	return fmt.Errorf("--ttl, --idle-timeout and --cost-limit are not supported with --detach: the orchestrator " +
		"relaunches with the bounds recorded when the sweep was launched, which a resume cannot change")
}
