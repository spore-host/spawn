package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/autoscaler"
)

func runAutoscaleAddSchedule(cmd *cobra.Command, args []string) error {
	groupName := args[0]

	// Validate required flags
	if autoscaleScheduleName == "" {
		return fmt.Errorf("--name required")
	}
	if autoscaleScheduleExpression == "" {
		return fmt.Errorf("--schedule required")
	}
	if autoscaleScheduleDesired <= 0 {
		return fmt.Errorf("--desired-capacity must be > 0")
	}

	ctx := context.Background()
	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// Load group
	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	// Validate schedule expression
	evaluator := autoscaler.NewScheduleEvaluator()
	if err := evaluator.ValidateSchedule(autoscaleScheduleExpression); err != nil {
		return fmt.Errorf("invalid schedule expression: %w", err)
	}

	// Initialize schedule config if needed
	if group.ScheduleConfig == nil {
		group.ScheduleConfig = &autoscaler.ScheduleConfig{
			Actions: []autoscaler.ScheduledAction{},
		}
	}

	// Check if schedule with same name exists
	for i, action := range group.ScheduleConfig.Actions {
		if action.Name == autoscaleScheduleName {
			// Update existing schedule
			group.ScheduleConfig.Actions[i] = autoscaler.ScheduledAction{
				Name:            autoscaleScheduleName,
				Schedule:        autoscaleScheduleExpression,
				DesiredCapacity: autoscaleScheduleDesired,
				MinCapacity:     autoscaleScheduleMin,
				MaxCapacity:     autoscaleScheduleMax,
				Timezone:        autoscaleScheduleTimezone,
				Enabled:         autoscaleScheduleEnabled,
			}
			if err := as.UpdateGroup(ctx, group); err != nil {
				return fmt.Errorf("update group: %w", err)
			}
			fmt.Printf("Updated schedule %q for group %s\n", autoscaleScheduleName, groupName)

			// Show next trigger time
			nextTime, _ := evaluator.GetNextTriggerTime(autoscaleScheduleExpression, autoscaleScheduleTimezone)
			if !nextTime.IsZero() {
				fmt.Printf("Next trigger: %s (%s)\n", nextTime.Format(time.RFC3339), time.Until(nextTime).Round(time.Second))
			}
			return nil
		}
	}

	// Add new schedule
	group.ScheduleConfig.Actions = append(group.ScheduleConfig.Actions, autoscaler.ScheduledAction{
		Name:            autoscaleScheduleName,
		Schedule:        autoscaleScheduleExpression,
		DesiredCapacity: autoscaleScheduleDesired,
		MinCapacity:     autoscaleScheduleMin,
		MaxCapacity:     autoscaleScheduleMax,
		Timezone:        autoscaleScheduleTimezone,
		Enabled:         autoscaleScheduleEnabled,
	})

	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Added schedule %q to group %s\n", autoscaleScheduleName, groupName)

	// Show next trigger time
	nextTime, _ := evaluator.GetNextTriggerTime(autoscaleScheduleExpression, autoscaleScheduleTimezone)
	if !nextTime.IsZero() {
		fmt.Printf("Next trigger: %s (%s)\n", nextTime.Format(time.RFC3339), time.Until(nextTime).Round(time.Second))
	}

	return nil
}

func runAutoscaleRemoveSchedule(cmd *cobra.Command, args []string) error {
	groupName := args[0]
	scheduleName := args[1]

	ctx := context.Background()
	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// Load group
	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	if group.ScheduleConfig == nil || len(group.ScheduleConfig.Actions) == 0 {
		return fmt.Errorf("no schedules configured for group %s", groupName)
	}

	// Find and remove schedule
	found := false
	newActions := make([]autoscaler.ScheduledAction, 0)
	for _, action := range group.ScheduleConfig.Actions {
		if action.Name == scheduleName {
			found = true
		} else {
			newActions = append(newActions, action)
		}
	}

	if !found {
		return fmt.Errorf("schedule %q not found in group %s", scheduleName, groupName)
	}

	if !confirmYes(autoscaleRemoveScheduleYes, fmt.Sprintf("Remove schedule %q from group %s?", scheduleName, groupName)) {
		return fmt.Errorf("aborted")
	}

	group.ScheduleConfig.Actions = newActions
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Removed schedule %q from group %s\n", scheduleName, groupName)
	return nil
}

func runAutoscaleListSchedules(cmd *cobra.Command, args []string) error {
	groupName := args[0]

	ctx := context.Background()
	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// Load group
	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	if group.ScheduleConfig == nil || len(group.ScheduleConfig.Actions) == 0 {
		fmt.Printf("No scheduled actions for group %s\n", groupName)
		return nil
	}

	evaluator := autoscaler.NewScheduleEvaluator()

	fmt.Printf("Scheduled Actions for %s:\n\n", groupName)
	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "NAME\tSCHEDULE\tDESIRED\tMIN\tMAX\tTIMEZONE\tENABLED\tNEXT TRIGGER")

	for _, action := range group.ScheduleConfig.Actions {
		status := "yes"
		if !action.Enabled {
			status = "no"
		}

		minStr := "-"
		if action.MinCapacity > 0 {
			minStr = fmt.Sprintf("%d", action.MinCapacity)
		}

		maxStr := "-"
		if action.MaxCapacity > 0 {
			maxStr = fmt.Sprintf("%d", action.MaxCapacity)
		}

		tz := action.Timezone
		if tz == "" {
			tz = "UTC"
		}

		nextTrigger := "-"
		if action.Enabled {
			nextTime, err := evaluator.GetNextTriggerTime(action.Schedule, action.Timezone)
			if err == nil && !nextTime.IsZero() {
				nextTrigger = time.Until(nextTime).Round(time.Second).String()
			}
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			action.Name, action.Schedule, action.DesiredCapacity,
			minStr, maxStr, tz, status, nextTrigger)
	}

	return w.Flush()
}
