package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// scriptedPeers is a provider whose DiscoverPeers answers from a script, one
// entry per call, repeating the last. It counts the calls.
type scriptedPeers struct {
	*stubProvider
	mu     sync.Mutex
	script []peerAnswer
	calls  int
}

type peerAnswer struct {
	peers []provider.PeerInfo
	err   error
}

func (s *scriptedPeers) DiscoverPeers(_ context.Context, _ string) ([]provider.PeerInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	s.calls++
	return s.script[i].peers, s.script[i].err
}

func (s *scriptedPeers) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// peersFileInTemp points the agent's peers file at a temporary directory.
func peersFileInTemp(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spawn", "job-array-peers.json")
	old := jobArrayPeersFile
	jobArrayPeersFile = path
	t.Cleanup(func() { jobArrayPeersFile = old })
	return path
}

func ec2Peer(i int) provider.PeerInfo {
	return provider.PeerInfo{Index: i, InstanceID: "i-burst" + string(rune('0'+i)), IP: "10.0.0." + string(rune('1'+i)), Provider: "ec2"}
}

func localPeer(i int) provider.PeerInfo {
	return provider.PeerInfo{Index: i, InstanceID: "local-node-" + string(rune('0'+i)), IP: "192.168.1." + string(rune('1'+i)), Provider: "local"}
}

// hybridAgent is a burst member of job array "genomics" on EC2.
func hybridAgent(p provider.Provider) *Agent {
	return &Agent{
		provider: p,
		identity: &provider.Identity{InstanceID: "i-burst0", Region: "us-west-2", Provider: "ec2"},
		config:   &provider.Config{JobArrayID: "genomics", Burst: true},
	}
}

func readPeers(t *testing.T, path string) []provider.PeerInfo {
	t.Helper()
	peers, _, err := provider.ReadPeersFile(path)
	if err != nil {
		t.Fatalf("read peers file: %v", err)
	}
	return peers
}

// TestHybridMemberWritesItsPeersFile is #933: spored discovered a hybrid
// member's peers (including the local nodes, #918) and only logged the count.
// The workload must be able to read the list, from the same file --mpi uses.
func TestHybridMemberWritesItsPeersFile(t *testing.T) {
	path := peersFileInTemp(t)
	want := []provider.PeerInfo{localPeer(0), ec2Peer(1)}
	a := hybridAgent(&scriptedPeers{stubProvider: &stubProvider{}, script: []peerAnswer{{peers: want}}})

	if err := a.refreshPeers(context.Background()); err != nil {
		t.Fatalf("refreshPeers: %v", err)
	}
	got := readPeers(t, path)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("peers file = %+v, want %+v", got, want)
	}
	// Windows has no POSIX mode bits (it synthesises 0666), and spored runs on
	// Linux; only the mode assertion is Linux-only (#1002).
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("peers file mode = %v (%v), want 0644 so a non-root workload can read it", info.Mode().Perm(), err)
		}
	}
	if mem, _ := a.Peers(); len(mem) != 2 {
		t.Errorf("agent holds %d peers, want 2", len(mem))
	}
}

// TestPeersFileFollowsMembership: discovery at boot runs before most siblings
// have registered, so the list must be refreshed. A member that joins later must
// appear, and one that leaves must disappear.
func TestPeersFileFollowsMembership(t *testing.T) {
	path := peersFileInTemp(t)
	p := &scriptedPeers{stubProvider: &stubProvider{}, script: []peerAnswer{
		{peers: []provider.PeerInfo{ec2Peer(0)}},                           // boot: only itself
		{peers: []provider.PeerInfo{localPeer(0), ec2Peer(0), ec2Peer(1)}}, // a local node and a sibling join
		{peers: []provider.PeerInfo{ec2Peer(0), ec2Peer(1)}},               // the local node leaves
	}}
	a := hybridAgent(p)
	if err := a.refreshPeers(context.Background()); err != nil {
		t.Fatalf("boot refresh: %v", err)
	}

	stop := runPeerLoop(a, 5*time.Millisecond)
	waitFor(t, func() bool { return p.callCount() >= 3 })
	stop()
	waitFor(t, func() bool { return len(readPeers(t, path)) == 2 })
	for _, peer := range readPeers(t, path) {
		if peer.Provider == "local" {
			t.Fatalf("peer list still lists the local node that left: %+v", readPeers(t, path))
		}
	}
}

