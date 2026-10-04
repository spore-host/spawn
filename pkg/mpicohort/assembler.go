package mpicohort

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
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
	return Assembler{WireUp: func(ctx context.Context, members []cohort.Observation) (err error) {
		// Report our own failure before returning it. cohort collapses any
		// assembly error to "terminal/AssemblyFailed at phase=cohort-assembly",
		// which tells an operator nothing about WHICH node or WHY — the cause was
		// only reachable by SSM-ing onto a box afterwards, and on a failed cohort
		// the boxes are already drained. spawn#684.
		defer func() {
			if err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  MPI assembly failed: %v\n", err)
			}
		}()

		data, err := PeersJSON(members, accountBase36)
		if err != nil {
			return fmt.Errorf("build peers file: %w", err)
		}

		// Distribute the cluster SSH key BEFORE the peers file (#684).
		//
		// The peers file is what the user-data waits on, and the step right after
		// that wait is the hostfile build and then mpirun — which needs ssh to the
		// other ranks. Pushing the key first means it is in place before any node
		// is released to use it, so the ordering cannot race.
		zero, ok := rankZero(members)
		if !ok {
			return fmt.Errorf("no rank 0 among %d members: cannot establish the cluster "+
				"SSH key, so mpirun could not reach the other ranks", len(members))
		}
		if zero.ProviderID == "" {
			return fmt.Errorf("rank 0 (%s) has no instance ID for SSM", zero.ID)
		}
		if err := client.WaitForSSMOnline(ctx, region, zero.ProviderID, ssmOnlineTimeout); err != nil {
			return fmt.Errorf("ssm not online for rank 0 %s (%s): %w", zero.ID, zero.ProviderID, err)
		}
		pubKey, err := readRankZeroPubKey(ctx, client, region, zero.ProviderID, runTimeout)
		if err != nil {
			return fmt.Errorf("read cluster SSH key: %w", err)
		}
		keyCmd := installPubKeyCommand(pubKey)

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
				// Cluster SSH key first, then the peers file — the peers file is
				// the release signal, and the key must already be installed when
				// a node acts on it (#684).
				kres, err := client.RunShellScript(gctx, region, m.ProviderID, keyCmd, runTimeout)
				if err != nil {
					return fmt.Errorf("install cluster SSH key on %s (%s): %w", m.ID, m.ProviderID, err)
				}
				if kres.Status != "Success" || kres.ResponseCode != 0 {
					return fmt.Errorf("install cluster SSH key on %s (%s): status=%s code=%d stderr=%s",
						m.ID, m.ProviderID, kres.Status, kres.ResponseCode, kres.Stderr)
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

// Cluster SSH key distribution (#684).
//
// mpirun reaches the other ranks over ssh as root, so every node needs rank 0's
// public key in root's authorized_keys. This used to go through S3 and could
// never work: MPIConfig.BinariesBucket was never set by any caller, so the
// upload rendered "s3:///" and failed parameter validation, which aborted
// cloud-init's scripts-user module and took the rest of the MPI setup with it —
// mpirun was never installed, so enrollment could not pass and every --mpi
// launch failed. Naming the bucket would not have helped either: the spored role
// grants only s3:GetObject on spawn-binaries-*, so the upload would have been
// AccessDenied.
//
// Distribution now rides SSM, the same control-plane path the peers file already
// uses. The PRIVATE key stays on rank 0 and is never transported: putting it in
// user-data would expose it to any local user via IMDS, and sending it over SSM
// would record it in CloudTrail.
const (
	rankZeroPrivKeyPath = "/root/.ssh/id_rsa"
	rankZeroPubKeyPath  = "/root/.ssh/id_rsa.pub"
	authorizedKeysPath  = "/root/.ssh/authorized_keys"
)

// keyWaitBudget bounds how long the assembler waits for rank 0 to finish
// ssh-keygen. Enrollment already gated on mpirun (and on fi_info for EFA), both
// of which the user-data does BEFORE generating the key, so in practice the key
// is present or seconds away; this is slack, not a real wait.
// Vars, not consts, so tests can shrink them — the same pattern taskproto uses
// for flushScriptPath. A 3-minute wait is right in production and intolerable in
// a unit test, and a test that sits through the real budget gets skipped or
// deleted rather than kept honest.
var (
	keyWaitBudget   = 3 * time.Minute
	keyWaitInterval = 5 * time.Second
)

// SetKeyWaitForTest shrinks the cluster-key wait for tests, restoring it via the
// returned func.
func SetKeyWaitForTest(budget, interval time.Duration) func() {
	prevB, prevI := keyWaitBudget, keyWaitInterval
	keyWaitBudget, keyWaitInterval = budget, interval
	return func() { keyWaitBudget, keyWaitInterval = prevB, prevI }
}

// rankZero returns the member with job-array index 0 — the node that owns the
// cluster key.
func rankZero(members []cohort.Observation) (cohort.Observation, bool) {
	for _, m := range members {
		if indexFromName(string(m.ID)) == 0 {
			return m, true
		}
	}
	return cohort.Observation{}, false
}

// readRankZeroPubKey fetches rank 0's public key over SSM, retrying until
// keyWaitBudget expires. An empty or malformed result is treated as not-yet-ready
// rather than as success, so a half-written file cannot be distributed.
func readRankZeroPubKey(ctx context.Context, client LaunchAPI, region, instanceID string, runTimeout time.Duration) (string, error) {
	deadline := time.Now().Add(keyWaitBudget)
	var last string
	for {
		// (ctx, region, instanceID, command, timeout) — instanceID BEFORE the
		// command. Both are strings, so the wrong order compiles silently.
		res, err := client.RunShellScript(ctx, region, instanceID,
			fmt.Sprintf("test -s %s && cat %s", rankZeroPubKeyPath, rankZeroPubKeyPath),
			runTimeout)
		if err == nil && res.Status == "Success" && res.ResponseCode == 0 {
			key := strings.TrimSpace(res.Stdout)
			// An ssh public key is "<type> <base64>[ comment]"; require at least
			// the first two fields so a partial read is not distributed.
			if fields := strings.Fields(key); len(fields) >= 2 && strings.HasPrefix(fields[0], "ssh-") {
				return key, nil
			}
			last = fmt.Sprintf("unusable key content (%d bytes)", len(key))
		} else if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("status=%s code=%d stderr=%s", res.Status, res.ResponseCode, res.Stderr)
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("rank 0 (%s) produced no usable %s within %s: %s",
				instanceID, rankZeroPubKeyPath, keyWaitBudget, last)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(keyWaitInterval):
		}
	}
}

// installPubKeyCommand appends pubKey to root's authorized_keys, idempotently.
//
// Base64-encoded for the same reason the peers file is: an ssh key's own base64
// payload plus an optional comment must not be re-parsed by the shell. The grep
// guard keeps a retried assembly from appending duplicates.
func installPubKeyCommand(pubKey string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(pubKey + "\n"))
	return fmt.Sprintf(
		"set -e; mkdir -p /root/.ssh; chmod 700 /root/.ssh; "+
			"touch %s; chmod 600 %s; "+
			"printf %%s %s | base64 -d > /tmp/.spawn-cluster-key; "+
			"grep -qxFf /tmp/.spawn-cluster-key %s || cat /tmp/.spawn-cluster-key >> %s; "+
			"rm -f /tmp/.spawn-cluster-key",
		authorizedKeysPath, authorizedKeysPath, b64, authorizedKeysPath, authorizedKeysPath)
}
