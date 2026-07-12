package plugin_test

import (
	"errors"
	"os"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

func TestLocalStore_SaveLoadListDelete(t *testing.T) {
	store := plugin.NewLocalStore(t.TempDir())

	rec := &plugin.LocalRecord{
		Name:        "globus-personal-endpoint",
		Ref:         "globus-personal-endpoint",
		InstanceID:  "i-0abc123",
		Config:      map[string]string{"display_name": "spore-ep"},
		Outputs:     map[string]string{"endpoint_id": "uuid-123"},
		Deprovision: []plugin.Step{{Type: "run", Run: "globus endpoint delete {{ outputs.endpoint_id }} --yes"}},
	}
	if err := store.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load("i-0abc123", "globus-personal-endpoint")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Outputs["endpoint_id"] != "uuid-123" {
		t.Errorf("Outputs[endpoint_id] = %q, want uuid-123 (the value deprovision needs)", got.Outputs["endpoint_id"])
	}
	if len(got.Deprovision) != 1 || got.Deprovision[0].Run == "" {
		t.Errorf("Deprovision steps not round-tripped: %+v", got.Deprovision)
	}

	recs, err := store.List("i-0abc123")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 || recs[0].Name != "globus-personal-endpoint" {
		t.Errorf("List returned %d records, want 1 named globus-personal-endpoint", len(recs))
	}

	if err := store.Delete("i-0abc123", "globus-personal-endpoint"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load("i-0abc123", "globus-personal-endpoint"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load after delete: err = %v, want os.ErrNotExist", err)
	}
}

func TestLocalStore_ListMissingInstanceIsEmpty(t *testing.T) {
	store := plugin.NewLocalStore(t.TempDir())
	recs, err := store.List("i-never-seen")
	if err != nil {
		t.Fatalf("List of missing instance should not error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("List of missing instance = %d records, want 0", len(recs))
	}
}

func TestLocalStore_RejectsUnsafeIdentifiers(t *testing.T) {
	store := plugin.NewLocalStore(t.TempDir())

	// Path-traversal-ish instance id must be rejected on Save.
	err := store.Save(&plugin.LocalRecord{Name: "p", InstanceID: "../escape"})
	if err == nil {
		t.Error("Save with unsafe instance id should error")
	}
	// Unsafe plugin name must be rejected.
	err = store.Save(&plugin.LocalRecord{Name: "../p", InstanceID: "i-0abc123"})
	if err == nil {
		t.Error("Save with unsafe plugin name should error")
	}
}

func TestLocalStore_DeleteMissingIsNoError(t *testing.T) {
	store := plugin.NewLocalStore(t.TempDir())
	if err := store.Delete("i-0abc123", "nope"); err != nil {
		t.Errorf("Delete of missing record should be a no-op, got: %v", err)
	}
}
