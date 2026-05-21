package userdata

import "fmt"

// GenerateQueueRunnerUserData creates user-data script for batch queue execution
func GenerateQueueRunnerUserData(s3QueueURL, queueID string) string {
	return fmt.Sprintf(`#!/bin/bash
set -e

# Queue runner bootstrap for spawn batch execution
echo "Starting queue runner bootstrap..."

# Set environment variables
export SPAWN_QUEUE_S3_URL="%s"
export SPAWN_QUEUE_ID="%s"

# Create queue runner directories
mkdir -p /var/lib/spored
mkdir -p /var/log/spored/jobs
chmod 755 /var/lib/spored
chmod 755 /var/log/spored/jobs

# Download queue configuration from S3
echo "Downloading queue configuration..."
aws s3 cp "$SPAWN_QUEUE_S3_URL" /var/lib/spored/queue.json

if [ ! -f /var/lib/spored/queue.json ]; then
    echo "ERROR: Failed to download queue configuration from $SPAWN_QUEUE_S3_URL"
    exit 1
fi

echo "Queue configuration downloaded successfully"

# Wait for spored to be installed (from standard spawn launch process)
# This user-data will be appended to standard spored setup
# The spored binary should already be installed and configured

# Check if spored is available
if ! command -v spored &> /dev/null; then
    echo "ERROR: spored command not found. This user-data expects spored to be pre-installed."
    echo "Make sure this user-data is combined with standard spawn user-data."
    exit 1
fi

echo "Starting batch queue execution with spored..."

# Run queue (this blocks until all jobs complete or fail)
spored run-queue /var/lib/spored/queue.json

# Capture exit code
QUEUE_EXIT_CODE=$?

echo "Queue execution finished with exit code: $QUEUE_EXIT_CODE"

# Signal completion to spawn (if completion tracking is enabled)
if [ -n "$SPAWN_COMPLETION_FILE" ]; then
    echo "exit_code=$QUEUE_EXIT_CODE" > "$SPAWN_COMPLETION_FILE"
    echo "timestamp=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" >> "$SPAWN_COMPLETION_FILE"
fi

# Exit with queue's exit code
exit $QUEUE_EXIT_CODE
`, s3QueueURL, queueID)
}
