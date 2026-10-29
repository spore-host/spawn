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
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// spored is published to spawn/versions/<v>/ BEFORE GoReleaser makes the CLI
// installable (#1415): a release spawn installs only its own version's spored
// and retires the instance without it (#1412). Like the body tests beside
// these, they RUN the workflow's steps with stand-in go/aws/gh.

const (
	sporedBuildStepName  = "Build spored for the release"
	sporedVerifyStepName = "Verify the published versioned spored"
	sporedMatchStepName  = "Check GoReleaser built the published spored"
	sporedLatestStepName = "Update the current spored"
)

// The spored names the workflow publishes, and where GoReleaser puts each.
var sporedDist = map[string]string{
	"spored-linux-amd64":       "dist/spored_linux_amd64_v1/spored",
	"spored-linux-arm64":       "dist/spored_linux_arm64_v8.0/spored",
	"spored-windows-amd64.exe": "dist/spored_windows_amd64_v1/spored.exe",
}

type relKey struct {
	priv, pub string
	key       *ecdsa.PrivateKey
}

// signingKeys writes a P-256 key pair into the sandbox, as KMS holds one and
// spawn compiles in the public half.
func (sb *relSandbox) signingKeys(name string) relKey {
	sb.t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		sb.t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		sb.t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		sb.t.Fatal(err)
	}
	r := relKey{priv: name + ".key", pub: name + ".pub", key: k}
	sb.write(r.priv, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})))
	sb.write(r.pub, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})))
	r.priv, r.pub = filepath.Join(sb.dir, r.priv), filepath.Join(sb.dir, r.pub)
	return r
}

// signEnv: KMS signs with kms; the spawn being released compiles in compiled.
func signEnv(kms, compiled relKey) []string {
	return []string{"FAKE_KMS_KEY=" + kms.priv, "FAKE_SIGNING_PUB=" + compiled.pub}
}

func (sb *relSandbox) mustRun(name, keyID string, extra ...string) {
	sb.t.Helper()
	if out, err := sb.run(name, keyID, extra...); err != nil {
		sb.t.Fatalf("%q failed: %v\n%s", name, err, out)
	}
}

// publishVersioned runs every pre-GoReleaser spored step.
func (sb *relSandbox) publishVersioned(keys relKey) {
	sb.t.Helper()
	sb.mustRun(sporedBuildStepName, "")
	sb.mustRun(sporedStepName, "alias/test-signing", signEnv(keys, keys)...)
	sb.mustRun(sporedVerifyStepName, "")
}

// buckets is the job's bucket list, which every publish step reads.
func (sb *relSandbox) buckets() []string {
	b := strings.Fields(sb.job.Env["SPAWN_BINARY_BUCKETS"])
	if len(b) < 11 {
		sb.t.Fatalf("SPAWN_BINARY_BUCKETS lists %d buckets", len(b))
	}
	return b
}

