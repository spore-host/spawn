# Scheduled Scaling E2E Test

## Test Scenario: Workday Scaling

Test time-based scheduled actions that override desired capacity.

### Prerequisites

```bash
# Build latest spawn CLI
make build

# Verify Lambda is deployed (no code changes needed)
AWS_PROFILE=spore-host-infra aws lambda get-function \
  --function-name spawn-autoscale-orchestrator-production

# Set environment
export AWS_PROFILE=spore-host-dev
```

### Test Setup

```bash
# Create test autoscale group
spawn autoscale launch \
  --name schedule-test \
  --desired-capacity 1 \
  --min-capacity 0 \
  --max-capacity 10 \
  --instance-type t3.micro \
  --ami ami-0c02fb55b34c1a27d \
  --key-name spawn-dev \
  --subnet-id subnet-0123456789abcdef0 \
  --security-groups sg-0123456789abcdef0 \
  --env production

# Verify group created
spawn autoscale status schedule-test --env production
# Expected: Desired=1, Min=0, Max=10, No schedules
```

### Test 1: Add Immediate Schedule (Next Minute)

```bash
# Get current time in UTC
date -u +"%Y-%m-%d %H:%M:%S"

# Add schedule for next minute (adjust minute value)
# Format: second minute hour day month weekday
# Example: If current time is 12:34, schedule for 12:35
spawn autoscale add-schedule schedule-test \
  --name immediate-test \
  --schedule "0 35 12 * * *" \
  --desired-capacity 5 \
  --env production

# Verify schedule added
spawn autoscale list-schedules schedule-test --env production
# Expected: Shows schedule with "Next trigger: ~30s" (or similar)

# Wait 90 seconds for:
# - Schedule to trigger
# - Lambda 1-min interval to run
sleep 90

# Check if schedule was applied
spawn autoscale status schedule-test --env production
# Expected: Desired=5 (changed from 1)
# Expected: Shows schedule in output

# Check Lambda logs
AWS_PROFILE=spore-host-infra aws logs tail \
  /aws/lambda/spawn-autoscale-orchestrator-production \
  --since 5m --follow
# Expected: Log line "scheduled action \"immediate-test\" triggered"
```

### Test 2: Multiple Schedules (Morning/Evening)

```bash
# Add morning scale-up (9 AM)
spawn autoscale add-schedule schedule-test \
  --name morning-scaleup \
  --schedule "0 0 9 * * MON-FRI" \
  --desired-capacity 10 \
  --min-capacity 5 \
  --timezone America/New_York \
  --env production

# Add evening scale-down (6 PM)
spawn autoscale add-schedule schedule-test \
  --name evening-scaledown \
  --schedule "0 0 18 * * MON-FRI" \
  --desired-capacity 2 \
  --min-capacity 0 \
  --timezone America/New_York \
  --env production

# List all schedules
spawn autoscale list-schedules schedule-test --env production
# Expected: Shows 3 schedules with next trigger times
```

### Test 3: Schedule with Min/Max Overrides

```bash
# Add schedule that overrides both desired and bounds
spawn autoscale add-schedule schedule-test \
  --name weekend-minimal \
  --schedule "0 0 0 * * SAT" \
  --desired-capacity 0 \
  --min-capacity 0 \
  --max-capacity 2 \
  --timezone UTC \
  --env production

# Verify schedule with overrides
spawn autoscale list-schedules schedule-test --env production
# Expected: Shows min=0, max=2 for weekend-minimal
```

### Test 4: Schedule Priority (Most Recent Wins)

```bash
# Add two overlapping schedules
spawn autoscale add-schedule schedule-test \
  --name overlap-1 \
  --schedule "0 */5 * * * *" \
  --desired-capacity 3 \
  --env production

spawn autoscale add-schedule schedule-test \
  --name overlap-2 \
  --schedule "0 */5 * * * *" \
  --desired-capacity 7 \
  --env production

# Wait for next 5-minute mark
sleep 60

# Check which schedule won
AWS_PROFILE=spore-host-infra aws logs tail \
  /aws/lambda/spawn-autoscale-orchestrator-production \
  --since 5m
# Expected: Log shows one schedule triggered (most recent in DynamoDB)

spawn autoscale status schedule-test --env production
# Expected: Desired=3 or 7 (whichever triggered)
```

