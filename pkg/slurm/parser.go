package slurm

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SlurmJob represents a parsed Slurm batch script
type SlurmJob struct {
	// Script metadata
	FilePath    string
	ScriptLines []string

	// SBATCH directives
	JobName      string
	Partition    string
	TimeLimit    time.Duration
	MemoryMB     int
	CPUsPerTask  int
	Nodes        int
	TasksPerNode int
	GPUs         int
	GPUType      string
	Array        *ArraySpec
	Output       string
	Error        string
	MailType     string
	MailUser     string
	WorkingDir   string
	ExcludeNodes []string
	Constraint   string
	QOS          string
	Account      string

	// Script body (non-SBATCH lines)
	ScriptBody string

	// Spawn overrides (custom #SPAWN directives)
	SpawnInstanceType string
	SpawnRegion       string
	SpawnSpot         bool
	SpawnAMI          string
}

// ArraySpec represents a Slurm array job specification
type ArraySpec struct {
	Start      int
	End        int
	Step       int
	MaxRunning int // %N suffix
}

// ParseSlurmScript parses a Slurm batch script file
func ParseSlurmScript(filePath string) (*SlurmJob, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	job := &SlurmJob{
		FilePath:     filePath,
		ScriptLines:  []string{},
		Nodes:        1, // Default to single node
		TasksPerNode: 1, // Default to single task
	}

	scanner := bufio.NewScanner(file)
	var scriptBody strings.Builder
	inShebang := true

	for scanner.Scan() {
		line := scanner.Text()
		job.ScriptLines = append(job.ScriptLines, line)

		trimmed := strings.TrimSpace(line)

		// Skip shebang
		if inShebang && strings.HasPrefix(trimmed, "#!") {
			inShebang = false
			continue
		}

		// Parse #SBATCH directives
		if strings.HasPrefix(trimmed, "#SBATCH") {
			if err := parseSBATCHLine(job, trimmed); err != nil {
				return nil, fmt.Errorf("line %d: %w", len(job.ScriptLines), err)
			}
			continue
		}

		// Parse #SPAWN directives (custom extension)
		if strings.HasPrefix(trimmed, "#SPAWN") {
			if err := parseSPAWNLine(job, trimmed); err != nil {
				return nil, fmt.Errorf("line %d: %w", len(job.ScriptLines), err)
			}
			continue
		}

		// Skip other comments
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Accumulate script body
		if trimmed != "" {
			scriptBody.WriteString(line)
			scriptBody.WriteString("\n")
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	job.ScriptBody = scriptBody.String()

	return job, nil
}

// parseSBATCHLine parses a single #SBATCH directive line
func parseSBATCHLine(job *SlurmJob, line string) error {
	// Remove #SBATCH prefix
	line = strings.TrimPrefix(line, "#SBATCH")
	line = strings.TrimSpace(line)

	// Parse --key=value or --key value format
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}

	for i := 0; i < len(parts); i++ {
		arg := parts[i]

		// Handle --key=value
		if strings.Contains(arg, "=") {
			kv := strings.SplitN(arg, "=", 2)
			key := strings.TrimPrefix(kv[0], "--")
			value := kv[1]

			if err := setSBATCHField(job, key, value); err != nil {
				return err
			}
			continue
		}

		// Handle --key value
		if strings.HasPrefix(arg, "--") {
			key := strings.TrimPrefix(arg, "--")

			// Boolean flags (no value)
			if isBooleanFlag(key) {
				if err := setSBATCHField(job, key, "true"); err != nil {
					return err
				}
				continue
			}

			// Flags with values
			if i+1 < len(parts) {
				value := parts[i+1]
				if err := setSBATCHField(job, key, value); err != nil {
					return err
				}
				i++ // Skip next part (value)
			}
		}
	}

	return nil
}

// setSBATCHField sets a job field from a parsed SBATCH directive
func setSBATCHField(job *SlurmJob, key, value string) error {
	switch key {
	case "job-name", "J":
		job.JobName = value
	case "partition", "p":
		job.Partition = value
	case "time", "t":
		duration, err := parseTimeLimit(value)
		if err != nil {
			return fmt.Errorf("invalid time limit %q: %w", value, err)
		}
		job.TimeLimit = duration
	case "mem":
		memMB, err := parseMemory(value)
		if err != nil {
			return fmt.Errorf("invalid memory %q: %w", value, err)
		}
		job.MemoryMB = memMB
	case "cpus-per-task", "c":
		cpus, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid cpus-per-task %q: %w", value, err)
		}
		job.CPUsPerTask = cpus
	case "nodes", "N":
		nodes, err := parseNodes(value)
		if err != nil {
			return fmt.Errorf("invalid nodes %q: %w", value, err)
		}
		job.Nodes = nodes
	case "ntasks-per-node":
		tasks, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid ntasks-per-node %q: %w", value, err)
		}
		job.TasksPerNode = tasks
	case "gres":
		gpus, gpuType, err := parseGRES(value)
		if err != nil {
			return fmt.Errorf("invalid gres %q: %w", value, err)
		}
		job.GPUs = gpus
		job.GPUType = gpuType
	case "array", "a":
		arraySpec, err := parseArray(value)
		if err != nil {
			return fmt.Errorf("invalid array %q: %w", value, err)
		}
		job.Array = arraySpec
	case "output", "o":
		job.Output = value
	case "error", "e":
		job.Error = value
	case "mail-type":
		job.MailType = value
	case "mail-user":
		job.MailUser = value
	case "chdir", "D":
		job.WorkingDir = value
	case "exclude":
		job.ExcludeNodes = strings.Split(value, ",")
	case "constraint", "C":
		job.Constraint = value
	case "qos":
		job.QOS = value
	case "account", "A":
		job.Account = value
	}

	return nil
}

