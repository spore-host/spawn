package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/curlcompat"
	"github.com/spore-host/spawn/internal/treewalk"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// Gates for the two ways spored silently failed to come up on Enterprise Linux
// AMIs, each of which left an instance with no TTL and no cost limit:
//
//   - #1056: EL8's curl 7.61.1 rejected --retry-all-errors (curl 7.71+), so the
//     installer could not download anything and the bootstrap aborted.
//   - #1052: on SELinux-enforcing EL9, spored was renamed into place from /tmp
//     with cloud-init's cloud_init_tmp_t label, and systemd could not exec it.
//
// The executable versions of both (a fake curl 7.61 on PATH, a fake restorecon)
// live in pkg/sporedinstall. These assert that every generated script is covered.

// renderedLinuxScripts returns every Linux install/boot script spawn generates
// that installs or starts spored, rendered as it ships.
func renderedLinuxScripts(t *testing.T) map[string]string {
	t.Helper()
	boot, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{Username: "ec2-user"})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	decode := func(s string) string {
		if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
			return string(raw)
		}
		return s
	}
	return map[string]string{
		"boot bootstrap (pkg/launcher)":   decode(boot),
		"spored upgrade (SSM)":            buildSporedUpgradeScript("0.135.2"),
		"web app user-data":               decode(buildWebUserData("ghcr.io/x/y:1", 8080, false, false, "us-east-1", nil)),
		"DCV app user-data":               decode(buildDCVUserData("echo hi", "sess-1")),
		"DCV desktop user-data":           decode(buildDesktopDCVUserData("sess-1")),
		"DCV container app user-data":     decode(buildContainerDCVUserData("ghcr.io/x/y:1", false, false, "us-east-1", "sess-1")),
		"verified installer (standalone)": sporedinstall.KeySetupScript() + sporedinstall.VerifiedInstallFunc(),
	}
}

// No curl invocation in anything spawn generates may use an option curl 7.61.1
// (EL8) does not have (#1056).
//
// Two sweeps, because neither alone sees everything:
//   - every non-test Go source file in the module, nested lambda modules
//     included, so a generator nobody listed is still checked (rule 4); and
//   - every rendered script, because the installer's call sites are Sprintf
//     verbs in the source (%[4]s 5 2 -f ...) and only exist in the output.
//
// Invocations are matched as commands — comment lines are skipped — so the
// comments that explain why --retry-all-errors is gone do not trip it (rule 3).
func TestNoCurlOptionNewerThanEL8(t *testing.T) {
	el8 := curlcompat.EL8()
	root := repoRootFromCmd(t)

	sourceInvocations := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // walking the repo's own source tree
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, inv := range curlcompat.Invocations(string(b)) {
			sourceInvocations++
			if bad := el8.Unsupported(inv); len(bad) > 0 {
				t.Errorf("%s:%d: curl option(s) %v are not in %s, which EL8 AMIs ship; the script "+
					"aborts there (#1056):\n\t%s", rel, inv.Line, bad, curlcompat.Floor, inv.Text)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// A floor, not an exact count: there are dozens today (IMDS lookups, the DCV
	// and EFA downloads, the pipeline stage script). Far fewer means the matcher
	// has stopped finding them, not that they were all removed.
	if sourceInvocations < 20 {
		t.Fatalf("found only %d curl invocations in the Go source; discovery has broken", sourceInvocations)
	}

	for name, script := range renderedLinuxScripts(t) {
		invs := curlcompat.Invocations(script)
		if len(invs) == 0 {
			t.Errorf("%s: found no curl invocation in the rendered script; discovery has broken", name)
		}
		for _, inv := range invs {
			if bad := el8.Unsupported(inv); len(bad) > 0 {
				t.Errorf("%s, line %d: curl option(s) %v are not in %s (#1056):\n\t%s",
					name, inv.Line, bad, curlcompat.Floor, inv.Text)
			}
		}
	}
}

var (
	unitWrite   = regexp.MustCompile(`(?m)^cat > /etc/systemd/system/spored\.service <<'(\w+)'$`)
	unitRelabel = regexp.MustCompile(`(?m)^\s*` + sporedinstall.RelabelFuncName + ` /etc/systemd/system/spored\.service$`)
	unitStart   = regexp.MustCompile(`(?m)^\s*systemctl start spored$`)
	installCall = regexp.MustCompile(`(?m)^\s*if ! ` + sporedinstall.BucketInstallFuncName + ` `)
	relabelDef  = regexp.MustCompile(`(?m)^` + sporedinstall.RelabelFuncName + `\(\) \{$`)
)

// Every script that installs spored carries the relabel helper, and every script
// that writes the spored unit relabels it before starting the service (#1052).
//
// That the installer relabels the BINARY after its rename is asserted, and
// executed, in pkg/sporedinstall; here the property is that no generated script
// starts spored from a unit it wrote without relabelling it.
func TestEveryScriptRelabelsSporedForSELinux(t *testing.T) {
	unitWriters := 0
	for name, script := range renderedLinuxScripts(t) {
		if installCall.MatchString(script) && !relabelDef.MatchString(script) {
			t.Errorf("%s installs spored but does not define %s(), so the binary keeps its /tmp "+
				"SELinux label and systemd cannot exec it on an enforcing host (#1052)", name, sporedinstall.RelabelFuncName)
		}
		w := unitWrite.FindStringSubmatchIndex(script)
		if w == nil {
			continue
		}
		unitWriters++
		delim := script[w[2]:w[3]]
		end := strings.Index(script[w[1]:], "\n"+delim+"\n")
		if end < 0 {
			t.Errorf("%s: the spored unit heredoc (%s) never closes", name, delim)
			continue
		}
		after := script[w[1]+end:]
		r := unitRelabel.FindStringIndex(after)
		s := unitStart.FindStringIndex(after)
		switch {
		case r == nil:
			t.Errorf("%s writes the spored unit but never runs %s on it (#1052)", name, sporedinstall.RelabelFuncName)
		case s == nil:
			t.Errorf("%s writes the spored unit but never starts spored; this gate's anchor has gone stale", name)
		case r[0] > s[0]:
			t.Errorf("%s relabels the spored unit only after starting spored (#1052)", name)
		}
	}
	// The boot bootstrap and both app user-data cores write the unit; the four
	// app builders share two cores, so at least three of the rendered scripts do.
	if unitWriters < 3 {
		t.Fatalf("found only %d scripts writing the spored unit; the anchor %q has stopped matching", unitWriters, unitWrite)
	}

	// The upgrade's rollback renames a /tmp backup into place: the same label
	// problem as the install itself.
	upgrade := buildSporedUpgradeScript("0.135.2")
	rollback := regexp.MustCompile(`(?m)^\s*if \[ -s "\$BACKUP" \]; then mv -f "\$BACKUP" /usr/local/bin/spored && ` +
		sporedinstall.RelabelFuncName + ` /usr/local/bin/spored; fi$`)
	if !rollback.MatchString(upgrade) {
		t.Errorf("the upgrade rollback renames the /tmp backup into place without %s, so a rolled-back "+
			"spored cannot start under SELinux enforcing (#1052)", sporedinstall.RelabelFuncName)
	}
}
