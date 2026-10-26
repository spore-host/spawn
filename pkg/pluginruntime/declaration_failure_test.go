package pluginruntime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

// savedStore is a StateStore that reports each Save, so a test can wait for the
// goroutine LoadFromDeclarations starts rather than sleep.
type savedStore struct {
	plugin.StateStore
	saved chan plugin.PluginState
}

func (s *savedStore) Save(st *plugin.PluginState) error {
	err := s.StateStore.Save(st)
	s.saved <- *st
	return err
}

type fixedResolver struct {
	spec *plugin.PluginSpec
	err  error
}

func (f fixedResolver) Resolve(context.Context, string) (*plugin.PluginSpec, error) {
	return f.spec, f.err
}

func (f fixedResolver) ResolveWithProvenance(ctx context.Context, ref string) (*plugin.PluginSpec, *plugin.Provenance, error) {
	s, err := f.Resolve(ctx, ref)
	return s, nil, err
}

// A launch-time plugin that cannot be installed at all — its spec or its
// config does not resolve — must leave a failed state carrying the reason, so
// `spawn plugin status` reports the error instead of "not installed" (#1226).
func TestLoadFromDeclarations_UnresolvableRecordsFailedState(t *testing.T) {
	prev := hostGOOS
	hostGOOS = "linux" // LoadFromDeclarations refuses on a Windows spore (#1001)
	t.Cleanup(func() { hostGOOS = prev })

	spec := &plugin.PluginSpec{
		Name: "needs-key", Version: "1.0.0",
		Config: map[string]plugin.ConfigParam{"auth": {Required: true}},
	}
	cases := []struct {
		name     string
		decl     plugin.Declaration
		resolver plugin.RegistryResolver
		wantName string
		wantErr  []string
	}{
		{"missing required config key", plugin.Declaration{Ref: "needs-key"},
			fixedResolver{spec: spec}, "needs-key", []string{"resolve config", `"auth"`}},
		{"undeclared config key", plugin.Declaration{Ref: "needs-key", Config: map[string]string{"auth": "x", "auth_key": "y"}},
			fixedResolver{spec: spec}, "needs-key", []string{"resolve config", "auth_key"}},
		{"spec does not resolve", plugin.Declaration{Ref: "tailscale@v9.9.9"},
			fixedResolver{err: errors.New("no such release")}, "tailscale", []string{"resolve spec", "no such release"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newTestRuntime(t)
			store := &savedStore{StateStore: rt.store, saved: make(chan plugin.PluginState, 8)}
			rt.store = store

			rt.LoadFromDeclarations(context.Background(), []plugin.Declaration{tc.decl}, tc.resolver)
			select {
			case <-store.saved:
			case <-time.After(30 * time.Second): // a hang guard, not a speed bound
				t.Fatal("no plugin state was saved for a declaration that could not be installed")
			}

			st, err := rt.Status(tc.wantName)
			if err != nil {
				t.Fatalf("plugin status %s: %v (want a failed state, not \"not installed\")", tc.wantName, err)
			}
			if st.Status != plugin.StatusFailed {
				t.Errorf("status = %q, want %q", st.Status, plugin.StatusFailed)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(st.Error, w) {
					t.Errorf("error %q does not contain %q", st.Error, w)
				}
			}
			all, err := rt.ListAll()
			if err != nil || len(all) != 1 || all[0].Name != tc.wantName {
				t.Errorf("plugin list = %v, %v; want the one failed plugin", all, err)
			}
		})
	}
}

// A spec fetch failing on a later boot must not mark a plugin that is running
// as failed: the fetch failing has not stopped it.
func TestRecordDeclarationFailure_LeavesRunningPluginAlone(t *testing.T) {
	for _, status := range []plugin.PluginStatus{plugin.StatusRunning, plugin.StatusDegraded} {
		rt := newTestRuntime(t)
		if err := rt.store.Save(&plugin.PluginState{Name: "p", Status: status}); err != nil {
			t.Fatal(err)
		}
		rt.recordDeclarationFailure("p", "", errors.New("registry unreachable"))
		st, err := rt.Status("p")
		if err != nil || st.Status != status || st.Error != "" {
			t.Errorf("%s plugin after a resolve failure: %+v, %v; want it unchanged", status, st, err)
		}
	}
	// A previously failed one is updated with the new reason.
	rt := newTestRuntime(t)
	if err := rt.store.Save(&plugin.PluginState{Name: "p", Status: plugin.StatusFailed, Error: "old"}); err != nil {
		t.Fatal(err)
	}
	rt.recordDeclarationFailure("p", "", errors.New("new reason"))
	if st, _ := rt.Status("p"); st == nil || st.Error != "new reason" {
		t.Errorf("failed plugin after another failure: %+v, want the new reason", st)
	}
}
