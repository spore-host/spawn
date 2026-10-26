package tasklaunch

import (
	"context"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// ReadFlushStatus reads the tag of the instance the task last ENDED on: a
// newer live re-run has not flushed yet, and an older attempt's tag describes
// a different run (#1295).
func TestReadFlushStatus_ReadsTheLatestEndedInstance(t *testing.T) {
	now := time.Now()
	tag := func(task, v string) map[string]string {
		m := map[string]string{"spawn:task-id": task}
		if v != "" {
			m[taskproto.FlushStatusTag] = v
		}
		return m
	}
	all := []aws.InstanceInfo{
		{InstanceID: "i-old", State: "terminated", LaunchTime: now.Add(-3 * time.Hour), Tags: tag("t1", "failed:timeout")},
		{InstanceID: "i-mid", State: "terminated", LaunchTime: now.Add(-2 * time.Hour), Tags: tag("t1", "ok")},
		{InstanceID: "i-live", State: "running", LaunchTime: now, Tags: tag("t1", "")},
		{InstanceID: "i-other", State: "terminated", LaunchTime: now, Tags: tag("t2", "failed:exit-3")},
	}
	var filter string
	list := func(_ context.Context, region, stateFilter string) ([]aws.InstanceInfo, error) {
		filter = stateFilter
		return all, nil
	}
	fs, err := ReadFlushStatus(context.Background(), list, "us-west-2", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if filter != "all" {
		t.Errorf("listed with filter %q; a terminated instance is visible only with \"all\"", filter)
	}
	if fs.InstanceID != "i-mid" || fs.Value != "ok" || !fs.Available() || !fs.Recorded() {
		t.Errorf("got %+v, want i-mid with ok", fs)
	}
}

func TestFlushStatusOf_NoInstanceIsUnavailable(t *testing.T) {
	fs := FlushStatusOf(nil)
	if fs.Available() || fs.Recorded() {
		t.Errorf("no instance: got %+v, want unavailable", fs)
	}
}
