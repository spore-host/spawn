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

// DependenciesMet checks if all dependencies for a job have been completed
func DependenciesMet(job *JobConfig, completedJobs map[string]bool) bool {
	for _, dep := range job.DependsOn {
		if !completedJobs[dep] {
			return false
		}
	}
	return true
}

// GetReadyJobs returns jobs that have all dependencies satisfied
func GetReadyJobs(jobs []JobConfig, completedJobs map[string]bool, runningJobs map[string]bool) []string {
	ready := []string{}
	for _, job := range jobs {
		// Skip if already completed or running
		if completedJobs[job.JobID] || runningJobs[job.JobID] {
			continue
		}
		// Check if all dependencies are met
		if DependenciesMet(&job, completedJobs) {
			ready = append(ready, job.JobID)
		}
	}
	return ready
}
