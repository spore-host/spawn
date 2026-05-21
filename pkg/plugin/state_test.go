package plugin_test

import (
	"os"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

func TestDiskStateStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := plugin.NewDiskStateStore(dir)

	now := time.Now().Truncate(time.Second)
	original := &plugin.PluginState{
		Name:        "test-plugin",
		Version:     "1.0.0",
		Status:      plugin.StatusRunning,
		Config:      map[string]string{"key": "val"},
		Outputs:     map[string]string{"out": "result"},
		Pushed:      map[string]string{"token": "abc"},
		InstalledAt: now,
	}

	if err := store.Save(original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load("test-plugin")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Name != original.Name {
		t.Errorf("Name: got %q want %q", loaded.Name, original.Name)
	}
	if loaded.Status != original.Status {
		t.Errorf("Status: got %q want %q", loaded.Status, original.Status)
	}
	if loaded.Config["key"] != "val" {
		t.Errorf("Config[key]: got %q want %q", loaded.Config["key"], "val")
	}
}

func TestDiskStateStore_NotFound(t *testing.T) {
	store := plugin.NewDiskStateStore(t.TempDir())
	_, err := store.Load("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent plugin")
	}
}

func TestDiskStateStore_List(t *testing.T) {
	dir := t.TempDir()
	store := plugin.NewDiskStateStore(dir)

	for _, name := range []string{"plugin-a", "plugin-b"} {
		if err := store.Save(&plugin.PluginState{Name: name, Status: plugin.StatusRunning}); err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
	}

	states, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(states) != 2 {
		t.Errorf("List returned %d plugins, want 2", len(states))
	}
}

func TestDiskStateStore_Delete(t *testing.T) {
	dir := t.TempDir()
	store := plugin.NewDiskStateStore(dir)

	if err := store.Save(&plugin.PluginState{Name: "p", Status: plugin.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("p"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Directory should be gone.
	if _, err := os.Stat(dir + "/p"); !os.IsNotExist(err) {
		t.Errorf("expected plugin dir to be removed, got err=%v", err)
	}
}
