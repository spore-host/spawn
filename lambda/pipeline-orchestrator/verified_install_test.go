package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// #1173: every pipeline stage instance installs spored, which runs as root,
// from this Lambda's user data. That install used its own inline curl and a
// sha256 served from the same bucket as the binary, and never checked the
// signature: the gap GHSA-3x4q-99gh-fxhx closed for the other install paths.
// The intent (pkg/sporedinstall, #819): checksum AND signature against the
// key compiled into spawn, failing closed, nothing installed on any failure.
//
// These EXECUTE the generated user data (execUserData) with real openssl
// verifying a real ECDSA P-256 signature. Only the key is a test key.

type sigMode int

const (
	sigValid   sigMode = iota // signed by the key the user data embeds
	sigForeign                // signed, but by some other key
	sigMissing                // no .sig published
)

func newP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// useTestSigningKey makes the user data embed a fresh test public key for the
// rest of the test, and returns a file holding the .sig the fake bucket serves
// for binPath: base64 DER ECDSA_SHA_256, the release signer's format.
func useTestSigningKey(t *testing.T, binPath string, mode sigMode) string {
	t.Helper()
	trusted := newP256(t)
	der, err := x509.MarshalPKIXPublicKey(&trusted.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	prev := sporedinstall.SigningKeyPEM
	t.Cleanup(func() { sporedinstall.SigningKeyPEM = prev })
	sporedinstall.SigningKeyPEM = strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))

	sigPath := filepath.Join(t.TempDir(), "spored.sig")
	if binPath == "" || mode == sigMissing {
		return sigPath
	}
	signer := trusted
	if mode == sigForeign {
		signer = newP256(t)
	}
	bin, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bin)
	sig, err := ecdsa.SignASN1(rand.Reader, signer, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sigPath, []byte(base64.StdEncoding.EncodeToString(sig)), 0o600); err != nil {
		t.Fatal(err)
	}
	return sigPath
}

func runUserDataWithSig(t *testing.T, mode sigMode) (string, error) {
	t.Helper()
	state, def := twoStagePipeline("stop", time.Now())
	fake := filepath.Join(t.TempDir(), "spored")
	if err := os.WriteFile(fake, []byte("#!/bin/bash\necho \"spored $*\" >> \"$CALLS\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return execUserData(t, userDataRun{
		state: state, stageDef: def.GetStage("train"), stageState: getStageState(state, "train"),
		region: "eu-west-1", spored: fake, sig: mode,
	})
}

// A validly signed spored is installed and runs the stage. The positive
// control for the two refusals below: without it, they would also pass on a
// script that could never install anything.
func TestUserData_InstallsASporedSignedByTheEmbeddedKey(t *testing.T) {
	calls, err := runUserDataWithSig(t, sigValid)
	if err != nil {
		t.Errorf("script failed with a validly signed spored: %v", err)
	}
	if !strings.Contains(calls, "spored run-pipeline-stage") {
		t.Errorf("a validly signed spored was not installed and run; calls:\n%s", calls)
	}
}

// The checksum matches but the signature is from another key: a bucket
// compromise can publish a matching sha256, so only the signature proves the
// binary is spore.host's. It must not be installed or run; the stage fails,
// writes FAILED, and the instance terminates.
func TestUserData_RefusesASporedSignedByAnotherKey(t *testing.T) {
	assertRefused(t, sigForeign)
}

// No signature published: refused the same way, not downgraded to sha256-only.
func TestUserData_RefusesAnUnsignedSpored(t *testing.T) {
	assertRefused(t, sigMissing)
}

func assertRefused(t *testing.T, mode sigMode) {
	t.Helper()
	calls, err := runUserDataWithSig(t, mode)
	if err == nil {
		t.Error("script exited 0 although spored's signature did not verify")
	}
	if strings.Contains(calls, "spored run-pipeline-stage") {
		t.Fatalf("a spored whose signature did not verify was installed and RUN as root; calls:\n%s", calls)
	}
	if !strings.Contains(calls, "/FAILED") {
		t.Errorf("FAILED marker not written for a refused install; calls:\n%s", calls)
	}
	assertSelfTerminated(t, calls)
}

// The production user data (no test key swapped in) carries the real spore.host
// key and turns verification on. Without SPORED_SIG_VERIFY=1 the installer
// checks only the sha256.
func TestUserData_EmbedsTheSporeHostSigningKey(t *testing.T) {
	if !sporedinstall.VerificationEnabled() {
		t.Skip("no signing key compiled into this build")
	}
	state, def := twoStagePipeline("stop", time.Now())
	ud, err := generatePipelineUserData(state, def.GetStage("train"), getStageState(state, "train"), 0, "eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(ud)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"\nSPORED_SIG_VERIFY=1\n", sporedinstall.SigningKeyPEM, sporedinstall.FuncName + "() {"} {
		if !strings.Contains(s, want) {
			t.Errorf("stage user data is missing %q", want)
		}
	}
	// EC2 caps user data at 16 KB (before base64).
	if len(raw) > 16*1024 {
		t.Errorf("stage user data is %d bytes, over EC2's 16 KB limit", len(raw))
	}
}

// #1412: a stage instance installs the spored of this orchestrator's own
// release, the one `spored run-pipeline-stage` was built with, from
// spawn/versions/<version>/ only; a build that is not a release installs the
// latest. Executed: every spored URL the user data asks curl for is checked.
func TestUserData_InstallsThisReleasesSpored(t *testing.T) {
	sporedURLs := func(calls string) []string {
		var out []string
		for _, line := range strings.Split(calls, "\n") {
			if u, ok := strings.CutPrefix(line, "curl "); ok && strings.Contains(u, "/spored-linux-") {
				out = append(out, u)
			}
		}
		return out
	}
	pin := func(t *testing.T, v string) {
		prev := sporedVersion
		t.Cleanup(func() { sporedVersion = prev })
		sporedVersion = func() string { return v }
	}
	t.Run("release build", func(t *testing.T) {
		pin(t, "9.9.9")
		calls, err := runUserDataWithSig(t, sigValid)
		if err != nil || !strings.Contains(calls, "spored run-pipeline-stage") {
			t.Fatalf("a release build's stage did not install and run its spored (err %v); calls:\n%s", err, calls)
		}
		urls := sporedURLs(calls)
		if len(urls) == 0 {
			t.Fatal("recorded no spored download; the harness has broken")
		}
		for _, u := range urls {
			if !strings.HasPrefix(u, "https://spawn-binaries-eu-west-1.s3.amazonaws.com/spawn/versions/9.9.9/spored-linux-amd64") {
				t.Errorf("a 9.9.9 orchestrator's stage fetched %s, which is not its own regional spored (#1412)", u)
			}
		}
	})
	t.Run("not a release", func(t *testing.T) {
		pin(t, "")
		calls, err := runUserDataWithSig(t, sigValid)
		if err != nil {
			t.Fatalf("%v; calls:\n%s", err, calls)
		}
		if urls := sporedURLs(calls); len(urls) == 0 || urls[0] != "https://spawn-binaries-eu-west-1.s3.amazonaws.com/spawn/spored-linux-amd64.sig" {
			t.Errorf("a dev build did not install the latest regional spored first: %v", urls)
		}
	})
	t.Run("a test binary is not a release", func(t *testing.T) {
		if v := sporedVersion(); v != "" {
			t.Errorf("sporedVersion() = %q in a test binary, which no release published", v)
		}
	})
}
