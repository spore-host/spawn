package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/provider"
)

func writeTestPeers(t *testing.T, age time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "job-array-peers.json")
	if err := provider.WritePeersFile(path, []provider.PeerInfo{
		{Index: 0, InstanceID: "local-node-0", IP: "192.168.1.10", Provider: "local"},
		{Index: 1, InstanceID: "i-0burst1", IP: "10.0.0.5", Provider: "ec2"},
	}); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestStatusShowsTheHybridPeers is #933: `spored status` must expose the peer
// list a hybrid member's workload reads, and how fresh it is.
func TestStatusShowsTheHybridPeers(t *testing.T) {
	path := writeTestPeers(t, 30*time.Second)
	st := buildPeersStatus("genomics", path, true, time.Minute, time.Now())

	if st.Error != "" || st.Count != 2 || len(st.Peers) != 2 || st.Peers[0].InstanceID != "local-node-0" {
		t.Fatalf("peers status = %+v, want the 2 peers in the file", st)
	}
	if st.Stale || st.UpdatedAt == nil || st.AgeSeconds < 29 || st.AgeSeconds > 60 {
		t.Fatalf("freshness = stale %v, updated %v, age %.0fs; want fresh, about 30s old", st.Stale, st.UpdatedAt, st.AgeSeconds)
	}

	report := &statusReport{Peers: st}
	out := captureStdout(t, func() { _ = renderStatusTable(report) })
	for _, want := range []string{"Job array:", "genomics", "Peers:", "2 in " + path, "refreshed every", "local-node-0", "i-0burst1"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	js := captureStdout(t, func() { _ = renderStatusJSON(report) })
	var decoded statusReport
	if err := json.Unmarshal([]byte(js), &decoded); err != nil || decoded.Peers == nil || decoded.Peers.Count != 2 {
		t.Fatalf("JSON peers = %+v (%v), want count 2:\n%s", decoded.Peers, err, js)
	}
}

// TestStatusFlagsAStalePeerList: spored rewrites the file every interval, so a
// file several intervals old means refreshes are failing and members that
// joined since are missing.
func TestStatusFlagsAStalePeerList(t *testing.T) {
	path := writeTestPeers(t, 10*time.Minute)
	st := buildPeersStatus("genomics", path, true, time.Minute, time.Now())
	if !st.Stale {
		t.Fatalf("a 10-minute-old file refreshed every minute is not stale: %+v", st)
	}
	out := captureStdout(t, func() { _ = renderStatusTable(&statusReport{Peers: st}) })
	if !strings.Contains(out, "stale") {
		t.Errorf("table does not warn about the stale list:\n%s", out)
	}

	// An Assembler-written (--mpi) file is written once, so its age is not
	// staleness.
	if st := buildPeersStatus("mpi", path, false, time.Minute, time.Now()); st.Stale {
		t.Errorf("an --mpi peers file was reported stale: %+v", st)
	}
}

// TestStatusSaysWhenThereIsNoPeersFile: a missing file is reported as such,
// not as a member with zero peers.
func TestStatusSaysWhenThereIsNoPeersFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.json")
	st := buildPeersStatus("genomics", missing, true, time.Minute, time.Now())
	if st.Error != "not written yet" {
		t.Fatalf("missing file: error = %q, want \"not written yet\"", st.Error)
	}
	out := captureStdout(t, func() { _ = renderStatusTable(&statusReport{Peers: st}) })
	if !strings.Contains(out, "unknown") {
		t.Errorf("table does not say the peers are unknown:\n%s", out)
	}
}

// TestStatusReportIncludesPeersForJobArrayMembersOnly wires buildStatusReport:
// a burst member's report carries the peers section, reading the real peers
// file path; an instance outside a job array has none.
func TestStatusReportIncludesPeersForJobArrayMembersOnly(t *testing.T) {
	identity := &provider.Identity{InstanceID: "i-0burst0", Region: "us-west-2", Provider: "ec2"}
	burst := &provider.Config{JobArrayID: "genomics", Burst: true, EBSHourlyCost: 0.01}
	ag, err := agent.NewAgentForQuery(context.Background(), &stubStatusProvider{identity: identity, config: burst})
	if err != nil {
		t.Fatal(err)
	}
	report := buildStatusReport(ag, burst, identity, false)
	if report.Peers == nil {
		t.Fatal("a burst member's status has no peers section")
	}
	if report.Peers.File != provider.JobArrayPeersFile || !report.Peers.MaintainedBySpored || report.Peers.JobArrayID != "genomics" {
		t.Fatalf("peers section = %+v, want the spored-maintained %s", report.Peers, provider.JobArrayPeersFile)
	}

	if solo := newTestReport(t); solo.Peers != nil {
		t.Errorf("an instance outside a job array reports peers: %+v", solo.Peers)
	}
}
