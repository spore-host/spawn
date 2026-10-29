package launcher

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/sporedinstall"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// These are the gates for #1274's second half: a task's command and flush hook
// staged in S3 instead of carried in the user data.

const stagedURI = "s3://spawn-staging-123456789012-eu-west-1/t1/bootstrap-x.sh"

// stagedConfigs are the launch shapes that stage: a command, a flush hook, or
// both, with the per-launch content that sits around them.
func stagedConfigs() map[string]BootstrapConfig {
	flush := "#!/bin/bash\ncat <<'JSON'\n{\"flushed\": true}\nJSON\n"
	return map[string]BootstrapConfig{
		"command and flush hook": {Username: "ec2-user", Command: "/opt/task/wrapper.sh 'a b'", TaskFlushScript: flush},
		"command only":           {Username: "ec2-user", Command: "echo hi"},
		"with storage, gates and plugins": {
			Username: "ec2-user", Command: "ls /efs", TaskFlushScript: flush, StorageScript: "mount -t efs fs-1 /efs\n",
			ReadyMountPoints: []string{"/efs"}, ReadyGates: []string{MPIReadyGate}, CompletionFile: "/data/done",
		},
	}
}

func staged(cfg BootstrapConfig) BootstrapConfig {
	st := NewStagedFiles(stagedURI, "eu-west-1", cfg.Command, cfg.TaskFlushScript)
	cfg.StagedFiles = &st
	return cfg
}

// TestStagedFilesAreTheInlineFiles: a bootstrap that stages its command and
// flush hook does exactly what the inline one does once the fetch block is
// replaced by the object it fetches, with the release stub and with the inline
// body. Nothing of either file is left in the user data.
func TestStagedFilesAreTheInlineFiles(t *testing.T) {
	for name, cfg := range stagedConfigs() {
		for _, version := range []string{testRelease, ""} {
			t.Run(name+"/release="+version, func(t *testing.T) {
				inline, err := buildLinuxBootstrap(cfg, version, "", BootstrapBody())
				if err != nil {
					t.Fatal(err)
				}
				scfg := staged(cfg)
				out, err := buildLinuxBootstrap(scfg, version, "", BootstrapBody())
				if err != nil {
					t.Fatal(err)
				}
				fetch, err := stagedFilesFetch(scfg, *scfg.StagedFiles)
				if err != nil {
					t.Fatal(err)
				}
				if n := strings.Count(out, fetch); n != 1 {
					t.Fatalf("the staged bootstrap carries the fetch block %d times, want once", n)
				}
				for _, gone := range []string{"EOFSPAWNCMD", "EOFSPAWNTASKFLUSH", cfg.Command} {
					if strings.Contains(out, gone) {
						t.Errorf("the staged bootstrap still carries %q inline", gone)
					}
				}
				if !strings.Contains(out, scfg.StagedFiles.SHA256) {
					t.Error("the staged bootstrap does not carry the object's sha256")
				}
				obj := string(StagedFilesObject(cfg.Command, cfg.TaskFlushScript))
				spliced := strings.Replace(out, fetch, obj, 1)
				if got, want := hoistedTree(t, "staged+object", spliced), hoistedTree(t, "inline", inline); got != want {
					t.Errorf("the staged bootstrap plus its object does not do what the inline bootstrap does\n--- staged+object\n%s\n--- inline\n%s", spliced, inline)
				}
			})
		}
	}
}

// TestStagedFilesRefusesAnotherLaunchsObject: the reference must be the hash
// of this config's own command and flush hook, so a bootstrap cannot be built
// that would accept bytes other than the ones the launcher stages.
func TestStagedFilesRefusesAnotherLaunchsObject(t *testing.T) {
	cfg := staged(BootstrapConfig{Username: "ec2-user", Command: "echo hi", TaskFlushScript: "echo flush\n"})
	if _, err := buildLinuxBootstrap(cfg, testRelease, "", BootstrapBody()); err != nil {
		t.Fatalf("a matching reference was refused: %v", err)
	}
	for name, mutate := range map[string]func(*BootstrapConfig){
		"other command":    func(c *BootstrapConfig) { c.Command = "echo other" },
		"other flush hook": func(c *BootstrapConfig) { c.TaskFlushScript = "echo other\n" },
		"other sha256": func(c *BootstrapConfig) {
			st := *c.StagedFiles
			st.SHA256 = strings.Repeat("0", 64)
			c.StagedFiles = &st
		},
		"not s3":           func(c *BootstrapConfig) { st := *c.StagedFiles; st.URI = "https://example.com/x"; c.StagedFiles = &st },
		"no region":        func(c *BootstrapConfig) { st := *c.StagedFiles; st.Region = ""; c.StagedFiles = &st },
		"nothing to stage": func(c *BootstrapConfig) { c.Command, c.TaskFlushScript = "", "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := cfg
			mutate(&c)
			if _, err := buildLinuxBootstrap(c, testRelease, "", BootstrapBody()); err == nil {
				t.Error("built a bootstrap whose staged reference does not match its config")
			}
		})
	}
}

