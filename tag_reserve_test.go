package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// The tag reserve's discovery gate (#1355).
//
// A launch keeps a slot free for every tag written on the instance after
// RunInstances, because EC2 fails a CreateTags that would take a resource past
// 50 tags whole: a launch at the limit leaves spored unable to write
// spawn:compute-seconds, so a stop/start resets the cost clock. The reserve is
// computed from one list, pkg/aws postLaunchInstanceTags. This holds the list
// to the code: the tag writes are discovered, not listed.
//
//   - every call that tags an existing resource (CreateTags,
//     Client.UpdateInstanceTags, spored's putTags) is found, and the
//     function it is in must be classified in tagWriteSites, as writing an
//     instance's tags or as not;
//   - every key spored's code (sporedDirs, wholesale) and each instance
//     writer outside it spells as a tag write, or in a shape the scan cannot
//     place (assigned, returned, an unkeyed field), must be on the list;
//   - every key on the list must be one some writer spells.

// sporedDirs is the code that runs on the instance, every tag write in which
// is a write on the instance itself.
var sporedDirs = []string{"pkg/agent", "cmd/spored", "pkg/provider"}

// tagWriteSite classifies a function that calls a tag-writing sink.
type tagWriteSite struct {
	// instance: it writes tags on a launched instance. Its keys are what it
	// spells itself and what the keysFrom functions spell.
	instance bool
	keysFrom []string
	// family is a key family it writes that is not a spawn: key, which the
	// scan does not see (spore:plugin:<name>); checked against its source.
	family string
	// why says what it tags when it is not an instance, or why its keys are
	// counted elsewhere.
	why string
}

// tagWriteSites is every function outside sporedDirs that calls a sink. A new
// one fails the gate until it is classified here.
var tagWriteSites = map[string]tagWriteSite{
	"cmd.runExtend":                            {instance: true, keysFrom: []string{"cmd.extensionTags", "cmd.computeExtendedTTLTags"}},
	"cmd.extendJobArrayWithAudit":              {instance: true, keysFrom: []string{"cmd.extensionTags", "cmd.computeExtendedTTLTags"}},
	"cmd.runStart":                             {instance: true, keysFrom: []string{"cmd.startTags"}},
	"cmd.startJobArray":                        {instance: true, keysFrom: []string{"cmd.startTags"}},
	"cmd.stopOrHibernateInstance":              {instance: true, keysFrom: []string{"cmd.stopTags"}},
	"cmd.stopOrHibernateJobArray":              {instance: true, keysFrom: []string{"cmd.stopTags"}},
	"cmd.runDNSRegister":                       {instance: true},
	"cmd.runUpgradeSpored":                     {instance: true},
	"cmd.launchWithProgress":                   {instance: true},
	"cmd.recordPluginProvenanceTag":            {instance: true, family: "spore:plugin:"},
	"pkg/launcher.createAndAttachEphemeralFSx": {instance: true},
	"pkg/aws.recordLaunchSpotPrice":            {instance: true},

	"pkg/aws.Client.UpdateInstanceTags": {why: "generic sink: writes the keys its callers pass, and each caller is classified here"},
	"pkg/aws.tagOriginalInstance": {why: "tags the persistent Spot request, not the instance; the request carries the " +
		"instance's launch tag set, so the instance's reserve covers its one slot"},
	"pkg/aws.Client.propagateSnapshotTagsToVolumes": {why: "tags EBS volumes restored from a snapshot, within their own limit (fitVolumeTags)"},
	"pkg/aws.Client.TagAMIWindows":                  {why: "tags an AMI"},
	"pkg/aws.Client.TagAMIWindowsWarm":              {why: "tags an AMI"},
	"lambda/ttl-reaper.reaper.stampDiscovery":       {why: "the reaper's first-seen stamp on security groups and placement groups"},
	"lambda/ttl-reaper.reaper.stampEIPDiscovery":    {why: "the reaper's first-seen stamp on Elastic IPs"},
}

// notWrittenMentions are keys spored's code spells in a shape the scan cannot
// place that are not writes. Only a mention is excused, never a write.
var notWrittenMentions = map[string]string{
	"spawn:config": "spored reads it to load the launch config object (pkg/provider launchConfigLoader) and never writes it",
}

func inSporedDirs(declID string) bool {
	for _, d := range sporedDirs {
		if strings.HasPrefix(declID, d+".") {
			return true
		}
	}
	return false
}