// TestPeersFileKeepsTheLastListWhenDiscoveryFails: a transient EC2 or DynamoDB
// failure must not empty the list the workload is using.
func TestPeersFileKeepsTheLastListWhenDiscoveryFails(t *testing.T) {
	path := peersFileInTemp(t)
	p := &scriptedPeers{stubProvider: &stubProvider{}, script: []peerAnswer{
		{peers: []provider.PeerInfo{ec2Peer(0), localPeer(1)}},
		{err: errors.New("ThrottlingException")},
	}}
	a := hybridAgent(p)
	if err := a.refreshPeers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.refreshPeers(context.Background()); err == nil {
		t.Fatal("refreshPeers hid the discovery failure")
	}
	if got := readPeers(t, path); len(got) != 2 {
		t.Fatalf("a failed refresh changed the peers file to %+v", got)
	}
}

// TestOnlyHybridMembersMaintainThePeersFile: for `spawn launch --mpi` the cohort
// Assembler writes this file, and its arrival releases the MPI user-data, after
// the cluster SSH key is in place (#684). spored must never write it for such a
// member, or for an ordinary `spawn launch --count` member; neither carries
// spawn:burst. A local node and a burst instance must.
func TestOnlyHybridMembersMaintainThePeersFile(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		cfg      provider.Config
		want     bool
	}{
		{"burst member on EC2", "ec2", provider.Config{JobArrayID: "a", Burst: true}, true},
		{"local node in a job array", "local", provider.Config{JobArrayID: "a"}, true},
		{"spawn launch --mpi / --count member", "ec2", provider.Config{JobArrayID: "a"}, false},
		{"not in a job array (EC2)", "ec2", provider.Config{Burst: true}, false},
		{"not in a job array (local)", "local", provider.Config{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := peersFileInTemp(t)
			cfg := tc.cfg
			p := &scriptedPeers{stubProvider: &stubProvider{}, script: []peerAnswer{{peers: []provider.PeerInfo{ec2Peer(0)}}}}
			a := &Agent{provider: p, identity: &provider.Identity{Provider: tc.provider}, config: &cfg}
			if got := a.MaintainsPeersFile(); got != tc.want {
				t.Fatalf("MaintainsPeersFile() = %v, want %v", got, tc.want)
			}

			// Through the loop as well: a non-hybrid member must not write the
			// file however long spored runs.
			stop := runPeerLoop(a, time.Millisecond)
			time.Sleep(30 * time.Millisecond)
			stop()
			_, err := os.Stat(path)
			if wrote := err == nil; wrote != tc.want {
				t.Fatalf("peers file written = %v, want %v", wrote, tc.want)
			}
		})
	}
}

// TestBootWritesTheHybridPeersFile: the daemon writes the file during startup,
// not only from the first refresh a minute later. It runs the real NewAgent
// with every AWS credential source removed, so the registry registration it
// also attempts cannot reach AWS.
func TestBootWritesTheHybridPeersFile(t *testing.T) {
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	path := peersFileInTemp(t)

	p := &scriptedPeers{
		stubProvider: &stubProvider{
			identity:       &provider.Identity{InstanceID: "i-burst0", Region: "us-west-2", AccountID: "123456789012", Provider: "ec2"},
			config:         &provider.Config{JobArrayID: "genomics", Burst: true, EBSHourlyCost: 0.01},
			otherInstances: -1,
		},
		script: []peerAnswer{{peers: []provider.PeerInfo{localPeer(0), ec2Peer(0)}}},
	}
	if _, err := NewAgent(context.Background(), p); err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if got := readPeers(t, path); len(got) != 2 {
		t.Fatalf("peers file after boot = %+v, want the 2 discovered peers", got)
	}
}

// runPeerLoop starts the refresh loop and returns a function that stops it and
// waits for it to exit.
func runPeerLoop(a *Agent, interval time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.maintainPeers(ctx, interval)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
