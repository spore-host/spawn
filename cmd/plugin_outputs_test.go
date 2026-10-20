package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// `spawn plugin inspect` promises a plugin's outputs are surfaced after install
// (#1117). They were captured into the controller's local record and never
// shown. This drives the real install-side record write and the status-side
// read: a declared output a local provision step captured is shown (text and
// JSON); a declared output nothing captured is listed as not recorded; and a
// capture the spec does not declare (a credential captured to push) is never
// shown, nor kept on disk when the record exists only for outputs.
func TestPluginStatusSurfacesDeclaredOutputs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	spec := &plugin.PluginSpec{
		Name:    "globus-like",
		Version: "v1.0.0",
		Outputs: map[string]plugin.OutputSpec{
			"endpoint_id": {Description: "Globus endpoint UUID"},
			"url":         {},
		},
	}
	captured := map[string]string{"endpoint_id": "ep-123", "setup_key": "SECRET-SETUP-KEY"}
	saveLocalPluginRecord("i-0123456789abcdef0", "globus-like", spec, nil, map[string]string{"id": "i-0123456789abcdef0"}, nil, captured)

	outputs := recordedPluginOutputs("i-0123456789abcdef0", "globus-like")
	if len(outputs) != 2 {
		t.Fatalf("recordedPluginOutputs = %+v, want the 2 declared outputs", outputs)
	}

	st := &pluginStateResponse{Name: "globus-like", Version: "v1.0.0", Status: plugin.StatusRunning}
	var text bytes.Buffer
	if err := writePluginStatus(&text, st, outputs, false); err != nil {
		t.Fatal(err)
	}
	got := text.String()
	for _, want := range []string{"Outputs:", "endpoint_id: ep-123", "Globus endpoint UUID", "url: (not recorded"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "SECRET-SETUP-KEY") || strings.Contains(got, "setup_key") {
		t.Errorf("status shows an undeclared capture:\n%s", got)
	}

	var jsBuf bytes.Buffer
	if err := writePluginStatus(&jsBuf, st, outputs, true); err != nil {
		t.Fatal(err)
	}
	var decoded pluginStateResponse
	if err := json.Unmarshal(jsBuf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if js := jsBuf.String(); decoded.Outputs["endpoint_id"] != "ep-123" || len(decoded.Outputs) != 1 || strings.Contains(js, "SECRET") {
		t.Errorf("status JSON outputs = %s, want only the recorded declared output", js)
	}

	store, err := plugin.DefaultLocalStore()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := store.Load("i-0123456789abcdef0", "globus-like")
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := rec.Outputs["setup_key"]; kept {
		t.Error("a record kept only for outputs stored an undeclared capture on disk")
	}
}

// A plugin with teardown steps keeps every capture: its deprovision may
// reference any of them ({{ outputs.endpoint_id }}).
func TestRecordOutputsKeepsEveryCaptureForTeardown(t *testing.T) {
	spec := &plugin.PluginSpec{
		Local:   plugin.LocalBlock{Deprovision: []plugin.Step{{Type: "run", Run: "true"}}},
		Outputs: map[string]plugin.OutputSpec{"endpoint_id": {}},
	}
	got := recordOutputs(spec, map[string]string{"endpoint_id": "e", "setup_key": "s"})
	if len(got) != 2 {
		t.Errorf("recordOutputs = %v, want both captures", got)
	}
}

// Install records a plugin that declares outputs even with no teardown steps;
// otherwise `spawn plugin status` has nothing to show for it.
func TestPluginNeedsLocalRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec plugin.PluginSpec
		want bool
	}{
		{"outputs only", plugin.PluginSpec{Outputs: map[string]plugin.OutputSpec{"url": {}}}, true},
		{"deprovision", plugin.PluginSpec{Local: plugin.LocalBlock{Deprovision: []plugin.Step{{Type: "run"}}}}, true},
		{"reconcile", plugin.PluginSpec{Local: plugin.LocalBlock{Reconcile: []plugin.Step{{Type: "run"}}}}, true},
		{"nothing to record", plugin.PluginSpec{Local: plugin.LocalBlock{Provision: []plugin.Step{{Type: "run"}}}}, false},
	} {
		if got := pluginNeedsLocalRecord(&tc.spec); got != tc.want {
			t.Errorf("%s: pluginNeedsLocalRecord = %v, want %v", tc.name, got, tc.want)
		}
	}
}
