package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launchconfig"
)

// The launch config object's two classification gates (#1359).
//
// pkg/launchconfig records, for every key a launch writes, whether it lives in
// the object (Settings) or stays a tag (TagKeys, with the reason). These hold
// that record to the code: the keys are discovered, from the tag builder's
// source and from what it emits, never listed here.

// launchTagKeys is every key a launch can write: what scanTagUses finds
// written in pkg/aws.buildTags, plus what BuildLaunchTags emits for a set of
// configs between them setting every section (keys written through a constant
// from another package, such as spotrequest.TypeTag, are seen there).
func launchTagKeys(t *testing.T, sc *tagScan) map[string]string {
	t.Helper()
	keys := map[string]string{}
	for k, site := range sc.writesIn["pkg/aws.buildTags"] {
		keys[k] = site
	}
	sampling := 0.5
	configs := []spawnaws.LaunchConfig{
		{
			Name: "a", TTL: "4h", IdleTimeout: "1h", HibernateOnIdle: true, CostLimit: 10, PricePerHour: 0.1, PriceSource: "live",
			SessionTimeout: "8h", PreStop: "sync", PreStopTimeout: "5m", Username: "u", SpawnVersion: "1.0.0", TargetOS: "linux",
			SpotInterruptionWebhookURL: "https://s", WebhookCorrelation: "c", WebhookTimeout: "2s", CompletionWebhookURL: "https://d",
			NotifyURL: "https://n", NotifyCommand: "/s", SlackWorkspaceID: "T", NotifyPlatform: "slack",
			ActivePortsRaw: "1", ActiveProcessesRaw: "p", DCVSessionID: "console", AppName: "app",
			JobArrayID: "j", JobArrayName: "jn", JobArraySize: 2, JobArrayIndex: 1,
			SweepID: "s", SweepName: "sn", SweepSize: 2, SweepIndex: 1, Parameters: map[string]string{"p": "v"},
			EFSID: "fs-1", FSxLustreID: "fs-2", FSxMountName: "m", DNSName: "d", DNSDomain: "e.org", DNSEndpoint: "https://x",
			OnComplete: "terminate", CompletionFile: "/f", CompletionDelay: "1m", JobArrayCommand: "run",
			Spot: true, IdleCPUPercent: 7,
			Observability: spawnaws.SporedObservability{MetricsEnabled: true, MetricsPort: 9100, MetricsBind: "0.0.0.0", MetricsPath: "/m",
				TracingEnabled: true, TracingSampling: &sampling},
		},
		{Name: "b", TerminateOnIdle: true, AppMode: "web", ReadyPort: 8888, ReadyHealthPath: "/h", WebAuth: "none",
			FSxPending: "fs-3", FSxImportPath: "s3://b/i", FSxExportPath: "s3://b/o"},
	}
	for i, c := range configs {
		tags, err := spawnaws.BuildLaunchTags(c, "111122223333", "arn:aws:iam::111122223333:user/u", "acct")
		if err != nil {
			t.Fatalf("config %d: %v", i, err)
		}
		for _, tg := range tags {
			k := *tg.Key
			if strings.HasPrefix(k, "spawn:param:") {
				k = "spawn:param:"
			}
			if _, ok := keys[k]; !ok {
				keys[k] = "emitted by BuildLaunchTags"
			}
		}
	}
	if len(keys) < 50 {
		t.Fatalf("discovered only %d keys a launch writes; the discovery has broken", len(keys))
	}
	return keys
}

