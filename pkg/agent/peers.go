package agent

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// Hybrid job-array peers (#933).
//
// spored used to discover a job-array member's peers once, at boot, and only log
// the count. Nothing stored the list, wrote it anywhere, exposed it in
// `spored status`, or looked again. A burst member's workload had no way to
// learn the local nodes it was launched to work alongside (#918 made discovery
// find them), and since discovery ran before most siblings had registered or
// booted, even the logged count was a lower bound.
//
// For a hybrid member spored now writes the merged list to
// provider.JobArrayPeersFile and rewrites it every PeerRefreshInterval, so
// members that join later appear and members whose registry heartbeat lapses
// drop out. It is the file and format `spawn launch --mpi` already uses for its
// peers (pkg/mpicohort), so a workload reads one file either way.

// PeerRefreshInterval is how often spored re-discovers a hybrid member's peers
// and rewrites the peers file. The registry heartbeat is 30s
// (registry.HeartbeatInterval), so a minute catches a new member within two
// heartbeats without polling EC2 and DynamoDB every few seconds from every
// member.
const PeerRefreshInterval = time.Minute

// peerRefreshTimeout bounds one discovery, so a hung EC2 or DynamoDB call cannot
// pile up refreshes.
const peerRefreshTimeout = 30 * time.Second

// jobArrayPeersFile is where spored writes the peers. A variable only so tests
// can point it at a temporary directory.
var jobArrayPeersFile = provider.JobArrayPeersFile

// peerState is what the last refresh found, under its own lock because the
// refresh loop runs off the monitor goroutine.
type peerState struct {
	mu           sync.Mutex
	peers        []provider.PeerInfo
	refreshed    time.Time
	lastErr      string
	writeFailing bool // the last write failed; log the next failure only on a change
	written      bool // a list has been written at least once
	loggedCount  int  // peer count last logged, so steady state is not logged every minute
}

// MaintainsPeersFile reports whether spored owns this member's peers file and
// keeps it current: a member of a hybrid job array, which is a burst instance on
// EC2 (spawn:burst=true) or a node running the local provider.
//
// Every other member is excluded on purpose. For `spawn launch --mpi` the
// cohort Assembler writes the file, and its arrival is the MPI user-data's
// release signal: the Assembler pushes the cluster SSH key first and the file
// second, so mpirun can reach the other ranks (#684). If spored wrote the file
// too, a node could be released before it had the key. An ordinary
// `spawn launch --count` member is not in the hybrid registry at all (#904).
//
// It is the same predicate that decides registry registration
// (joinsHybridRegistry), so a member maintains the file exactly when it is in
// the registry the file is built from.
func (a *Agent) MaintainsPeersFile() bool {
	return joinsHybridRegistry(a.identity, a.cfg())
}

// PeersFilePath is the file a job-array member's workload reads its peers from.
func (a *Agent) PeersFilePath() string { return jobArrayPeersFile }

// Peers returns the peers the last refresh found and when it ran. Empty when
// this agent does not maintain the peers file or has not refreshed yet.
func (a *Agent) Peers() ([]provider.PeerInfo, time.Time) {
	a.peerState.mu.Lock()
	defer a.peerState.mu.Unlock()
	return append([]provider.PeerInfo(nil), a.peerState.peers...), a.peerState.refreshed
}

// refreshPeers discovers this member's peers and replaces the peers file with
// them. On a discovery failure the previous file is left as it was, since the
// last good list is more useful to the workload than no list. The file's
// modification time therefore says when the list was last confirmed, which
// `spored status` reports.
func (a *Agent) refreshPeers(ctx context.Context) error {
	jobArrayID := a.cfg().JobArrayID
	ctx, cancel := context.WithTimeout(ctx, peerRefreshTimeout)
	defer cancel()

	peers, err := a.provider.DiscoverPeers(ctx, jobArrayID)
	st := &a.peerState
	if err != nil {
		st.mu.Lock()
		st.lastErr = err.Error()
		st.mu.Unlock()
		log.Printf("Warning: could not refresh peers for job array %s: %v (keeping %s as it was)", jobArrayID, err, jobArrayPeersFile)
		return err
	}

	writeErr := provider.WritePeersFile(jobArrayPeersFile, peers)

	st.mu.Lock()
	defer st.mu.Unlock()
	st.peers = peers
	st.refreshed = time.Now()
	if writeErr != nil {
		st.lastErr = writeErr.Error()
		if !st.writeFailing {
			log.Printf("Warning: could not write %s: %v", jobArrayPeersFile, writeErr)
		}
		st.writeFailing = true
		return writeErr
	}
	st.lastErr = ""
	if !st.written || st.writeFailing || st.loggedCount != len(peers) {
		log.Printf("✓ %d peers in job array %s, written to %s", len(peers), jobArrayID, jobArrayPeersFile)
	}
	st.writeFailing = false
	st.written = true
	st.loggedCount = len(peers)
	return nil
}

// maintainPeers refreshes the peers file every interval until ctx ends. It runs
// in its own goroutine, never on the monitor loop, so a slow EC2 or DynamoDB
// call cannot delay TTL, idle or completion enforcement (#65). Whether this
// member maintains the file is checked on every tick rather than once, so it
// follows the config the monitor loop refreshes.
func (a *Agent) maintainPeers(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.MaintainsPeersFile() {
				_ = a.refreshPeers(ctx) // failures are logged and recorded
			}
		}
	}
}
