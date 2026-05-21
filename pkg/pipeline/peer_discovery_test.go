package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToEnvVarName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "preprocess",
			expected: "PREPROCESS",
		},
		{
			input:    "train-model",
			expected: "TRAIN_MODEL",
		},
		{
			input:    "stage_1",
			expected: "STAGE_1",
		},
		{
			input:    "my.stage.name",
			expected: "MY_STAGE_NAME",
		},
		{
			input:    "stage@123",
			expected: "STAGE_123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := toEnvVarName(tt.input)
			if result != tt.expected {
				t.Errorf("toEnvVarName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestJoin(t *testing.T) {
	tests := []struct {
		name     string
		strs     []string
		sep      string
		expected string
	}{
		{
			name:     "empty slice",
			strs:     []string{},
			sep:      ",",
			expected: "",
		},
		{
			name:     "single element",
			strs:     []string{"a"},
			sep:      ",",
			expected: "a",
		},
		{
			name:     "multiple elements comma",
			strs:     []string{"a", "b", "c"},
			sep:      ",",
			expected: "a,b,c",
		},
		{
			name:     "multiple elements space",
			strs:     []string{"10.0.1.1", "10.0.1.2"},
			sep:      " ",
			expected: "10.0.1.1 10.0.1.2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := join(tt.strs, tt.sep)
			if result != tt.expected {
				t.Errorf("join(%v, %q) = %q, want %q", tt.strs, tt.sep, result, tt.expected)
			}
		})
	}
}

func TestWriteAndLoadPeerDiscoveryFile(t *testing.T) {
	t.Skip("Skipping test that requires /etc/spawn directory - covered by integration tests")
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "peers.json")

	peerFile := &PipelinePeerFile{
		PipelineID:    "test-pipeline",
		StageID:       "stage1",
		StageIndex:    0,
		InstanceIndex: 0,
		StagePeers: []PeerInfo{
			{
				StageID:    "stage1",
				StageIndex: 0,
				Index:      0,
				InstanceID: "i-123",
				PrivateIP:  "10.0.1.1",
				DNSName:    "stage1-0.pipeline.spore.host",
				State:      "running",
			},
		},
		UpstreamStages: map[string][]PeerInfo{
			"stage0": {
				{
					StageID:    "stage0",
					StageIndex: 0,
					Index:      0,
					InstanceID: "i-000",
					PrivateIP:  "10.0.0.1",
					DNSName:    "stage0-0.pipeline.spore.host",
					State:      "running",
				},
			},
		},
		DownstreamStages: map[string][]PeerInfo{},
		AllStages: map[string][]PeerInfo{
			"stage0": {
				{
					StageID:    "stage0",
					StageIndex: 0,
					Index:      0,
					InstanceID: "i-000",
					PrivateIP:  "10.0.0.1",
					DNSName:    "stage0-0.pipeline.spore.host",
					State:      "running",
				},
			},
			"stage1": {
				{
					StageID:    "stage1",
					StageIndex: 0,
					Index:      0,
					InstanceID: "i-123",
					PrivateIP:  "10.0.1.1",
					DNSName:    "stage1-0.pipeline.spore.host",
					State:      "running",
				},
			},
		},
	}

	// Test write
	err := WritePeerDiscoveryFile(peerFile, testFile)
	if err != nil {
		t.Fatalf("WritePeerDiscoveryFile failed: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Fatal("Peer discovery file was not created")
	}

	// Test load
	loaded, err := LoadPeerDiscoveryFile(testFile)
	if err != nil {
		t.Fatalf("LoadPeerDiscoveryFile failed: %v", err)
	}

	// Verify content
	if loaded.PipelineID != peerFile.PipelineID {
		t.Errorf("PipelineID mismatch: got %q, want %q", loaded.PipelineID, peerFile.PipelineID)
	}
	if loaded.StageID != peerFile.StageID {
		t.Errorf("StageID mismatch: got %q, want %q", loaded.StageID, peerFile.StageID)
	}
	if len(loaded.StagePeers) != len(peerFile.StagePeers) {
		t.Errorf("StagePeers count mismatch: got %d, want %d", len(loaded.StagePeers), len(peerFile.StagePeers))
	}
	if len(loaded.UpstreamStages) != len(peerFile.UpstreamStages) {
		t.Errorf("UpstreamStages count mismatch: got %d, want %d", len(loaded.UpstreamStages), len(peerFile.UpstreamStages))
	}
}

func TestLoadPeerDiscoveryFile_NotFound(t *testing.T) {
	_, err := LoadPeerDiscoveryFile("/nonexistent/peers.json")
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

func TestPeerFileJSONMarshal(t *testing.T) {
	// Test JSON marshaling without file I/O
	peerFile := &PipelinePeerFile{
		PipelineID:    "test-pipeline",
		StageID:       "stage1",
		StageIndex:    0,
		InstanceIndex: 0,
		StagePeers: []PeerInfo{
			{
				StageID:    "stage1",
				InstanceID: "i-123",
				PrivateIP:  "10.0.1.1",
				DNSName:    "stage1-0.pipeline.spore.host",
			},
		},
		UpstreamStages: map[string][]PeerInfo{
			"stage0": {
				{InstanceID: "i-000", PrivateIP: "10.0.0.1"},
			},
		},
		DownstreamStages: map[string][]PeerInfo{},
		AllStages:        map[string][]PeerInfo{},
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(peerFile, "", "  ")
	if err != nil {
		t.Fatalf("JSON marshal failed: %v", err)
	}

	// Unmarshal back
	var decoded PipelinePeerFile
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("JSON unmarshal failed: %v", err)
	}

	// Verify
	if decoded.PipelineID != peerFile.PipelineID {
		t.Errorf("PipelineID mismatch: got %q, want %q", decoded.PipelineID, peerFile.PipelineID)
	}
	if decoded.StageID != peerFile.StageID {
		t.Errorf("StageID mismatch: got %q, want %q", decoded.StageID, peerFile.StageID)
	}
	if len(decoded.StagePeers) != len(peerFile.StagePeers) {
		t.Errorf("StagePeers count mismatch: got %d, want %d", len(decoded.StagePeers), len(peerFile.StagePeers))
	}
}

func TestLoadPeerDiscoveryFile_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "invalid.json")

	// Write invalid JSON
	if err := os.WriteFile(testFile, []byte("invalid json {"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadPeerDiscoveryFile(testFile)
	if err == nil {
		t.Error("Expected error for invalid JSON")
	}
}

func TestPipelinePeerFile_GetUpstreamPeers(t *testing.T) {
	peerFile := &PipelinePeerFile{
		UpstreamStages: map[string][]PeerInfo{
			"stage0": {
				{InstanceID: "i-000", PrivateIP: "10.0.0.1"},
			},
			"stage1": {
				{InstanceID: "i-111", PrivateIP: "10.0.1.1"},
				{InstanceID: "i-112", PrivateIP: "10.0.1.2"},
			},
		},
	}

	// Test existing stage
	peers := peerFile.GetUpstreamPeers("stage1")
	if len(peers) != 2 {
		t.Errorf("Expected 2 peers, got %d", len(peers))
	}

	// Test nonexistent stage
	peers = peerFile.GetUpstreamPeers("nonexistent")
	if peers != nil {
		t.Errorf("Expected nil for nonexistent stage, got %v", peers)
	}
}

func TestPipelinePeerFile_GetDownstreamPeers(t *testing.T) {
	peerFile := &PipelinePeerFile{
		DownstreamStages: map[string][]PeerInfo{
			"stage2": {
				{InstanceID: "i-222", PrivateIP: "10.0.2.1"},
			},
		},
	}

	// Test existing stage
	peers := peerFile.GetDownstreamPeers("stage2")
	if len(peers) != 1 {
		t.Errorf("Expected 1 peer, got %d", len(peers))
	}

	// Test nonexistent stage
	peers = peerFile.GetDownstreamPeers("nonexistent")
	if peers != nil {
		t.Errorf("Expected nil for nonexistent stage, got %v", peers)
	}
}

func TestPipelinePeerFile_GetFirstUpstreamPeer(t *testing.T) {
	tests := []struct {
		name     string
		peerFile *PipelinePeerFile
		wantNil  bool
	}{
		{
			name: "has upstream peers",
			peerFile: &PipelinePeerFile{
				UpstreamStages: map[string][]PeerInfo{
					"stage0": {
						{InstanceID: "i-000", PrivateIP: "10.0.0.1"},
						{InstanceID: "i-001", PrivateIP: "10.0.0.2"},
					},
				},
			},
			wantNil: false,
		},
		{
			name: "no upstream stages",
			peerFile: &PipelinePeerFile{
				UpstreamStages: map[string][]PeerInfo{},
			},
			wantNil: true,
		},
		{
			name: "upstream stage with no peers",
			peerFile: &PipelinePeerFile{
				UpstreamStages: map[string][]PeerInfo{
					"stage0": {},
				},
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := tt.peerFile.GetFirstUpstreamPeer()
			if (peer == nil) != tt.wantNil {
				t.Errorf("GetFirstUpstreamPeer() = %v, wantNil %v", peer, tt.wantNil)
			}
			if !tt.wantNil && peer.InstanceID != "i-000" {
				t.Errorf("Expected first peer i-000, got %s", peer.InstanceID)
			}
		})
	}
}

func TestPipelinePeerFile_GetMyStagePeers(t *testing.T) {
	peerFile := &PipelinePeerFile{
		StagePeers: []PeerInfo{
			{InstanceID: "i-123", PrivateIP: "10.0.1.1"},
			{InstanceID: "i-124", PrivateIP: "10.0.1.2"},
		},
	}

	peers := peerFile.GetMyStagePeers()
	if len(peers) != 2 {
		t.Errorf("Expected 2 stage peers, got %d", len(peers))
	}
}

func TestPipelinePeerFile_GenerateHostfile(t *testing.T) {
	tmpDir := t.TempDir()
	hostfile := filepath.Join(tmpDir, "hostfile")

	peerFile := &PipelinePeerFile{
		StagePeers: []PeerInfo{
			{InstanceID: "i-000", PrivateIP: "10.0.0.1", DNSName: "host1.spore.host"},
			{InstanceID: "i-001", PrivateIP: "10.0.0.2", DNSName: "host2.spore.host"},
			{InstanceID: "i-002", PrivateIP: "10.0.0.3", DNSName: "host3.spore.host"},
		},
	}

	err := peerFile.GenerateHostfile(hostfile, 4)
	if err != nil {
		t.Fatalf("GenerateHostfile failed: %v", err)
	}

	// Read and verify content
	content, err := os.ReadFile(hostfile)
	if err != nil {
		t.Fatalf("Failed to read hostfile: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 3 {
		t.Errorf("Expected 3 lines in hostfile, got %d", len(lines))
	}

	// Verify format
	expectedLines := []string{
		"10.0.0.1 slots=4",
		"10.0.0.2 slots=4",
		"10.0.0.3 slots=4",
	}

	for i, expected := range expectedLines {
		if lines[i] != expected {
			t.Errorf("Line %d: got %q, want %q", i, lines[i], expected)
		}
	}
}

func TestPipelinePeerFile_GenerateHostfile_UseDNS(t *testing.T) {
	tmpDir := t.TempDir()
	hostfile := filepath.Join(tmpDir, "hostfile-dns")

	// Peer with no private IP should fall back to DNS
	peerFile := &PipelinePeerFile{
		StagePeers: []PeerInfo{
			{InstanceID: "i-000", PrivateIP: "", DNSName: "host1.spore.host"},
		},
	}

	err := peerFile.GenerateHostfile(hostfile, 1)
	if err != nil {
		t.Fatalf("GenerateHostfile failed: %v", err)
	}

	content, err := os.ReadFile(hostfile)
	if err != nil {
		t.Fatalf("Failed to read hostfile: %v", err)
	}

	expectedLine := "host1.spore.host slots=1"
	actualLine := strings.TrimSpace(string(content))
	if actualLine != expectedLine {
		t.Errorf("Got %q, want %q", actualLine, expectedLine)
	}
}

func TestPipelinePeerFile_GetEnvironmentVariables(t *testing.T) {
	peerFile := &PipelinePeerFile{
		StagePeers: []PeerInfo{
			{InstanceID: "i-100", PrivateIP: "10.0.1.1", DNSName: "peer1.spore.host"},
			{InstanceID: "i-101", PrivateIP: "10.0.1.2", DNSName: "peer2.spore.host"},
		},
		UpstreamStages: map[string][]PeerInfo{
			"preprocess": {
				{InstanceID: "i-000", PrivateIP: "10.0.0.1", DNSName: "pre1.spore.host"},
				{InstanceID: "i-001", PrivateIP: "10.0.0.2", DNSName: "pre2.spore.host"},
			},
			"train-model": {
				{InstanceID: "i-200", PrivateIP: "10.0.2.1", DNSName: "train1.spore.host"},
			},
		},
	}

	env := peerFile.GetEnvironmentVariables()

	// Verify upstream single peer variables
	if env["UPSTREAM_PREPROCESS"] != "10.0.0.1" {
		t.Errorf("UPSTREAM_PREPROCESS = %q, want %q", env["UPSTREAM_PREPROCESS"], "10.0.0.1")
	}
	if env["UPSTREAM_PREPROCESS_DNS"] != "pre1.spore.host" {
		t.Errorf("UPSTREAM_PREPROCESS_DNS = %q, want %q", env["UPSTREAM_PREPROCESS_DNS"], "pre1.spore.host")
	}

	// Verify upstream all peers variables
	expectedIPs := "10.0.0.1,10.0.0.2"
	if env["UPSTREAM_PREPROCESS_ALL"] != expectedIPs {
		t.Errorf("UPSTREAM_PREPROCESS_ALL = %q, want %q", env["UPSTREAM_PREPROCESS_ALL"], expectedIPs)
	}

	expectedDNS := "pre1.spore.host,pre2.spore.host"
	if env["UPSTREAM_PREPROCESS_DNS_ALL"] != expectedDNS {
		t.Errorf("UPSTREAM_PREPROCESS_DNS_ALL = %q, want %q", env["UPSTREAM_PREPROCESS_DNS_ALL"], expectedDNS)
	}

	// Verify train-model (with hyphen converted to underscore)
	if env["UPSTREAM_TRAIN_MODEL"] != "10.0.2.1" {
		t.Errorf("UPSTREAM_TRAIN_MODEL = %q, want %q", env["UPSTREAM_TRAIN_MODEL"], "10.0.2.1")
	}

	// Verify stage peer variables
	expectedStagePeers := "10.0.1.1,10.0.1.2"
	if env["STAGE_PEERS"] != expectedStagePeers {
		t.Errorf("STAGE_PEERS = %q, want %q", env["STAGE_PEERS"], expectedStagePeers)
	}

	expectedStageDNS := "peer1.spore.host,peer2.spore.host"
	if env["STAGE_PEERS_DNS"] != expectedStageDNS {
		t.Errorf("STAGE_PEERS_DNS = %q, want %q", env["STAGE_PEERS_DNS"], expectedStageDNS)
	}

	if env["STAGE_PEER_COUNT"] != "2" {
		t.Errorf("STAGE_PEER_COUNT = %q, want %q", env["STAGE_PEER_COUNT"], "2")
	}
}

func TestPipelinePeerFile_GetEnvironmentVariables_Empty(t *testing.T) {
	peerFile := &PipelinePeerFile{
		StagePeers:     []PeerInfo{},
		UpstreamStages: map[string][]PeerInfo{},
	}

	env := peerFile.GetEnvironmentVariables()

	// Should return empty map
	if len(env) != 0 {
		t.Errorf("Expected empty env map, got %v", env)
	}
}

func TestPeerInfoJSON(t *testing.T) {
	peer := PeerInfo{
		StageID:    "stage1",
		StageIndex: 0,
		InstanceID: "i-123",
		PrivateIP:  "10.0.1.1",
		PublicIP:   "54.1.2.3",
		DNSName:    "stage1-0.pipeline.spore.host",
		State:      "running",
		Index:      0,
	}

	// Test marshal
	data, err := json.Marshal(peer)
	if err != nil {
		t.Fatalf("JSON marshal failed: %v", err)
	}

	// Test unmarshal
	var decoded PeerInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("JSON unmarshal failed: %v", err)
	}

	// Verify
	if decoded.InstanceID != peer.InstanceID {
		t.Errorf("InstanceID mismatch: got %q, want %q", decoded.InstanceID, peer.InstanceID)
	}
	if decoded.PrivateIP != peer.PrivateIP {
		t.Errorf("PrivateIP mismatch: got %q, want %q", decoded.PrivateIP, peer.PrivateIP)
	}
}
