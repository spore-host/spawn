package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/s3hosts"
	"github.com/spore-host/spawn/internal/treewalk"
	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// The gates for #1436 here, beside pkg/launcher's for the Linux bootstrap and
// the pipeline orchestrator's for its stage script: a private-only instance
// (--no-public-ip, #1433) reaches S3 only through a VPC endpoint, which serves
// the regional names alone, so every S3 name a script spawn generates at boot
// must be the launch region's regional one (internal/s3hosts says exactly
// what passes).

// Every script cmd renders that runs on an instance: the Linux ones
// renderedLinuxScripts lists (the boot bootstrap, the SSM spored upgrade, the
// four app user-data kinds) and the Windows bootstrap, for dev and release
// builds, rendered for a region other than us-east-1.
func TestEveryGeneratedScriptS3HostIsRegional(t *testing.T) {
	const region = "eu-west-2"
	scripts := renderedLinuxScripts(t)
	win, err := buildWindowsUserData("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample u@h")
	if err != nil {
		t.Fatal(err)
	}
	scripts["Windows bootstrap"] = win
	release, err := sporedinstall.WindowsInstallScriptFor(sporedinstall.SigningKeyPEM, "9.9.9", false)
	if err != nil {
		t.Fatal(err)
	}
	scripts["Windows spored install (release build)"] = release
	for name, script := range scripts {
		refs, problems := s3hosts.Check(script, region)
		if len(refs) == 0 && name != "verified installer (standalone)" {
			// The standalone installer takes its bucket URLs as arguments.
			t.Errorf("%s: found no S3 location in the rendered script; discovery has broken", name)
		}
		for _, p := range problems {
			t.Errorf("%s: %s", name, p)
		}
		t.Logf("%s: %d S3 location(s)", name, len(refs))
	}
	// The Windows bootstrap installs the SSM agent from S3 when the AMI lacks
	// it, and spored from the source table; both must have been seen.
	refs, _ := s3hosts.Check(win, region)
	var ssm, sources int
	for _, r := range refs {
		switch {
		case strings.Contains(r.Host, "amazon-ssm-"):
			ssm++
		case strings.Contains(r.Text, "u = "):
			sources++
		}
	}
	if ssm != 1 || sources < 2 {
		t.Errorf("Windows bootstrap: %d SSM agent download(s) and %d spored source(s), want 1 and at least 2; discovery has broken", ssm, sources)
	}
}

// No source file the repo ships (Go, shell, PowerShell, workflows) names S3
// by its global name in a URL, so a generator nobody added to the rendered
// lists above is still covered. Tests are excluded: they hold the global
// names on purpose, as negatives and as fake S3 responses.
func TestNoSourceUsesTheGlobalS3Name(t *testing.T) {
	root := repoRootFromCmd(t)
	files := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata", "docs", "web":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".sh", ".ps1", ".yaml", ".yml", ".py", ".tmpl":
		default:
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // walking the repo's own source tree
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(root, path)
		for _, r := range s3hosts.GlobalHosts(string(b)) {
			t.Errorf("%s:%d: %s is the global S3 name, which no VPC endpoint serves; use https://<bucket>.s3.<region>.amazonaws.com (#1436):\n\t%s",
				rel, r.Line, r.Host, r.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	t.Logf("swept %d source files", files)
	if files < 500 {
		t.Fatalf("read only %d source files; discovery has broken", files)
	}
}
