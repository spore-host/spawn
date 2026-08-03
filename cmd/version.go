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
		commit, date := versionDetail()
		fmt.Printf("🌱 Spawn - EC2 Instance Lifecycle Manager\n\n")
		fmt.Printf("Version:    %s\n", v)
		fmt.Printf("Git Commit: %s\n", commit)
		fmt.Printf("Build Date: %s\n", date)
		fmt.Printf("\nProject:    https://spore.host\n")

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
}
