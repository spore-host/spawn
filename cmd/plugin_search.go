package cmd

// Plugin discovery: `spawn plugin search`, `spawn plugin info`, and the index
// generator `spawn plugin gen-index`.
//
// `plugin list` answers "what is installed on this instance". These answer "what
// exists at all" — previously unanswerable, so a plugin you hadn't read the
// registry README for was undiscoverable, and a typo looked identical to a
// plugin that didn't exist (spawn#448).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/plugin"
)

var (
	pluginSearchRefresh  bool
	pluginGenIndexOut    string
	pluginGenIndexSource string
	pluginGenIndexTime   string
)

// pluginIndex loads the official registry index, reporting provenance on stderr
// so a cached or stale answer is never mistaken for a live one.
func pluginIndex(ctx context.Context, cmd *cobra.Command, refresh bool) (*plugin.Index, error) {
	res, err := plugin.NewIndexFetcher().Fetch(ctx, plugin.OfficialIndexSource(), refresh)
	if err != nil {
		// "No index published" is a registry-side state, not user error, and it
		// leaves discovery with nothing to show. Say what still works, so the
		// answer isn't a dead end — installing by name never needed the index.
		if strings.Contains(err.Error(), "no plugin index published") {
			return nil, fmt.Errorf("%w\n"+
				"  The registry has not published a discovery index yet.\n"+
				"  Installing by name still works: spawn plugin install <name> --instance <id>", err)
		}
		return nil, err
	}
	errOut := cmd.ErrOrStderr()
	if res.StaleErr != nil {
		fmt.Fprintf(errOut, "warning: could not reach the plugin registry (%v)\n", res.StaleErr)
		fmt.Fprintf(errOut, "         showing a cached index from %s — a plugin published since then won't appear\n",
			humanizeAge(res.CachedAt))
	}
	return res.Index, nil
}

// indexProvenance is the one-line footer describing where a listing came from
// and how old it is, so a user comparing plugins knows the answer's vintage.
func indexProvenance(idx *plugin.Index) string {
	src := idx.Source
	if src == "" {
		src = "spore-host/spore-plugins"
	}
	return fmt.Sprintf("index: %s, generated %s", src, humanizeAge(idx.GeneratedAt))
}