// sporedKeys lists every spored object in the mirror, as bucket/key.
func (sb *relSandbox) sporedKeys() []string {
	sb.t.Helper()
	var out []string
	_ = filepath.Walk(sb.mirror, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.Contains(info.Name(), "spored") {
			rel, _ := filepath.Rel(sb.mirror, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// copyBuiltToDist makes dist/ hold the bytes the pre-GoReleaser build made,
// as an identical GoReleaser build does.
func (sb *relSandbox) copyBuiltToDist() {
	sb.t.Helper()
	for name, dist := range sporedDist {
		b, err := os.ReadFile(filepath.Join(sb.dir, "rt/spored-release", name))
		if err != nil {
			sb.t.Fatal(err)
		}
		sb.write(dist, string(b))
	}
}

// TestReleasePublishesTheVersionedSporedBeforeTheCLI: spored is built,
// published and verified before GoReleaser, the dist/ comparison runs after it
// (even when it fails late), and latest moves last, only once that comparison
// passed.
func TestReleasePublishesTheVersionedSporedBeforeTheCLI(t *testing.T) {
	j := releaseJobWithStep(t)
	at := func(name string) int {
		t.Helper()
		i := j.index(func(s releaseStep) bool { return s.Name == name })
		if len(i) != 1 {
			t.Fatalf("found %d %q steps, want 1", len(i), name)
		}
		return i[0]
	}
	gorel := j.index(func(s releaseStep) bool { return strings.HasPrefix(s.Uses, "goreleaser/goreleaser-action@") })
	if len(gorel) != 1 {
		t.Fatalf("found %d GoReleaser steps, want 1", len(gorel))
	}
	build, publish, verify := at(sporedBuildStepName), at(sporedStepName), at(sporedVerifyStepName)
	match, latest := at(sporedMatchStepName), at(sporedLatestStepName)
	if !(build < publish && publish < verify && verify < gorel[0]) {
		t.Errorf("spored is built at step %d, published at %d and verified at %d, but GoReleaser publishes the CLI at %d: a spawn installed before its versioned spored exists retires every launch (#1415)",
			build, publish, verify, gorel[0])
	}
	if !(gorel[0] < match && match < latest) {
		t.Errorf("GoReleaser runs at step %d, the dist/ spored check at %d and the latest update at %d; want them in that order", gorel[0], match, latest)
	}
	creds := j.index(func(s releaseStep) bool {
		return strings.HasPrefix(s.Uses, "aws-actions/configure-aws-credentials@") && s.With["role-to-assume"] != ""
	})
	if len(creds) == 0 || creds[0] > publish {
		t.Errorf("no AWS credentials are configured before the versioned spored is published (credential steps at %v)", creds)
	}
	for _, i := range []int{build, publish, verify} {
		if j.Steps[i].If != "" {
			t.Errorf("%q runs only if %q; it must gate GoReleaser unconditionally", j.Steps[i].Name, j.Steps[i].If)
		}
	}
	m := j.Steps[match]
	if m.ID == "" || !strings.Contains(m.If, "!cancelled()") {
		t.Errorf("the dist/ spored check (id %q, if %q) needs an id and must run even when GoReleaser fails late", m.ID, m.If)
	}
	// latest, and the credentials it uses, move only on that check's success.
	want := "steps." + m.ID + ".outcome == 'success'"
	for i := match + 1; i <= latest; i++ {
		s := j.Steps[i]
		if (i == latest || strings.HasPrefix(s.Uses, "aws-actions/configure-aws-credentials@")) && !strings.Contains(s.If, want) {
			t.Errorf("%q runs if %q, which does not require %s: latest would move to a release whose spored did not match", s.Name+s.Uses, s.If, want)
		}
	}
}

// TestReleaseBuildsSporedLikeGoReleaser: the pre-GoReleaser build is the spored
// build in .goreleaser.yaml, with the same env, ldflags, package and targets,
// and nothing else that could change the bytes. Read from both files, and from
// the go invocations the step actually makes.
func TestReleaseBuildsSporedLikeGoReleaser(t *testing.T) {
	b, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Builds []map[string]any `yaml:"builds"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	var spored map[string]any
	for _, bl := range cfg.Builds {
		if bl["id"] == "spored" {
			spored = bl
		}
	}
	if spored == nil {
		t.Fatal(".goreleaser.yaml has no build with id spored")
	}
	// Keys the workflow mirrors, or that cannot change the binary's bytes
	// (mod_timestamp sets only the file's mtime). Anything else (flags,
	// gcflags, tags, goamd64, overrides, builder...) must be mirrored first.
	mirrored := map[string]bool{"id": true, "main": true, "binary": true, "env": true, "goos": true, "goarch": true, "ignore": true, "ldflags": true, "mod_timestamp": true}
	for k := range spored {
		if !mirrored[k] {
			t.Errorf(".goreleaser.yaml's spored build sets %q, which the pre-GoReleaser build does not mirror; mirror it in %q, or the published spored differs from the released one", k, sporedBuildStepName)
		}
	}
	strs := func(k string) []string {
		var out []string
		for _, v := range asList(spored[k]) {
			out = append(out, v.(string))
		}
		return out
	}
	ldflags := strings.ReplaceAll(strings.Join(strs("ldflags"), " "), "{{.Version}}", "9.9.9")
	if strings.Contains(ldflags, "{{") {
		t.Fatalf("spored ldflags %q use a template this test does not resolve", ldflags)
	}
	skip := map[string]bool{}
	for _, ig := range asList(spored["ignore"]) {
		m := ig.(map[string]any)
		skip[m["goos"].(string)+"/"+m["goarch"].(string)] = true
	}
	var wantTargets []string
	for _, goos := range strs("goos") {
		for _, goarch := range strs("goarch") {
			if !skip[goos+"/"+goarch] {
				wantTargets = append(wantTargets, goos+"/"+goarch)
			}
		}
	}
	sort.Strings(wantTargets)
	main, _ := spored["main"].(string)
	wantEnv := strs("env")

	sb := newRelSandbox(t, 0, fakeBody)
	sb.mustRun(sporedBuildStepName, "")
	raw, err := os.ReadFile(filepath.Join(sb.dir, "go-builds"))
	if err != nil {
		t.Fatalf("the build step ran no go build: %v", err)
	}
	var gotTargets []string
	for _, call := range strings.Split(strings.TrimSpace(string(raw)), "END") {
		if strings.TrimSpace(call) == "" {
			continue
		}
		env := map[string]string{}
		var args []string
		for _, line := range strings.Split(strings.TrimSpace(call), "\n") {
			if kv, ok := strings.CutPrefix(line, "env "); ok {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			} else if a, ok := strings.CutPrefix(line, "arg "); ok {
				args = append(args, a)
			}
		}
		gotTargets = append(gotTargets, env["GOOS"]+"/"+env["GOARCH"])
		for _, e := range wantEnv {
			k, v, _ := strings.Cut(e, "=")
			if env[k] != v {
				t.Errorf("go build for %s/%s runs with %s=%q; .goreleaser.yaml builds spored with %s", env["GOOS"], env["GOARCH"], k, env[k], e)
			}
			delete(env, k)
		}
		delete(env, "GOOS")
		delete(env, "GOARCH")
		for k, v := range env {
			t.Errorf("go build runs with %s=%s, which .goreleaser.yaml's spored build does not set", k, v)
		}
		// Exactly: build -ldflags <ldflags> -o <out> <main>, in any order.
		var pos []string
		gotLD, sawO := "", false
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "-ldflags" && i+1 < len(args):
				gotLD = args[i+1]
				i++
			case args[i] == "-o" && i+1 < len(args):
				sawO = true
				i++
			case strings.HasPrefix(args[i], "-"):
				t.Errorf("go build passes %q, which .goreleaser.yaml's spored build does not", args[i])
			default:
				pos = append(pos, args[i])
			}
		}
		if len(args) == 0 || args[0] != "build" || !sawO {
			t.Errorf("unexpected go invocation %q", args)
		}
		if gotLD != ldflags {
			t.Errorf("go build -ldflags %q; .goreleaser.yaml builds spored with %q", gotLD, ldflags)
		}
		if len(pos) != 1 || pos[0] != main {
			t.Errorf("go build builds %q; .goreleaser.yaml builds spored from %q", pos, main)
		}
	}
	sort.Strings(gotTargets)
	if strings.Join(gotTargets, " ") != strings.Join(wantTargets, " ") {
		t.Errorf("the step builds spored for %v; .goreleaser.yaml builds it for %v", gotTargets, wantTargets)
	}
	if len(gotTargets) != len(sporedDist) {
		t.Errorf("the step builds %d spored, the workflow publishes %d (%v)", len(gotTargets), len(sporedDist), sporedDist)
	}
	// Outside the checkout: an untracked file flips vcs.modified.
	if out, _ := filepath.Glob(filepath.Join(sb.dir, "spored-*")); len(out) != 0 {
		t.Errorf("the build step wrote %v into the checkout; that changes GoReleaser's vcs.modified stamp, so its spored would differ", out)
	}
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// TestReleasePublishesTheSignedVersionedSpored: every bucket gets every
// versioned binary, its sha256 and a signature that verifies against the key
// compiled into spawn, and nothing before GoReleaser touches latest.
func TestReleasePublishesTheSignedVersionedSpored(t *testing.T) {
	sb := newRelSandbox(t, 0, fakeBody)
	keys := sb.signingKeys("release")
	sb.publishVersioned(keys)

	var want []string
	for _, bucket := range sb.buckets() {
		for name := range sporedDist {
			bin, ok := sb.object(bucket, "spawn/versions/9.9.9/"+name)
			if !ok || !strings.HasPrefix(bin, "spored ") {
				t.Errorf("%s: no versioned %s", bucket, name)
				continue
			}
			sum := sha256.Sum256([]byte(bin))
			if got, _ := sb.object(bucket, "spawn/versions/9.9.9/"+name+".sha256"); strings.TrimSpace(got) != hexString(sum[:]) {
				t.Errorf("%s: %s.sha256 is %q", bucket, name, got)
			}
			sig, _ := sb.object(bucket, "spawn/versions/9.9.9/"+name+".sig")
			der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
			if err != nil || !ecdsa.VerifyASN1(&keys.key.PublicKey, sum[:], der) {
				t.Errorf("%s: %s.sig %q does not verify against the compiled-in key", bucket, name, sig)
			}
			for _, f := range []string{name, name + ".sha256", name + ".sig"} {
				want = append(want, bucket+"/spawn/versions/9.9.9/"+f)
			}
		}
	}
	sort.Strings(want)
	if got := sb.sporedKeys(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("before GoReleaser, the mirror holds %d spored objects, want exactly the %d versioned ones; latest must not move until the release is published:\n%s",
			len(got), len(want), strings.Join(got, "\n"))
	}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// A signature the released spawn would reject, or none, must stop the release
// before anything is published.
func TestReleaseRefusesASporedSignatureSpawnWouldReject(t *testing.T) {
	for name, tc := range map[string]struct {
		keyID     string
		wrongKMS  bool
		wantError string
	}{
		"KMS signs with another key": {"alias/test-signing", true, "does not verify"},
		"no signing key":             {"", false, "SPORED_SIGNING_KEY_ID unset"},
	} {
		t.Run(name, func(t *testing.T) {
			sb := newRelSandbox(t, 0, fakeBody)
			compiled := sb.signingKeys("compiled")
			kms := compiled
			if tc.wrongKMS {
				kms = sb.signingKeys("other")
			}
			sb.mustRun(sporedBuildStepName, "")
			out, err := sb.run(sporedStepName, tc.keyID, signEnv(kms, compiled)...)
			if err == nil || !strings.Contains(out, tc.wantError) {
				t.Fatalf("the publish step returned %v without %q; GoReleaser would then ship a CLI whose every launch retires\n%s", err, tc.wantError, out)
			}
			if got := sb.sporedKeys(); len(got) != 0 {
				t.Errorf("a step that failed published %d spored objects, first %s", len(got), got[0])
			}
		})
	}
}

// The read-back catches an object that is missing or not what was published.
func TestReleaseVerifyCatchesAMissingOrAlteredSpored(t *testing.T) {
	for name, damage := range map[string]func(path string) error{
		"missing": os.Remove,
		"altered": func(p string) error { return os.WriteFile(p, []byte("stale"), 0o644) },
	} {
		for _, f := range []string{"spored-linux-arm64", "spored-windows-amd64.exe.sig"} {
			t.Run(name+" "+f, func(t *testing.T) {
				sb := newRelSandbox(t, 0, fakeBody)
				sb.publishVersioned(sb.signingKeys("release"))
				bucket := sb.buckets()[len(sb.buckets())-1]
				if err := damage(filepath.Join(sb.mirror, bucket, "spawn/versions/9.9.9", f)); err != nil {
					t.Fatal(err)
				}
				out, err := sb.run(sporedVerifyStepName, "")
				if err == nil {
					t.Fatalf("the verify step passed with %s/spawn/versions/9.9.9/%s %s\n%s", bucket, f, name, out)
				}
				if name == "altered" && !strings.Contains(out, "::error::s3://"+bucket+"/spawn/versions/9.9.9/"+f) {
					t.Errorf("the altered object is not named in a workflow error\n%s", out)
				}
			})
		}
	}
}

// After GoReleaser, dist/'s spored must be the published bytes, or the job fails.
func TestReleaseFailsWhenGoReleasersSporedDiffers(t *testing.T) {
	sb := newRelSandbox(t, 0, fakeBody)
	sb.publishVersioned(sb.signingKeys("release"))
	sb.copyBuiltToDist()
	sb.mustRun(sporedMatchStepName, "")

	for name, dist := range sporedDist {
		t.Run(name, func(t *testing.T) {
			sb.copyBuiltToDist()
			sb.write(dist, "a different build\n")
			out, err := sb.run(sporedMatchStepName, "")
			if err == nil || !strings.Contains(out, "::error::GoReleaser built "+dist) {
				t.Fatalf("the check returned %v with GoReleaser's %s differing from the published %s\n%s", err, dist, name, out)
			}
			if err := os.Remove(filepath.Join(sb.dir, dist)); err != nil {
				t.Fatal(err)
			}
			if out, err := sb.run(sporedMatchStepName, ""); err == nil {
				t.Fatalf("the check passed with no %s\n%s", dist, out)
			}
		})
	}
}

// The current spored (spawn/current/, #1419) moves, with the verified bytes,
// only once the GitHub release is published; a release that failed to publish
// leaves it alone.
func TestReleaseUpdatesLatestOnlyOnceTheReleaseIsPublished(t *testing.T) {
	for name, draft := range map[string]string{"published": "false", "draft": "true", "no release": ""} {
		t.Run(name, func(t *testing.T) {
			sb := newRelSandbox(t, 0, fakeBody)
			sb.publishVersioned(sb.signingKeys("release"))
			before := len(sb.sporedKeys())
			out, err := sb.run(sporedLatestStepName, "", "FAKE_GH_DRAFT="+draft)
			if draft != "false" {
				if err == nil {
					t.Errorf("the current spored was updated for an unpublished release\n%s", out)
				}
				if got := len(sb.sporedKeys()); got != before {
					t.Errorf("an unpublished release wrote %d current spored objects", got-before)
				}
				return
			}
			if err != nil {
				t.Fatalf("the current step failed: %v\n%s", err, out)
			}
			for _, bucket := range sb.buckets() {
				for n := range sporedDist {
					for _, f := range []string{n, n + ".sha256", n + ".sig"} {
						v, _ := sb.object(bucket, "spawn/versions/9.9.9/"+f)
						if got, ok := sb.object(bucket, "spawn/current/"+f); !ok || got != v {
							t.Errorf("%s/spawn/current/%s is %q (present %v), want the published versioned %s", bucket, f, got, ok, f)
						}
					}
				}
			}
		})
	}
}

// frozenLatestKeys are the unversioned spored keys frozen at v0.149.0 (#1419):
// spawn/<name> and the bucket-root <name>, with their .sha256 and .sig.
func frozenLatestKeys() []string {
	var out []string
	for n := range sporedDist {
		for _, f := range []string{n, n + ".sha256", n + ".sig"} {
			out = append(out, "spawn/"+f, f)
		}
	}
	sort.Strings(out)
	return out
}

// TestReleaseNeverWritesTheFrozenLatest (#1419): from v0.150.0 on, no release
// step writes the unversioned spawn/spored-* or bucket-root spored-* keys.
// They stay at v0.149.0, the last spored that reads the launch-fixed settings
// from tags (#1359), which spawn releases before v0.149.0 install. Every
// spored step of a successful release is RUN, end to end, and the mirror is
// then checked; and the workflow's text names no such destination.
func TestReleaseNeverWritesTheFrozenLatest(t *testing.T) {
	sb := newRelSandbox(t, 0, fakeBody)
	keys := sb.signingKeys("release")
	sb.publishVersioned(keys)
	sb.copyBuiltToDist()
	sb.mustRun(sporedMatchStepName, "")
	sb.mustRun(sporedLatestStepName, "", "FAKE_GH_DRAFT=false")
	wrote := 0
	for _, bucket := range sb.buckets() {
		for _, k := range frozenLatestKeys() {
			if _, ok := sb.object(bucket, k); ok {
				t.Errorf("a release wrote %s/%s, frozen at v0.149.0 (#1419)", bucket, k)
			}
		}
		if _, ok := sb.object(bucket, "spawn/current/spored-linux-amd64"); ok {
			wrote++
		}
	}
	if wrote != len(sb.buckets()) {
		t.Fatalf("the release wrote spawn/current/ in %d of %d buckets; the run proves nothing", wrote, len(sb.buckets()))
	}

	b, err := os.ReadFile(sporedPublisher)
	if err != nil {
		t.Fatal(err)
	}
	frozenDest := regexp.MustCompile(`s3://\$\{?bucket\}?/(spawn/)?(\$f|\$\{f\}|\$name|\$\{name\}|spored-)`)
	for i, line := range logicalLines(string(b)) {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := frozenDest.FindString(line); m != "" {
			t.Errorf("%s:%d writes %s, a frozen latest key (#1419)", sporedPublisher, i+1, m)
		}
	}
}
