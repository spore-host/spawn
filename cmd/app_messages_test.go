package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/libs/catalog"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// The fixes for #1153's misleading app/service output.

// embeddedCatalogOnly makes the catalog the built-in one for this test, without
// the developer's ~/.spawn/catalog.yaml overlay (which can rebind or hide apps).
func embeddedCatalogOnly(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(path, []byte("apps: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog.SetOverlayPath(path)
	catalog.Reload()
	t.Cleanup(func() {
		catalog.SetOverlayPath("")
		catalog.Reload()
	})
}

// cmd/app.go's own comment: "a bare ref uses --app-version or 'latest'". A
// recipe entry has no tag_default, so a bare ref used to fail.
func TestBYOImageTag(t *testing.T) {
	embeddedCatalogOnly(t)
	for _, c := range []struct{ ref, version, wantImg, wantTag string }{
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview", "", "123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview", "latest"},
		{"someorg/app", "1.2", "someorg/app", "1.2"},
		{"someorg/app:3.0", "", "someorg/app", "3.0"},
		{"localhost:5000/app", "", "localhost:5000/app", "latest"},
	} {
		img, tag := byoImageTag(c.ref, c.version)
		if img != c.wantImg || tag != c.wantTag {
			t.Errorf("byoImageTag(%q, %q) = %q, %q; want %q, %q", c.ref, c.version, img, tag, c.wantImg, c.wantTag)
		}
	}
	// And the tag it yields resolves on a recipe entry, the case that failed.
	entry, ok := catalog.Lookup("paraview")
	if !ok {
		t.Fatal("paraview is not in the catalog")
	}
	e := *entry
	_, tag := byoImageTag("123456789012.dkr.ecr.us-east-1.amazonaws.com/paraview", "")
	e.TagDefault, e.TagsAvailable = tag, []string{tag}
	if got, err := e.ResolveTag(""); err != nil || got != "latest" {
		t.Errorf("ResolveTag after a bare --image = %q, %v; want latest", got, err)
	}
}

func TestApplyWebFlags(t *testing.T) {
	t.Run("--health-path on a catalog web app is applied", func(t *testing.T) {
		e := catalog.AppEntry{Name: "jupyter", KindRaw: catalog.KindWeb, Port: 8888}
		kind, err := applyWebFlags(&e, 0, "/api/status")
		if err != nil || kind != catalog.KindWeb || e.HealthPath != "/api/status" {
			t.Errorf("kind=%q err=%v health=%q; want web, nil, /api/status", kind, err, e.HealthPath)
		}
	})
	t.Run("--health-path on a DCV app is refused, not ignored", func(t *testing.T) {
		e := catalog.AppEntry{Name: "paraview"}
		if _, err := applyWebFlags(&e, 0, "/healthz"); err == nil || !strings.Contains(err.Error(), "--health-path") {
			t.Errorf("err = %v, want a --health-path refusal", err)
		}
	})
	t.Run("--web-port makes it a web app", func(t *testing.T) {
		e := catalog.AppEntry{Name: "paraview"}
		kind, err := applyWebFlags(&e, 8080, "/healthz")
		if err != nil || kind != catalog.KindWeb || e.Port != 8080 || e.HealthPath != "/healthz" {
			t.Errorf("kind=%q err=%v port=%d health=%q", kind, err, e.Port, e.HealthPath)
		}
	})
}

// docs/bring-your-own-app.md and the README launch an image under a name that is
// not in the catalog; that used to fail "not found in catalog".
func TestAdhocWebEntry(t *testing.T) {
	if _, ok := catalog.Lookup("myapp"); ok {
		t.Fatal("myapp is in the catalog; pick another name for this test")
	}
	e, err := adhocWebEntry("myapp", "someorg/myapp", 8080, "m7i.large")
	if err != nil || e.Name != "myapp" {
		t.Fatalf("adhocWebEntry = %+v, %v", e, err)
	}
	if kind, err := applyWebFlags(e, 8080, ""); err != nil || kind != catalog.KindWeb {
		t.Errorf("an ad-hoc entry with --web-port is kind %q (%v), want web", kind, err)
	}
	for _, c := range []struct {
		name, image string
		port        int
		itype, want string
	}{
		{"myapp", "", 0, "m7i.large", "not found in catalog"},
		{"myapp", "someorg/myapp", 0, "m7i.large", "not found in catalog"},
		{"myapp", "someorg/myapp", 8080, "", "--instance-type"},
		{"My_App", "someorg/myapp", 8080, "m7i.large", "lowercase"},
	} {
		if _, err := adhocWebEntry(c.name, c.image, c.port, c.itype); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("adhocWebEntry(%q, %q, %d, %q) err = %v, want one mentioning %q", c.name, c.image, c.port, c.itype, err, c.want)
		}
	}
}

// The Reconnect hint must not launch a new instance.
func TestAppReconnectHint(t *testing.T) {
	dcv := appReconnectHint(false, "i-0abc")
	if !strings.Contains(dcv, "spawn connect i-0abc") || strings.Contains(dcv, "app launch") {
		t.Errorf("DCV reconnect hint = %q, want spawn connect <id>", dcv)
	}
	if web := appReconnectHint(true, "i-0abc"); strings.Contains(web, "spawn ") {
		t.Errorf("web reconnect hint = %q; spawn connect opens a shell there, not the app", web)
	}
}

// Plain connect on a DCV instance opens the session, so a DCV failure hint
// that says "inspect: spawn connect <id>, then /var/log/…" gave no shell.
func TestDCVInspectHintsOpenAShell(t *testing.T) {
	dcvInst := &aws.InstanceInfo{Tags: map[string]string{"spawn:dcv-session-id": "console"}}
	if !connectOpensDCV(dcvInst, false) {
		t.Fatal("plain connect on a DCV instance should open the session")
	}
	if connectOpensDCV(dcvInst, true) {
		t.Error("connect --session-manager on a DCV instance opens the session, so there is no way to a shell")
	}
	for _, status := range []string{dcvStatusNotInstalled, dcvStatusServerNotRunning, dcvStatusSessionNotCreated, dcvStatusWaiting, "", "something-else"} {
		msg := dcvFailureMessage(status, "i-0abc")
		if strings.Contains(msg, "spawn connect i-0abc") && !strings.Contains(msg, "spawn connect i-0abc --session-manager") {
			t.Errorf("status %q: %q points at plain spawn connect, which opens the DCV session", status, msg)
		}
	}
}

// The session page's restart button navigated to a spawn:// URL nothing
// registers.
func TestSessionPageHasNoUnregisteredURLScheme(t *testing.T) {
	if strings.Contains(sessionHTMLTemplate, "spawn://") {
		t.Error("the session page still uses the spawn:// URL scheme, which spawn never registers")
	}
	if !strings.Contains(sessionHTMLTemplate, "spawn connect {{.InstanceID}}") {
		t.Error("the session page no longer tells the user how to restart the session")
	}
}

func TestAppListJSON(t *testing.T) {
	embeddedCatalogOnly(t)
	rows := appListRows(catalog.List(), "")
	if len(rows) == 0 {
		t.Fatal("no catalog apps listed")
	}
	var out, errOut bytes.Buffer
	if err := writeAppList(&out, &errOut, rows, "json"); err != nil {
		t.Fatal(err)
	}
	var got []appListRow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("-o json is not JSON: %v\n%s", err, out.String())
	}
	if len(got) != len(rows) {
		t.Errorf("decoded %d rows, want %d", len(got), len(rows))
	}
	statuses := map[string]string{}
	for _, r := range got {
		statuses[r.Name] = r.Status
	}
	if statuses["jupyter"] != appStatusLaunchable || statuses["paraview"] != appStatusRecipe {
		t.Errorf("statuses = %v, want jupyter launchable and paraview recipe", statuses)
	}
}