// humanizeAge renders a timestamp as an age ("3h ago"). A zero time is unknown
// rather than 1970.
func humanizeAge(t time.Time) string {
	if t.IsZero() {
		return "at an unknown time"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		// A clock-skew or future-stamped index; report the stamp itself rather
		// than a nonsensical negative age.
		return t.UTC().Format(time.RFC3339)
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

var pluginSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search the plugin registry for available plugins",
	Long: `Search the official plugin registry (spore-host/spore-plugins) for plugins
available to install. With no query, lists everything.

Reads a generated index published by the registry, cached locally so this works
offline; the age of what you're seeing is always shown. This lists what EXISTS —
use 'spawn plugin list --instance <id>' for what is installed on an instance.

Examples:
  spawn plugin search
  spawn plugin search jupyter
  spawn plugin search --refresh`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var query string
		if len(args) == 1 {
			query = args[0]
		}
		idx, err := pluginIndex(cmd.Context(), cmd, pluginSearchRefresh)
		if err != nil {
			return err
		}
		hits := idx.Search(query)

		if getOutputFormat() == "json" {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(hits)
		}

		out := cmd.OutOrStdout()
		if len(hits) == 0 {
			fmt.Fprintf(out, "No plugins match %q.\n", query)
			fmt.Fprintf(out, "  %d plugin(s) in the registry; run 'spawn plugin search' to list them all.\n", len(idx.Plugins))
			fmt.Fprintf(out, "  %s\n", indexProvenance(idx))
			return nil
		}

		w := newTableWriter(out)
		_, _ = fmt.Fprintln(w, "NAME\tVERSION\tDESCRIPTION")
		for _, e := range hits {
			desc := e.Description
			// Flag root up front: it's the single most consequential thing about
			// a plugin you haven't installed yet.
			if e.RequiresRoot {
				desc += " [root]"
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", e.Name, e.Version, desc)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(out, "\n  %s\n", indexProvenance(idx))
		fmt.Fprintf(out, "  install: spawn plugin install <name> --instance <id>\n")
		return nil
	},
}

var pluginInfoCmd = &cobra.Command{
	Use:   "info <name>",
	Short: "Show registry details for a plugin",
	Long: `Show what the registry knows about a plugin: version, description, config
parameters, and declared capability surface.

This reads the registry index and contacts no instance. It describes the plugin
as PUBLISHED — for the full spec of what installing would run, including every
step, use 'spawn plugin inspect <ref>'.

Examples:
  spawn plugin info tailscale
  spawn plugin info jupyterlab --output json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		idx, err := pluginIndex(cmd.Context(), cmd, pluginSearchRefresh)
		if err != nil {
			return err
		}
		entry, ok := idx.Lookup(name)
		if !ok {
			// Suggest near-misses: a typo and a nonexistent plugin used to be
			// indistinguishable, which is half of what the index is for.
			if near := idx.Search(name); len(near) > 0 {
				var names []string
				for _, e := range near {
					names = append(names, e.Name)
				}
				return fmt.Errorf("no plugin named %q in the registry; did you mean: %s", name, strings.Join(names, ", "))
			}
			return fmt.Errorf("no plugin named %q in the registry (%d available; run 'spawn plugin search')", name, len(idx.Plugins))
		}

		if getOutputFormat() == "json" {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(entry)
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "%s %s\n", entry.Name, entry.Version)
		if entry.Description != "" {
			fmt.Fprintf(out, "  %s\n", entry.Description)
		}
		fmt.Fprintln(out)
		src := idx.Source
		if src == "" {
			src = "spore-host/spore-plugins"
		}
		fmt.Fprintf(out, "Source:     %s (%s)\n", src, entry.Path)
		if entry.Verified {
			fmt.Fprintf(out, "Provenance: official registry — released builds are signed and verified at install\n")
		} else {
			fmt.Fprintf(out, "Provenance: unofficial — not signed by the official registry workflow\n")
		}

		if len(entry.ConfigParams) > 0 {
			fmt.Fprintf(out, "\nConfig:\n")
			w := newTableWriter(out)
			_, _ = fmt.Fprintln(w, "  KEY\tTYPE\tREQUIRED")
			for _, p := range entry.ConfigParams {
				req := ""
				if p.Required {
					req = "yes"
				}
				typ := p.Type
				if typ == "" {
					typ = "string"
				}
				_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", p.Name, typ, req)
			}
			if err := w.Flush(); err != nil {
				return err
			}
		}

		// Only print a capability line when the plugin declared permissions —
		// silence here means "not declared", and printing "root: no" for an
		// undeclared plugin would assert something the spec never said.
		if entry.RequiresRoot || len(entry.OpensPorts) > 0 {
			fmt.Fprintf(out, "\nDeclared capabilities:\n")
			if entry.RequiresRoot {
				fmt.Fprintf(out, "  runs steps as root on the instance\n")
			}
			if len(entry.OpensPorts) > 0 {
				var ports []string
				for _, p := range entry.OpensPorts {
					ports = append(ports, fmt.Sprint(p))
				}
				fmt.Fprintf(out, "  opens port(s): %s\n", strings.Join(ports, ", "))
			}
		}

		fmt.Fprintf(out, "\n  %s\n", indexProvenance(idx))
		fmt.Fprintf(out, "  full spec: spawn plugin inspect %s\n", entry.Name)
		fmt.Fprintf(out, "  install:   spawn plugin install %s --instance <id>\n", entry.Name)
		return nil
	},
}

var pluginGenIndexCmd = &cobra.Command{
	Use:   "gen-index <plugins-dir>",
	Short: "Generate the registry discovery index (offline)",
	// Not hidden, matching `plugin manifest` — the other registry-side generator.
	// A hidden command is also undetectable: `gen-index --help` on a spawn that
	// lacks it falls through to `plugin --help` and exits 0, so a registry CI
	// probing for support would think an old binary had it.
	Long: `Generate index.json for a plugin registry from its plugins/ directory.

This is the generator side of plugin discovery, in the same arrangement as
'spawn plugin manifest': it lives here, and the registry's CI invokes it, so the
index is always derived by the same parser that installs plugins and can never
describe a spec differently from the spec itself.

--generated-at takes an RFC3339 timestamp (CI passes the commit time) so
regenerating an unchanged registry produces a byte-identical file. It defaults to
now, which makes every run differ — fine locally, churn in CI.

Examples:
  spawn plugin gen-index ./plugins -o index.json
  spawn plugin gen-index ./plugins --generated-at 2026-07-29T00:00:00Z`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		stamp := time.Now()
		if pluginGenIndexTime != "" {
			t, err := time.Parse(time.RFC3339, pluginGenIndexTime)
			if err != nil {
				return fmt.Errorf("parse --generated-at: %w", err)
			}
			stamp = t
		}
		// verified tracks whether this is the spore-host-owned registry, which is
		// the only source whose releases the signature policy can verify
		// (pkg/plugin/signature.go pins that workflow identity).
		verified := pluginGenIndexSource == "spore-host/spore-plugins"
		idx, err := plugin.BuildIndex(args[0], pluginGenIndexSource, verified, stamp)
		if err != nil {
			return err
		}
		data, err := idx.Encode()
		if err != nil {
			return err
		}
		if pluginGenIndexOut == "" || pluginGenIndexOut == "-" {
			_, err = cmd.OutOrStdout().Write(data)
			return err
		}
		if err := os.WriteFile(pluginGenIndexOut, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", pluginGenIndexOut, err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s: %d plugin(s) from %s\n",
			pluginGenIndexOut, len(idx.Plugins), pluginGenIndexSource)
		return nil
	},
}

func init() {
	pluginCmd.AddCommand(pluginSearchCmd)
	pluginCmd.AddCommand(pluginInfoCmd)
	pluginCmd.AddCommand(pluginGenIndexCmd)

	for _, sub := range []*cobra.Command{pluginSearchCmd, pluginInfoCmd} {
		sub.Flags().BoolVar(&pluginSearchRefresh, "refresh", false, "Bypass the local cache and refetch the registry index")
	}

	pluginGenIndexCmd.Flags().StringVarP(&pluginGenIndexOut, "output", "o", "", "Write index to this file instead of stdout")
	pluginGenIndexCmd.Flags().StringVar(&pluginGenIndexSource, "source", "spore-host/spore-plugins", "owner/repo the index describes")
	pluginGenIndexCmd.Flags().StringVar(&pluginGenIndexTime, "generated-at", "", "RFC3339 generation timestamp (default: now)")
}
