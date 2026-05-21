#!/bin/bash
set -e

# Complete setup for Dashboard API infrastructure
# Usage: ./setup-all.sh [aws-profile]

PROFILE=${1:-default}

echo "╔════════════════════════════════════════════════════════╗"
echo "║   Dashboard API Complete Infrastructure Setup         ║"
echo "╚════════════════════════════════════════════════════════╝"
echo ""
echo "This script will:"
echo "  1. Create IAM role and policies"
echo "  2. Deploy Lambda function"
echo "  3. Set up API Gateway"
echo "  4. Configure custom domain (api.spore.host)"
echo ""
echo "Profile: $PROFILE"
echo ""
read -p "Continue? [y/N] " -n 1 -r
echo ""

if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Aborted."
    exit 0
fi

# Step 1: Create IAM role
echo ""
echo "═══════════════════════════════════════════════════════"
echo "  Step 1/4: Creating IAM Role"
echo "═══════════════════════════════════════════════════════"
./scripts/setup-dashboard-lambda-role.sh "$PROFILE"

# Step 2: Deploy Lambda
echo ""
echo "═══════════════════════════════════════════════════════"
echo "  Step 2/4: Deploying Lambda Function"
echo "═══════════════════════════════════════════════════════"
cd ..
./deploy.sh "$PROFILE"
cd scripts

# Step 3: Set up API Gateway
echo ""
echo "═══════════════════════════════════════════════════════"
echo "  Step 3/4: Setting up API Gateway"
echo "═══════════════════════════════════════════════════════"
./setup-dashboard-api-gateway.sh "$PROFILE"

# Step 4: Configure custom domain
echo ""
echo "═══════════════════════════════════════════════════════"
echo "  Step 4/4: Configuring Custom Domain"
echo "═══════════════════════════════════════════════════════"
./setup-dashboard-domain.sh "$PROFILE"

echo ""
echo "╔════════════════════════════════════════════════════════╗"
echo "║   ✅ Complete Setup Finished!                          ║"
echo "╚════════════════════════════════════════════════════════╝"
echo ""
echo "Your Dashboard API is now live at:"
echo "  https://api.spore.host"
echo ""
echo "Test endpoints:"
echo "  curl https://api.spore.host/api/user/profile"
echo "  curl https://api.spore.host/api/instances"
echo "  curl https://api.spore.host/api/sweeps"
echo "  curl https://api.spore.host/api/autoscale-groups"
echo ""
echo "Frontend:"
echo "  https://spore.host"
echo ""
