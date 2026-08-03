package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/service"
)

// resetServiceFlags puts the package-level flag vars back to their registered
// defaults. They are globals shared by every test in this package, so a test that
// sets one without restoring it changes another test's meaning.
func resetServiceFlags(t *testing.T) {
	t.Helper()
	saved := struct {
		instanceType, ami, ttl, idle, name, upload, remotePath, addrArgs string
		user, key, host, subnet                                          string
		spot, dryRun                                                     bool
		sgs                                                              []string
		boot                                                             time.Duration
		localPort                                                        int
		output                                                           string
		noTimeout                                                        bool
	}{
		serviceInstanceType, serviceAMI, serviceTTL, serviceIdleTimeout, serviceName,
		serviceUpload, serviceRemotePath, serviceAddrArgs,
		serviceUser, serviceKey, serviceExistingHost, serviceSubnetID,
		serviceSpot, serviceDryRun, serviceSecurityGroup,
		serviceBootTimeout, serviceLocalPort, spawnOutputFormat, noTimeout,
	}
	t.Cleanup(func() {
		serviceInstanceType, serviceAMI, serviceTTL, serviceIdleTimeout, serviceName =
			saved.instanceType, saved.ami, saved.ttl, saved.idle, saved.name
		serviceUpload, serviceRemotePath, serviceAddrArgs = saved.upload, saved.remotePath, saved.addrArgs
		serviceUser, serviceKey, serviceExistingHost, serviceSubnetID = saved.user, saved.key, saved.host, saved.subnet
		serviceSpot, serviceDryRun, serviceSecurityGroup = saved.spot, saved.dryRun, saved.sgs
		serviceBootTimeout, serviceLocalPort = saved.boot, saved.localPort
		spawnOutputFormat, noTimeout = saved.output, saved.noTimeout
	})
	serviceInstanceType, serviceAMI, serviceTTL, serviceIdleTimeout, serviceName = "", "", "", "", ""
	serviceUpload, serviceRemotePath = "", ""
	serviceAddrArgs = "--addr 127.0.0.1:0"
	serviceUser, serviceKey, serviceExistingHost, serviceSubnetID = "", "", "", ""
	serviceSpot, serviceDryRun, serviceSecurityGroup = false, false, nil
	serviceBootTimeout, serviceLocalPort = 0, 0
	noTimeout = false
}

func TestServiceLaunchConfigAlwaysTerminates(t *testing.T) {
	resetServiceFlags(t)
	serviceInstanceType = "m7i.large"
	serviceTTL = "2h"

	cfg := serviceLaunchConfig("svc", "us-east-1")

	// A service never "completes" — it serves until its lifetime ends — so the
	// only correct on-complete is terminate. "stop" would leave a billable EBS
	// volume behind and break the everything-dies-eventually invariant.
	if cfg.OnComplete != "terminate" {
		t.Errorf("OnComplete = %q, want terminate", cfg.OnComplete)
	}
	if cfg.InstanceType != "m7i.large" || cfg.Region != "us-east-1" || cfg.TTL != "2h" {
		t.Errorf("flags did not reach the config: %+v", cfg)
	}
	// Provision fills these; presetting them would bypass the AMI/bootstrap logic
	// every other DCV-free path relies on.
	if cfg.UserData != "" {
		t.Error("UserData must be left for launcher.Provision to build")
	}
	// The command must NOT ride in user-data: its stdout would go to a logfile on
	// the box, where the readiness line can never reach the launcher.
	if cfg.JobArrayCommand != "" {
		t.Error("the workload command must not be embedded in the launch config — " +
			"it is started over the SSH session so its stdout streams back")
	}
}

func TestServiceLaunchConfigGetsTheZombieGuard(t *testing.T) {
	resetServiceFlags(t)
	serviceInstanceType = "m7i.large"
	// No TTL and no idle timeout: this is the flag-driven verb's exposure. Unlike
	// the spec-driven task path there is no schema requiring a TTL, so without the
	// guard the instance would run until someone noticed the bill.
	cfg := serviceLaunchConfig("svc", "us-east-1")
	if cfg.TTL != "" || cfg.IdleTimeout != "" {
		t.Fatalf("precondition: expected an unbounded config, got ttl=%q idle=%q", cfg.TTL, cfg.IdleTimeout)
	}
	if err := guardZombieInstance(&cfg); err != nil {
		t.Fatalf("guardZombieInstance: %v", err)
	}
	if cfg.TTL == "" && cfg.IdleTimeout == "" {
		t.Error("the launch config is still unbounded after the guard — a service with no --ttl would run forever")
	}
}