### Test 5: Update Existing Schedule

```bash
# Update immediate-test schedule
spawn autoscale add-schedule schedule-test \
  --name immediate-test \
  --schedule "0 */10 * * * *" \
  --desired-capacity 8 \
  --env production

# Verify update
spawn autoscale list-schedules schedule-test --env production
# Expected: immediate-test now shows "0 */10 * * * *" and desired=8
```

### Test 6: Remove Schedule

```bash
# Remove a schedule
spawn autoscale remove-schedule schedule-test overlap-1 --env production

# Verify removal
spawn autoscale list-schedules schedule-test --env production
# Expected: overlap-1 no longer listed
```

### Test 7: Schedule + Queue Policy Interaction

```bash
# Add queue-based policy
spawn autoscale set-policy schedule-test \
  --scaling-policy queue-depth \
  --queue-url https://sqs.us-east-1.amazonaws.com/.../test-queue \
  --target-messages-per-instance 10 \
  --env production

# Verify schedule takes priority
spawn autoscale status schedule-test --env production
# Expected: Shows both schedule and queue policy

# When schedule is NOT active, queue policy should apply
# When schedule IS active, schedule should override queue policy
```

### Test 8: Timezone Handling

```bash
# Add schedule in Pacific time
spawn autoscale add-schedule schedule-test \
  --name pacific-test \
  --schedule "0 0 15 * * *" \
  --desired-capacity 6 \
  --timezone America/Los_Angeles \
  --env production

# Verify next trigger accounts for timezone
spawn autoscale list-schedules schedule-test --env production
# Expected: Next trigger calculated in Pacific time

# At 3 PM Pacific (11 PM UTC), schedule should trigger
```

### Test 9: Invalid Schedule Validation

```bash
# Try invalid cron expression (should fail)
spawn autoscale add-schedule schedule-test \
  --name invalid-test \
  --schedule "0 0 25 * *" \
  --desired-capacity 5 \
  --env production
# Expected: Error "invalid schedule expression"

# Try invalid timezone (should fail)
spawn autoscale add-schedule schedule-test \
  --name invalid-tz \
  --schedule "0 0 12 * * *" \
  --desired-capacity 5 \
  --timezone Invalid/Zone \
  --env production
# Expected: Schedule created but logs show warning, falls back to UTC
```

### Test 10: Schedule Examples

```bash
# Test common schedule patterns from GetScheduleExamples()

# Hourly
spawn autoscale add-schedule schedule-test \
  --name hourly \
  --schedule "0 0 * * * *" \
  --desired-capacity 4

# Every 15 minutes
spawn autoscale add-schedule schedule-test \
  --name every-15min \
  --schedule "0 */15 * * * *" \
  --desired-capacity 3

# Daily at midnight
spawn autoscale add-schedule schedule-test \
  --name daily-midnight \
  --schedule "0 0 0 * * *" \
  --desired-capacity 1

# Verify all patterns are valid
spawn autoscale list-schedules schedule-test --env production
```

### Cleanup

```bash
# Terminate test group
spawn autoscale terminate schedule-test --env production

# Verify cleanup
spawn autoscale status schedule-test --env production
# Expected: Error "group not found" or status=terminated
```

## Expected Behavior Summary

1. **Schedule Evaluation**: Runs every Lambda invocation (1-minute intervals)
2. **Trigger Window**: Schedules active for 1 minute after trigger time
3. **Priority**: Schedules override queue and metric policies
4. **Multiple Schedules**: Most recent trigger wins
5. **Timezone Support**: Converts to specified timezone before evaluation
6. **Min/Max Overrides**: Optional, only applied if > 0
7. **Validation**: Cron expressions validated before saving
8. **Next Trigger**: Calculated and displayed by list-schedules

## Success Criteria

- [ ] Schedules trigger within 1 minute of scheduled time
- [ ] Desired capacity updated when schedule active
- [ ] Multiple schedules handled correctly (most recent wins)
- [ ] Timezone conversion works (America/New_York, America/Los_Angeles)
- [ ] Min/max overrides applied when specified
- [ ] Schedules override queue/metric policies
- [ ] Invalid cron expressions rejected
- [ ] list-schedules shows accurate next trigger times
- [ ] Lambda logs show schedule trigger events
- [ ] No performance degradation for groups without schedules