// --host launches nothing, so a dry run must not say an idle timeout is applied.
func TestServiceDryRunWithHostHasNoIdleNote(t *testing.T) {
	resetServiceFlags(t)
	serviceExistingHost = "my-box"
	var out bytes.Buffer
	if err := renderServiceDryRun(context.Background(), &out, []string{"./svc"}, "--addr 127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "idle timeout is applied") {
		t.Errorf("--host dry run claims an idle timeout is applied:\n%s", out.String())
	}

	resetServiceFlags(t)
	serviceInstanceType, spawnRegion = "m7i.large", ""
	out.Reset()
	if err := renderServiceDryRun(context.Background(), &out, []string{"./svc"}, "--addr 127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "idle timeout is applied") {
		t.Errorf("a launching dry run lost its idle-timeout note:\n%s", out.String())
	}
}

// The results_prefix error names the TaskSpec's real field: its JSON tag, which
// is top-level, not resources.results_prefix (#1145).
func TestTaskResultsPrefixErrorNamesTheField(t *testing.T) {
	f, ok := reflect.TypeOf(taskproto.TaskSpec{}).FieldByName("ResultsPrefix")
	if !ok {
		t.Fatal("TaskSpec has no ResultsPrefix field")
	}
	tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	// The launch, and with it this error, moved from task.go to pkg/tasklaunch.
	src, err := os.ReadFile(filepath.Join("..", "pkg", "tasklaunch", "tasklaunch.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`fmt\.Errorf\("([a-z_.]+) %q is not an s3://bucket/prefix URI", spec\.ResultsPrefix\)`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("the results_prefix URI error is not in pkg/tasklaunch/tasklaunch.go; the discovery is broken")
	}
	if m[1] != tag {
		t.Errorf("the error names %q, but the TaskSpec field is %q", m[1], tag)
	}
}

func TestTaskHelpListsEverySubcommand(t *testing.T) {
	for _, c := range taskGroupCmd.Commands() {
		if c.Hidden {
			continue
		}
		if !strings.Contains(taskGroupCmd.Long, "spawn task "+c.Name()) {
			t.Errorf("spawn task --help does not mention %q:\n%s", c.Name(), taskGroupCmd.Long)
		}
	}
}