func TestServiceRemoteCommandQuotesAndAppends(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		addrArgs string
		want     string
	}{
		{"simple", []string{"./svc"}, "--addr 127.0.0.1:0", `'./svc' --addr 127.0.0.1:0`},
		{"with args", []string{"./svc", "--verbose"}, "--addr 127.0.0.1:0", `'./svc' '--verbose' --addr 127.0.0.1:0`},
		// A path with a space or a quote must not split into two remote arguments.
		{"space in path", []string{"/opt/my tools/svc"}, "", `'/opt/my tools/svc'`},
		{"quote in path", []string{"/opt/it's/svc"}, "", `'/opt/it'\''s/svc'`},
		// An explicitly empty --addr-args arrives as a space and must append nothing.
		{"empty addr args", []string{"./svc"}, " ", `'./svc'`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceRemoteCommand(tc.args, tc.addrArgs); got != tc.want {
				t.Errorf("serviceRemoteCommand() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServiceCommandDoesNotDoubleTheAddrArgs(t *testing.T) {
	resetServiceFlags(t)
	// pkg/service appends AddrFlagArgs itself. If the command handed to RunRemote
	// already carried them, the service would receive --addr twice — and the second
	// one, being :0 again, would silently defeat an explicit --local-port setup or
	// error out, depending on the flag library the workload uses.
	cmd, err := serviceCommandWithUpload(t.Context(), service.SSHTarget{}, []string{"./svc"})
	if err != nil {
		t.Fatalf("serviceCommandWithUpload: %v", err)
	}
	if strings.Contains(cmd, "--addr") {
		t.Errorf("the command passed to RunRemote must not contain the addr args (RunRemote appends them): %q", cmd)
	}
}

func TestSubstituteUploadedPath(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		local string
		want  []string
	}{
		{
			// The command IS the uploaded binary: it must run the copy on the box,
			// not the local path, which doesn't exist there.
			name: "command is the upload", args: []string{"./svc", "-v"},
			local: "./svc", want: []string{"/tmp/spawn-service-bin", "-v"},
		},
		{
			// The upload is a data file for something already on the box; rewriting
			// the command here would run the data file as the service.
			name: "upload is a data file", args: []string{"/opt/bin/svc", "--db", "data.db"},
			local: "data.db", want: []string{"/opt/bin/svc", "--db", "data.db"},
		},
		{
			// Only argv[0] is the program; a matching later argument is an argument.
			name: "match is not argv0", args: []string{"/opt/bin/svc", "./svc"},
			local: "./svc", want: []string{"/opt/bin/svc", "./svc"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := substituteUploadedPath(tc.args, tc.local, "/tmp/spawn-service-bin")
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("substituteUploadedPath() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSubstituteUploadedPathDoesNotMutateTheCallersArgs(t *testing.T) {
	// args comes straight from cobra; mutating it in place would corrupt anything
	// that reads the original argv afterwards (error messages, the dry-run render).
	args := []string{"./svc", "-v"}
	_ = substituteUploadedPath(args, "./svc", "/tmp/spawn-service-bin")
	if args[0] != "./svc" {
		t.Errorf("the caller's args were mutated: %v", args)
	}
}

func TestSCPArgs(t *testing.T) {
	args := scpArgs(service.SSHTarget{User: "ec2-user", Host: "203.0.113.7", KeyPath: "/k/id"}, "./svc", "/tmp/bin")
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"BatchMode=yes",     // never prompt: this runs unattended inside a launch
		"ControlMaster=no",  // don't join the user's multiplexed session (#56)
		"ControlPath=none",  //
		"-i /k/id",          //
		"ConnectTimeout=10", //
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// Source then destination, in that order, and the destination is remote.
	if args[len(args)-2] != "./svc" {
		t.Errorf("source should be the second-to-last argument, got %q", args[len(args)-2])
	}
	if args[len(args)-1] != "ec2-user@203.0.113.7:/tmp/bin" {
		t.Errorf("destination = %q, want ec2-user@203.0.113.7:/tmp/bin", args[len(args)-1])
	}
	// scp spells the port -P. A lowercase -p would mean "preserve times" and leave
	// the connection on port 22, which fails confusingly against a non-default port.
	withPort := strings.Join(scpArgs(service.SSHTarget{User: "u", Host: "h", Port: 2222}, "a", "b"), " ")
	if !strings.Contains(withPort, "-P 2222") {
		t.Errorf("a non-default port must be passed as -P:\n%s", withPort)
	}
	if strings.Contains(withPort, "-p ") {
		t.Errorf("-p means preserve-times, not port:\n%s", withPort)
	}
	noPort := strings.Join(scpArgs(service.SSHTarget{User: "u", Host: "h"}, "a", "b"), " ")
	if strings.Contains(noPort, "-P") {
		t.Errorf("port 22 should not emit -P:\n%s", noPort)
	}
}

func TestRenderServiceDryRunShowsTheCostBound(t *testing.T) {
	resetServiceFlags(t)
	serviceInstanceType = "m7i.large"
	serviceTTL = "2h"
	serviceUpload = "./svc"

	var out bytes.Buffer
	if err := renderServiceDryRun(&out, []string{"./svc"}, "--addr 127.0.0.1:0"); err != nil {
		t.Fatalf("renderServiceDryRun: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"DRY RUN",
		"m7i.large",
		"ttl=2h",
		"on-complete=terminate",
		"'./svc' --addr 127.0.0.1:0",
		"./svc → /tmp/spawn-service-bin",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run is missing %q:\n%s", want, got)
		}
	}
	// A dry run that doesn't launch must still not launch.
	if strings.Contains(got, "Launching") {
		t.Errorf("dry run claims to launch:\n%s", got)
	}
}

func TestRenderServiceDryRunReportsTheAppliedIdleDefault(t *testing.T) {
	resetServiceFlags(t)
	serviceInstanceType = "m7i.large"
	// No --ttl and no --idle-timeout. The real run gets a 1h idle timeout applied
	// by the guard; a dry run that showed "idle=-" would under-report what bounds
	// the spend, which is the one number this output exists for.
	var out bytes.Buffer
	if err := renderServiceDryRun(&out, []string{"./svc"}, "--addr 127.0.0.1:0"); err != nil {
		t.Fatalf("renderServiceDryRun: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "idle=1h") {
		t.Errorf("dry run did not show the idle timeout a real run would apply:\n%s", got)
	}
	if !strings.Contains(got, "1h idle timeout is applied automatically") {
		t.Errorf("dry run did not explain where the 1h came from:\n%s", got)
	}
}

func TestRenderServiceDryRunForAnExistingHostPromisesNoTeardown(t *testing.T) {
	resetServiceFlags(t)
	serviceExistingHost = "my-box"

	var out bytes.Buffer
	if err := renderServiceDryRun(&out, []string{"./svc"}, "--addr 127.0.0.1:0"); err != nil {
		t.Fatalf("renderServiceDryRun: %v", err)
	}
	got := out.String()
	// Whose instance it is decides whether spawn may terminate it, so the preview
	// has to say so plainly.
	if !strings.Contains(got, "not launched and not terminated") {
		t.Errorf("--host preview must say the instance is left alone:\n%s", got)
	}
	if strings.Contains(got, "Rate:") {
		t.Errorf("no launch means no launch cost to quote:\n%s", got)
	}
}

func TestReportServiceReadyJSONCarriesTheTokenOnlyInTheURL(t *testing.T) {
	resetServiceFlags(t)
	spawnOutputFormat = "json"
	const token = "3f8c1d0b5a2e4761"

	rem := &service.Remote{
		Ready:  &service.ReadyEvent{Event: "ready", Addr: "127.0.0.1:54321", Token: token},
		Tunnel: &service.Tunnel{LocalAddr: "127.0.0.1:51234", RemoteAddr: "127.0.0.1:54321"},
	}
	var out bytes.Buffer
	if err := reportServiceReady(&out, &aws.InstanceInfo{InstanceID: "i-abc", Region: "us-east-1"}, rem); err != nil {
		t.Fatalf("reportServiceReady: %v", err)
	}

	var rec struct {
		InstanceID string `json:"instance_id"`
		LocalURL   string `json:"local_url"`
		LocalAddr  string `json:"local_addr"`
		RemoteAddr string `json:"remote_addr"`
	}
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON (%v):\n%s", err, out.String())
	}
	if rec.InstanceID != "i-abc" || rec.RemoteAddr != "127.0.0.1:54321" {
		t.Errorf("wrong record: %+v", rec)
	}
	// The URL must carry the credential or the caller can't connect...
	if !strings.Contains(rec.LocalURL, token) {
		t.Errorf("local_url does not carry the token: %q", rec.LocalURL)
	}
	// ...and local_addr must not, so there is a loggable form of the endpoint.
	if strings.Contains(rec.LocalAddr, token) {
		t.Errorf("local_addr leaks the token: %q", rec.LocalAddr)
	}
}

func TestReportServiceReadyTextIncludesBothEnds(t *testing.T) {
	resetServiceFlags(t)
	spawnOutputFormat = "table"

	rem := &service.Remote{
		Ready: &service.ReadyEvent{
			Event: "ready", Addr: "127.0.0.1:54321",
			Provenance: json.RawMessage(`{"sourceHash":"deadbeef","commit":"abc123"}`),
		},
		Tunnel: &service.Tunnel{LocalAddr: "127.0.0.1:51234", RemoteAddr: "127.0.0.1:54321"},
	}
	var out bytes.Buffer
	if err := reportServiceReady(&out, &aws.InstanceInfo{InstanceID: "i-abc", Region: "us-east-1"}, rem); err != nil {
		t.Fatalf("reportServiceReady: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"http://127.0.0.1:51234", // the URL the user actually opens
		"i-abc",                  // what to terminate if this goes wrong
		"127.0.0.1:54321",        // what the service chose, for debugging
		"commit=abc123",          // what exactly got spawned
		"sourceHash=deadbeef",
		"terminate the instance", // spawn launched it, so spawn reaps it
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ready output is missing %q:\n%s", want, got)
		}
	}
}

func TestReportServiceReadyOnAnExistingHostDoesNotPromiseTermination(t *testing.T) {
	resetServiceFlags(t)
	spawnOutputFormat = "table"
	serviceExistingHost = "my-box"

	rem := &service.Remote{
		Ready:  &service.ReadyEvent{Event: "ready", Addr: "127.0.0.1:54321"},
		Tunnel: &service.Tunnel{LocalAddr: "127.0.0.1:51234"},
	}
	var out bytes.Buffer
	if err := reportServiceReady(&out, &aws.InstanceInfo{InstanceID: "i-abc", Region: "us-east-1"}, rem); err != nil {
		t.Fatalf("reportServiceReady: %v", err)
	}
	// Telling someone Ctrl-C will terminate an instance spawn didn't launch — and
	// won't touch — is the kind of wrong promise that stops people using Ctrl-C.
	if strings.Contains(out.String(), "terminate the instance") {
		t.Errorf("must not promise to terminate a --host instance:\n%s", out.String())
	}
}

func TestServiceProvenance(t *testing.T) {
	tests := []struct {
		name string
		prov string
		want string
	}{
		{"sorted", `{"commit":"abc","sourceHash":"def"}`, "commit=abc sourceHash=def"},
		// Provenance is the workload's shape; unknown fields are shown, not rejected.
		{"unknown fields", `{"builder":"me","zebra":1}`, "builder=me zebra=1"},
		// Malformed or absent provenance is not a reason to fail a working service.
		{"malformed", `"not-an-object"`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := &service.ReadyEvent{}
			if tc.prov != "" {
				ev.Provenance = json.RawMessage(tc.prov)
			}
			if got := serviceProvenance(ev); got != tc.want {
				t.Errorf("serviceProvenance() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServiceEffectiveDefaults(t *testing.T) {
	resetServiceFlags(t)
	if got := serviceEffectiveBootTimeout(); got != service.DefaultBootTimeout {
		t.Errorf("boot timeout = %v, want the package default %v", got, service.DefaultBootTimeout)
	}
	serviceBootTimeout = 9 * time.Minute
	if got := serviceEffectiveBootTimeout(); got != 9*time.Minute {
		t.Errorf("boot timeout = %v, want the flag value", got)
	}
	if got := serviceEffectiveRemotePath(); got != "/tmp/spawn-service-bin" {
		t.Errorf("remote path = %q", got)
	}
	if got := serviceEffectiveName(); got != "spawn-service" {
		t.Errorf("name = %q", got)
	}
	if got := localPortLabel(); got != "<free port>" {
		t.Errorf("local port label = %q, want a placeholder when unset", got)
	}
	serviceLocalPort = 51234
	if got := localPortLabel(); got != "51234" {
		t.Errorf("local port label = %q", got)
	}
}

func TestServiceAddrArgsFor(t *testing.T) {
	resetServiceFlags(t)
	// Unchanged: the registered default.
	if got := serviceAddrArgsFor(serviceCmd); got != "--addr 127.0.0.1:0" {
		t.Errorf("default addr args = %q", got)
	}

	// Explicitly emptied: must mean "append nothing", for a service that takes its
	// address from a config file or a different flag spelling. It cannot be passed
	// through as "" — pkg/service reads that as "use the default" and would append
	// --addr anyway.
	if err := serviceCmd.Flags().Set("addr-args", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = serviceCmd.Flags().Set("addr-args", "--addr 127.0.0.1:0")
		serviceCmd.Flags().Lookup("addr-args").Changed = false
	})
	got := serviceAddrArgsFor(serviceCmd)
	if got == "" {
		t.Error(`an explicitly empty --addr-args must not arrive as "" (pkg/service would substitute the default)`)
	}
	if strings.TrimSpace(got) != "" {
		t.Errorf("an explicitly empty --addr-args should append nothing, got %q", got)
	}
}

func TestHoldServiceDistinguishesALifetimeEndFromADroppedSession(t *testing.T) {
	// These two look identical from here — the session simply ends — but they mean
	// opposite things: one is the designed end of a bounded service, the other is a
	// failure with a live, still-billing instance behind it. Reporting both the same
	// way either cries wolf on every TTL expiry or hides the case worth acting on.
	tests := []struct {
		name        string
		gone        bool
		wantErr     bool
		wantMessage string
	}{
		{"lifetime ended", true, false, "lifetime ended"},
		{"session dropped while the instance lives", false, true, "SSH session to the service ended"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			close(done)
			var out bytes.Buffer
			err := holdService(t.Context(), &out, done, func() bool { return tc.gone })
			if tc.wantErr && err == nil {
				t.Error("a dropped session with a live instance must be an error, not a clean exit")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("a completed lifetime is the designed end, not an error: %v", err)
			}
			if !strings.Contains(out.String(), tc.wantMessage) {
				t.Errorf("output %q does not explain the exit (want %q)", out.String(), tc.wantMessage)
			}
		})
	}
}

func TestHoldServiceReturnsOnContextCancel(t *testing.T) {
	// The hold must be cancellable, or a parent's shutdown can't reach the teardown
	// that terminates the instance.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	if err := holdService(ctx, &out, make(chan struct{}), func() bool { return false }); err == nil {
		t.Error("a cancelled context should end the hold with its error")
	}
}

func TestInstanceStateIsGone(t *testing.T) {
	// A stopped instance no longer serves, even though it still exists — so it
	// counts as gone for the purpose of explaining why the session ended.
	for state, wantGone := range map[string]bool{
		"running":       false,
		"pending":       false,
		"stopping":      true,
		"stopped":       true,
		"shutting-down": true,
		"terminated":    true,
	} {
		if got := instanceStateIsGone(state); got != wantGone {
			t.Errorf("instanceStateIsGone(%q) = %v, want %v", state, got, wantGone)
		}
	}
}

func TestServiceCommandIsRegisteredAndNotDestructive(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"service"})
	if err != nil || cmd == nil || cmd.Name() != "service" {
		t.Fatalf("spawn service is not registered: %v", err)
	}
	// It must take the command to run, so an argument-less invocation is an error
	// rather than a launch of nothing that then times out costing money.
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("spawn service with no arguments should be rejected")
	}
	// Structured output comes from the root -o/--output; a local --json would
	// fragment the surface (spawn#40, enforced by TestFlagConventions).
	if f := cmd.Flags().Lookup("json"); f != nil {
		t.Error("must not define a local --json flag; use the root -o/--output")
	}
}
