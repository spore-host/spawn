package shellcheckgate

import (
	"strings"
	"testing"
)

// The gate is only worth something if shellcheck, invoked the way Check
// invokes it, reports the classes #689 is after. A script with an unquoted
// expansion inside [ ] (SC2086 is info, so not that) and an unassigned
// variable reference must produce findings at warning level.
func TestFindings_ReportsWarnings(t *testing.T) {
	bin := Binary(t)
	bad := "#!/bin/bash\nfor f in $(ls /tmp); do echo \"$f\"; done\nif [ $undefined_var = x ]; then :; fi\ncd /somewhere\n"
	got, err := Findings(bin, bad)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "")
	for _, code := range []string{"SC2045", "SC2154"} {
		if !strings.Contains(joined, code) {
			t.Errorf("shellcheck did not report %s on a script that has it:\n%s", code, joined)
		}
	}
}

func TestFindings_CleanScriptHasNone(t *testing.T) {
	bin := Binary(t)
	got, err := Findings(bin, "#!/bin/bash\nset -euo pipefail\necho \"ok\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("clean script reported findings: %v", got)
	}
}

// A fragment with no shebang must still be checked as bash, since every
// fragment is concatenated into a #!/bin/bash script.
func TestFindings_FragmentWithoutShebang(t *testing.T) {
	bin := Binary(t)
	got, err := Findings(bin, "for f in $(ls /tmp); do echo \"$f\"; done\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Error("fragment without a shebang was not checked")
	}
}

func TestNestedScripts(t *testing.T) {
	script := "#!/bin/bash\ncat > /tmp/a.sh <<'EOF'\n#!/bin/bash\necho a\nEOF\n" +
		"cat > /tmp/b.json <<'EOF'\n{\"x\": 1}\nEOF\n" +
		"cat > /tmp/c.sh <<EOF\n#!/bin/bash\necho $HOME\nEOF\n" +
		"cat > /tmp/d.sh <<-\"END\"\n\t#!/bin/bash\n\techo d\n\tEND\n"
	got := NestedScripts(script)
	if len(got) != 2 || !strings.Contains(got[0], "echo a") || got[1] != "#!/bin/bash\necho d\n" {
		t.Fatalf("NestedScripts = %q, want the quoted-delimiter shell heredocs a.sh and d.sh (tabs stripped)", got)
	}
}
