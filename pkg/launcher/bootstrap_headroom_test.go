package launcher

import (
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// The heredocs whose comment lines #1422 moved out of the data and into shell
// comments above them, as they were in v0.149.0, comments and all. A comment
// inside a heredoc is data, so shellmin rightly keeps it, and every launch's
// user data carried these ~1 KB gzipped. Changing either heredoc on purpose
// means updating its pin here, to the new text.
const (
	sporedUnitAsOfV0149 = `[Unit]
Description=Spawn Agent - Instance self-monitoring
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# SigV4-sign DNS registration requests (#173). This is now REQUIRED, not
# anticipatory: the DNS Function URL is AuthType: AWS_IAM (verified against the
# live function 2026-10-11), so an unsigned request is rejected at the URL and the
# instance never registers. The comment here said the URL "is still AuthType:
# NONE, which ignores the signature" long after the cutover landed, which reads as
# though this variable were optional — it is load-bearing.
#
# The staged rollout it describes did work as intended: signing shipped first so a
# fleet was already signing before the flip, and old non-signing instances aged
# out under their TTLs ("everything dies").
Environment=SPORE_DNS_SIGV4=1
ExecStart=/usr/local/bin/spored
# on-failure (not always) prevents restart attempts during graceful shutdown
Restart=on-failure
RestartSec=10
# Give spored time to deregister DNS and clean up before SIGKILL
TimeoutStopSec=30
StandardOutput=journal
StandardError=journal
NoNewPrivileges=true
# Do NOT set PrivateTmp=true: spored must see the host /tmp/SPAWN_COMPLETE
# completion file written by users / spawn connect / nf-spawn. PrivateTmp
# isolates the daemon's /tmp and silently breaks --on-complete/--pre-stop (#66).

[Install]
WantedBy=multi-user.target
`
	commandHeaderAsOfV0149 = `#!/bin/bash
set -e
# errexit is ON here, and 'set -uo pipefail' does NOT clear it (#707) — so a
# script opening with that line, believing it chose its own error policy, is
# actually running -e -u -o pipefail. The two usual casualties are a SIGPIPE'd
# pipeline under pipefail ('... | head -n N' returns 141) and 'wait \$pid' on a
# failed background job, a pattern for CAPTURING a failure that -e destroys. It
# cost four r8gd.8xlarge runs and three misdiagnoses to find, because nothing
# said so. Fail-fast stays (running on after a broken step to TTL is worse).
#
# The notice that says so is NOT printed here. It used to be — two lines at the
# top of every /var/log/spawn-command.log, which is the file users actually
# read, on every member of every array, whether or not anything failed (#897).
# It is now written by the root-side caller, into the command log only when the
# command exits non-zero (the only case in which errexit can have cut a run
# short), and announced once per boot in cloud-init-output.log. Nothing in this
# script may write /var/log: it runs as $LOCAL_USERNAME via su, which cannot,
# and under set -e a failed write kills the command before it starts.
[ -f /etc/profile.d/spawn-params.sh ] && source /etc/profile.d/spawn-params.sh
`
)

// heredocWrittenTo returns the data of the heredoc the body writes to path,
// found by parsing the body, not by matching its text.
func heredocWrittenTo(t *testing.T, body, path string) string {
	t.Helper()
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(body), "body")
	if err != nil {
		t.Fatalf("the body does not parse as bash: %v", err)
	}
	var found []string
	syntax.Walk(f, func(n syntax.Node) bool {
		st, ok := n.(*syntax.Stmt)
		if !ok {
			return true
		}
		target := ""
		var hdoc *syntax.Word
		for _, r := range st.Redirs {
			switch r.Op {
			case syntax.RdrOut:
				target = r.Word.Lit()
			case syntax.Hdoc, syntax.DashHdoc:
				hdoc = r.Hdoc
			}
		}
		if hdoc == nil || target != path {
			return true
		}
		var b strings.Builder
		for _, p := range hdoc.Parts {
			lit, ok := p.(*syntax.Lit)
			if !ok {
				t.Fatalf("the heredoc written to %s is not literal data: %T", path, p)
			}
			b.WriteString(lit.Value)
		}
		found = append(found, b.String())
		return true
	})
	if len(found) != 1 {
		t.Fatalf("the body writes %d heredocs to %s, want 1", len(found), path)
	}
	return found[0]
}

// unitDirectives is what systemd reads of a unit: every line that is not
// blank or a comment ('#' or ';'), section headers included, in order.
func unitDirectives(unit string) []string {
	var out []string
	for _, l := range strings.Split(unit, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// Moving the comments out of the spored unit and the --command script's
// header (#1422) must change nothing either one does: the unit systemd reads
// has the same directives in the same sections and order, and the script
// parses to the same bash syntax tree, as v0.149.0's.
func TestMovedHeredocCommentsChangeNothing(t *testing.T) {
	body := BootstrapBody()

	unit := heredocWrittenTo(t, body, "/etc/systemd/system/spored.service")
	if got, want := strings.Join(unitDirectives(unit), "\n"), strings.Join(unitDirectives(sporedUnitAsOfV0149), "\n"); got != want {
		t.Errorf("the spored unit the body writes reads differently to systemd from v0.149.0's:\n got:\n%s\nwant:\n%s", got, want)
	}

	header := heredocWrittenTo(t, body, "/tmp/spawn-command.sh")
	if printedTree(t, "command header", header) != printedTree(t, "v0.149.0 command header", commandHeaderAsOfV0149) {
		t.Errorf("the --command script header the body writes does something different from v0.149.0's:\n%s", header)
	}
}
