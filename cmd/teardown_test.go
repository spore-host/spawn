package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	libtags "github.com/spore-host/libs/tags"

	fs "github.com/spore-host/spawn/pkg/footprintspawn"
)

// spawn#1653: `spawn teardown --plan`.

const tdAcct = "123456789012"

// recordingDeleter answers every Describe with the planned resource and
// records the deletes.
type recordingDeleter struct{ deleted []string }

func (d *recordingDeleter) Describe(_ context.Context, r footprint.Resource) (footprint.Resource, error) {
	return footprint.Resource{Tags: r.Tags}, nil
}

func (d *recordingDeleter) Delete(_ context.Context, r footprint.Resource) error {
	d.deleted = append(d.deleted, r.ARN)
	return nil
}

func tdPlan(t *testing.T) *footprint.Report {
	t.Helper()
	rep := footprint.NewReport(fs.Tool, tdAcct, time.Now())
	tags := map[string]string{libtags.ManagedBy: "spawn"}
	sg := footprint.Resource{ARN: "arn:aws:ec2:us-east-1:" + tdAcct + ":security-group/sg-1", Type: fs.TypeSecurityGroup, Region: "us-east-1", Name: "spawn-mpi-x", Tags: tags}
	inst := footprint.Resource{ARN: "arn:aws:ec2:us-east-1:" + tdAcct + ":instance/i-0123456789abcdef0", Type: fs.TypeInstance, Region: "us-east-1",
		Name: "i-0123456789abcdef0", Tags: tags, DependsOn: []string{sg.ARN}}
	for _, r := range []*footprint.Resource{&sg, &inst} {
		fs.Finish(r, fs.Facts{Account: tdAcct})
	}
	rep.Resources = []footprint.Resource{sg, inst}
	// Through the wire form, as a plan file is.
	var buf bytes.Buffer
	if err := writeFootprintJSON(&buf, rep); err != nil {
		t.Fatal(err)
	}
	got, err := readTeardownPlan(&buf, "-")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func noLease(context.Context) (*lifecycle.Lease, error) {
	panic("report mode must not take a lease")
}

// Report is the default and makes no delete call.
func TestTeardownReportModeIsTheDefaultAndDeletesNothing(t *testing.T) {
	if f := teardownCmd.Flags().Lookup("mode"); f == nil || f.DefValue != string(footprint.ModeReport) {
		t.Fatalf("--mode default is not report: %+v", f)
	}
	mode, err := teardownModeOf(string(footprint.ModeReport), false, false)
	if err != nil || mode != footprint.ModeReport {
		t.Fatalf("report mode: %v %v", mode, err)
	}
	d := &recordingDeleter{}
	res, err := executeTeardown(context.Background(), tdPlan(t), tdAcct, mode, d, noLease)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.deleted) != 0 {
		t.Errorf("report mode deleted %v", d.deleted)
	}
	if n := res.Count(footprint.OutcomeWouldDelete); n != 2 {
		t.Errorf("%d would-delete, want 2: %+v", n, res.Actions)
	}
	var out bytes.Buffer
	renderTeardown(&out, res)
	if !strings.Contains(out.String(), "Nothing was deleted") {
		t.Errorf("report output does not say nothing was deleted:\n%s", out.String())
	}
}

// Reap needs --yes, and, until the shared-account lease exists, the hidden
// override.
func TestTeardownReapNeedsYesAndTheNoLeaseOverride(t *testing.T) {
	if _, err := teardownModeOf("reap", false, true); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("reap without --yes: %v", err)
	}
	if _, err := teardownModeOf("reap", true, false); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Errorf("reap without the lease override: %v", err)
	}
	if m, err := teardownModeOf("reap", true, true); err != nil || m != footprint.ModeReap {
		t.Errorf("reap with both: %v %v", m, err)
	}
	if _, err := teardownModeOf("delete", true, true); err == nil {
		t.Error("an unknown mode was accepted")
	}
	f := teardownCmd.Flags().Lookup(noLeaseFlag)
	if f == nil || !f.Hidden {
		t.Errorf("--%s is not a hidden flag: %+v", noLeaseFlag, f)
	}
}

// Reap mode runs under a lease and deletes dependents first.
func TestTeardownReapRunsUnderALeaseInOrder(t *testing.T) {
	d := &recordingDeleter{}
	res, err := executeTeardown(context.Background(), tdPlan(t), tdAcct, footprint.ModeReap, d, localLease)
	if err != nil {
		t.Fatalf("reap: %v (%+v)", err, res)
	}
	if len(d.deleted) != 2 || !strings.Contains(d.deleted[0], ":instance/") || !strings.Contains(d.deleted[1], ":security-group/") {
		t.Errorf("deleted %v: want the instance, then its security group", d.deleted)
	}
}

// A plan is carried out only by its tool, in its account, and reap never in
// the infra account.
func TestTeardownRefusesTheWrongPlan(t *testing.T) {
	d := &recordingDeleter{}
	lag := tdPlan(t)
	lag.Tool = libtags.ToolLagotto
	if _, err := executeTeardown(context.Background(), lag, tdAcct, footprint.ModeReport, d, noLease); err == nil {
		t.Error("ran lagotto's footprint")
	}
	if _, err := executeTeardown(context.Background(), tdPlan(t), "999999999999", footprint.ModeReport, d, noLease); err == nil {
		t.Error("ran a plan in another account")
	}
	infra := tdPlan(t)
	infra.Account = fs.InfraAccountID
	if _, err := executeTeardown(context.Background(), infra, fs.InfraAccountID, footprint.ModeReap, d, localLease); err == nil {
		t.Error("reaped in the infra account")
	}
	if len(d.deleted) != 0 {
		t.Errorf("deleted %v", d.deleted)
	}
}
