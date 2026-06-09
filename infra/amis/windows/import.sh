#!/usr/bin/env bash
# Import a Packer-built Windows VHD into an EC2 AMI (#83).
#
# Usage: ./import.sh <path-to-exported.vhd> <s3-bucket> [region]
#
# Requires: the `vmimport` IAM service role in the target account (deploy
# deployment/cloudformation/vmimport-role.yaml first) and an S3 bucket the role
# can read. Uploads the VHD, runs `aws ec2 import-image`, polls to completion,
# and tags the resulting AMI spawn:os=windows so `spawn launch --os windows`
# can use it. See README.md.
set -euo pipefail

DISK="${1:?usage: import.sh <disk.vmdk|.vhd|.vhdx> <s3-bucket> [region]}"
BUCKET="${2:?usage: import.sh <disk.vmdk|.vhd|.vhdx> <s3-bucket> [region]}"
REGION="${3:-us-east-1}"

[ -f "$DISK" ] || { echo "disk image not found: $DISK" >&2; exit 1; }

# import-image's Format is the file type. Infer it from the extension (qemu
# outputs vmdk; Hyper-V vhd/vhdx — all are accepted by import-image).
ext="${DISK##*.}"
case "$ext" in
  vmdk|VMDK) format="vmdk" ;;
  vhd|VHD)   format="vhd" ;;
  vhdx|VHDX) format="vhdx" ;;
  raw|img)   format="raw" ;;
  *) echo "unsupported disk format '.$ext' (expected vmdk/vhd/vhdx/raw)" >&2; exit 1 ;;
esac

key="ami-imports/$(basename "$DISK")"
echo "Uploading $DISK ($format) → s3://$BUCKET/$key ..."
aws s3 cp "$DISK" "s3://$BUCKET/$key" --region "$REGION"

echo "Starting import-image..."
containers=$(cat <<JSON
[{"Description":"spore.host Windows 11 custom AMI","Format":"$format","UserBucket":{"S3Bucket":"$BUCKET","S3Key":"$key"}}]
JSON
)
task=$(aws ec2 import-image \
  --region "$REGION" \
  --description "spore.host Windows 11 custom AMI" \
  --disk-containers "$containers" \
  --query 'ImportTaskId' --output text)
echo "Import task: $task"

echo "Polling import (this takes 20-40 min)..."
while true; do
  read -r status progress ami <<<"$(aws ec2 describe-import-image-tasks \
    --region "$REGION" --import-task-ids "$task" \
    --query 'ImportImageTasks[0].[Status,Progress,ImageId]' --output text)"
  echo "  status=$status progress=${progress:-?} ami=${ami:-pending}"
  case "$status" in
    completed) break ;;
    deleted|*[Ee]rror*) echo "import failed: $status" >&2; exit 1 ;;
  esac
  sleep 60
done

echo "Imported AMI: $ami"
# Tag so spawn/reaper recognize the OS. (Imported AMIs often have empty Platform
# metadata, so `spawn launch --os windows` is still required at launch time.)
aws ec2 create-tags --region "$REGION" --resources "$ami" \
  --tags Key=spawn:os,Value=windows Key=Name,Value=spore-windows11 Key=spawn:source,Value=iso-import
echo "Tagged $ami (spawn:os=windows). Launch with: spawn launch <name> --ami $ami --os windows --ttl 4h"
echo "NOTE: a Windows *client* AMI requires Dedicated Host tenancy (see README)."