// TestEveryLaunchTagIsClassified is the discovery gate: every spawn: key a
// launch writes is classified, as staying a tag or living in the launch config
// object, so a new key cannot be added without that decision; and none of the
// object's settings is written as a tag any more.
func TestEveryLaunchTagIsClassified(t *testing.T) {
	sc := scanTagUses(t, ".")
	keys := launchTagKeys(t, sc)
	for _, k := range sortedKeySet(keys) {
		if !strings.HasPrefix(k, "spawn:") {
			continue // Name
		}
		where, ok := launchconfig.Classify(k)
		switch {
		case !ok:
			t.Errorf("%s is written by a launch (%s) and not classified: add it to pkg/launchconfig.TagKeys with the "+
				"reason it stays a tag, or to Settings if only spored reads it, at boot, and nothing edits it after launch", k, keys[k])
		case where == "object":
			t.Errorf("%s is a launch config object setting, and a launch still writes it as a tag (%s)", k, keys[k])
		}
	}
	// And the classification names no key a launch no longer writes, which
	// would be a stale record of a decision.
	for k := range launchconfig.TagKeys {
		if _, ok := keys["spawn:"+k]; !ok {
			t.Errorf("pkg/launchconfig.TagKeys lists spawn:%s, which no launch writes; remove it", k)
		}
	}
	if _, ok := keys[launchconfig.Tag]; !ok {
		t.Errorf("no launch writes %s", launchconfig.Tag)
	}
}

// nonGoReaderExts are the other files that read tags: generated shell, the
// templates and deploy scripts, the dashboard's JavaScript.
var nonGoReaderExts = map[string]bool{".sh": true, ".ps1": true, ".tmpl": true, ".yaml": true, ".yml": true, ".js": true, ".py": true}

// TestNoReaderReadsAMovedKeyFromATag is the second gate: a launch-fixed
// setting is read only from the launch config object. Every tag read in the
// repo's Go (tagScan's reads: a literal, tagprefix.Tag/FilterTag with a
// literal or constant, a constant holding the key, an EC2 filter, a shell
// describe-tags) and every non-comment line of its shell, templates and
// scripts is checked, so a reader of one that still reads it from a tag
// fails here.
func TestNoReaderReadsAMovedKeyFromATag(t *testing.T) {
	sc := scanTagUses(t, ".")
	if _, ok := sc.reads[launchconfig.Tag]; !ok {
		t.Fatalf("the scan sees no read of %s, which spored reads with tagprefix.Tag(launchconfig.TagSuffix): "+
			"it cannot see a constant-spelled read, so it would miss a moved key read the same way", launchconfig.Tag)
	}
	for _, k := range launchconfig.SettingKeys() {
		if site, ok := sc.reads["spawn:"+k]; ok {
			t.Errorf("spawn:%s is read from a tag at %s; it is a launch config object setting (#1359), read only from the object", k, site)
		}
	}

	var moved []string
	for _, k := range launchconfig.SettingKeys() {
		moved = append(moved, regexp.QuoteMeta("spawn:"+k))
	}
	re := regexp.MustCompile(`(` + strings.Join(moved, "|") + `)\b`)
	scanned := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(".", path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != "." && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata" || n == "node_modules" || n == "docs" || n == "docs-gen") {
				return filepath.SkipDir
			}
			return nil
		}
		if !nonGoReaderExts[filepath.Ext(path)] {
			return nil
		}
		scanned++
		f, err := os.Open(path) //nolint:gosec // the repo's own files
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for n := 1; sc.Scan(); n++ {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
				continue
			}
			if m := re.FindString(line); m != "" {
				t.Errorf("%s:%d reads %s from a tag; it is a launch config object setting (#1359)", path, n, m)
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d shell/template/script files; the walk has broken", scanned)
	}
}

// The classification table the PR and the docs quote, generated rather than
// written: `go test -run TestPrintLaunchTagClassification -v`.
func TestPrintLaunchTagClassification(t *testing.T) {
	if os.Getenv("SPAWN_PRINT_CLASSIFICATION") == "" {
		t.Skip("set SPAWN_PRINT_CLASSIFICATION=1 to print the table")
	}
	var rows []string
	for k, why := range launchconfig.Settings {
		rows = append(rows, "| `spawn:"+k+"` | object | "+why+" |")
	}
	for k, why := range launchconfig.TagKeys {
		name := "spawn:" + k
		if k == "param:" {
			name = "spawn:param:*"
		}
		rows = append(rows, "| `"+name+"` | tag | "+why+" |")
	}
	sort.Strings(rows)
	t.Log("\n" + strings.Join(rows, "\n"))
}
