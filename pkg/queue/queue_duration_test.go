package queue

import "testing"

// #990: job and global timeouts use the shared parser, which the on-instance
// queue runner also uses. global_timeout becomes the instance TTL on a batch
// queue launch, so it must also be positive.
func TestValidateQueueDurations(t *testing.T) {
	cfg := func(job, global string) *QueueConfig {
		return &QueueConfig{QueueID: "q", GlobalTimeout: global, Jobs: []JobConfig{{JobID: "a", Command: "true", Timeout: job}}}
	}
	for _, c := range [][2]string{{"1d", "2d"}, {"30m", ""}, {"1h", "2d12h"}} {
		if err := ValidateQueue(cfg(c[0], c[1])); err != nil {
			t.Errorf("timeout %q, global %q: %v", c[0], c[1], err)
		}
	}
	for _, c := range [][2]string{{"1w", ""}, {"1h", "1w"}, {"1h", "0"}} {
		if err := ValidateQueue(cfg(c[0], c[1])); err == nil {
			t.Errorf("timeout %q, global %q: accepted", c[0], c[1])
		}
	}
}
