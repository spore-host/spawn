//go:build e2e_tier0

package e2e

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

// Tier 0 regression coverage for #544: the CLI --volume-size flag (and its
// param-file equivalent, volume_size:) must actually reach the launched
// instances' root EBS volume size on the sweep path, exactly as it already
// does on the single-instance path (cmd/launch_config.go).
//
// Before the fix neither route worked: --volume-size was never read by
// launch_sweep.go at all, and volume_size: in a param file was actively
// REJECTED (#545, "--disk-size is a command-line flag" — a flag that has
// never existed in this codebase). Every sweep row landed on spawn's own
// hardcoded 20 GiB default (pkg/aws/ebs.go) regardless of what was asked for.
//
// ## Why these tests read the wire request instead of DescribeVolumes
//
// This repo's convention (see tier0_sweep_spend_controls_test.go,
// tier0_sweep_iam_test.go) is to assert on observable AWS state, not an
// in-process value — a test that only checks "the flag was parsed" would have
// passed throughout the bug's life, since the flag WAS parsed, onto a config
// nobody read from.
//
// The natural observable here would be DescribeVolumes on the launched
// instance's root volume. It does not work against the pinned Substrate
// version (go.mod: v0.97.0): RunInstances' BlockDeviceMappings are recorded
// nowhere — confirmed empirically (a launch with --volume-size 77 followed by
// DescribeVolumes returns zero volumes) — and DescribeInstanceAttribute
// explicitly rejects "blockDeviceMapping" as an attribute it can answer
// (vendor/.../emulator/ec2_instanceattribute.go documents this as deliberate:
// Substrate answers only the attributes it actually holds state for).
// Substrate's CreateVolume/DescribeVolumes/AttachVolume family (added for
// #256) is a separate, freestanding EBS API surface that a launch's own
// BlockDeviceMappings never populates.
//
// What IS observable, and is exactly the thing #544 is about, is the actual
// RunInstances request spawn sent — the wire-level BlockDeviceMapping.1.Ebs.
// VolumeSize EC2's query protocol carries, and the one field that determines
// the root volume size on real AWS. runInstancesCaptureProxy sits between the
// spawn binary and the real Substrate server, decodes that field out of every
// RunInstances call, and forwards the request byte-for-byte unmodified —
// Substrate still launches and answers exactly as if the proxy were not
// there. This is strictly stronger than a DescribeVolumes assertion would
// have been: it is the literal value that becomes the EBS CreateVolume
// request on real EC2, not a value Substrate echoes back after the fact.

// runInstancesCaptureProxy records the BlockDeviceMapping.1.Ebs.VolumeSize
// (root volume size) EC2's query-protocol form body carries on every
// RunInstances call, keyed by instance type, then forwards the request
// unmodified to backend. See the file doc comment for why this exists instead
// of a DescribeVolumes assertion.
type runInstancesCaptureProxy struct {
	mu    sync.Mutex
	sizes map[string]int32 // instance type -> Ebs.VolumeSize from BlockDeviceMapping.1
}

// volumeSize returns the captured root volume size for instanceType and
// whether a RunInstances call for that type was seen at all.
func (c *runInstancesCaptureProxy) volumeSize(instanceType string) (int32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.sizes[instanceType]
	return v, ok
}

