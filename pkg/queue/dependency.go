package queue

import "fmt"

// TopologicalSort resolves job dependencies and returns execution order using Kahn's algorithm
func TopologicalSort(jobs []JobConfig) ([]string, error) {
	// Build dependency graph and in-degree map
	graph := make(map[string][]string)
	inDegree := make(map[string]int)

	// Initialize all jobs with in-degree 0
	for _, job := range jobs {
		inDegree[job.JobID] = 0
		graph[job.JobID] = []string{}
	}

	// Build graph: for each job, track which jobs depend on it
	for _, job := range jobs {
		inDegree[job.JobID] = len(job.DependsOn)
		for _, dep := range job.DependsOn {
			graph[dep] = append(graph[dep], job.JobID)
		}
	}

	// Find all jobs with no dependencies (in-degree = 0)
	queue := []string{}
	for jobID, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, jobID)
		}
	}

	// Process jobs in topological order
	result := []string{}
	for len(queue) > 0 {
		// Dequeue
		current := queue[0]
		queue = queue[1:]
		result = append(result, current)

		// For each job that depends on current job
		for _, dependent := range graph[current] {
			// Reduce in-degree
			inDegree[dependent]--
			// If all dependencies satisfied, add to queue
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	// Check if all jobs were processed (no cycles)
	if len(result) != len(jobs) {
		return nil, fmt.Errorf("circular dependency detected: %d jobs processed out of %d",
			len(result), len(jobs))
	}

	return result, nil
}

// GetJobConfig returns the JobConfig for a given job ID
func GetJobConfig(jobs []JobConfig, jobID string) *JobConfig {
	for i := range jobs {
		if jobs[i].JobID == jobID {
			return &jobs[i]
		}
	}
	return nil
}