// parseSPAWNLine parses a custom #SPAWN directive
func parseSPAWNLine(job *SlurmJob, line string) error {
	// Remove #SPAWN prefix
	line = strings.TrimPrefix(line, "#SPAWN")
	line = strings.TrimSpace(line)

	// Parse --key=value format
	if !strings.Contains(line, "=") {
		return nil
	}

	kv := strings.SplitN(line, "=", 2)
	key := strings.TrimPrefix(kv[0], "--")
	key = strings.TrimSpace(key)
	value := strings.TrimSpace(kv[1])

	switch key {
	case "instance-type":
		job.SpawnInstanceType = value
	case "region":
		job.SpawnRegion = value
	case "spot":
		job.SpawnSpot = value == "true" || value == "yes"
	case "ami":
		job.SpawnAMI = value
	}

	return nil
}

// parseTimeLimit parses Slurm time format: HH:MM:SS, MM:SS, DD-HH:MM:SS
func parseTimeLimit(s string) (time.Duration, error) {
	// Handle days format: DD-HH:MM:SS
	if strings.Contains(s, "-") {
		parts := strings.Split(s, "-")
		if len(parts) != 2 {
			return 0, fmt.Errorf("invalid time format with days: %s", s)
		}
		days, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		timeStr := parts[1]
		duration, err := parseHMS(timeStr)
		if err != nil {
			return 0, err
		}
		return time.Duration(days)*24*time.Hour + duration, nil
	}

	return parseHMS(s)
}

// parseHMS parses HH:MM:SS or MM:SS format
func parseHMS(s string) (time.Duration, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("invalid time format: %s", s)
	}

	var hours, minutes, seconds int
	var err error

	if len(parts) == 3 {
		// HH:MM:SS
		hours, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		minutes, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, err
		}
		seconds, err = strconv.Atoi(parts[2])
		if err != nil {
			return 0, err
		}
	} else {
		// MM:SS
		minutes, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		seconds, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, err
		}
	}

	return time.Duration(hours)*time.Hour +
		time.Duration(minutes)*time.Minute +
		time.Duration(seconds)*time.Second, nil
}

