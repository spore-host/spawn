package autoscaler

import (
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
)

// ScheduledAction defines a time-based capacity adjustment
type ScheduledAction struct {
	Name            string `dynamodbav:"name"`             // Human-readable name
	Schedule        string `dynamodbav:"schedule"`         // Cron expression
	DesiredCapacity int    `dynamodbav:"desired_capacity"` // Target capacity
	MinCapacity     int    `dynamodbav:"min_capacity"`     // Optional min override
	MaxCapacity     int    `dynamodbav:"max_capacity"`     // Optional max override
	Timezone        string `dynamodbav:"timezone"`         // Timezone (default: UTC)
	Enabled         bool   `dynamodbav:"enabled"`          // Enable/disable without deleting
}

// ScheduleConfig holds scheduled actions for a group
type ScheduleConfig struct {
	Actions []ScheduledAction `dynamodbav:"actions"`
}

// ScheduleEvaluator evaluates scheduled actions
type ScheduleEvaluator struct {
	parser cron.Parser
}

// NewScheduleEvaluator creates a new schedule evaluator
func NewScheduleEvaluator() *ScheduleEvaluator {
	// Use standard cron parser with seconds support
	// Format: second minute hour day month weekday
	parser := cron.NewParser(
		cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)

	return &ScheduleEvaluator{
		parser: parser,
	}
}

// EvaluateSchedule checks if any scheduled action should apply now
// Returns: (desiredCapacity, minCapacity, maxCapacity, actionName, shouldApply)
func (s *ScheduleEvaluator) EvaluateSchedule(
	scheduleConfig *ScheduleConfig,
	now time.Time,
) (int, int, int, string, bool) {
	if scheduleConfig == nil || len(scheduleConfig.Actions) == 0 {
		return 0, 0, 0, "", false
	}

	// Find all active schedules
	activeActions := make([]scheduledActionWithTime, 0)

	for _, action := range scheduleConfig.Actions {
		if !action.Enabled {
			continue
		}

		// Parse cron schedule
		schedule, err := s.parser.Parse(action.Schedule)
		if err != nil {
			log.Printf("invalid cron schedule %q for action %s: %v", action.Schedule, action.Name, err)
			continue
		}

		// Get timezone
		location := time.UTC
		if action.Timezone != "" {
			loc, err := time.LoadLocation(action.Timezone)
			if err != nil {
				log.Printf("invalid timezone %q for action %s: %v", action.Timezone, action.Name, err)
			} else {
				location = loc
			}
		}

		// Convert current time to action's timezone
		nowInZone := now.In(location)

		// Check if schedule matches current time
		// To find the most recent trigger, get the next trigger after (now - trigger window)
		// If that trigger is in the past and within the window, the schedule is active
		triggerWindow := time.Minute
		searchStart := nowInZone.Add(-triggerWindow)
		lastTrigger := schedule.Next(searchStart)

		// Check if this trigger is in the past and within the trigger window
		if lastTrigger.Before(nowInZone) || lastTrigger.Equal(nowInZone) {
			timeSinceLastTrigger := nowInZone.Sub(lastTrigger)
			if timeSinceLastTrigger < triggerWindow {
				activeActions = append(activeActions, scheduledActionWithTime{
					action:           action,
					triggerTime:      lastTrigger,
					timeSinceTrigger: timeSinceLastTrigger,
				})
			}
		}
	}

	if len(activeActions) == 0 {
		return 0, 0, 0, "", false
	}

	// If multiple schedules are active, use the most recent one
	sort.Slice(activeActions, func(i, j int) bool {
		return activeActions[i].triggerTime.After(activeActions[j].triggerTime)
	})

	mostRecent := activeActions[0].action
	log.Printf("active scheduled action: %s (triggered %v ago)",
		mostRecent.Name, activeActions[0].timeSinceTrigger)

	const maxScheduledCapacity = 10_000
	if mostRecent.DesiredCapacity < 0 || mostRecent.DesiredCapacity > maxScheduledCapacity ||
		mostRecent.MinCapacity < 0 || mostRecent.MinCapacity > maxScheduledCapacity ||
		mostRecent.MaxCapacity < 0 || mostRecent.MaxCapacity > maxScheduledCapacity {
		log.Printf("warning: scheduled action %q has out-of-range capacity values, skipping", mostRecent.Name)
		return 0, 0, 0, "", false
	}

	return mostRecent.DesiredCapacity,
		mostRecent.MinCapacity,
		mostRecent.MaxCapacity,
		mostRecent.Name,
		true
}

// ValidateSchedule validates a cron schedule expression
func (s *ScheduleEvaluator) ValidateSchedule(cronExpr string) error {
	_, err := s.parser.Parse(cronExpr)
	if err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	return nil
}

// GetNextTriggerTime returns the next time a schedule will trigger
func (s *ScheduleEvaluator) GetNextTriggerTime(cronExpr string, timezone string) (time.Time, error) {
	schedule, err := s.parser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression: %w", err)
	}

	location := time.UTC
	if timezone != "" {
		loc, err := time.LoadLocation(timezone)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid timezone: %w", err)
		}
		location = loc
	}

	now := time.Now().In(location)
	return schedule.Next(now), nil
}

type scheduledActionWithTime struct {
	action           ScheduledAction
	triggerTime      time.Time
	timeSinceTrigger time.Duration
}

// GetScheduleExamples returns common schedule patterns
func GetScheduleExamples() map[string]string {
	return map[string]string{
		"workday-morning": "0 0 8 * * MON-FRI",  // 8 AM weekdays
		"workday-evening": "0 0 18 * * MON-FRI", // 6 PM weekdays
		"weekend-start":   "0 0 0 * * SAT",      // Midnight Saturday
		"hourly":          "0 0 * * * *",        // Every hour
		"every-15-min":    "0 */15 * * * *",     // Every 15 minutes
		"daily-midnight":  "0 0 0 * * *",        // Midnight daily
		"weekly-monday":   "0 0 9 * * MON",      // 9 AM Monday
		"monthly-first":   "0 0 0 1 * *",        // 1st of month
	}
}
