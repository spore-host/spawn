package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// resolveInstance finds an instance by ID or name
func resolveInstance(ctx context.Context, client *aws.Client, identifier string) (*aws.InstanceInfo, error) {
	fmt.Fprintf(os.Stderr, "Looking up instance %s...\n", identifier)

	instances, err := client.ListInstances(ctx, "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	// Check if identifier is an instance ID (starts with "i-")
	isInstanceID := strings.HasPrefix(identifier, "i-")

	var matches []aws.InstanceInfo
	for _, inst := range instances {
		if isInstanceID {
			// Exact match on instance ID
			if inst.InstanceID == identifier {
				return &inst, nil
			}
		} else {
			// Match on name (case-insensitive)
			if strings.EqualFold(inst.Name, identifier) {
				matches = append(matches, inst)
			}
		}
	}

	if isInstanceID {
		return nil, fmt.Errorf("instance %s not found (must be spawn-managed)", identifier)
	}

	// Handle name matches
	if len(matches) == 0 {
		return nil, fmt.Errorf("no instance found with name: %s", identifier)
	}

	if len(matches) == 1 {
		return &matches[0], nil
	}

	// Multiple matches — prefer running over stopped/terminated (fixes #313).
	// When a cluster is re-launched, old stopped instances share names with
	// the new running ones. Connect/status should target the running instance.
	var running []aws.InstanceInfo
	for _, inst := range matches {
		if inst.State == "running" {
			running = append(running, inst)
		}
	}
	if len(running) == 1 {
		return &running[0], nil
	}

	// Still ambiguous — show only running instances if any, else all
	candidates := running
	if len(candidates) == 0 {
		candidates = matches
	}
	fmt.Fprintf(os.Stderr, "\nMultiple instances found with name '%s':\n\n", identifier)
	for _, inst := range candidates {
		fmt.Fprintf(os.Stderr, "  %s (%s in %s, state: %s)\n",
			inst.InstanceID, inst.InstanceType, inst.Region, inst.State)
	}
	fmt.Fprintf(os.Stderr, "\nPlease use the specific instance ID instead.\n")

	return nil, fmt.Errorf("multiple instances found with name: %s", identifier)
}
