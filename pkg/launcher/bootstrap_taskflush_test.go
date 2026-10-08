package launcher

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

func taskFlushBootstrap(t *testing.T, hook string) string {
	t.Helper()
	script, err := BuildLinuxBootstrap(BootstrapConfig{
		Username:        "ec2-user",
		TaskFlushScript: hook,
	})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}
	return script
}

// TestTaskFlushHookIsInstalledRootOwnedAndNotWorldWritable is a security
// assertion, not a convenience one.
//
// spored runs as root and execs this file. If it lived anywhere an unprivileged
// local user could write — /tmp being the obvious candidate, since that is how
// the existing /tmp/SPAWN_COMPLETE channel works — then any local user could
// choose what root runs on the next shutdown. SPAWN_COMPLETE is only ever READ
// as data, which is why /tmp is acceptable for it and not for this.
func TestTaskFlushHookIsInstalledRootOwnedAndNotWorldWritable(t *testing.T) {
	script := taskFlushBootstrap(t, "#!/bin/bash\necho hi\n")
	path := taskproto.FlushScriptPath()

	if strings.HasPrefix(path, "/tmp/") {
		t.Fatalf("the hook path %q is under /tmp, which is world-writable; spored execs this as root", path)
	}
	if !strings.HasPrefix(path, "/etc/") {
		t.Errorf("expected the hook under /etc (root-owned), got %q", path)
	}
	if !strings.Contains(script, "chown root:root "+path) {
		t.Errorf("the bootstrap must chown the hook to root:root\n---\n%s", script)
	}
	if !strings.Contains(script, "chmod 700 "+path) {
		t.Errorf("the bootstrap must chmod the hook 0700 (root-only); a group- or world-writable "+
			"script execed by root is a local privilege-escalation path\n---\n%s", script)
	}
}

// TestTaskFlushHookHeredocDoesNotCollideWithItsOwnBody. The hook's body contains
// `JSON` heredocs of its own, so reusing that delimiter for the outer write would
// terminate the outer heredoc early — truncating the installed script and
// spilling the remainder into the bootstrap as commands.
func TestTaskFlushHookHeredocDoesNotCollideWithItsOwnBody(t *testing.T) {
	hook, err := taskproto.GenerateFlushScript(
		&taskproto.TaskSpec{TaskID: "t1", Command: []string{"true"}},
		taskproto.WrapperOptions{ResultsPrefix: "bucket", Region: "us-east-1", RunID: "run-1"})
	if err != nil {
		t.Fatalf("GenerateFlushScript: %v", err)
	}
	script := taskFlushBootstrap(t, hook)

	delim := regexp.MustCompile(`cat > ` + regexp.QuoteMeta(taskproto.FlushScriptPath()) + ` <<'(\w+)'`)
	m := delim.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("could not find the hook heredoc\n---\n%s", script)
	}
	outer := m[1]
	if strings.Contains(hook, "\n"+outer+"\n") {
		t.Errorf("the outer heredoc delimiter %q appears in the hook body — the installed script "+
			"would be truncated there", outer)
	}
	// Quoted delimiter: the body must be installed verbatim, with no parameter
	// expansion. It contains "$RESULTS_PREFIX" and "${1:-unknown}", which the
	// bootstrap shell would otherwise expand to empty at install time.
	if !strings.Contains(script, "<<'"+outer+"'") {
		t.Errorf("the hook heredoc must be quoted (<<'%s') so $RESULTS_PREFIX and ${1:-unknown} "+
			"survive installation", outer)
	}
	// And the whole body must actually be present.
	if !strings.Contains(script, hook) {
		t.Error("the hook body was not embedded verbatim in the bootstrap")
	}
}

// TestNoTaskFlushHookOnAPlainLaunch: a plain `spawn launch` has no task identity,
// so nothing should be installed and spored's stat simply misses.
func TestNoTaskFlushHookOnAPlainLaunch(t *testing.T) {
	script := taskFlushBootstrap(t, "")
	if strings.Contains(script, taskproto.FlushScriptPath()) {
		t.Errorf("a launch with no task flush script must not reference the hook path\n---\n%s", script)
	}
}
