#!/usr/bin/env python3
"""
Backfill user_id field in spawn-autoscale-groups-production table.
Extracts user_id from spawn:iam-user tag on instances in each autoscale group.
"""

import boto3
import sys

def backfill_autoscale_groups(profile='mycelium-infra'):
    session = boto3.Session(profile_name=profile)
    dynamodb = session.client('dynamodb', region_name='us-east-1')

    # Use dev account for EC2 queries (where instances live)
    dev_session = boto3.Session(profile_name='mycelium-dev')
    ec2 = dev_session.client('ec2', region_name='us-east-1')

    print("Scanning spawn-autoscale-groups-production table...")

    # Get all autoscale groups
    response = dynamodb.scan(TableName='spawn-autoscale-groups-production')
    groups = response['Items']

    print(f"Found {len(groups)} autoscale groups")
    print()

    updated = 0
    skipped = 0
    failed = 0

    for group in groups:
        group_id = group['autoscale_group_id']['S']

        # Skip if already has user_id
        if 'user_id' in group and group['user_id'].get('S'):
            print(f"⊘ {group_id}: already has user_id")
            skipped += 1
            continue

        try:
            # Find instances with this autoscale group tag
            instances_response = ec2.describe_instances(
                Filters=[
                    {'Name': 'tag:spawn:autoscale-group', 'Values': [group_id]},
                    {'Name': 'instance-state-name', 'Values': ['running', 'stopped', 'pending']}
                ]
            )

            # Extract user_id from first instance
            if instances_response['Reservations']:
                instance = instances_response['Reservations'][0]['Instances'][0]
                tags = {tag['Key']: tag['Value'] for tag in instance.get('Tags', [])}
                user_id = tags.get('spawn:iam-user')

                if user_id:
                    # Update group with user_id
                    dynamodb.update_item(
                        TableName='spawn-autoscale-groups-production',
                        Key={'autoscale_group_id': {'S': group_id}},
                        UpdateExpression='SET user_id = :uid',
                        ExpressionAttributeValues={':uid': {'S': user_id}}
                    )
                    print(f"✓ {group_id}: set user_id = {user_id}")
                    updated += 1
                else:
                    print(f"✗ {group_id}: instance has no spawn:iam-user tag")
                    failed += 1
            else:
                print(f"⊘ {group_id}: no instances found (group may be terminated)")
                skipped += 1

        except Exception as e:
            print(f"✗ {group_id}: error - {e}")
            failed += 1

    print()
    print(f"Summary: {updated} updated, {skipped} skipped, {failed} failed")

    if failed > 0:
        print()
        print("Warning: Some groups could not be backfilled. Review errors above.")
        sys.exit(1)

if __name__ == '__main__':
    profile = sys.argv[1] if len(sys.argv) > 1 else 'mycelium-infra'
    backfill_autoscale_groups(profile)
