package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent and the launch user-data must agree on where the TLS certs live
// (#809).
//
// The certs are written by the user-data in `cmd/app.go` and read by the agent
// here. Nothing connected the two, and they drifted: a dead `installDCVCert` on
// the Agent fetched the DCV cert into `/etc/dcv/` and edited `/etc/dcv/dcv.conf`,
// while the user-data had moved to
// `/var/lib/dcv/.config/NICE/dcv/private/dcv.{pem,key}` — the path DCV actually
// reads. The Go function was never called, so the disagreement was invisible; it
// only looked like "the cert is never installed" from the Go side, because the
// live reader is an `aws s3 cp` in shell that a Go-only search cannot see.
//
// This is the same class as #790's Lambda names and #804's bucket: two sides
// constructing the same identifier independently, agreeing only by luck. The
// difference here is that one side is shell, so the sharing has to be an
// assertion rather than a constant.

// certConsumers maps a path the agent believes in to the thing that must write
// it. The value is a substring the user-data has to contain, so this fails if
// either side moves.
var certConsumers = map[string]string{
	webProxyCertPath: "/etc/spore/webproxy/cert.pem",
	webProxyKeyPath:  "/etc/spore/webproxy/key.pem",
}

func TestUserDataWritesThePathsTheAgentReads(t *testing.T) {
	userData := readUserDataSource(t)

	if len(certConsumers) == 0 {
		t.Fatal("no cert paths to check — the map is empty, which proves nothing")
	}
	for agentPath, mustAppear := range certConsumers {
		if agentPath == "" {
			t.Error("an agent cert path constant is empty")
			continue
		}
		if !strings.Contains(userData, mustAppear) {
			t.Errorf("the agent reads %q, but cmd/app.go's user-data never writes %q.\n"+
				"  The certs are written by shell at boot and read by Go at runtime, so nothing but "+
				"this assertion connects the two — and they have drifted before (#809).",
				agentPath, mustAppear)
		}
	}
}

// The superseded DCV cert scheme must not come back.
//
// `/etc/dcv/` was where the dead installDCVCert put things. DCV reads its certs
// from its own private directory, so a path under /etc/dcv/ in Go is either dead
// code or a second writer fighting the user-data for the same state.
func TestAgentDoesNotUseTheSupersededDCVCertPath(t *testing.T) {
	root := repoRootFromAgent(t)
	var offenders []string

	err := filepath.Walk(filepath.Join(root, "pkg"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // walking a fixed repo subtree
		if rerr != nil {
			return rerr
		}
		// String literals only: the explanatory comment in agent.go names the old
		// path deliberately, and a gate its own documentation trips is rule 3.
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, `"/etc/dcv/`) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+": "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk pkg: %v", err)
	}

	for _, o := range offenders {
		t.Errorf("Go code references the superseded /etc/dcv/ cert location:\n\t%s\n"+
			"DCV reads its certs from /var/lib/dcv/.config/NICE/dcv/private/, which the launch "+
			"user-data writes. A writer here either does nothing or fights the user-data for the "+
			"same state (#809).", o)
	}
}

// The DCV cert path the user-data writes must be DCV's own private directory.
//
// Pinning it because the whole defect was a plausible-looking path that DCV does
// not read, and nothing would have noticed: the cert simply would not take effect
// and DCV would fall back to self-signed, silently.
func TestUserDataWritesTheDCVCertWhereDCVReadsIt(t *testing.T) {
	userData := readUserDataSource(t)
	const dcvPrivateDir = "/var/lib/dcv/.config/NICE/dcv/private"
	if !strings.Contains(userData, dcvPrivateDir) {
		t.Errorf("cmd/app.go's user-data does not write the DCV cert to %q.\n"+
			"  A cert installed anywhere else has no effect and DCV falls back to self-signed "+
			"without saying so.", dcvPrivateDir)
	}
}

func readUserDataSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRootFromAgent(t), "cmd", "app.go")) //nolint:gosec // fixed path
	if err != nil {
		t.Fatalf("read cmd/app.go: %v", err)
	}
	return string(b)
}

func repoRootFromAgent(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			if _, serr := os.Stat(filepath.Join(dir, "cmd", "app.go")); serr == nil {
				return dir
			}
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repo root from pkg/agent")
	return ""
}
