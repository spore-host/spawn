package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

// The dashboard's DynamoDB-stream wiring is named in four places that nothing
// tied together: the processor's table switch, the role script's
// ProcessorStreams grant, and the TABLES lists of enable-streams.sh and
// setup-event-source-mappings.sh. When #772 removed autoscale, the table went
// and all four kept naming it, and the live mapping sat Disabled with "Stream
// is closed" for weeks (#1434). These gates keep the four in agreement, keep
// each one naming only a table this repo creates, and check the mapping
// script's deleter.

const (
	processorMain  = "lambda/dashboard-websocket-processor/main.go"
	roleScriptPath = "lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh"
	enableStreams  = "lambda/dashboard-websocket/scripts/enable-streams.sh"
	mappingScript  = "lambda/dashboard-websocket/scripts/setup-event-source-mappings.sh"
)

// processorTables returns the table names parseStreamRecord's switch handles.
func processorTables(t *testing.T) []string {
	t.Helper()
	src := readRepoFile(t, processorMain)
	fn := regexp.MustCompile(`(?s)func parseStreamRecord\(.*?\n}\n`).FindString(src)
	sw := regexp.MustCompile(`(?s)switch tableName \{.*?\n\t}\n`).FindString(fn)
	if sw == "" {
		t.Fatalf("no `switch tableName` in parseStreamRecord in %s", processorMain)
	}
	return submatches(regexp.MustCompile(`(?m)^\tcase ("[^"]+"(?:, "[^"]+")*):`), sw, func(s string) []string {
		var out []string
		for _, q := range strings.Split(s, ", ") {
			out = append(out, strings.Trim(q, `"`))
		}
		return out
	})
}

// roleStreamTables returns the tables whose streams ProcessorStreams grants.
func roleStreamTables(t *testing.T) []string {
	t.Helper()
	src := readRepoFile(t, roleScriptPath)
	stmt := regexp.MustCompile(`(?s)"Sid": "ProcessorStreams".*?"Resource": \[([^\]]*)\]`).FindStringSubmatch(src)
	if stmt == nil {
		t.Fatalf("no ProcessorStreams statement with a Resource list in %s", roleScriptPath)
	}
	res := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(stmt[1], -1)
	var out []string
	for _, r := range res {
		m := regexp.MustCompile(`^\$\{T\}/([^/]+)/stream/\*$`).FindStringSubmatch(r[1])
		if m == nil {
			t.Errorf("ProcessorStreams resource %q is not ${T}/<table>/stream/*", r[1])
			continue
		}
		out = append(out, m[1])
	}
	slices.Sort(out)
	return out
}

// scriptTables returns the entries of a script's TABLES=( ... ) array.
func scriptTables(t *testing.T, path string) []string {
	t.Helper()
	arr := regexp.MustCompile(`(?s)\nTABLES=\((.*?)\n\)`).FindStringSubmatch(readRepoFile(t, path))
	if arr == nil {
		t.Fatalf("no TABLES=( ... ) array in %s", path)
	}
	return submatches(regexp.MustCompile(`(?m)^\s*"([^"]+)"\s*$`), arr[1], func(s string) []string { return []string{s} })
}

func submatches(re *regexp.Regexp, s string, split func(string) []string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, split(m[1])...)
	}
	slices.Sort(out)
	return out
}

// skipWalkDir names the directories the tree walks below skip: git metadata
// and build output, none of it source.
func skipWalkDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "bin", "build", "dist":
		return true
	}
	return false
}

