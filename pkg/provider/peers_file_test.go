package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWritePeersFileRoundTrips: what spored writes is what a workload, and
// spored status, read back.
func TestWritePeersFileRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "spawn", "job-array-peers.json")
	want := []PeerInfo{
		{Index: 0, InstanceID: "local-node-0", IP: "192.168.1.10", Provider: "local"},
		{Index: 1, InstanceID: "i-0abc", IP: "10.0.0.5", DNS: "b-1.acct.spore.host", Provider: "ec2"},
	}
	if err := WritePeersFile(path, want); err != nil {
		t.Fatalf("WritePeersFile: %v", err)
	}
	got, updated, err := ReadPeersFile(path)
	if err != nil {
		t.Fatalf("ReadPeersFile: %v", err)
	}
	if updated.IsZero() || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("read back %+v (updated %v), want %+v", got, updated, want)
	}

	// Atomic replace: no temporary file is left beside it.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries after a write, want only the peers file", len(entries))
	}
}

// TestMarshalPeersEmptyIsAnArray: a member with no peers yet gets "[]", which a
// jq or json reader can iterate, never "null".
func TestMarshalPeersEmptyIsAnArray(t *testing.T) {
	for _, peers := range [][]PeerInfo{nil, {}} {
		data, err := MarshalPeers(peers)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(data)) != "[]" {
			t.Fatalf("MarshalPeers(%#v) = %q, want []", peers, data)
		}
	}
}
