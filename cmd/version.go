package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/update"
	"github.com/spore-host/spawn/pkg/buildinfo"
)

// Injected at build time by GoReleaser. Left empty rather than "unknown" so the
// build-metadata fallback can tell "not injected" from "injected as unknown".
var (
	GitCommit = ""
	BuildDate = ""
)

// version reports the running binary's version: the value injected at release
// time, else what the Go toolchain stamped in. See pkg/buildinfo for why there
// is no hardcoded default.
func version() string {
	return buildinfo.Version(Version)
}

// versionDetail returns the commit and build date to display, each falling back
// to the toolchain's stamp and then to "unknown".
func versionDetail() (commit, date string) {
	revision, buildTime := buildinfo.Revision()
	commit = firstNonEmpty(GitCommit, revision, "unknown")
	date = firstNonEmpty(BuildDate, buildTime, "unknown")
	return commit, date
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Long:  `Display version, build date, and git commit information for spawn.`,
	Run: func(cmd *cobra.Command, args []string) {
		v := version()
		fmt.Print(versionText())

		// Explicit, user-initiated check — report whether a newer release exists.
		// CheckNow is synchronous and ungated (unlike the incidental CheckAsync
		// notice on other commands); nil means we couldn't reach GitHub. Skipped
		// entirely for a dev build: there is nothing to compare, so the request
		// would only cost latency.
		var res *update.Result
		if !buildinfo.IsDev(v) {
			res = update.CheckNow("spawn", v)
		}
		fmt.Print(renderUpdateNotice(res, v))
	},
}

// versionText is the version block shared by `spawn version` and
// `spawn --version`, so the two can never report different things. Only the
// subcommand adds the network update check after it: a flag that is often run
// by scripts to probe the binary should not make a request to GitHub.
func versionText() string {
	commit, date := versionDetail()
	var b strings.Builder
	fmt.Fprintf(&b, "🌱 spawn - ephemeral compute for AWS\n\n")
	fmt.Fprintf(&b, "Version:    %s\n", version())
	fmt.Fprintf(&b, "Git Commit: %s\n", commit)
	fmt.Fprintf(&b, "Build Date: %s\n", date)
	fmt.Fprintf(&b, "\nProject:    https://spore.host\n")
	return b.String()
}

// renderUpdateNotice formats the on-demand update check for the version command:
// a dev build → say the comparison is meaningless rather than fake an answer; a
// nil result (GitHub unreachable) → "couldn't check"; a newer release → an
// upgrade line; otherwise → "on the latest version". Pure, so it's unit-tested
// without a network call.
func renderUpdateNotice(res *update.Result, current string) string {
	switch {
	// A dev build has no release number to compare, and libs' semver parser reads
	// "dev" as 0.0.0 — so without this branch every source build would announce
	// an upgrade to whatever is latest, which is noise at best and, since it also
	// fires on a build NEWER than the last release, wrong.
	case buildinfo.IsDev(current):
		return "\n(development build — not comparing against releases)\n"
	case res == nil:
		return "\n(couldn't check for updates)\n"
	case res.HasUpdate():
		return fmt.Sprintf("\n⬆️  A newer version is available: %s → %s\n    %s\n",
			res.CurrentVersion, res.LatestVersion, res.UpdateURL)
	default:
		return "\n✓ You're on the latest version.\n"
	}
}

func init() {
	rootCmd.AddCommand(versionCmd)

	// `spawn --version` (spore-host/truffle#189). Setting Version is what makes
	// cobra register the flag; the template replaces cobra's default
	// "spawn version X" line with the same block `spawn version` prints. The
	// template text is computed here, once, which is fine: nothing about the
	// binary's version changes while it runs.
	rootCmd.Version = version()
	rootCmd.SetVersionTemplate(versionTemplateEscape(versionText()))
}

// versionTemplateEscape makes literal text safe to use as a text/template.
func versionTemplateEscape(s string) string {
	return strings.NewReplacer("{{", `{{"{{"}}`).Replace(s)
}