// parseMemory parses memory specification: 16GB, 32000MB, 1024
func parseMemory(s string) (int, error) {
	s = strings.ToUpper(s)

	// Extract numeric part and unit
	re := regexp.MustCompile(`^(\d+)([KMGT]?B?)$`)
	matches := re.FindStringSubmatch(s)
	if matches == nil {
		return 0, fmt.Errorf("invalid memory format: %s", s)
	}

	num, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, err
	}

	unit := matches[2]
	switch unit {
	case "", "M", "MB":
		return num, nil
	case "G", "GB":
		return num * 1024, nil
	case "K", "KB":
		return num / 1024, nil
	case "T", "TB":
		return num * 1024 * 1024, nil
	default:
		return 0, fmt.Errorf("unknown memory unit: %s", unit)
	}
}

// parseNodes parses node specification: 4, 2-4, 1-10
func parseNodes(s string) (int, error) {
	// Handle range: 2-4 (take min for now)
	if strings.Contains(s, "-") {
		parts := strings.Split(s, "-")
		if len(parts) != 2 {
			return 0, fmt.Errorf("invalid node range: %s", s)
		}
		min, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		return min, nil
	}

	return strconv.Atoi(s)
}

// parseGRES parses GRES specification: gpu:1, gpu:v100:2, gpu:a100:4
func parseGRES(s string) (int, string, error) {
	// Format: gpu:1 or gpu:type:count
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return 0, "", fmt.Errorf("invalid gres format: %s", s)
	}

	if parts[0] != "gpu" {
		// Only support GPU for now
		return 0, "", nil
	}

	if len(parts) == 2 {
		// gpu:1
		count, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, "", err
		}
		return count, "", nil
	}

	// gpu:type:count
	gpuType := parts[1]
	count, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, "", err
	}

	return count, gpuType, nil
}

// parseArray parses array specification: 1-100, 1-100:2, 1-100%10
func parseArray(s string) (*ArraySpec, error) {
	spec := &ArraySpec{Step: 1}

	// Handle max running suffix: 1-100%10
	if strings.Contains(s, "%") {
		parts := strings.Split(s, "%")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid array format: %s", s)
		}
		s = parts[0]
		maxRunning, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}
		spec.MaxRunning = maxRunning
	}

	// Handle step suffix: 1-100:2
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid array format: %s", s)
		}
		s = parts[0]
		step, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}
		spec.Step = step
	}

	// Parse range: 1-100
	if !strings.Contains(s, "-") {
		return nil, fmt.Errorf("invalid array format (missing range): %s", s)
	}

	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid array format: %s", s)
	}

	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, err
	}
	spec.Start = start

	end, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, err
	}
	spec.End = end

	return spec, nil
}

// isBooleanFlag returns true if the flag is a boolean flag (no value)
func isBooleanFlag(key string) bool {
	boolFlags := map[string]bool{
		"wait":       true,
		"hold":       true,
		"requeue":    true,
		"no-requeue": true,
		"exclusive":  true,
	}
	return boolFlags[key]
}

// GetTotalTasks returns the total number of tasks in the job
func (j *SlurmJob) GetTotalTasks() int {
	if j.Array != nil {
		count := 0
		for i := j.Array.Start; i <= j.Array.End; i += j.Array.Step {
			count++
		}
		return count
	}
	return j.Nodes * j.TasksPerNode
}

// IsMPIJob returns true if this is a multi-node MPI job
func (j *SlurmJob) IsMPIJob() bool {
	return j.Nodes > 1
}

// IsArrayJob returns true if this is an array job
func (j *SlurmJob) IsArrayJob() bool {
	return j.Array != nil
}

// IsGPUJob returns true if this job requires GPUs
func (j *SlurmJob) IsGPUJob() bool {
	return j.GPUs > 0
}
