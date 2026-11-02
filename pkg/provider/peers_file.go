package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// JobArrayPeersFile is where a job-array member's workload reads the list of
// its peers. One path and one format for every writer:
//
//   - for `spawn launch --mpi`, the cohort Assembler pushes it over SSM
//     (pkg/mpicohort), and the MPI user-data waits for it and builds its
//     hostfile from it (pkg/userdata/mpi.go);
//   - for a hybrid member (a `spawn burst` instance, or a local node), spored
//     writes it and keeps it current (pkg/agent, #933).
//
// The content is MarshalPeers' output: a JSON array of PeerInfo.
const JobArrayPeersFile = "/etc/spawn/job-array-peers.json"

// MarshalPeers is the peers file's content: a JSON array of PeerInfo, indented
// by two spaces. The MPI user-data's hostfile step reads its "ip" members with
// awk, not jq, which stock AL2 lacks (#1483), so they must stay plain strings.
// An empty or nil list is "[]", never "null", so a reader can always iterate it.
func MarshalPeers(peers []PeerInfo) ([]byte, error) {
	if peers == nil {
		peers = []PeerInfo{}
	}
	return json.MarshalIndent(peers, "", "  ")
}

// WritePeersFile replaces path with peers, atomically: the content goes to a
// temporary file in the same directory, which is then renamed over path. A
// reader sees the old list or the new one, never a partial one. The file is
// world-readable (0644), because the workload reading it is not necessarily
// root.
func WritePeersFile(path string, peers []PeerInfo) error {
	data, err := MarshalPeers(peers)
	if err != nil {
		return fmt.Errorf("encode peers: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write peers file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write peers file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write peers file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write peers file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write peers file: %w", err)
	}
	return nil
}

// ReadPeersFile returns the peers in path and when the file was last written.
func ReadPeersFile(path string) ([]PeerInfo, time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	var peers []PeerInfo
	if err := json.Unmarshal(data, &peers); err != nil {
		return nil, info.ModTime(), fmt.Errorf("parse %s: %w", path, err)
	}
	return peers, info.ModTime(), nil
}
