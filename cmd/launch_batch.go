package cmd

import (
	"sync"

	"github.com/spore-host/spawn/pkg/aws"
)

// launchResult carries the outcome of one instance launch within a parallel
// batch: its position in the batch, the launch result (nil on error), and the
// error (nil on success). Shared by the parameter-sweep and job-array launch
// paths, which fan out identically and differ only in how they post-process
// the collected results.
type launchResult struct {
	index  int
	result *aws.LaunchResult
	err    error
}

// runLaunchBatch launches n instances concurrently, invoking launch(index) in a
// goroutine per item, and returns the results ordered by index. Each caller
// decides how to interpret partial failure (sweeps treat parameter sets as
// independent and keep successes; a job array is a unit and cleans up on any
// failure) — this helper only owns the fan-out + collect that both share.
func runLaunchBatch(n int, launch func(index int) (*aws.LaunchResult, error)) []launchResult {
	resultsChan := make(chan launchResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			result, err := launch(index)
			resultsChan <- launchResult{index: index, result: result, err: err}
		}(i)
	}
	wg.Wait()
	close(resultsChan)

	ordered := make([]launchResult, n)
	for r := range resultsChan {
		ordered[r.index] = r
	}
	return ordered
}
