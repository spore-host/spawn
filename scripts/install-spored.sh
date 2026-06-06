#!/bin/bash
# spored installer - Downloads from S3 for fast regional access
#
# Environment variables:
#   PROJECT  - Project name for S3 key prefix (default: spawn)
#              Prism usage: PROJECT=prism ./install-spored.sh
set -e

PROJECT=${PROJECT:-spawn}

echo "=== Installing spored (project: ${PROJECT}) ==="

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)
        BINARY="spored-linux-amd64"
        ;;
    aarch64|arm64)
        BINARY="spored-linux-arm64"
        ;;
    *)
        echo "❌ Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

echo "Architecture: $ARCH ($BINARY)"

# Detect region from instance metadata
echo "Detecting AWS region..."
TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" 2>/dev/null)
if [ -n "$TOKEN" ]; then
    REGION=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region 2>/dev/null)
else
    # Fallback without token
    REGION=$(curl -s http://169.254.169.254/latest/meta-data/placement/region 2>/dev/null)
fi

if [ -z "$REGION" ]; then
    echo "⚠️  Could not detect region, using us-east-1"
    REGION="us-east-1"
else
    echo "Region: $REGION"
fi

# S3 bucket name (regional). Try project-prefixed path first, then root path.
S3_BUCKET="spawn-binaries-${REGION}"
S3_PATH_PREFIX="s3://${S3_BUCKET}/${PROJECT}/${BINARY}"
S3_PATH_ROOT="s3://${S3_BUCKET}/${BINARY}"

echo "Downloading spored..."

# Try regional bucket with project prefix, then root, then us-east-1 fallbacks
if aws s3 cp "$S3_PATH_PREFIX" /usr/local/bin/spored --region "$REGION" 2>/dev/null; then
    echo "✅ Downloaded from regional bucket (${PROJECT}/ prefix)"
elif aws s3 cp "$S3_PATH_ROOT" /usr/local/bin/spored --region "$REGION" 2>/dev/null; then
    echo "✅ Downloaded from regional bucket (root)"
elif aws s3 cp "s3://spawn-binaries-us-east-1/${PROJECT}/${BINARY}" /usr/local/bin/spored --region us-east-1 2>/dev/null; then
    echo "✅ Downloaded from us-east-1 (${PROJECT}/ prefix)"
elif aws s3 cp "s3://spawn-binaries-us-east-1/${BINARY}" /usr/local/bin/spored --region us-east-1 2>/dev/null; then
    echo "✅ Downloaded from us-east-1 (root)"
else
    echo "❌ Failed to download spored from any location"
    exit 1
fi

# Make executable
chmod +x /usr/local/bin/spored

# Verify installation
if /usr/local/bin/spored version; then
    echo "✅ spored installed successfully"
else
    echo "❌ spored binary verification failed"
    exit 1
fi

# Install acpid so AWS stop/terminate ACPI signals trigger a graceful OS
# shutdown rather than a hard kill after the AWS-side timeout.
echo "Installing acpid for graceful AWS stop/terminate handling..."
dnf install -y acpid 2>/dev/null || yum install -y acpid 2>/dev/null || true
systemctl enable --now acpid

# Create systemd service
echo "Installing systemd service..."
cat > /etc/systemd/system/spored.service <<'EOF'
[Unit]
Description=Spawn Agent - Instance self-monitoring
Documentation=https://github.com/spore-host/spawn
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/spored
# on-failure (not always) prevents restart attempts during graceful shutdown
Restart=on-failure
RestartSec=10
# Give spored time to deregister DNS and clean up before SIGKILL
TimeoutStopSec=30
StandardOutput=journal
StandardError=journal

# Security hardening
NoNewPrivileges=true
# NOTE: do NOT set PrivateTmp=true. spored watches the host-side completion
# file (default /tmp/SPAWN_COMPLETE) written by users, `spawn connect`, and
# nf-spawn. PrivateTmp gives the daemon an isolated /tmp where it can never
# see that file, silently disabling --on-complete and --pre-stop (#66).

[Install]
WantedBy=multi-user.target
EOF

# Reload systemd
systemctl daemon-reload

# Enable and start spored
systemctl enable spored
systemctl start spored

# Wait a moment for startup
sleep 2

# Check status
if systemctl is-active --quiet spored; then
    echo "✅ spored is running"
    systemctl status spored --no-pager --lines=5
else
    echo "⚠️  spored may have issues"
    journalctl -u spored -n 20 --no-pager
fi

echo ""
echo "=== Installation complete ==="
echo "View logs: journalctl -u spored -f"
echo "Check status: systemctl status spored"