// TestStagedFilesFetchRunsOnlyTheStagedObject executes the real staged
// bootstrap under bash, with a stand-in aws CLI serving a stand-in bucket and
// a test-signed stand-in body. The command and flush hook are written, with
// the inline bootstrap's paths and modes, only from the exact object the
// launch staged. Otherwise nothing of it runs, neither does the body (so no
// spored, no workload), and the instance is retired: it would otherwise bill
// with nothing enforcing its TTL.
func TestStagedFilesFetchRunsOnlyTheStagedObject(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	prevKey := sporedinstall.SigningKeyPEM
	t.Cleanup(func() { sporedinstall.SigningKeyPEM = prevKey })
	sporedinstall.SigningKeyPEM = strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	body := []byte("echo body > \"$HOME/BODY_RAN\"\nSPAWN_SPORED_UP=1\n")
	sum := sha256.Sum256(body)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}

	const flush = "#!/bin/bash\necho flushed\n"
	cfg := staged(BootstrapConfig{Username: "tester", Command: "echo the-command", TaskFlushScript: flush})
	script, err := buildLinuxBootstrap(cfg, testRelease, "", string(body))
	if err != nil {
		t.Fatal(err)
	}
	obj := StagedFilesObject(cfg.Command, cfg.TaskFlushScript)
	objKey := strings.TrimPrefix(stagedURI, "s3://")

	// The awsStaged stand-in serves s3 cp from $HOME/s3/<bucket>/<key>, and
	// terminates. The object's paths are rewritten into the sandbox like the
	// script's are, so the script must carry the rewritten object's sha256.
	const awsServes = "#!/bin/bash\necho \"$*\" >> \"$HOME/aws-calls\"\n" +
		"if [ \"$1\" = s3 ]; then\n" +
		"  for a in \"$@\"; do case \"$a\" in s3://*) src=\"${a#s3://}\";; esac; done\n" +
		"  [ -f \"$HOME/s3/$src\" ] || exit 1\n" +
		"  cat \"$HOME/s3/$src\" > \"${@: -1}\"\nfi\nexit 0\n"
	chown := "#!/bin/bash\necho \"$*\" >> \"$HOME/chowns\"\n"

	cases := map[string]struct {
		object  []byte // nil = no object
		aws     string // stand-in aws CLI; "" = absent
		awsMode awsMode
		reason  string // "" = runs
	}{
		"the staged object runs":   {obj, awsServes, awsTerminates, ""},
		"tampered object":          {append(append([]byte{}, obj...), "echo pwned > /tmp/PWNED\n"...), awsServes, awsTerminates, "are not the ones this launch staged (sha256)"},
		"another launchs object":   {StagedFilesObject("echo other", flush), awsServes, awsTerminates, "are not the ones this launch staged (sha256)"},
		"missing object":           {nil, awsServes, awsTerminates, "could not fetch this launch's staged files from " + stagedURI},
		"empty object":             {[]byte{}, awsServes, awsTerminates, "could not fetch this launch's staged files"},
		"aws fails, no access":     {obj, "#!/bin/bash\necho \"$*\" >> \"$HOME/aws-calls\"\nexit 255\n", awsFails, "could not fetch this launch's staged files"},
		"no aws CLI on the AMI":    {obj, "", awsAbsent, "no aws CLI to fetch this launch's staged files"},
		"partial write, then fail": {obj, "#!/bin/bash\necho \"$*\" >> \"$HOME/aws-calls\"\n[ \"$1\" = s3 ] && echo partial > \"${@: -1}\"\nexit 255\n", awsFails, "are not the ones this launch staged (sha256)"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newStubServer(t, "eu-west-1")
			srv.put("eu-west-1", BootstrapBodyKey(testRelease), body)
			srv.put("eu-west-1", BootstrapBodyKey(testRelease)+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"))
			standIns := map[string]string{"chown": chown}
			if tc.aws != "" {
				standIns["aws"] = tc.aws
			}
			sb := newBootSandbox(t, srv, awsAbsent, standIns)
			rw := strings.NewReplacer("/etc/", filepath.Join(sb.dir, "etc")+"/", "/tmp/", filepath.Join(sb.dir, "tmp")+"/")
			sandboxed := func(b []byte) []byte { return []byte(rw.Replace(string(b))) }
			s := script
			want := sha256.Sum256(sandboxed(obj))
			s = strings.Replace(s, cfg.StagedFiles.SHA256, hex.EncodeToString(want[:]), 1)
			if tc.object != nil {
				p := filepath.Join(sb.dir, "s3", objKey)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, sandboxed(tc.object), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := sb.run(s)

			cmdPath := filepath.Join(sb.dir, "etc", "spawn", "command")
			flushPath := filepath.Join(sb.dir, strings.TrimPrefix(taskproto.FlushScriptPath(), "/"))
			if tc.reason == "" {
				if err != nil || !fileExists(filepath.Join(sb.dir, "BODY_RAN")) {
					t.Fatalf("the staged object did not run, or the body after it did not: %v\n%s", err, out)
				}
				for path, mode := range map[string]os.FileMode{cmdPath: 0o600, flushPath: 0o700} {
					fi, serr := os.Stat(path)
					if serr != nil {
						t.Fatalf("%s was not written: %v\n%s", path, serr, out)
					}
					if fi.Mode().Perm() != mode {
						t.Errorf("%s has mode %v, want %v (as the inline bootstrap writes it)", path, fi.Mode().Perm(), mode)
					}
				}
				if got := string(readOr(cmdPath)); got != cfg.Command+"\n" {
					t.Errorf("command = %q, want %q", got, cfg.Command+"\n")
				}
				if got := string(readOr(flushPath)); got != flush+"\n" {
					t.Errorf("flush hook = %q, want %q", got, flush+"\n")
				}
				if !strings.Contains(sb.read("chowns"), "root:root "+flushPath) {
					t.Errorf("the flush hook was not made root-owned: %q", sb.read("chowns"))
				}
				if !strings.Contains(sb.read("aws-calls"), "s3 cp --only-show-errors --region eu-west-1") {
					t.Errorf("the fetch is not region-pinned: %q", sb.read("aws-calls"))
				}
				if fileExists(filepath.Join(sb.dir, "etc", "spawn", "staged-files.sh")) {
					t.Error("the staged object was left on disk after it ran")
				}
				return
			}
			if fileExists(cmdPath) || fileExists(flushPath) || fileExists(filepath.Join(sb.dir, "tmp", "PWNED")) {
				t.Errorf("an unverified object wrote files\n%s", out)
			}
			if fileExists(filepath.Join(sb.dir, "BODY_RAN")) || srv.hit("eu-west-1", BootstrapBodyKey(testRelease)) != 0 {
				t.Errorf("the body was fetched or ran after the staged files failed\n%s", out)
			}
			if fileExists(filepath.Join(sb.dir, "etc", "spawn", "staged-files.sh")) {
				t.Error("the unverified object was left on disk")
			}
			assertRetired(t, sb, out, err, tc.reason, tc.awsMode)
		})
	}

	t.Run("a failed fetch is retried, boundedly", func(t *testing.T) {
		srv := newStubServer(t, "eu-west-1")
		sb := newBootSandbox(t, srv, awsAbsent, map[string]string{"chown": chown, "aws": awsServes})
		out, err := sb.run(script)
		assertRetired(t, sb, out, err, "could not fetch", awsTerminates)
		if n := strings.Count(sb.read("aws-calls"), "s3 cp "); n != 6 {
			t.Errorf("the fetch was tried %d times, want 6", n)
		}
	})
}
