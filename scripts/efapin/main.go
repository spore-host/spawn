// Command efapin reads and moves the EFA installer the MPI user data runs as
// root with --efa (#1505): the two constants efaInstallerVersion and
// efaInstallerSHA256 in pkg/userdata/efa.go.
//
// It is the Go half of scripts/efa-pin.sh, which the weekly
// .github/workflows/efa-pin.yml runs (bump) and the release workflow runs
// (verify). That script downloads the installer tarball and its .sig from
// efa-installer.amazonaws.com and verifies the signature with gpg against AWS's
// EFA key committed beside this file; this tool only reads and edits Go source
// and the CHANGELOG, and writes the fallback issue, so what a test can check
// offline is in Go:
//
//	go -C scripts run ./efapin show  --source ../pkg/userdata/efa.go
//	go -C scripts run ./efapin set   --source ../pkg/userdata/efa.go \
//	    --changelog ../CHANGELOG.md --version 1.52.0 --sha256 <hex>
//	gh pr create ... 2>&1 | go -C scripts run ./efapin pr-refused
//	go -C scripts run ./efapin issue-title --from 1.51.0 --version 1.52.0
//	go -C scripts run ./efapin issue-body  --from ... --version ... --sha256 <hex> --compare <url>
//
// The constants are found with go/parser (scripts/internal/pinkit), and each
// must be declared exactly once as a string literal, so renaming one fails
// here instead of making the bump a silent no-op.
//
// Moving the pin changes only the MPI user data that --efa renders at launch,
// not the published bootstrap body, so no Lambda redeploy follows; the
// CHANGELOG entry goes under Changed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spore-host/spawn/scripts/internal/pinkit"
)

// The constants the bump moves, in the order `show` prints them.
const (
	constVersion = "efaInstallerVersion"
	constSHA256  = "efaInstallerSHA256"
)

var pinConsts = []string{constVersion, constSHA256}

// KeyFingerprint is AWS's EFA installer signing key ("Amazon EC2 EFA
// <ec2-efa-maintainers@amazon.com>"), as the EC2 User Guide publishes it
// (https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-start.html: "4E90
// 91BC BB97 A96B 26B1 5E59 A054 80B1 DD2D 3CCC"). The key material is
// aws-efa-installer.key beside this file, downloaded from the URL that page
// gives, https://efa-installer.amazonaws.com/aws-efa-installer.key; the test
// checks its fingerprint is this one, and efa-pin.sh accepts a signature only
// from it.
const KeyFingerprint = "4E9091BCBB97A96B26B15E59A05480B1DD2D3CCC"

