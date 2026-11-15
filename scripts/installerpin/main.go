// Command installerpin reads and moves the installer pins the weekly
// .github/workflows/installer-pins.yml maintains (#1620): the sha256s of the
// AWS CLI MSI and SSM agent setup the Windows bootstrap runs
// (pkg/sporedinstall/installer_pins.go), and of the Amazon DCV tarballs plus
// the DCV signing key's fingerprints (cmd/app.go).
//
// It is the Go half of scripts/installer-pins-windows.ps1 and
// scripts/installer-pins-dcv.sh, which download and verify (Authenticode on a
// Windows runner; the DCV key and every RPM's signature on Linux). This tool
// only reads and edits Go source and the CHANGELOG, and writes the PR's or
// fallback issue's text, so what a test can check offline is here:
//
//	go -C scripts run ./installerpin show
//	go -C scripts run ./installerpin set [--msi-version V --msi-sha256 H] \
//	    [--ssm-version V --ssm-sha256 H] \
//	    [--dcv-version V --dcv-sha256-x86_64 H --dcv-sha256-aarch64 H --dcv-key-fingerprints "F F"] \
//	    --summary-out FILE
//	gh pr create ... 2>&1 | go -C scripts run ./installerpin pr-refused
//	go -C scripts run ./installerpin issue-title
//	go -C scripts run ./installerpin issue-body --summary FILE --compare URL
//
// Paths default to the checkout layout seen from scripts/ (go -C scripts).
// The constants are found with go/parser (scripts/internal/pinkit), each
// declared once as a string literal, so a renamed one fails here instead of
// making the refresh a silent no-op.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spore-host/spawn/scripts/internal/pinkit"
)

// The constants, per file.
var (
	appConsts       = []string{"dcvVersion", "dcvVersionMajorMinor", "dcvPinVersion", "dcvTGZSHA256x86_64", "dcvTGZSHA256aarch64", "dcvKeyFingerprints"}
	pinConsts       = []string{"windowsAWSCLIMSIVersion", "windowsAWSCLIMSISHA256", "windowsSSMAgentVersion", "windowsSSMAgentSHA256"}
	bootstrapConsts = []string{"awsCLIVersion"}
)

var (
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	cliRe     = regexp.MustCompile(`^2\.\d+\.\d+$`)
	ssmRe     = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
	dcvRe     = regexp.MustCompile(`^\d{4}\.\d+-\d+$`)
	fprListRe = regexp.MustCompile(`^[0-9A-F]{40}( [0-9A-F]{40})*$`)
)

var errNotRefused = errors.New("not GitHub's refusal to let Actions create pull requests")

// IssueTitle is the one fallback issue's title; the workflow updates the open
// issue with this title rather than opening another.
const IssueTitle = "Installer pins: a verified refresh is ready to open as a PR"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if !errors.Is(err, errNotRefused) {
			fmt.Fprintln(os.Stderr, "installerpin:", err)
		}
		os.Exit(1)
	}
}

type paths struct{ app, pins, bootstrap, changelog string }

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: installerpin show|set|pr-refused|issue-title|issue-body [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var p paths
	fs.StringVar(&p.app, "app", "../cmd/app.go", "path to cmd/app.go")
	fs.StringVar(&p.pins, "pins", "../pkg/sporedinstall/installer_pins.go", "path to pkg/sporedinstall/installer_pins.go")
	fs.StringVar(&p.bootstrap, "bootstrap", "../pkg/launcher/bootstrap.go", "path to pkg/launcher/bootstrap.go (show)")
	fs.StringVar(&p.changelog, "changelog", "../CHANGELOG.md", "path to CHANGELOG.md (set)")
	var in Inputs
	fs.StringVar(&in.MSIVersion, "msi-version", "", "AWS CLI MSI release")
	fs.StringVar(&in.MSISHA256, "msi-sha256", "", "sha256 of AWSCLIV2-<version>.msi")
	fs.StringVar(&in.SSMVersion, "ssm-version", "", "SSM agent release")
	fs.StringVar(&in.SSMSHA256, "ssm-sha256", "", "sha256 of its AmazonSSMAgentSetup.exe")
	fs.StringVar(&in.DCVVersion, "dcv-version", "", "the dcvVersion the DCV pins are for")
	fs.StringVar(&in.DCVx8664, "dcv-sha256-x86_64", "", "sha256 of the x86_64 tarball")
	fs.StringVar(&in.DCVaarch64, "dcv-sha256-aarch64", "", "sha256 of the aarch64 tarball")
	fs.StringVar(&in.DCVKeyFprs, "dcv-key-fingerprints", "", "NICE-GPG-KEY primary fingerprints, space-separated")
	summaryOut := fs.String("summary-out", "", "write the markdown summary of what moved here (set)")
	summary := fs.String("summary", "", "the summary file set wrote (issue-body)")
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
		_, err := fmt.Fprintln(stdout, IssueTitle)
		return err
	case "issue-body":
		if !strings.HasPrefix(*compare, "https://github.com/") {
			return fmt.Errorf("--compare %q is not a github.com URL", *compare)
		}
		s, err := os.ReadFile(*summary) //nolint:gosec // a file this tool wrote
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(stdout, issueBody(string(s), *compare))
		return err
	case "show":
		cur, err := readAll(p, true)
		if err != nil {
			return err
		}
		names := append(append(append([]string(nil), appConsts...), pinConsts...), bootstrapConsts...)
		for _, n := range names {
			fmt.Fprintf(stdout, "%s=%s\n", n, cur[n])
		}
		return nil
	case "set":
		changed, sum, err := set(p, in)
		if err != nil {
			return err
		}
		if *summaryOut != "" {
			if err := os.WriteFile(*summaryOut, []byte(sum), 0o600); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "changed=%t\n", changed)
		return err
	}
	return fmt.Errorf("unknown subcommand %q (want show, set, pr-refused, issue-title or issue-body)", args[0])
}