// startRunInstancesCaptureProxy starts an HTTP proxy in front of backend (a
// running Substrate server's base URL) and returns the capture plus the
// proxy's own URL. Point AWS_ENDPOINT_URL at the returned URL instead of
// backend to observe RunInstances calls without changing Substrate's
// behavior: every request — RunInstances or otherwise — is forwarded
// byte-for-byte and Substrate's response is relayed back unchanged.
func startRunInstancesCaptureProxy(t *testing.T, backend string) (*runInstancesCaptureProxy, string) {
	t.Helper()
	capture := &runInstancesCaptureProxy{sizes: map[string]int32{}}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = r.Body.Close()

		// EC2's query protocol sends the action and every parameter as a
		// form-urlencoded POST body to the root path. RunInstances'
		// BlockDeviceMapping.1.Ebs.VolumeSize is the root device (buildBlockDevices
		// always emits the root mapping first, see pkg/aws/ebs.go).
		if vals, perr := url.ParseQuery(string(body)); perr == nil && vals.Get("Action") == "RunInstances" {
			if sizeStr := vals.Get("BlockDeviceMapping.1.Ebs.VolumeSize"); sizeStr != "" {
				if n, aerr := strconv.ParseInt(sizeStr, 10, 32); aerr == nil {
					capture.mu.Lock()
					capture.sizes[vals.Get("InstanceType")] = int32(n)
					capture.mu.Unlock()
				}
			}
		}

		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, backend+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		outReq.Header = r.Header.Clone()
		outReq.Header.Del("Content-Length") // recomputed by the client from body length
		outReq.ContentLength = int64(len(body))

		resp, err := http.DefaultClient.Do(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return capture, srv.URL
}

// startSpawnSubstrateWithVolumeCapture is startSpawnSubstrate plus a
// runInstancesCaptureProxy spliced in front of it. e.URL (what e.run points
// the spawn binary's AWS_ENDPOINT_URL at) is redirected through the proxy;
// e.AWSConfig / e.EC2Client() etc. — built once, before the redirect — still
// talk to the real Substrate server directly, so tagsByInstanceType() and
// friends are unaffected.
func startSpawnSubstrateWithVolumeCapture(t *testing.T) (*spawnEnv, *runInstancesCaptureProxy) {
	t.Helper()
	env := startSpawnSubstrate(t)
	capture, proxyURL := startRunInstancesCaptureProxy(t, env.URL)
	env.URL = proxyURL
	return env, capture
}

// TestTier0_SweepAppliesCLIVolumeSize is the core #544 case: --volume-size on
// the command line must reach every row's RunInstances call, not just the
// single-instance path.
func TestTier0_SweepAppliesCLIVolumeSize(t *testing.T) {
	env, capture := startSpawnSubstrateWithVolumeCapture(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
  - instance_type: c5.xlarge
`)
	env.launchForegroundSweep(file, "--volume-size", "100")

	for _, itype := range []string{"c5.large", "c5.xlarge"} {
		got, ok := capture.volumeSize(itype)
		if !ok {
			t.Errorf("%s: no RunInstances call observed", itype)
			continue
		}
		if got != 100 {
			t.Errorf("%s: root volume size = %d, want 100 (--volume-size) — pre-fix this launched "+
				"with the hardcoded 20 GiB default regardless of the flag", itype, got)
		}
	}
}

// TestTier0_SweepAppliesParamFileVolumeSize covers the other half of #544:
// volume_size: in the param file's defaults: must reach the instance with no
// --volume-size on the command line at all. Before the fix this key was
// actively REJECTED (#545) rather than silently dropped, so there was no
// route through the param file either.
func TestTier0_SweepAppliesParamFileVolumeSize(t *testing.T) {
	env, capture := startSpawnSubstrateWithVolumeCapture(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
  volume_size: 150
params:
  - instance_type: c5.large
`)
	env.launchForegroundSweep(file)

	got, ok := capture.volumeSize("c5.large")
	if !ok {
		t.Fatal("no RunInstances call observed for c5.large")
	}
	if got != 150 {
		t.Errorf("root volume size = %d, want 150 (volume_size: in defaults:)", got)
	}
}

// TestTier0_SweepRowVolumeSizeBeatsCLI pins the precedence rule established
// for the other sweep-default folds (#525's spend controls, #539's IAM role):
// a row's own volume_size: outranks --volume-size on the command line, the
// same "most specific wins" direction applyCLIVolumeSizeToSweep documents.
func TestTier0_SweepRowVolumeSizeBeatsCLI(t *testing.T) {
	env, capture := startSpawnSubstrateWithVolumeCapture(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
  - instance_type: c5.xlarge
    volume_size: 500
`)
	env.launchForegroundSweep(file, "--volume-size", "100")

	gotDefault, ok := capture.volumeSize("c5.large")
	if !ok {
		t.Fatal("no RunInstances call observed for c5.large")
	}
	if gotDefault != 100 {
		t.Errorf("row without its own volume_size: = %d, want 100 (the CLI flag)", gotDefault)
	}
	gotRow, ok := capture.volumeSize("c5.xlarge")
	if !ok {
		t.Fatal("no RunInstances call observed for c5.xlarge")
	}
	if gotRow != 500 {
		t.Errorf("row with volume_size: 500 = %d, want 500 — the row's own value must win over --volume-size", gotRow)
	}
}

// TestTier0_SweepDefaultsTo20GiBWithoutVolumeSize is the regression guard in
// the other direction: with no --volume-size and no volume_size: anywhere, a
// sweep row must still get spawn's unchanged 20 GiB default (pkg/aws/ebs.go) —
// this fix must not change behavior when nobody asked for an override.
func TestTier0_SweepDefaultsTo20GiBWithoutVolumeSize(t *testing.T) {
	env, capture := startSpawnSubstrateWithVolumeCapture(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
`)
	env.launchForegroundSweep(file)

	got, ok := capture.volumeSize("c5.large")
	if !ok {
		t.Fatal("no RunInstances call observed for c5.large")
	}
	if got != 20 {
		t.Errorf("root volume size = %d, want 20 (spawn's unchanged default)", got)
	}
}