var (
	versionRe = regexp.MustCompile(`^1\.\d+\.\d+$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// errNotRefused is pr-refused's "no": the failure was something else, which
// must fail the run. main prints nothing for it.
var errNotRefused = errors.New("not GitHub's refusal to let Actions create pull requests")

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if !errors.Is(err, errNotRefused) {
			fmt.Fprintln(os.Stderr, "efapin:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: efapin show|set|pr-refused|issue-title|issue-body [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	source := fs.String("source", "pkg/userdata/efa.go", "path to pkg/userdata/efa.go")
	changelog := fs.String("changelog", "CHANGELOG.md", "path to CHANGELOG.md (set only)")
	from := fs.String("from", "", "the version pinned now (issue-title, issue-body)")
	version := fs.String("version", "", "the new EFA installer version")
	sum := fs.String("sha256", "", "sha256 of aws-efa-installer-<version>.tar.gz")
	compare := fs.String("compare", "", "the compare URL of the pushed branch (issue-body)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "pr-refused":
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		if !pinkit.PRRefused(string(b)) {
			return errNotRefused
		}
		return nil
	case "issue-title":
		if !versionRe.MatchString(*from) || !versionRe.MatchString(*version) {
			return fmt.Errorf("--from %q / --version %q: want 1.N.N", *from, *version)
		}
		_, err := fmt.Fprintln(stdout, issueTitle(*from, *version))
		return err
	case "issue-body":
		if !versionRe.MatchString(*from) {
			return fmt.Errorf("--from %q: want 1.N.N", *from)
		}
		if err := validate(*version, *sum); err != nil {
			return err
		}
		if !strings.HasPrefix(*compare, "https://github.com/") {
			return fmt.Errorf("--compare %q is not a github.com URL", *compare)
		}
		_, err := fmt.Fprint(stdout, issueBody(*from, *version, *sum, *compare))
		return err
	case "show", "set":
	default:
		return fmt.Errorf("unknown subcommand %q (want show, set, pr-refused, issue-title or issue-body)", args[0])
	}
	src, err := os.ReadFile(*source)
	if err != nil {
		return err
	}
	pins, err := readPins(src)
	if err != nil {
		return fmt.Errorf("%s: %w", *source, err)
	}
	if args[0] == "show" {
		for _, n := range pinConsts {
			fmt.Fprintf(stdout, "%s=%s\n", n, pins[n].Value)
		}
		return nil
	}
	out, err := rewritePins(src, *version, *sum)
	if err != nil {
		return err
	}
	cl, err := os.ReadFile(*changelog)
	if err != nil {
		return err
	}
	cl, err = pinkit.AddUnreleasedChanged(cl, changelogEntry(pins[constVersion].Value, *version, *sum))
	if err != nil {
		return fmt.Errorf("%s: %w", *changelog, err)
	}
	if err := os.WriteFile(*source, out, 0o644); err != nil { //nolint:gosec // a tracked source file
		return err
	}
	return os.WriteFile(*changelog, cl, 0o644) //nolint:gosec // a tracked source file
}

func readPins(src []byte) (map[string]pinkit.Const, error) {
	return pinkit.ReadConsts(src, "efa.go", "scripts/efapin", pinConsts)
}

func validate(version, sum string) error {
	if !versionRe.MatchString(version) {
		return fmt.Errorf("--version %q is not an EFA installer release (1.N.N)", version)
	}
	if !sha256Re.MatchString(sum) {
		return fmt.Errorf("--sha256 %q is not a lowercase hex sha256", sum)
	}
	return nil
}

// rewritePins replaces the two literals in src and nothing else.
func rewritePins(src []byte, version, sum string) ([]byte, error) {
	if err := validate(version, sum); err != nil {
		return nil, err
	}
	return pinkit.RewriteConsts(src, "efa.go", "scripts/efapin", map[string]string{constVersion: version, constSHA256: sum})
}

func changelogEntry(from, version, sum string) string {
	return fmt.Sprintf("- **`--efa` installs EFA installer %[2]s instead of %[1]s** (#1505). "+
		"Its tarball's signature verified against AWS's EFA installer key (`%[3]s`) before its sha256 was pinned: `%[4]s`. "+
		"This changes only the MPI user data, not the published bootstrap body. Check AWS's EFA release notes "+
		"(https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-changelog.html) for distributions it drops. "+
		"Opened by the weekly EFA installer pin workflow.",
		from, version, KeyFingerprint, sum)
}

func issueTitle(from, version string) string {
	return fmt.Sprintf("EFA installer pin bump available: %s → %s", from, version)
}

func issueBody(from, version, sum, compare string) string {
	return strings.Join([]string{
		fmt.Sprintf("A newer EFA installer exists for `--efa`: **%s**, pinned now at %s (#1505).", version, from),
		"",
		"The weekly `EFA installer pin` workflow verified it and pushed the bump, but GitHub would not let `GITHUB_TOKEN` open the PR (\"Allow GitHub Actions to create and approve pull requests\" is off). **Open the PR from this link:**",
		"",
		compare,
		"",
		"| | |",
		"|---|---|",
		"| Version | `" + version + "` |",
		"| sha256, `aws-efa-installer-" + version + ".tar.gz` | `" + sum + "` |",
		"| Signing key fingerprint | `" + KeyFingerprint + "` |",
		"",
		"The tarball's detached signature verified with gpg as made by that key (`scripts/efapin/aws-efa-installer.key`) before the sha256 was pinned; the workflow run's log has the output. Read AWS's EFA release notes for dropped distributions before merging. Close this issue once the PR is open.",
		"",
	}, "\n")
}
