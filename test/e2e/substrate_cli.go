//go:build e2e_tier0

// Tier 0 — the real spawn binary driven against the Substrate AWS emulator.
//
// Unlike Tiers 1–3 (which hit a real AWS account and, for 2/3, launch real
// instances), Tier 0 exercises the full user surface — argument parsing →
// cobra → RunE → AWS client → AWS API — deterministically, for free, with no
// AWS account. It works because spawn's AWS client uses config.LoadDefaultConfig,
// which honors the SDK v2 AWS_ENDPOINT_URL env var: we point the binary at the
// Substrate server and assert stdout JSON, exit codes, and resulting emulator
// state.
//
// Substrate emulates the AWS CONTROL PLANE only — there is no real instance
// boot, SSH, spored, user-data execution, or capacity exhaustion. Those live in
// Tiers 2–3. Tier 0 asserts spawn's behavior GIVEN AWS responses, which is
// exactly the internal-bug surface we want broad coverage of.
//
// Run: go test -tags=e2e_tier0 ./test/e2e/
package e2e

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/spore-host/spawn/pkg/testutil"
)

// spawnEnv is a Tier 0 test environment: a running Substrate server plus a
// configured runner for the real spawn binary pointed at it.
type spawnEnv struct {
	*testutil.TestEnv
	bin  string
	home string // isolated HOME with a pre-seeded SSH key
	t    *testing.T
}

// startSpawnSubstrate starts a Substrate server, locates/builds the spawn
// binary, and prepares an isolated HOME with a pre-seeded SSH public key so the
// launch path's setupSSHKey finds a key instead of shelling out to ssh-keygen
// (which may be absent on CI runners). Returns an env whose run() drives the
// binary against the emulator.
func startSpawnSubstrate(t *testing.T) *spawnEnv {
	t.Helper()
	env := testutil.SubstrateServer(t)
	home := seedFakeHome(t)
	return &spawnEnv{TestEnv: env, bin: tier0SpawnBin(t), home: home, t: t}
}

// seedFakeHome creates a temp HOME containing ~/.ssh/id_rsa(.pub) so spawn reads
// an existing key rather than shelling out to ssh-keygen. The public key is a
// real, freshly-generated ed25519 key in authorized-keys format, so spawn's
// fingerprinting (ssh.ParseAuthorizedKey) and substrate's ImportKeyPair both
// accept it. No key material is committed to the repo.
func seedFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}

	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	authorized := ssh.MarshalAuthorizedKey(sshPub) // "ssh-ed25519 AAAA... \n"
	if err := os.WriteFile(filepath.Join(sshDir, "id_rsa.pub"), authorized, 0o644); err != nil {
		t.Fatalf("write id_rsa.pub: %v", err)
	}

	pemBlock, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_rsa"), pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatalf("write id_rsa: %v", err)
	}
	return home
}

// tier0SpawnBin returns a path to the spawn binary, building it once if needed.
// Looks for ./bin/spawn relative to the module root, else builds to a temp path.
func tier0SpawnBin(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0) // .../spawn/test/e2e/substrate_cli.go
	root := filepath.Join(filepath.Dir(file), "..", "..")
	if p := filepath.Join(root, "bin", "spawn"); fileExists(p) {
		return p
	}
	// Build once into the test binary's temp dir.
	out := filepath.Join(t.TempDir(), "spawn")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build spawn binary: %v\n%s", err, b)
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// run executes `spawn <args...>` against the Substrate server and returns
// stdout, stderr, and the process exit code. The binary picks up Substrate via
// AWS_ENDPOINT_URL; static test creds + region keep the SDK happy.
//
// The environment is built CLEAN (not inherited from os.Environ): a developer's
// real AWS_PROFILE / SSO / AWS_CONFIG_FILE leaking in would make the SDK attempt
// real credential/SSO resolution and hang for minutes even with AWS_ENDPOINT_URL
// set. Passing only PATH + HOME + the test AWS vars keeps Tier 0 hermetic and
// fast (a launch completes in ~1s).
func (e *spawnEnv) run(args ...string) (stdout, stderr string, code int) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.home,
		"AWS_ENDPOINT_URL=" + e.URL,
		"AWS_ACCESS_KEY_ID=test",
		"AWS_SECRET_ACCESS_KEY=test",
		"AWS_REGION=us-east-1",
		"AWS_DEFAULT_REGION=us-east-1",
		// spawn's infra/compute config loaders default to the spore-host-infra /
		// spore-host-dev NAMED PROFILES (see pkg/config). Those don't exist in
		// the hermetic test env, so force the ambient credential chain (our
		// static test creds + AWS_ENDPOINT_URL → Substrate) by blanking them.
		"SPAWN_INFRA_PROFILE=",
		"SPAWN_COMPUTE_PROFILE=",
	}
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	code = 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			e.t.Fatalf("exec spawn %v: %v", args, err)
		}
	}
	return so.String(), se.String(), code
}

// runOK runs spawn and fails the test unless the exit code is 0.
func (e *spawnEnv) runOK(args ...string) string {
	e.t.Helper()
	so, se, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("spawn %s: expected exit 0, got %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), code, so, se)
	}
	return so
}

// launchOK runs `spawn launch <name> <extra...>` with the flags every Tier 0
// launch wants: a fixed region, JSON output, auto-yes, and — crucially — no
// post-launch waiting. Substrate instances never actually boot, so
// --wait-for-running / --wait-for-ssh would spin until timeout; disabling them
// keeps Tier 0 fast. Returns the parsed launch-result array.
func (e *spawnEnv) launchOK(name string, extra ...string) []map[string]any {
	e.t.Helper()
	args := append([]string{
		"launch", name,
		"--region", "us-east-1",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y", "-o", "json",
	}, extra...)
	return mustJSONArray(e.t, e.runOK(args...))
}

// ── assertion helpers ─────────────────────────────────────────────────────────

// mustJSONObject parses stdout as a JSON object.
func mustJSONObject(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("expected a JSON object, got error %v\noutput:\n%s", err, s)
	}
	return m
}

// mustJSONArray parses stdout as a JSON array of objects.
func mustJSONArray(t *testing.T, s string) []map[string]any {
	t.Helper()
	var a []map[string]any
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		t.Fatalf("expected a JSON array, got error %v\noutput:\n%s", err, s)
	}
	return a
}

// requireKeys fails if any of the named keys is absent from obj.
func requireKeys(t *testing.T, obj map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			t.Errorf("expected key %q in JSON object; keys present: %v", k, mapKeys(obj))
		}
	}
}

func mapKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
