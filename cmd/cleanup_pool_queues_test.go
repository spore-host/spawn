package cmd

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/poolqueue"
	"github.com/spore-host/spawn/pkg/poolqueue/poolqueuetest"
)

func runPoolQueuesCleanup(t *testing.T, s *poolqueuetest.Scenario, dryRun bool) (string, error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	savedDry, savedYes := cleanupDryRun, cleanupYes
	t.Cleanup(func() { cleanupDryRun, cleanupYes = savedDry, savedYes })
	cleanupDryRun, cleanupYes = dryRun, true
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetErr(&out)
	clients := poolqueue.Clients{
		SQS: func(string) poolqueue.SQSAPI { return s.SQS },
		S3:  func(string) poolqueue.S3API { return s.S3 },
		EC2: s.EC2Clients(),
	}
	err := runCleanupPoolQueues(context.Background(), c, poolqueuetest.Account, []string{poolqueuetest.Region}, clients, s.Now)
	return out.String(), err
}

// `spawn cleanup --pool-queues` deletes exactly what the shared scenario says
// (the reaper's TestPoolQueueSweepAgreesWithTheCLI runs the same one): the
// abandoned queue and the specs of it and of an already drained pool, and
// none of the queues in use (by a worker in a region --region did not
// name), holding messages, inside the grace, untagged or someone else's.
// --dry-run changes nothing and names exactly those.
func TestCleanupPoolQueues(t *testing.T) {
	dry := poolqueuetest.New()
	out, err := runPoolQueuesCleanup(t, dry, true)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(dry.SQS.Deleted) != 0 || len(dry.DeletedSpecRuns()) != 0 {
		t.Errorf("--dry-run deleted queues %v, specs %v", dry.SQS.Deleted, dry.DeletedSpecRuns())
	}
	want := len(poolqueuetest.WantDeletedQueues) + len(poolqueuetest.WantDeletedSpecRuns)
	if !strings.Contains(out, "Dry run: "+strconv.Itoa(want)+" abandoned pool queue(s) or spec set(s) would be deleted") {
		t.Errorf("dry run did not say %d would go:\n%s", want, out)
	}

	s := poolqueuetest.New()
	out, err = runPoolQueuesCleanup(t, s, false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := append([]string(nil), s.SQS.Deleted...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(poolqueuetest.WantDeletedQueues, ",") {
		t.Errorf("deleted queues %v, want %v\n%s", got, poolqueuetest.WantDeletedQueues, out)
	}
	if g := s.DeletedSpecRuns(); strings.Join(g, ",") != strings.Join(poolqueuetest.WantDeletedSpecRuns, ",") {
		t.Errorf("deleted the specs of %v, want %v\n%s", g, poolqueuetest.WantDeletedSpecRuns, out)
	}
	if len(s.S3.BadOwner) != 0 {
		t.Errorf("S3 calls without the account as expected owner: %v", s.S3.BadOwner)
	}
	if _, ok := s.S3.Objects["tasks/some-task/completion.json"]; !ok {
		t.Error("deleted a task record outside the staged specs")
	}
}
