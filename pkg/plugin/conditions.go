package plugin

import (
	"context"
	"fmt"
	"strings"
)

// ConditionHost is the machine a pre-flight condition is evaluated on: the
// controller for conditions.local, the spore (spored) for conditions.remote.
type ConditionHost struct {
	// GOOS is the host's operating system as a runtime.GOOS value. A platform
	// condition's os is compared against it.
	GOOS string
	// RunCommand runs a rendered command condition's script and returns nil iff
	// it exited 0. The controller and the spore run scripts differently (the
	// controller inherits the caller's environment so a condition can check
	// that a variable such as GITHUB_TOKEN is set; spored runs as root with a
	// minimal environment, like its run steps), so each side supplies its own.
	RunCommand func(ctx context.Context, script string) error
}

// CheckConditions evaluates conditions in order and returns the first failure.
// It is how both halves of an install honour a spec's "pre-flight checks
// before install" (spore-plugins AUTHORING.md): spawn runs conditions.local on
// the controller before any local step, and spored runs conditions.remote
// before the remote install phase (#1040).
func CheckConditions(ctx context.Context, conds []Condition, tmplCtx TemplateContext, host ConditionHost) error {
	for _, c := range conds {
		if err := CheckCondition(ctx, c, tmplCtx, host); err != nil {
			return err
		}
	}
	return nil
}

// CheckCondition evaluates one condition. The error leads with the condition's
// message (the text a plugin author wrote for the user) when it has one.
//
// Every type the validator accepts (validConditionTypes) must have a case here,
// and any other type is a failure, not a pass: an unknown check that is skipped
// is the silent-ignore #1040 is about.
func CheckCondition(ctx context.Context, c Condition, tmplCtx TemplateContext, host ConditionHost) error {
	err := evalCondition(ctx, c, tmplCtx, host)
	if err == nil {
		return nil
	}
	if c.Message != "" {
		return fmt.Errorf("%s: %w", c.Message, err)
	}
	return fmt.Errorf("condition %q failed: %w", c.Type, err)
}

func evalCondition(ctx context.Context, c Condition, tmplCtx TemplateContext, host ConditionHost) error {
	switch c.Type {
	case "command":
		if strings.TrimSpace(c.Run) == "" {
			return fmt.Errorf("command condition has no run")
		}
		// Condition commands are template text like run steps (Validate scans
		// them for {{ config.* }} references), so render them the same way,
		// shell-escaped.
		rendered, err := RenderShellStep(Step{Type: "run", Run: c.Run}, tmplCtx)
		if err != nil {
			return fmt.Errorf("render: %w", err)
		}
		if host.RunCommand == nil {
			return fmt.Errorf("no command runner on this host")
		}
		if err := host.RunCommand(ctx, rendered.Run); err != nil {
			return fmt.Errorf("command %q: %w", rendered.Run, err)
		}
		return nil
	case "platform":
		if c.OS == "" {
			// No os means no platform constraint (the field is optional in the
			// spec and the validator accepts its absence).
			return nil
		}
		if !MatchesOS(c.OS, host.GOOS) {
			return fmt.Errorf("requires OS %q, running on %q", c.OS, host.GOOS)
		}
		return nil
	default:
		return fmt.Errorf("unknown condition type %q", c.Type)
	}
}

// MatchesOS reports whether a platform condition's os names goos. It is
// case- and whitespace-insensitive, the same normalisation plugin.CheckSporeOS
// applies to the Windows refusal (#1001), so "Linux" in a spec matches a Linux
// spore and the two checks never disagree about what OS a spore is.
func MatchesOS(want, goos string) bool {
	return NormalizeOS(want) == NormalizeOS(goos)
}

// NormalizeOS lower-cases and trims an OS name (a GOOS value, a spawn:os tag
// value, or a platform condition's os). Empty means Linux, the convention for
// an absent spawn:os tag that the Windows refusal also follows. (A platform
// condition with no os is handled before this: it imposes no constraint.)
func NormalizeOS(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "linux"
	}
	return s
}
