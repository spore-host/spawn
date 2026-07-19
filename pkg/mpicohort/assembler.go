package mpicohort

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/provider"
)

// peersFilePath is the on-instance file the MPI user-data waits on and builds its
// hostfile from (pkg/userdata/mpi.go). The control-plane Assembler writes it via
// SSM instead of relying on the instance to self-discover peers.
const peersFilePath = "/etc/spawn/job-array-peers.json"

// maxSSMPushConcurrency bounds how many nodes we push the peers file to at once.
const maxSSMPushConcurrency = 16

// PeersJSON builds the job-array peers file content from the live cohort members,
// byte-for-byte compatible with the on-instance writePeersFile output
// (json.MarshalIndent of []provider.PeerInfo, 2-space indent), so the MPI
// user-data's `jq` hostfile step is unchanged. IP is the private address
// (Observation.Address) — correct for intra-VPC / EFA rank-to-rank traffic.
// accountBase36 is used only for the best-effort DNS field (the hostfile keys on
// ip); pass "" to leave DNS empty.
func PeersJSON(members []cohort.Observation, accountBase36 string) ([]byte, error) {
	peers := make([]provider.PeerInfo, 0, len(members))
	for _, m := range members {
		name := string(m.ID)
		dns := ""
		if accountBase36 != "" {
			dns = fmt.Sprintf("%s.%s.spore.host", name, accountBase36)
		}
		peers = append(peers, provider.PeerInfo{
			Index:      indexFromName(name),
			InstanceID: m.ProviderID,
			IP:         m.Address, // private IP
			DNS:        dns,
			Provider:   "ec2",
		})
	}
	// Sort by index for stable, rank-ordered output.
	sort.Slice(peers, func(i, j int) bool { return peers[i].Index < peers[j].Index })
	return json.MarshalIndent(peers, "", "  ")
}

// indexFromName extracts the job-array index from a member name's trailing "-N"
// segment (mirrors formatInstanceName's "{name}-{index}"). Returns 0 if absent.
func indexFromName(name string) int {
	if i := strings.LastIndex(name, "-"); i >= 0 && i+1 < len(name) {
		if n, err := strconv.Atoi(name[i+1:]); err == nil {
			return n
		}
	}
	return 0
}

// NewSSMAssembler returns an Assembler whose WireUp builds the peers file and
// pushes it to every member over SSM (control-plane peer distribution). Each
// node is gated on SSM being online, then the base64-encoded JSON is written
// atomically to peersFilePath. Any node failing fails the whole assembly, which
// cohort surfaces as a non-Ready outcome (the caller then drains — cohort does
// not drain on assembly failure).
func NewSSMAssembler(client LaunchAPI, region, accountBase36 string, ssmOnlineTimeout, runTimeout time.Duration) Assembler {
	return Assembler{WireUp: func(ctx context.Context, members []cohort.Observation) error {
		data, err := PeersJSON(members, accountBase36)
		if err != nil {
			return fmt.Errorf("build peers file: %w", err)
		}
		cmd := pushPeersCommand(data)

		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(maxSSMPushConcurrency)
		for _, m := range members {
			m := m
			if m.ProviderID == "" {
				return fmt.Errorf("member %s has no instance ID for SSM push", m.ID)
			}
			g.Go(func() error {
				if err := client.WaitForSSMOnline(gctx, region, m.ProviderID, ssmOnlineTimeout); err != nil {
					return fmt.Errorf("ssm not online for %s (%s): %w", m.ID, m.ProviderID, err)
				}
				res, err := client.RunShellScript(gctx, region, m.ProviderID, cmd, runTimeout)
				if err != nil {
					return fmt.Errorf("push peers to %s (%s): %w", m.ID, m.ProviderID, err)
				}
				if res.Status != "Success" || res.ResponseCode != 0 {
					return fmt.Errorf("push peers to %s (%s): status=%s code=%d stderr=%s",
						m.ID, m.ProviderID, res.Status, res.ResponseCode, res.Stderr)
				}
				return nil
			})
		}
		return g.Wait()
	}}
}

// pushPeersCommand returns a shell command that writes data to peersFilePath
// atomically. The JSON is base64-encoded and decoded on the instance so no JSON
// quoting/escaping can break the shell command.
func pushPeersCommand(data []byte) string {
	b64 := base64.StdEncoding.EncodeToString(data)
	dir := peersFilePath[:strings.LastIndex(peersFilePath, "/")]
	tmp := peersFilePath + ".tmp"
	return fmt.Sprintf("set -e; mkdir -p %s; printf %%s %s | base64 -d > %s; mv %s %s",
		dir, b64, tmp, tmp, peersFilePath)
}
