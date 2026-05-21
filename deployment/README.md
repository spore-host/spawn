# Deployment Resources

This directory contains deployment configuration files for spawn infrastructure.

## Files

### trust-policy.json

IAM trust policy for the Lambda execution role. This policy allows AWS Lambda service to assume the role.

**Usage:**
```bash
aws iam create-role \
  --role-name SpawnDNSLambdaRole \
  --assume-role-policy-document file://deployment/trust-policy.json
```

## Deployment Scripts

See the `scripts/` directory for deployment scripts:

- `scripts/deploy-custom-dns.sh` - Deploy custom DNS infrastructure
- `scripts/enable-dnssec.sh` - Enable DNSSEC for your domain

## Documentation

- [CUSTOM_DNS.md](../CUSTOM_DNS.md) - Complete guide for custom DNS deployment
- [SECURITY.md](../SECURITY.md) - Security model and best practices
- [DNS_SETUP.md](../DNS_SETUP.md) - Original spore.host setup documentation
