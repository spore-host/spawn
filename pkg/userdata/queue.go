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

# Wait for spored to be installed and running (downloaded from S3 during boot)
echo "Waiting for spored to be installed..."
MAX_WAIT=300  # 5 minutes
WAITED=0
while ! command -v spored &> /dev/null && [ $WAITED -lt $MAX_WAIT ]; do
    sleep 5
    WAITED=$((WAITED + 5))
done

if ! command -v spored &> /dev/null; then
    echo "ERROR: spored not installed after ${MAX_WAIT}s. Check cloud-init logs."
    exit 1
fi

# Also wait for spored service to be active
WAITED=0
while ! systemctl is-active --quiet spored 2>/dev/null && [ $WAITED -lt $MAX_WAIT ]; do
    sleep 5
    WAITED=$((WAITED + 5))
done
echo "spored is ready"

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
