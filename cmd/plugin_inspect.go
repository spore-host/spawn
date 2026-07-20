package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spore-host/spawn/pkg/plugin"
)

// runPluginInspect resolves a plugin ref and prints a preview of what installing
// it would do — WITHOUT executing anything or contacting an instance. It calls
// only the resolver and the static validator; it never touches either executor
// (local_executor.go / pkg/pluginruntime), so it has no side effects.
//
// dryRun only changes the framing (it's reached via `plugin install --dry-run`);
// the rendered content is identical to `plugin inspect`.
func runPluginInspect(ctx context.Context, ref string, dryRun bool) error {
	spec, prov, err := plugin.DefaultResolver().ResolveWithProvenance(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolve plugin %q: %w", ref, err)
	}
	renderInspect(os.Stdout, plugin.ParseRef(ref), spec, prov, dryRun)
	return nil
}

// renderInspect writes the preview for an already-resolved spec. Split out from
// runPluginInspect so it can be unit-tested without a resolver or an instance.
func renderInspect(out io.Writer, pr plugin.PluginRef, spec *plugin.PluginSpec, prov *plugin.Provenance, dryRun bool) {
	if dryRun {
		fmt.Fprintf(out, "DRY RUN — nothing will be installed.\n\n")
	}

	fmt.Fprintf(out, "Plugin:      %s %s\n", spec.Name, orNone(spec.Version))
	fmt.Fprintf(out, "Description: %s\n", orNone(spec.Description))
	fmt.Fprintf(out, "Source:      %s\n", describeSource(pr))
	fmt.Fprintf(out, "Resolved:    %s\n", describeProvenance(prov))
	fmt.Fprintln(out)

	// Static validation, same checks as `spawn plugin validate`. Empty dirName
	// skips the registry dir-name match (we don't have a directory here).
	if verr := spec.Validate(""); verr != nil {
		fmt.Fprintf(out, "⚠ Validation problems:\n")
		for _, line := range strings.Split(verr.Error(), "\n") {
			fmt.Fprintf(out, "  %s\n", strings.TrimLeft(line, " "))
		}
		fmt.Fprintln(out)
	}

	// Trust banner — installing runs the author's code locally and (on the
	// instance) as root.
	fmt.Fprintf(out, "⚠ Installing runs this plugin's code on your machine and, on the\n")
	fmt.Fprintf(out, "  instance, as root. Review the steps below before installing.\n")
	if pr.Host == "github" && pr.Owner != "spore-host" {
		fmt.Fprintf(out, "  This is a third-party source (%s/%s) — content is not signed or audited.\n", pr.Owner, pr.Repo)
	}
	if pr.Version == "" && pr.Host != "local" {
		fmt.Fprintf(out, "  No version pinned — this tracks the default branch and can change. Pin with @<tag|commit>.\n")
	}
	fmt.Fprintln(out)

	renderLocalPlan(out, spec)
	renderRemotePlan(out, spec)
	renderHealth(out, spec)
	renderCleanup(out, spec)
	renderOutputs(out, spec)
	renderPermissions(out, spec)

	fmt.Fprintf(out, "Note: commands, ports, and files created inside `run` shell steps cannot be\n")
	fmt.Fprintf(out, "determined statically. The declared permissions block (if any) is the author's\n")
	fmt.Fprintf(out, "statement of intent, not an enforced sandbox.\n")
}

// describeProvenance renders the resolved commit / content digest line. It makes
// pinning status visible: a commit SHA means immutable; no SHA on a remote ref
// means the fetch tracked a mutable branch/tag (only the content digest pins it).
func describeProvenance(prov *plugin.Provenance) string {
	if prov == nil {
		return "(unknown)"
	}
	if prov.Host == "local" {
		return fmt.Sprintf("local file · sha256 %s", shortHash(prov.ContentSHA256))
	}
	if prov.CommitSHA != "" {
		return fmt.Sprintf("commit %s · sha256 %s", shortHash(prov.CommitSHA), shortHash(prov.ContentSHA256))
	}
	return fmt.Sprintf("unpinned (commit unknown) · sha256 %s", shortHash(prov.ContentSHA256))
}

// shortHash trims a hex digest to its first 12 chars for display (full value is
// recorded in the install record).
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	if h == "" {
		return "(none)"
	}
	return h
}

func describeSource(pr plugin.PluginRef) string {
	switch pr.Host {
	case "local":
		return fmt.Sprintf("local file %s", pr.Name)
	case "github":
		return fmt.Sprintf("github.com/%s/%s → %s @ %s", pr.Owner, pr.Repo, pr.Name, orNone(pr.Version))
	default: // official
		return fmt.Sprintf("official registry (%s/%s) → %s @ %s", pr.Owner, pr.Repo, pr.Name, orNone(pr.Version))
	}
}

