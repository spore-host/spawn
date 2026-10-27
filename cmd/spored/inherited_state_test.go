package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// The bootstrap body runs `spored discard-inherited-state` (#1241); an
// unregistered name would make cobra run the daemon's root command instead.
func TestDiscardInheritedStateIsASubcommand(t *testing.T) {
	cmd, _, err := newRootCmd().Find([]string{"discard-inherited-state"})
	if err != nil || cmd.Name() != "discard-inherited-state" {
		t.Fatalf("Find(discard-inherited-state) = %v, %v", cmd.Name(), err)
	}
}

func TestDiscardInheritedStateReconcilesTheIMDSInstance(t *testing.T) {
	var got []string
	reconcile := func(id string) ([]string, string, error) {
		got = append(got, id)
		return []string{"/etc/spawn/plugins.json"}, "i-0parent", nil
	}
	var w bytes.Buffer
	discardInheritedState(context.Background(), &w, func(context.Context) (string, error) { return "i-0child", nil }, reconcile)
	if len(got) != 1 || got[0] != "i-0child" {
		t.Fatalf("reconciled %v, want [i-0child]", got)
	}
	if !strings.Contains(w.String(), "i-0parent") || !strings.Contains(w.String(), "/etc/spawn/plugins.json") {
		t.Errorf("output does not say what was discarded from whom: %q", w.String())
	}

	// No instance ID: nothing is reconciled (the owner record would be wrong).
	w.Reset()
	discardInheritedState(context.Background(), &w, func(context.Context) (string, error) { return "", errors.New("no IMDS") }, reconcile)
	if len(got) != 1 || !strings.Contains(w.String(), "no IMDS") {
		t.Errorf("with no instance ID: reconciled %v, output %q", got, w.String())
	}
}

// #1343: the bootstrap's `spored discard-inherited-state` also strips the MPI
// cluster key root's authorized_keys inherited, for THIS instance, and says
// how many lines went without printing a key.
func TestDiscardInheritedStateStripsInheritedClusterKeys(t *testing.T) {
	saved := stripClusterKeys
	t.Cleanup(func() { stripClusterKeys = saved })
	var got []string
	stripClusterKeys = func(id string) (int, error) {
		got = append(got, id)
		return 2, nil
	}
	reconcile := func(string) ([]string, string, error) { return nil, "", nil }
	var w bytes.Buffer
	discardInheritedState(context.Background(), &w, func(context.Context) (string, error) { return "i-0child", nil }, reconcile)
	if len(got) != 1 || got[0] != "i-0child" {
		t.Fatalf("stripped for %v, want [i-0child]", got)
	}
	if !strings.Contains(w.String(), "removed 2 inherited MPI cluster-key line(s)") {
		t.Errorf("output does not give the count: %q", w.String())
	}

	// No instance ID: no strip (the record would be wrong).
	discardInheritedState(context.Background(), &w, func(context.Context) (string, error) { return "", errors.New("no IMDS") }, reconcile)
	if len(got) != 1 {
		t.Errorf("stripped with no instance ID: %v", got)
	}
}