// createdTables discovers every table a repo shell script creates with
// `aws dynamodb create-table`, from a literal --table-name or the file's
// TABLE_NAME="..." assignment.
func createdTables(t *testing.T) map[string]string {
	t.Helper()
	literal := regexp.MustCompile(`--table-name "?([A-Za-z0-9_.-]+)"?`)
	assigned := regexp.MustCompile(`(?m)^TABLE_NAME="([^"$]+)"`)
	created := map[string]string{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(".", path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() && skipWalkDir(d.Name()) {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(path, ".sh") {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // walking this repo
		if err != nil {
			if treewalk.Vanished(".", path, err) {
				return nil
			}
			return err
		}
		src := string(b)
		if !strings.Contains(src, "dynamodb create-table") {
			return nil
		}
		for _, m := range literal.FindAllStringSubmatch(src, -1) {
			created[m[1]] = path
		}
		for _, m := range assigned.FindAllStringSubmatch(src, -1) {
			created[m[1]] = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) == 0 {
		t.Fatal("found no script that runs `aws dynamodb create-table`; this gate is matching nothing")
	}
	return created
}

func TestDashboardStreamTablesAgreeAndExist(t *testing.T) {
	lists := map[string][]string{
		processorMain + " (parseStreamRecord switch)": processorTables(t),
		roleScriptPath + " (ProcessorStreams)":        roleStreamTables(t),
		enableStreams + " (TABLES)":                   scriptTables(t, enableStreams),
		mappingScript + " (TABLES)":                   scriptTables(t, mappingScript),
	}
	want := lists[processorMain+" (parseStreamRecord switch)"]
	if len(want) == 0 {
		t.Fatal("the processor handles no table; this gate is matching nothing")
	}
	created := createdTables(t)
	for where, got := range lists {
		if !slices.Equal(got, want) {
			t.Errorf("%s names %v, the processor's switch names %v", where, got, want)
		}
		for _, table := range got {
			if _, ok := created[table]; !ok {
				t.Errorf("%s names %s, which no setup script in this repo creates", where, table)
			}
		}
	}
}

// TestNothingNamesTheRemovedAutoscaleTable: autoscale and its table went in
// #772 (#1434). CHANGELOG.md keeps the history.
func TestNothingNamesTheRemovedAutoscaleTable(t *testing.T) {
	name := "spawn-autoscale" + "-groups"
	if hits := trackedFilesNaming(t, ".", name, "CHANGELOG.md"); len(hits) > 0 {
		t.Errorf("these still name the removed %s table: %v", name, hits)
	}
}

// trackedFilesNaming returns the files git tracks under root that contain
// needle, except those in skip. Tracked only, as TestNoTrackedCompiledBinaries
// does: a tree walk also read gitignored build output, and a stale
// dashboard-websocket-processor binary from an earlier build failed the gate
// locally while CI, checking out clean, passed (#1439).
func trackedFilesNaming(t *testing.T, root, needle string, skip ...string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(files) == 0 || files[0] == "" {
		t.Fatal("git tracks no files; the discovery has broken, not the tree clean")
	}
	var hits []string
	for _, rel := range files {
		if slices.Contains(skip, rel) {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			continue // deleted but still in the index, or a symlink
		}
		b, err := os.ReadFile(path) //nolint:gosec // a file git tracks in this repo
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), needle) {
			hits = append(hits, rel)
		}
	}
	return hits
}

// TestTrackedFilesNamingReadsOnlyTrackedFiles: build output git ignores does
// not count, and a tracked file still does (#1439).
func TestTrackedFilesNamingReadsOnlyTrackedFiles(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const needle = "removed-thing"
	git("init", "-q")
	write(".gitignore", "bin/\nlambda/fn/fn\n")
	write("src.go", "package x\n")
	write("CHANGELOG.md", needle+"\n")
	git("add", ".gitignore", "src.go", "CHANGELOG.md")
	write("bin/spawn", "\x7fELF "+needle)
	write("lambda/fn/fn", "\x7fELF "+needle)
	if hits := trackedFilesNaming(t, root, needle, "CHANGELOG.md"); len(hits) != 0 {
		t.Errorf("gitignored build output counted: %v", hits)
	}

	write("lambda/fn/main.go", "package main // "+needle+"\n")
	git("add", "lambda/fn/main.go")
	if hits := trackedFilesNaming(t, root, needle, "CHANGELOG.md"); !slices.Equal(hits, []string{"lambda/fn/main.go"}) {
		t.Errorf("tracked files naming it: got %v, want [lambda/fn/main.go]", hits)
	}
}

// fakeMappingAWS answers the mapping script's calls. $FAKE_DIR/mappings holds
// "UUID<TAB>SourceArn" lines, $FAKE_DIR/tables the tables that exist, and
// $FAKE_DIR/denied tables whose describe-table fails with AccessDenied.
const fakeMappingAWS = `#!/bin/bash
echo "$*" >> "$FAKE_DIR/calls"
svc=$1 op=$2; shift 2
query="" table="" uuid="" src=""
while [ $# -gt 0 ]; do
  case "$1" in
    --query) query=$2; shift ;;
    --table-name) table=$2; shift ;;
    --uuid) uuid=$2; shift ;;
    --event-source-arn) src=$2; shift ;;
  esac
  shift
done
case "$svc $op" in
  "lambda get-function") echo arn:aws:lambda:us-east-1:966362334030:function:spawn-dashboard-websocket-processor ;;
  "lambda list-event-source-mappings")
    if [ -n "$src" ]; then
      u=$(awk -F'\t' -v s="$src" '$2 == s {print $1; exit}' "$FAKE_DIR/mappings")
      echo "${u:-None}"
    else
      cat "$FAKE_DIR/mappings"
    fi ;;
  "lambda delete-event-source-mapping")
    awk -F'\t' -v u="$uuid" '$1 != u' "$FAKE_DIR/mappings" > "$FAKE_DIR/m.tmp"; mv "$FAKE_DIR/m.tmp" "$FAKE_DIR/mappings"
    echo "{\"UUID\": \"$uuid\"}" ;;
  "lambda create-event-source-mapping") echo new-uuid ;;
  "dynamodb describe-table")
    if grep -qx "$table" "$FAKE_DIR/denied"; then
      echo "An error occurred (AccessDeniedException) when calling the DescribeTable operation" >&2; exit 254
    fi
    if ! grep -qx "$table" "$FAKE_DIR/tables"; then
      echo "An error occurred (ResourceNotFoundException) when calling the DescribeTable operation: Requested resource not found: Table: $table not found" >&2; exit 254
    fi
    case "$query" in
      *LatestStreamArn*) echo "arn:aws:dynamodb:us-east-1:966362334030:table/$table/stream/2026-01-01T00:00:00.000" ;;
      *) echo "{\"Table\": {\"TableName\": \"$table\"}}" ;;
    esac ;;
esac
exit 0
`

func streamARN(table string) string {
	return "arn:aws:dynamodb:us-east-1:966362334030:table/" + table + "/stream/2026-01-01T00:00:00.000"
}

func TestMappingScriptDeletesOnlyMappingsWhoseTableIsGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs a bash script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fakeMappingAWS), 0o755); err != nil { //nolint:gosec // an executable test fake
		t.Fatal(err)
	}
	const (
		goneUUID   = "0ea19b0f-85c4-4709-b8c3-e10cf7588494"
		liveUUID   = "11111111-live-sweep"
		otherUUID  = "22222222-live-unlisted"
		deniedUUID = "33333333-access-denied"
		sqsUUID    = "44444444-sqs"
	)
	mappings := strings.Join([]string{
		goneUUID + "\t" + streamARN("spawn-removed-table"),
		liveUUID + "\t" + streamARN("spawn-sweep-orchestration"),
		otherUUID + "\t" + streamARN("some-other-existing-table"),
		deniedUUID + "\t" + streamARN("table-we-cannot-describe"),
		sqsUUID + "\tarn:aws:sqs:us-east-1:966362334030:some-queue",
	}, "\n") + "\n"
	for name, body := range map[string]string{
		"mappings": mappings,
		"tables":   "spawn-sweep-orchestration\nsome-other-existing-table\n",
		"denied":   "table-we-cannot-describe\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", mappingScript, "test-profile")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}
	raw := readRepoFile(t, filepath.Join(dir, "calls"))

	var deleted []string
	for _, c := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.HasPrefix(c, "lambda delete-event-source-mapping ") {
			f := strings.Fields(c)
			for i := range f {
				if f[i] == "--uuid" && i+1 < len(f) {
					deleted = append(deleted, f[i+1])
				}
			}
		}
	}
	if !slices.Equal(deleted, []string{goneUUID}) {
		t.Errorf("deleted mappings %v, want only %s (the one whose table is gone); calls:\n%s", deleted, goneUUID, raw)
	}
	if !strings.Contains(string(out), "Removed event source mapping "+goneUUID) {
		t.Errorf("the deletion of %s was not reported:\n%s", goneUUID, out)
	}
	if strings.Contains(raw, "lambda create-event-source-mapping") {
		t.Errorf("created a mapping although the sweep table's mapping exists; calls:\n%s", raw)
	}
	left := readRepoFile(t, filepath.Join(dir, "mappings"))
	for _, keep := range []string{liveUUID, otherUUID, deniedUUID, sqsUUID} {
		if !strings.Contains(left, keep) {
			t.Errorf("mapping %s was removed", keep)
		}
	}
}
