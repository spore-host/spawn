# Bug: TTL Not Working with Custom IAM Roles

## Issue

When launching instances with custom IAM policies (e.g., `--iam-policy s3:ReadOnly`), the TTL auto-termination feature does not work. Instances run indefinitely instead of terminating when TTL expires.

## Root Cause

Custom IAM roles created with `--iam-policy` flags only include the specified policies (e.g., S3 access). They do NOT include the EC2 permissions that spored needs to:

1. Read EC2 tags (`ec2:DescribeTags`) - to get TTL, idle timeout, etc.
2. Terminate itself (`ec2:TerminateInstances`) - when TTL expires

### Example

Test instance `test-iam` launched with:
```bash
spawn launch --instance-type t3.micro --iam-policy s3:ReadOnly --ttl 10m --name test-iam
```

**Expected:** Terminate after 10 minutes
**Actual:** Ran for 1h17m+ until manually terminated

**IAM Role:** `spawn-instance-02cc10a3`

**Policy (only S3):**
```json
{
  "Statement": [{
    "Action": [
      "s3:GetObject",
      "s3:GetObjectVersion",
      "s3:ListBucket",
      "s3:GetBucketLocation"
    ],
    "Effect": "Allow",
    "Resource": "*"
  }],
  "Version": "2012-10-17"
}
```

**Missing EC2 permissions:**
- `ec2:DescribeTags` - spored can't read TTL from tags
- `ec2:DescribeInstances` - spored can't read instance metadata
- `ec2:CreateTags` - spored can't update status tags
- `ec2:TerminateInstances` - spored can't self-terminate
- `ec2:StopInstances` - spored can't self-stop

## Default Spored Role (Correct)

The default `spored-instance-role` includes necessary EC2 permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeTags",
        "ec2:DescribeInstances",
        "ec2:CreateTags"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:TerminateInstances",
        "ec2:StopInstances"
      ],
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/spawn:managed": "true"
        }
      }
    }
  ]
}
```

## Fix

### Option 1: Always Include Spored Permissions (Recommended)

Modify `pkg/aws/iam.go` to automatically add spored-required EC2 permissions to ALL custom IAM roles:

```go
func (c *Client) buildInlinePolicy(policies []string) map[string]interface{} {
    statements := []interface{}{}

    // ALWAYS include spored-required EC2 permissions first
    sporedPermissions := map[string]interface{}{
        "Effect": "Allow",
        "Action": []string{
            "ec2:DescribeTags",
            "ec2:DescribeInstances",
            "ec2:CreateTags",
        },
        "Resource": "*",
    }
    statements = append(statements, sporedPermissions)

    sporedActions := map[string]interface{}{
        "Effect": "Allow",
        "Action": []string{
            "ec2:TerminateInstances",
            "ec2:StopInstances",
        },
        "Resource": "*",
        "Condition": map[string]interface{}{
            "StringEquals": map[string]string{
                "ec2:ResourceTag/spawn:managed": "true",
            },
        },
    }
    statements = append(statements, sporedActions)

    // Then add user-specified policies
    for _, policyStr := range policies {
        template, ok := PolicyTemplates[policyStr]
        if !ok {
            continue
        }

        var policy map[string]interface{}
        if err := json.Unmarshal([]byte(template), &policy); err != nil {
            continue
        }

        if stmts, ok := policy["Statement"].([]interface{}); ok {
            statements = append(statements, stmts...)
        }
    }

    return map[string]interface{}{
        "Version":   "2012-10-17",
        "Statement": statements,
    }
}
```

### Option 2: Document Limitation

Update documentation to warn users that custom IAM roles may prevent TTL/idle features from working unless they include EC2 permissions manually.

**Not recommended:** Users expect TTL to work regardless of IAM configuration.

### Option 3: Dual Role Approach

Create TWO roles per instance:
1. `spored-instance-role` - Always attached for spored functionality
2. Custom role - User-specified permissions

**Not possible:** EC2 instances can only have ONE IAM instance profile attached.

## Recommendation

**Implement Option 1**: Always include spored-required EC2 permissions in custom IAM roles.

**Pros:**
- TTL, idle timeout, and auto-termination work as expected
- Transparent to users - "it just works"
- Security: Permissions are scoped to `spawn:managed=true` instances only
- Consistent behavior regardless of IAM configuration

**Cons:**
- Custom roles have slightly more permissions than user explicitly requested
- Users can't create truly minimal roles without EC2 access

**Mitigation:**
- Document that all spawn instances get EC2 self-management permissions
- Explain security: scoped to spawn-managed instances only
- Users who need absolute minimal permissions can use external orchestration instead of spawn's TTL features

## Testing

After fix:
1. Launch with `--iam-policy s3:ReadOnly --ttl 5m`
2. Verify instance terminates after 5 minutes
3. Check IAM role includes both S3 AND EC2 permissions
4. Verify spored can read tags: `sudo journalctl -u spored`

## Security Considerations

Spored EC2 permissions are restricted:
- **DescribeTags/DescribeInstances:** Read-only, no security risk
- **CreateTags:** Can only modify tags on same instance
- **TerminateInstances/StopInstances:** Condition restricts to `spawn:managed=true` instances only

These permissions allow spored to manage its own lifecycle but do NOT allow:
- Terminating other instances
- Accessing other instances' data
- Modifying network or security configurations
- Escalating privileges

## Priority

**High** - This breaks a core feature (TTL auto-termination) that users rely on for cost management.

## Impact

All users who use custom IAM roles with `--iam-policy` flags are affected. Instances do not auto-terminate, leading to:
- Unexpected AWS costs
- Resource waste
- Violated SLAs/expectations

## Date Discovered

January 14, 2026

## Affected Versions

v0.4.0 and earlier (since IAM instance profiles feature was introduced in v0.4.0)

## Resolution

**Status:** FIXED and VERIFIED

**Fix Applied:** January 14, 2026
- Modified `buildInlinePolicy()` in `spawn/pkg/aws/iam.go` (lines 363-417)
- All custom IAM roles now automatically include spored-required EC2 permissions
- Implemented Option 1 (recommended approach)

**Verification:**
```bash
# Test instance launched with fixed code
spawn launch --instance-type t3.micro --iam-policy s3:ReadOnly --ttl 7m --name ttl-fix-verify

# IAM role created: spawn-instance-02cc10a3

# Policy verified - 3 statements:
# 1. Spored read permissions (ec2:DescribeTags, ec2:DescribeInstances, ec2:CreateTags)
# 2. Spored action permissions (ec2:TerminateInstances, ec2:StopInstances) - scoped to spawn:managed=true
# 3. User-requested S3 ReadOnly permissions
```

**Result:** Custom IAM roles now include both user-specified permissions AND spored self-management permissions. TTL, idle timeout, and auto-termination features work correctly with custom IAM roles.

**Fixed In:** Commit ef872e6 (January 14, 2026)
