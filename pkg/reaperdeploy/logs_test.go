package reaperdeploy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func deployWithLogs(t *testing.T, logs *fakeLogs, retention int32) (*Result, error) {
	t.Helper()
	d := newTestDeployer(&fakeIAM{}, &fakeLambda{}, &fakeEvents{}, &fakeS3{})
	d.Logs = logs
	return d.Deploy(context.Background(), Options{
		AccountID: "1", Region: "us-east-1", Version: "0.136.0", Artifact: "/tmp/x.zip",
		LogRetentionDays: retention,
	})
}

// #1156/#827: a fresh reaper has never been invoked, so its log group does not
// exist yet and a retention call alone cannot act on it. Deploy must create the
// group and then set retention, so the reaper's logs are never kept forever.
func TestDeployCreatesTheLogGroupWithRetention(t *testing.T) {
	logs := &fakeLogs{} // no group: the never-invoked case
	res, err := deployWithLogs(t, logs, 0)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !logs.createdGroup {
		t.Error("Deploy did not create the log group")
	}
	if logs.retention != DefaultLogRetentionDays {
		t.Errorf("retention = %d, want the %d-day default", logs.retention, DefaultLogRetentionDays)
	}
	if !strings.Contains(strings.Join(res.Actions, "\n"), "retention 30 days") {
		t.Errorf("deploy report should state the retention: %v", res.Actions)
	}
}

// The idle window assumes the default retention; keep them the same number.
func TestIdleWindowMatchesTheDefaultRetention(t *testing.T) {
	if idleWindow.Hours()/24 != DefaultLogRetentionDays {
		t.Errorf("idle window is %v days but deploy sets %d days of retention", idleWindow.Hours()/24, DefaultLogRetentionDays)
	}
}

func TestDeployOnAnExistingLogGroupStillSetsRetention(t *testing.T) {
	logs := &fakeLogs{groupExists: true}
	if _, err := deployWithLogs(t, logs, 90); err != nil {
		t.Fatalf("an existing group is not an error: %v", err)
	}
	if logs.retention != 90 {
		t.Errorf("retention = %d, want 90", logs.retention)
	}
}

// Only "already exists" is tolerated. Any other failure stops the deploy,
// rather than leaving a reaper whose logs are kept forever.
func TestDeployFailsWhenTheLogGroupCannotBeCreated(t *testing.T) {
	logs := &fakeLogs{createErr: errors.New("AccessDeniedException: not authorized to perform logs:CreateLogGroup")}
	if _, err := deployWithLogs(t, logs, 0); err == nil || !strings.Contains(err.Error(), "log group") {
		t.Errorf("want a log-group error, got %v", err)
	}
	d := newTestDeployer(&fakeIAM{}, &fakeLambda{}, &fakeEvents{}, &fakeS3{})
	d.Logs = nil
	if _, err := d.Deploy(context.Background(), Options{AccountID: "1", Region: "us-east-1", Version: "0.136.0", Artifact: "/tmp/x.zip"}); err == nil {
		t.Error("a Deployer with no Logs client must refuse to deploy rather than skip retention")
	}
}

// Teardown removes the log group, so nothing is left behind (#1156).
func TestTeardownDeletesTheLogGroup(t *testing.T) {
	logs := &fakeLogs{groupExists: true}
	d := newTestDeployer(&fakeIAM{roleExists: true}, &fakeLambda{exists: true}, &fakeEvents{}, &fakeS3{bucketExists: true})
	d.Logs = logs
	removed, err := d.Teardown(context.Background(), TeardownOptions{})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !logs.deleted {
		t.Error("teardown left the log group behind")
	}
	if !strings.Contains(strings.Join(removed, "\n"), "log group "+LogGroupName()) {
		t.Errorf("teardown should report removing the log group: %v", removed)
	}

	// A second teardown finds nothing and claims nothing.
	removed, err = d.Teardown(context.Background(), TeardownOptions{})
	if err != nil {
		t.Fatalf("second Teardown: %v", err)
	}
	if strings.Contains(strings.Join(removed, "\n"), "log group") {
		t.Errorf("an absent log group must not be reported removed: %v", removed)
	}
}

func TestTeardownKeepLogs(t *testing.T) {
	logs := &fakeLogs{groupExists: true}
	d := newTestDeployer(&fakeIAM{roleExists: true}, &fakeLambda{exists: true}, &fakeEvents{}, &fakeS3{bucketExists: true})
	d.Logs = logs
	removed, _ := d.Teardown(context.Background(), TeardownOptions{KeepLogs: true})
	if logs.deleted {
		t.Error("--keep-logs deleted the log group")
	}
	if !strings.Contains(strings.Join(removed, "\n"), "kept log group") {
		t.Errorf("teardown should say it kept the log group: %v", removed)
	}
}