func readAll(p paths, withBootstrap bool) (map[string]string, error) {
	out := map[string]string{}
	files := []struct {
		path  string
		names []string
	}{{p.app, appConsts}, {p.pins, pinConsts}}
	if withBootstrap {
		files = append(files, struct {
			path  string
			names []string
		}{p.bootstrap, bootstrapConsts})
	}
	for _, f := range files {
		src, err := os.ReadFile(f.path)
		if err != nil {
			return nil, err
		}
		cs, err := pinkit.ReadConsts(src, f.path, "scripts/installerpin", f.names)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.path, err)
		}
		for n, c := range cs {
			out[n] = c.Value
		}
	}
	return out, nil
}

// Inputs is what set may move; an empty group is left as it is.
type Inputs struct {
	MSIVersion, MSISHA256                        string
	SSMVersion, SSMSHA256                        string
	DCVVersion, DCVx8664, DCVaarch64, DCVKeyFprs string
}

func group(vals ...string) (set bool, err error) {
	n := 0
	for _, v := range vals {
		if v != "" {
			n++
		}
	}
	switch n {
	case 0:
		return false, nil
	case len(vals):
		return true, nil
	}
	return false, errors.New("each pin group is given whole or not at all")
}

// set validates in, rewrites the constants that move, and adds a CHANGELOG
// entry. It reports whether anything moved, and the markdown summary.
func set(p paths, in Inputs) (bool, string, error) {
	cur, err := readAll(p, false)
	if err != nil {
		return false, "", err
	}
	appWant, pinWant := map[string]string{}, map[string]string{}
	var lines []string
	if ok, err := group(in.MSIVersion, in.MSISHA256); err != nil {
		return false, "", fmt.Errorf("--msi-*: %w", err)
	} else if ok {
		if !cliRe.MatchString(in.MSIVersion) || !sha256Re.MatchString(in.MSISHA256) {
			return false, "", fmt.Errorf("--msi-version %q / --msi-sha256 %q malformed", in.MSIVersion, in.MSISHA256)
		}
		if cur["windowsAWSCLIMSIVersion"] != in.MSIVersion || cur["windowsAWSCLIMSISHA256"] != in.MSISHA256 {
			pinWant["windowsAWSCLIMSIVersion"], pinWant["windowsAWSCLIMSISHA256"] = in.MSIVersion, in.MSISHA256
			lines = append(lines, fmt.Sprintf("AWS CLI MSI `AWSCLIV2-%s.msi` (was %s): sha256 `%s`, Authenticode Valid, Amazon signer", in.MSIVersion, orNone(cur["windowsAWSCLIMSIVersion"]), in.MSISHA256))
		}
	}
	if ok, err := group(in.SSMVersion, in.SSMSHA256); err != nil {
		return false, "", fmt.Errorf("--ssm-*: %w", err)
	} else if ok {
		if !ssmRe.MatchString(in.SSMVersion) || !sha256Re.MatchString(in.SSMSHA256) {
			return false, "", fmt.Errorf("--ssm-version %q / --ssm-sha256 %q malformed", in.SSMVersion, in.SSMSHA256)
		}
		if cur["windowsSSMAgentVersion"] != in.SSMVersion || cur["windowsSSMAgentSHA256"] != in.SSMSHA256 {
			pinWant["windowsSSMAgentVersion"], pinWant["windowsSSMAgentSHA256"] = in.SSMVersion, in.SSMSHA256
			lines = append(lines, fmt.Sprintf("SSM agent `amazon-ssm-<region>/%s/windows_amd64/AmazonSSMAgentSetup.exe` (was %s): sha256 `%s`, Authenticode Valid, Amazon signer", in.SSMVersion, orNone(cur["windowsSSMAgentVersion"]), in.SSMSHA256))
		}
	}
	if ok, err := group(in.DCVVersion, in.DCVx8664, in.DCVaarch64, in.DCVKeyFprs); err != nil {
		return false, "", fmt.Errorf("--dcv-*: %w", err)
	} else if ok {
		fprs := strings.Fields(in.DCVKeyFprs)
		sort.Strings(fprs)
		joined := strings.Join(fprs, " ")
		if !dcvRe.MatchString(in.DCVVersion) || !sha256Re.MatchString(in.DCVx8664) || !sha256Re.MatchString(in.DCVaarch64) || !fprListRe.MatchString(joined) {
			return false, "", errors.New("a --dcv-* value is malformed")
		}
		if in.DCVVersion != cur["dcvVersion"] {
			return false, "", fmt.Errorf("--dcv-version %s is not cmd/app.go's dcvVersion %s", in.DCVVersion, cur["dcvVersion"])
		}
		if cur["dcvPinVersion"] != in.DCVVersion || cur["dcvTGZSHA256x86_64"] != in.DCVx8664 || cur["dcvTGZSHA256aarch64"] != in.DCVaarch64 || cur["dcvKeyFingerprints"] != joined {
			appWant["dcvPinVersion"], appWant["dcvTGZSHA256x86_64"], appWant["dcvTGZSHA256aarch64"], appWant["dcvKeyFingerprints"] = in.DCVVersion, in.DCVx8664, in.DCVaarch64, joined
			line := fmt.Sprintf("Amazon DCV %s (was %s): tarball sha256 x86_64 `%s`, aarch64 `%s`; every RPM in both verified against NICE-GPG-KEY, primary fingerprints `%s`", in.DCVVersion, orNone(cur["dcvPinVersion"]), in.DCVx8664, in.DCVaarch64, joined)
			if old := cur["dcvKeyFingerprints"]; old != "" && old != joined {
				line += fmt.Sprintf(". **The DCV signing key changed** (was `%s`): confirm the new key with AWS before merging", old)
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return false, "", nil
	}
	for _, w := range []struct {
		path string
		want map[string]string
	}{{p.app, appWant}, {p.pins, pinWant}} {
		if len(w.want) == 0 {
			continue
		}
		src, err := os.ReadFile(w.path)
		if err != nil {
			return false, "", err
		}
		out, err := pinkit.RewriteConsts(src, w.path, "scripts/installerpin", w.want)
		if err != nil {
			return false, "", err
		}
		if err := os.WriteFile(w.path, out, 0o644); err != nil { //nolint:gosec // a tracked source file
			return false, "", err
		}
	}
	cl, err := os.ReadFile(p.changelog)
	if err != nil {
		return false, "", err
	}
	cl, err = pinkit.AddUnreleasedChanged(cl, changelogEntry(lines))
	if err != nil {
		return false, "", fmt.Errorf("%s: %w", p.changelog, err)
	}
	if err := os.WriteFile(p.changelog, cl, 0o644); err != nil { //nolint:gosec // a tracked source file
		return false, "", err
	}
	var sum strings.Builder
	for _, l := range lines {
		sum.WriteString("- " + l + "\n")
	}
	return true, sum.String(), nil
}

func orNone(v string) string {
	if v == "" {
		return "unpinned"
	}
	return v
}

func changelogEntry(lines []string) string {
	return "- **Installer pins moved** (#1620): " + strings.Join(lines, "; ") +
		". The Windows bootstrap and the DCV install refuse any other bytes. " +
		"Opened by the weekly installer pins workflow."
}

func issueBody(summary, compare string) string {
	return strings.Join([]string{
		"The weekly `Installer pins` workflow (`.github/workflows/installer-pins.yml`, #1620) verified new installer pins and pushed them, but GitHub would not let `GITHUB_TOKEN` open the PR (\"Allow GitHub Actions to create and approve pull requests\" is off). **Open the PR from this link:**",
		"",
		compare,
		"",
		strings.TrimRight(summary, "\n"),
		"",
		"Each Windows installer was downloaded on a Windows runner and passed `Get-AuthenticodeSignature` (Valid, Amazon signer) before its sha256 was pinned; every DCV RPM verified with `rpmkeys -K` against the downloaded NICE key. The run's log has the output. Close this issue once the PR is open.",
		"",
	}, "\n")
}