func TestEveryPostLaunchTagIsListed(t *testing.T) {
	sc := scanTagUses(t, ".")
	sinks := scanTagSinks(t, ".")
	if len(sinks) < 25 {
		t.Fatalf("found only %d tag-writing calls; the scan has broken", len(sinks))
	}

	listed := map[string]bool{}
	for _, k := range spawnaws.PostLaunchTagKeys() {
		listed[k] = true
	}
	onList := func(key string) bool {
		if listed[key] {
			return true
		}
		for k := range listed {
			if isTagPrefix(k) && strings.HasPrefix(key, k) {
				return true
			}
		}
		return false
	}

	written := map[string]string{} // key → where
	note := func(key, where string) {
		if _, ok := written[key]; !ok {
			written[key] = where
		}
	}
	addDecl := func(declID string) bool {
		found := false
		for k, site := range sc.writesIn[declID] {
			note(k, declID+" at "+site)
			found = true
		}
		for k, site := range sc.mentionsIn[declID] {
			if _, ok := notWrittenMentions[k]; ok {
				continue
			}
			note(k, declID+" at "+site+" (spelled, not as a read)")
			found = true
		}
		return found
	}

	for id := range sc.writesIn {
		if inSporedDirs(id) {
			addDecl(id)
		}
	}
	for id := range sc.mentionsIn {
		if inSporedDirs(id) {
			addDecl(id)
		}
	}

	seenSite := map[string]bool{}
	for _, s := range sinks {
		if inSporedDirs(s.decl) {
			continue
		}
		seenSite[s.decl] = true
		site, ok := tagWriteSites[s.decl]
		if !ok {
			t.Errorf("%s calls %s at %s and is not classified: add it to tagWriteSites, as an instance "+
				"writer (its keys then need slots, postLaunchInstanceTags in pkg/aws/post_launch_tags.go) or "+
				"with what it tags instead", s.decl, s.call, s.site)
			continue
		}
		if !site.instance {
			continue
		}
		addDecl(s.decl)
		for _, from := range site.keysFrom {
			if !addDecl(from) {
				t.Errorf("tagWriteSites: %s takes its keys from %s, which writes no tag key: update the classification", s.decl, from)
			}
		}
		if site.family != "" {
			if !declSpells(t, s.site, site.family) {
				t.Errorf("tagWriteSites: %s no longer spells %q: update the classification", s.decl, site.family)
			}
			note(site.family, s.decl)
		}
	}
	for id := range tagWriteSites {
		if !seenSite[id] {
			t.Errorf("tagWriteSites classifies %s, which no longer calls a tag-writing sink: remove it", id)
		}
	}

	if len(written) < 25 {
		t.Fatalf("found only %d keys written after launch; the scan has broken", len(written))
	}
	for _, k := range sortedKeySet(written) {
		if !onList(k) {
			t.Errorf("%s is written on an instance after launch (%s) but is not on postLaunchInstanceTags "+
				"(pkg/aws/post_launch_tags.go), so a launch keeps no slot for it: at 50 tags its write fails "+
				"whole. Add it, with what writes it and when", k, written[k])
		}
	}
	for _, k := range spawnaws.PostLaunchTagKeys() {
		if _, ok := written[k]; !ok {
			t.Errorf("%s is on postLaunchInstanceTags but no post-launch writer spells it: remove it, "+
				"or classify the writer in tagWriteSites", k)
		}
	}
}

// declSpells reports whether the source file at site (file:line:col) contains
// the literal s.
func declSpells(t *testing.T, site, s string) bool {
	t.Helper()
	file := site
	if i := strings.Index(site, ".go:"); i >= 0 {
		file = site[:i+3]
	}
	src, err := os.ReadFile(file) //nolint:gosec // the repo's own sources
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return strings.Contains(string(src), `"`+s+`"`)
}

// tagSink is a call that writes tags on an existing resource.
type tagSink struct{ decl, site, call string }

// tagSinkNames are the calls that tag an existing resource: EC2's CreateTags,
// and the wrappers spawn and spored write through.
var tagSinkNames = map[string]bool{"CreateTags": true, "UpdateInstanceTags": true, "putTags": true}

// scanTagSinks finds every call to a tag sink in the non-test Go files under
// root, nested lambda modules included, with the function it is in.
func scanTagSinks(t *testing.T, root string) []tagSink {
	t.Helper()
	var out []tagSink
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		for _, decl := range f.Decls {
			id := dir + "." + tagDeclName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && tagSinkNames[sel.Sel.Name] {
					out = append(out, tagSink{decl: id, site: fset.Position(call.Pos()).String(), call: sel.Sel.Name})
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].site < out[j].site })
	return out
}
