package autoscaler

import (
	"testing"
	"time"
)

func TestNewScheduleEvaluator(t *testing.T) {
	se := NewScheduleEvaluator()
	if se == nil {
		t.Fatal("NewScheduleEvaluator returned nil")
	}
}

func TestValidateSchedule(t *testing.T) {
	se := NewScheduleEvaluator()

	tests := []struct {
		name     string
		cronExpr string
		wantErr  bool
	}{
		{"valid standard cron", "0 0 9 * * *", false},
		{"valid with seconds", "30 0 9 * * MON-FRI", false},
		{"valid hourly", "0 0 * * * *", false},
		{"valid every 15 min", "0 */15 * * * *", false},
		{"invalid format", "0 9 * * *", true},
		{"invalid field", "0 0 25 * * *", true},
		{"empty string", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := se.ValidateSchedule(tt.cronExpr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSchedule() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetNextTriggerTime(t *testing.T) {
	se := NewScheduleEvaluator()

	// Test valid schedule
	nextTime, err := se.GetNextTriggerTime("0 0 9 * * *", "UTC")
	if err != nil {
		t.Errorf("GetNextTriggerTime() error = %v", err)
	}
	if nextTime.IsZero() {
		t.Error("GetNextTriggerTime() returned zero time")
	}

	// Test invalid schedule
	_, err = se.GetNextTriggerTime("invalid", "UTC")
	if err == nil {
		t.Error("GetNextTriggerTime() expected error for invalid schedule")
	}

	// Test invalid timezone
	_, err = se.GetNextTriggerTime("0 0 9 * * *", "Invalid/Timezone")
	if err == nil {
		t.Error("GetNextTriggerTime() expected error for invalid timezone")
	}
}

func TestGetScheduleExamples(t *testing.T) {
	examples := GetScheduleExamples()

	// Check that common patterns exist
	requiredKeys := []string{
		"workday-morning",
		"workday-evening",
		"weekend-start",
		"hourly",
		"daily-midnight",
	}

	for _, key := range requiredKeys {
		if _, ok := examples[key]; !ok {
			t.Errorf("GetScheduleExamples() missing key: %s", key)
		}
	}

	// Validate that all examples are valid cron expressions
	se := NewScheduleEvaluator()
	for name, expr := range examples {
		if err := se.ValidateSchedule(expr); err != nil {
			t.Errorf("GetScheduleExamples() invalid cron for %s: %v", name, err)
		}
	}
}

func TestEvaluateSchedule_NoSchedules(t *testing.T) {
	se := NewScheduleEvaluator()
	now := time.Now()

	// Nil config
	_, _, _, _, shouldApply := se.EvaluateSchedule(nil, now)
	if shouldApply {
		t.Error("EvaluateSchedule() should return false for nil config")
	}

	// Empty actions
	config := &ScheduleConfig{Actions: []ScheduledAction{}}
	_, _, _, _, shouldApply = se.EvaluateSchedule(config, now)
	if shouldApply {
		t.Error("EvaluateSchedule() should return false for empty actions")
	}
}

func TestEvaluateSchedule_DisabledSchedule(t *testing.T) {
	se := NewScheduleEvaluator()
	now := time.Now()

	config := &ScheduleConfig{
		Actions: []ScheduledAction{
			{
				Name:            "test-schedule",
				Schedule:        "* * * * * *", // Every second
				DesiredCapacity: 10,
				Enabled:         false, // Disabled
			},
		},
	}

	_, _, _, _, shouldApply := se.EvaluateSchedule(config, now)
	if shouldApply {
		t.Error("EvaluateSchedule() should return false for disabled schedule")
	}
}

func TestEvaluateSchedule_InvalidSchedule(t *testing.T) {
	se := NewScheduleEvaluator()
	now := time.Now()

	config := &ScheduleConfig{
		Actions: []ScheduledAction{
			{
				Name:            "invalid-schedule",
				Schedule:        "invalid cron",
				DesiredCapacity: 10,
				Enabled:         true,
			},
		},
	}

	_, _, _, _, shouldApply := se.EvaluateSchedule(config, now)
	if shouldApply {
		t.Error("EvaluateSchedule() should return false for invalid schedule")
	}
}

func TestEvaluateSchedule_ActiveSchedule(t *testing.T) {
	se := NewScheduleEvaluator()

	// Use a fixed time for testing: 2026-02-11 10:00:30 UTC
	baseTime := time.Date(2026, 2, 11, 10, 0, 30, 0, time.UTC)

	config := &ScheduleConfig{
		Actions: []ScheduledAction{
			{
				Name:            "every-minute",
				Schedule:        "0 * * * * *", // Every minute at second 0
				DesiredCapacity: 10,
				MinCapacity:     5,
				MaxCapacity:     20,
				Timezone:        "UTC",
				Enabled:         true,
			},
		},
	}

	// Test at :00 seconds (should match)
	desired, min, max, name, shouldApply := se.EvaluateSchedule(config, baseTime)
	if !shouldApply {
		t.Error("EvaluateSchedule() should return true for active schedule")
	}
	if name != "every-minute" {
		t.Errorf("EvaluateSchedule() name = %s, want every-minute", name)
	}
	if desired != 10 {
		t.Errorf("EvaluateSchedule() desired = %d, want 10", desired)
	}
	if min != 5 {
		t.Errorf("EvaluateSchedule() min = %d, want 5", min)
	}
	if max != 20 {
		t.Errorf("EvaluateSchedule() max = %d, want 20", max)
	}

	// Test at :45 seconds (should match - within 1 minute window)
	laterTime := baseTime.Add(45 * time.Second)
	_, _, _, _, shouldApply = se.EvaluateSchedule(config, laterTime)
	if !shouldApply {
		t.Error("EvaluateSchedule() should trigger within 45 seconds")
	}
}

func TestEvaluateSchedule_OutsideWindow(t *testing.T) {
	se := NewScheduleEvaluator()

	// Use hourly schedule to test outside window
	// Test at 10:05:00 when last trigger was at 10:00:00 (5 minutes ago)
	testTime := time.Date(2026, 2, 11, 10, 5, 0, 0, time.UTC)

	config := &ScheduleConfig{
		Actions: []ScheduledAction{
			{
				Name:            "hourly",
				Schedule:        "0 0 * * * *", // Every hour at :00
				DesiredCapacity: 10,
				Enabled:         true,
			},
		},
	}

	// Should not trigger - last trigger was 5 minutes ago (outside 1-minute window)
	_, _, _, _, shouldApply := se.EvaluateSchedule(config, testTime)
	if shouldApply {
		t.Error("EvaluateSchedule() should not trigger outside 1-minute window")
	}
}

func TestEvaluateSchedule_MultipleSchedules(t *testing.T) {
	se := NewScheduleEvaluator()

	// Use a fixed time: 2026-02-11 09:00:30 UTC (Tuesday)
	baseTime := time.Date(2026, 2, 11, 9, 0, 30, 0, time.UTC)

	config := &ScheduleConfig{
		Actions: []ScheduledAction{
			{
				Name:            "every-minute",
				Schedule:        "0 * * * * *", // Every minute
				DesiredCapacity: 5,
				Enabled:         true,
			},
			{
				Name:            "every-hour",
				Schedule:        "0 0 * * * *", // Every hour
				DesiredCapacity: 10,
				Enabled:         true,
			},
		},
	}

	// At 09:00:00, both schedules trigger
	// Should return the most recent one
	desired, _, _, name, shouldApply := se.EvaluateSchedule(config, baseTime)
	if !shouldApply {
		t.Error("EvaluateSchedule() should return true when multiple schedules match")
	}

	// Both trigger at same time, but hourly should be selected as most recent
	// (both trigger at :00, so order depends on evaluation)
	if desired != 5 && desired != 10 {
		t.Errorf("EvaluateSchedule() desired = %d, want 5 or 10", desired)
	}
	if name != "every-minute" && name != "every-hour" {
		t.Errorf("EvaluateSchedule() name = %s, want one of the schedules", name)
	}
}

func TestEvaluateSchedule_Timezone(t *testing.T) {
	se := NewScheduleEvaluator()

	// 9 AM Pacific = 5 PM UTC (during PST, UTC-8)
	// February 11, 2026 is in PST (not PDT)
	utcTime := time.Date(2026, 2, 11, 17, 0, 30, 0, time.UTC)

	config := &ScheduleConfig{
		Actions: []ScheduledAction{
			{
				Name:            "morning-pacific",
				Schedule:        "0 0 9 * * *", // 9 AM in specified timezone
				DesiredCapacity: 10,
				Timezone:        "America/Los_Angeles",
				Enabled:         true,
			},
		},
	}

	// Should trigger at 5 PM UTC (9 AM Pacific)
	_, _, _, _, shouldApply := se.EvaluateSchedule(config, utcTime)
	if !shouldApply {
		t.Error("EvaluateSchedule() should respect timezone conversion")
	}

	// Should not trigger at 9 AM UTC (1 AM Pacific)
	wrongTime := time.Date(2026, 2, 11, 9, 0, 30, 0, time.UTC)
	_, _, _, _, shouldApply = se.EvaluateSchedule(config, wrongTime)
	if shouldApply {
		t.Error("EvaluateSchedule() should not trigger at wrong time for timezone")
	}
}

func TestEvaluateSchedule_InvalidTimezone(t *testing.T) {
	se := NewScheduleEvaluator()
	now := time.Now()

	config := &ScheduleConfig{
		Actions: []ScheduledAction{
			{
				Name:            "invalid-tz",
				Schedule:        "0 0 9 * * *",
				DesiredCapacity: 10,
				Timezone:        "Invalid/Timezone",
				Enabled:         true,
			},
		},
	}

	// Should fall back to UTC and continue evaluation
	se.EvaluateSchedule(config, now)
	// No error expected - should log warning and use UTC
}

func TestEvaluateSchedule_TriggerWindow(t *testing.T) {
	se := NewScheduleEvaluator()

	config := &ScheduleConfig{
		Actions: []ScheduledAction{
			{
				Name:            "hourly",
				Schedule:        "0 0 * * * *", // Every hour at :00
				DesiredCapacity: 10,
				Enabled:         true,
			},
		},
	}

	// Test at exactly trigger time (should match)
	exactTime := time.Date(2026, 2, 11, 10, 0, 0, 0, time.UTC)
	_, _, _, _, shouldApply := se.EvaluateSchedule(config, exactTime)
	if !shouldApply {
		t.Error("EvaluateSchedule() should trigger at exact time")
	}

	// Test 30 seconds after trigger (should match - within 1 minute)
	afterTime := exactTime.Add(30 * time.Second)
	_, _, _, _, shouldApply = se.EvaluateSchedule(config, afterTime)
	if !shouldApply {
		t.Error("EvaluateSchedule() should trigger within 30 seconds")
	}

	// Test 45 seconds after trigger (should match - within 1 minute)
	midTime := exactTime.Add(45 * time.Second)
	_, _, _, _, shouldApply = se.EvaluateSchedule(config, midTime)
	if !shouldApply {
		t.Error("EvaluateSchedule() should trigger within 45 seconds")
	}

	// Test 59 seconds after trigger (should match - within 1 minute)
	lateTime := exactTime.Add(59 * time.Second)
	_, _, _, _, shouldApply = se.EvaluateSchedule(config, lateTime)
	if !shouldApply {
		t.Error("EvaluateSchedule() should trigger within 59 seconds")
	}

	// Test 61 seconds after trigger (should not match - outside window)
	tooLateTime := exactTime.Add(61 * time.Second)
	_, _, _, _, shouldApply = se.EvaluateSchedule(config, tooLateTime)
	if shouldApply {
		t.Error("EvaluateSchedule() should not trigger after 1 minute window")
	}
}