func renderLocalPlan(out io.Writer, spec *plugin.PluginSpec) {
	if len(spec.Local.Provision) == 0 && len(spec.Local.EnvPassthrough) == 0 {
		return
	}
	fmt.Fprintf(out, "On your machine (controller):\n")
	if len(spec.Local.EnvPassthrough) > 0 {
		fmt.Fprintf(out, "  reads env: %s\n", strings.Join(spec.Local.EnvPassthrough, ", "))
	}
	for i, st := range spec.Local.Provision {
		fmt.Fprintf(out, "  %d. %s\n", i+1, describeStep(st))
	}
	fmt.Fprintln(out)
}

func renderRemotePlan(out io.Writer, spec *plugin.PluginSpec) {
	phases := []struct {
		label string
		steps []plugin.Step
	}{
		{"install", spec.Remote.Install},
		{"configure", spec.Remote.Configure},
		{"start", spec.Remote.Start},
	}
	total := 0
	for _, p := range phases {
		total += len(p.steps)
	}
	if total == 0 {
		return
	}
	fmt.Fprintf(out, "On the instance:\n")
	for _, p := range phases {
		for i, st := range p.steps {
			fmt.Fprintf(out, "  [%s %d] %s\n", p.label, i+1, describeStep(st))
		}
	}
	fmt.Fprintln(out)
}

func renderHealth(out io.Writer, spec *plugin.PluginSpec) {
	if len(spec.Remote.Health.Steps) == 0 {
		return
	}
	fmt.Fprintf(out, "Health check (every %s):\n", orNone(spec.Remote.Health.Interval))
	for i, st := range spec.Remote.Health.Steps {
		fmt.Fprintf(out, "  %d. %s\n", i+1, describeStep(st))
	}
	fmt.Fprintln(out)
}

func renderCleanup(out io.Writer, spec *plugin.PluginSpec) {
	if len(spec.Local.Deprovision) == 0 && len(spec.Remote.Stop) == 0 {
		return
	}
	fmt.Fprintf(out, "On removal:\n")
	for i, st := range spec.Remote.Stop {
		fmt.Fprintf(out, "  [instance stop %d] %s\n", i+1, describeStep(st))
	}
	for i, st := range spec.Local.Deprovision {
		fmt.Fprintf(out, "  [controller %d] %s\n", i+1, describeStep(st))
	}
	fmt.Fprintln(out)
}

func renderOutputs(out io.Writer, spec *plugin.PluginSpec) {
	if len(spec.Outputs) == 0 {
		return
	}
	names := make([]string, 0, len(spec.Outputs))
	for n := range spec.Outputs {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(out, "Outputs (surfaced after install):\n")
	for _, n := range names {
		fmt.Fprintf(out, "  %s (%s)\n", n, orNone(spec.Outputs[n].Source))
	}
	fmt.Fprintln(out)
}

func renderPermissions(out io.Writer, spec *plugin.PluginSpec) {
	if spec.Permissions == nil {
		fmt.Fprintf(out, "Declared permissions: none declared.\n\n")
		return
	}
	p := spec.Permissions
	fmt.Fprintf(out, "Declared permissions:\n")
	fmt.Fprintf(out, "  controller: network=%t", p.Controller.Network)
	if len(p.Controller.Env) > 0 {
		fmt.Fprintf(out, " env=[%s]", strings.Join(p.Controller.Env, ","))
	}
	if len(p.Controller.Commands) > 0 {
		fmt.Fprintf(out, " commands=[%s]", strings.Join(p.Controller.Commands, ","))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  instance:   root=%t network=%t", p.Instance.Root, p.Instance.Network)
	if len(p.Instance.Ports) > 0 {
		fmt.Fprintf(out, " ports=%v", p.Instance.Ports)
	}
	if len(p.Instance.Files) > 0 {
		fmt.Fprintf(out, " files=[%s]", strings.Join(p.Instance.Files, ","))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out)
}

// describeStep renders one step for the preview without executing it.
func describeStep(st plugin.Step) string {
	switch st.Type {
	case "fetch":
		if st.SHA256 != "" {
			return fmt.Sprintf("download %s → %s (sha256 %s…)", st.URL, st.Dest, st.SHA256[:12])
		}
		return fmt.Sprintf("download %s → %s (unverified — no sha256)", st.URL, st.Dest)
	case "extract":
		return fmt.Sprintf("extract %s → %s", st.Src, st.Dest)
	case "push":
		return fmt.Sprintf("push value to instance (%s)", st.Key)
	default: // run
		who := "root"
		if st.AsUser {
			who = "login user"
		}
		bg := ""
		if st.Background {
			bg = " (background)"
		}
		return fmt.Sprintf("run as %s%s: %s", who, bg, oneLine(st.Run))
	}
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
