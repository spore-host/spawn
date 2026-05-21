# How-To: Custom Networking

Configure custom VPCs, subnets, and networking for spawn instances.

## Default Networking

### What spawn Creates Automatically

By default, spawn creates:
- VPC with 10.0.0.0/16 CIDR
- Public subnet in single AZ
- Internet Gateway
- Route table with default route to IGW
- Security group allowing SSH from your IP

**This works for most use cases.**

---

## Custom VPC Setup

### Create Custom VPC

```bash
#!/bin/bash
# create-custom-vpc.sh

set -e

echo "Creating custom VPC for spawn..."

# Create VPC
VPC_ID=$(aws ec2 create-vpc \
  --cidr-block 10.100.0.0/16 \
  --tag-specifications 'ResourceType=vpc,Tags=[{Key=Name,Value=spawn-vpc}]' \
  --query 'Vpc.VpcId' \
  --output text)

echo "VPC created: $VPC_ID"

# Enable DNS hostnames
aws ec2 modify-vpc-attribute --vpc-id $VPC_ID --enable-dns-hostnames

# Create Internet Gateway
IGW_ID=$(aws ec2 create-internet-gateway \
  --tag-specifications 'ResourceType=internet-gateway,Tags=[{Key=Name,Value=spawn-igw}]' \
  --query 'InternetGateway.InternetGatewayId' \
  --output text)

# Attach IGW to VPC
aws ec2 attach-internet-gateway --vpc-id $VPC_ID --internet-gateway-id $IGW_ID

echo "Internet Gateway created: $IGW_ID"

# Create public subnet (us-east-1a)
PUBLIC_SUBNET=$(aws ec2 create-subnet \
  --vpc-id $VPC_ID \
  --cidr-block 10.100.1.0/24 \
  --availability-zone us-east-1a \
  --tag-specifications 'ResourceType=subnet,Tags=[{Key=Name,Value=spawn-public-1a}]' \
  --query 'Subnet.SubnetId' \
  --output text)

# Enable auto-assign public IP
aws ec2 modify-subnet-attribute \
  --subnet-id $PUBLIC_SUBNET \
  --map-public-ip-on-launch

echo "Public subnet created: $PUBLIC_SUBNET"

# Create route table
ROUTE_TABLE=$(aws ec2 create-route-table \
  --vpc-id $VPC_ID \
  --tag-specifications 'ResourceType=route-table,Tags=[{Key=Name,Value=spawn-public-rt}]' \
  --query 'RouteTable.RouteTableId' \
  --output text)

# Add default route to IGW
aws ec2 create-route \
  --route-table-id $ROUTE_TABLE \
  --destination-cidr-block 0.0.0.0/0 \
  --gateway-id $IGW_ID

# Associate route table with subnet
aws ec2 associate-route-table \
  --route-table-id $ROUTE_TABLE \
  --subnet-id $PUBLIC_SUBNET

echo "Route table configured: $ROUTE_TABLE"

# Create security group
SG_ID=$(aws ec2 create-security-group \
  --group-name spawn-sg \
  --description "spawn instances security group" \
  --vpc-id $VPC_ID \
  --query 'GroupId' \
  --output text)

# Allow SSH from anywhere (customize as needed)
aws ec2 authorize-security-group-ingress \
  --group-id $SG_ID \
  --protocol tcp \
  --port 22 \
  --cidr 0.0.0.0/0

echo "Security group created: $SG_ID"

# Save IDs
cat > vpc-config.txt << EOF
VPC_ID=$VPC_ID
PUBLIC_SUBNET=$PUBLIC_SUBNET
SECURITY_GROUP=$SG_ID
EOF

echo "VPC setup complete! Configuration saved to vpc-config.txt"
```

### Use Custom VPC

```bash
# Load configuration
source vpc-config.txt

# Launch in custom VPC
spawn launch \
  --vpc $VPC_ID \
  --subnet $PUBLIC_SUBNET \
  --security-groups $SECURITY_GROUP
```

---

## Private Subnets

### Setup Private Subnet with NAT Gateway

```bash
#!/bin/bash
# setup-private-subnet.sh

VPC_ID="vpc-xxx"
PUBLIC_SUBNET="subnet-xxx"  # Existing public subnet

# Create private subnet
PRIVATE_SUBNET=$(aws ec2 create-subnet \
  --vpc-id $VPC_ID \
  --cidr-block 10.100.10.0/24 \
  --availability-zone us-east-1a \
  --tag-specifications 'ResourceType=subnet,Tags=[{Key=Name,Value=spawn-private-1a}]' \
  --query 'Subnet.SubnetId' \
  --output text)

echo "Private subnet created: $PRIVATE_SUBNET"

# Create Elastic IP for NAT Gateway
EIP_ALLOC=$(aws ec2 allocate-address \
  --domain vpc \
  --query 'AllocationId' \
  --output text)

echo "Elastic IP allocated: $EIP_ALLOC"

# Create NAT Gateway in public subnet
NAT_GW=$(aws ec2 create-nat-gateway \
  --subnet-id $PUBLIC_SUBNET \
  --allocation-id $EIP_ALLOC \
  --tag-specifications 'ResourceType=natgateway,Tags=[{Key=Name,Value=spawn-nat}]' \
  --query 'NatGateway.NatGatewayId' \
  --output text)

echo "NAT Gateway created: $NAT_GW (waiting for available...)"

# Wait for NAT Gateway to be available
aws ec2 wait nat-gateway-available --nat-gateway-ids $NAT_GW

# Create route table for private subnet
PRIVATE_RT=$(aws ec2 create-route-table \
  --vpc-id $VPC_ID \
  --tag-specifications 'ResourceType=route-table,Tags=[{Key=Name,Value=spawn-private-rt}]' \
  --query 'RouteTable.RouteTableId' \
  --output text)

# Add default route to NAT Gateway
aws ec2 create-route \
  --route-table-id $PRIVATE_RT \
  --destination-cidr-block 0.0.0.0/0 \
  --nat-gateway-id $NAT_GW

# Associate route table with private subnet
aws ec2 associate-route-table \
  --route-table-id $PRIVATE_RT \
  --subnet-id $PRIVATE_SUBNET

echo "Private subnet setup complete!"
echo "PRIVATE_SUBNET=$PRIVATE_SUBNET"
```

### Launch in Private Subnet

```bash
# Launch instance with no public IP
spawn launch \
  --subnet $PRIVATE_SUBNET \
  --no-public-ip \
  --security-groups $SG_ID

# Access via Session Manager (no SSH keys needed)
spawn connect <instance-id> --ssm

# Or via bastion host
spawn connect <instance-id> --bastion <bastion-instance-id>
```

---

## Multi-AZ Setup

### High Availability Across AZs

```bash
#!/bin/bash
# setup-multi-az.sh

VPC_ID="vpc-xxx"
IGW_ID="igw-xxx"

# Create subnets in 3 AZs
SUBNET_1A=$(aws ec2 create-subnet \
  --vpc-id $VPC_ID \
  --cidr-block 10.100.1.0/24 \
  --availability-zone us-east-1a \
  --query 'Subnet.SubnetId' --output text)

SUBNET_1B=$(aws ec2 create-subnet \
  --vpc-id $VPC_ID \
  --cidr-block 10.100.2.0/24 \
  --availability-zone us-east-1b \
  --query 'Subnet.SubnetId' --output text)

SUBNET_1C=$(aws ec2 create-subnet \
  --vpc-id $VPC_ID \
  --cidr-block 10.100.3.0/24 \
  --availability-zone us-east-1c \
  --query 'Subnet.SubnetId' --output text)

echo "Subnets created:"
echo "  us-east-1a: $SUBNET_1A"
echo "  us-east-1b: $SUBNET_1B"
echo "  us-east-1c: $SUBNET_1C"

# Enable auto-assign public IP on all
for subnet in $SUBNET_1A $SUBNET_1B $SUBNET_1C; do
  aws ec2 modify-subnet-attribute --subnet-id $subnet --map-public-ip-on-launch
done
```

**Launch instances across AZs:**
```bash
# Distribute sweep across AZs for high availability
spawn launch --param-file sweep.yaml --subnets $SUBNET_1A,$SUBNET_1B,$SUBNET_1C
```

---

## VPC Peering

### Connect Two VPCs

```bash
# Create VPC peering connection
PEERING_ID=$(aws ec2 create-vpc-peering-connection \
  --vpc-id vpc-requester \
  --peer-vpc-id vpc-accepter \
  --query 'VpcPeeringConnection.VpcPeeringConnectionId' \
  --output text)

# Accept peering connection
aws ec2 accept-vpc-peering-connection \
  --vpc-peering-connection-id $PEERING_ID

# Add routes in both VPCs
# In requester VPC route table:
aws ec2 create-route \
  --route-table-id rtb-requester \
  --destination-cidr-block 10.200.0.0/16 \
  --vpc-peering-connection-id $PEERING_ID

# In accepter VPC route table:
aws ec2 create-route \
  --route-table-id rtb-accepter \
  --destination-cidr-block 10.100.0.0/16 \
  --vpc-peering-connection-id $PEERING_ID
```

**Use case:** Instances in VPC A need to access resources in VPC B.

---

## VPC Endpoints

### S3 Gateway Endpoint

**Benefit:** Access S3 without going through internet gateway (no data transfer charges).

```bash
# Create S3 gateway endpoint
ENDPOINT_ID=$(aws ec2 create-vpc-endpoint \
  --vpc-id $VPC_ID \
  --service-name com.amazonaws.us-east-1.s3 \
  --route-table-ids $ROUTE_TABLE \
  --query 'VpcEndpoint.VpcEndpointId' \
  --output text)

echo "S3 endpoint created: $ENDPOINT_ID"

# Now instances can access S3 via endpoint (no internet gateway)
```

### Interface Endpoints

**For other AWS services:**

```bash
# Create ECR endpoint (for Docker images)
aws ec2 create-vpc-endpoint \
  --vpc-id $VPC_ID \
  --service-name com.amazonaws.us-east-1.ecr.dkr \
  --vpc-endpoint-type Interface \
  --subnet-ids $PRIVATE_SUBNET \
  --security-group-ids $SG_ID

# Create Secrets Manager endpoint
aws ec2 create-vpc-endpoint \
  --vpc-id $VPC_ID \
  --service-name com.amazonaws.us-east-1.secretsmanager \
  --vpc-endpoint-type Interface \
  --subnet-ids $PRIVATE_SUBNET \
  --security-group-ids $SG_ID
```

**Use case:** Private instances accessing AWS services without NAT Gateway.

---

## Network ACLs

### Subnet-Level Firewall

```bash
# Create network ACL
NACL_ID=$(aws ec2 create-network-acl \
  --vpc-id $VPC_ID \
  --query 'NetworkAcl.NetworkAclId' \
  --output text)

# Allow inbound SSH (rule 100)
aws ec2 create-network-acl-entry \
  --network-acl-id $NACL_ID \
  --rule-number 100 \
  --protocol tcp \
  --port-range From=22,To=22 \
  --cidr-block 0.0.0.0/0 \
  --ingress \
  --rule-action allow

# Allow inbound ephemeral ports (rule 200)
aws ec2 create-network-acl-entry \
  --network-acl-id $NACL_ID \
  --rule-number 200 \
  --protocol tcp \
  --port-range From=1024,To=65535 \
  --cidr-block 0.0.0.0/0 \
  --ingress \
  --rule-action allow

# Allow outbound HTTP/HTTPS (rule 100)
aws ec2 create-network-acl-entry \
  --network-acl-id $NACL_ID \
  --rule-number 100 \
  --protocol tcp \
  --port-range From=80,To=80 \
  --cidr-block 0.0.0.0/0 \
  --egress \
  --rule-action allow

aws ec2 create-network-acl-entry \
  --network-acl-id $NACL_ID \
  --rule-number 110 \
  --protocol tcp \
  --port-range From=443,To=443 \
  --cidr-block 0.0.0.0/0 \
  --egress \
  --rule-action allow

# Associate with subnet
aws ec2 replace-network-acl-association \
  --association-id $(aws ec2 describe-network-acls \
    --filters "Name=association.subnet-id,Values=$SUBNET_ID" \
    --query 'NetworkAcls[0].Associations[0].NetworkAclAssociationId' \
    --output text) \
  --network-acl-id $NACL_ID
```

---

## DNS Configuration

### Custom DNS Servers

```bash
# Create DHCP options set
DHCP_OPTIONS=$(aws ec2 create-dhcp-options \
  --dhcp-configurations \
    "Key=domain-name-servers,Values=8.8.8.8,8.8.4.4" \
    "Key=domain-name,Values=example.com" \
  --query 'DhcpOptions.DhcpOptionsId' \
  --output text)

# Associate with VPC
aws ec2 associate-dhcp-options \
  --dhcp-options-id $DHCP_OPTIONS \
  --vpc-id $VPC_ID
```

### Private DNS

```bash
# Create private hosted zone
ZONE_ID=$(aws route53 create-hosted-zone \
  --name internal.example.com \
  --vpc VPCRegion=us-east-1,VPCId=$VPC_ID \
  --caller-reference $(date +%s) \
  --query 'HostedZone.Id' \
  --output text)

# Add DNS record for instance
aws route53 change-resource-record-sets \
  --hosted-zone-id $ZONE_ID \
  --change-batch '{
    "Changes": [{
      "Action": "CREATE",
      "ResourceRecordSet": {
        "Name": "app.internal.example.com",
        "Type": "A",
        "TTL": 300,
        "ResourceRecords": [{"Value": "10.100.1.100"}]
      }
    }]
  }'
```

---

## IPv6 Support

### Enable IPv6 on VPC

```bash
# Associate IPv6 CIDR with VPC
IPV6_CIDR=$(aws ec2 associate-vpc-cidr-block \
  --vpc-id $VPC_ID \
  --amazon-provided-ipv6-cidr-block \
  --query 'Ipv6CidrBlockAssociation.Ipv6CidrBlock' \
  --output text)

echo "IPv6 CIDR: $IPV6_CIDR"

# Associate IPv6 CIDR with subnet
aws ec2 associate-subnet-cidr-block \
  --subnet-id $SUBNET_ID \
  --ipv6-cidr-block ${IPV6_CIDR%::/56}::/64

# Enable auto-assign IPv6
aws ec2 modify-subnet-attribute \
  --subnet-id $SUBNET_ID \
  --assign-ipv6-address-on-creation

# Update security group for IPv6
aws ec2 authorize-security-group-ingress \
  --group-id $SG_ID \
  --ip-permissions IpProtocol=tcp,FromPort=22,ToPort=22,Ipv6Ranges='[{CidrIpv6=::/0}]'
```

---

## Network Monitoring

### VPC Flow Logs

```bash
# Create CloudWatch log group
aws logs create-log-group --log-group-name /aws/vpc/flow-logs

# Create IAM role for flow logs (one-time)
# See: https://docs.aws.amazon.com/vpc/latest/userguide/flow-logs-cwl.html

# Enable flow logs
aws ec2 create-flow-logs \
  --resource-type VPC \
  --resource-ids $VPC_ID \
  --traffic-type ALL \
  --log-destination-type cloud-watch-logs \
  --log-group-name /aws/vpc/flow-logs \
  --deliver-logs-permission-arn arn:aws:iam::123456789012:role/flowlogsRole
```

**Query flow logs:**
```bash
aws logs filter-log-events \
  --log-group-name /aws/vpc/flow-logs \
  --filter-pattern '[version, account, eni, source, destination, srcport, destport="22", protocol="6", packets, bytes, start, end, action="ACCEPT", logstatus]' \
  --start-time $(date -d '1 hour ago' +%s)000
```

---

## Cost Optimization

### NAT Gateway Alternatives

**NAT Gateway cost:** $0.045/hour + $0.045/GB processed

**Alternative 1: NAT Instance (cheaper):**
```bash
# Launch t4g.nano as NAT instance
spawn launch \
  --instance-type t4g.nano \
  --subnet $PUBLIC_SUBNET \
  --name nat-instance \
  --user-data "
    echo 1 > /proc/sys/net/ipv4/ip_forward
    iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE
  "

# Disable source/dest check
aws ec2 modify-instance-attribute \
  --instance-id <nat-instance-id> \
  --no-source-dest-check

# Update private subnet route table to use NAT instance
aws ec2 create-route \
  --route-table-id $PRIVATE_RT \
  --destination-cidr-block 0.0.0.0/0 \
  --instance-id <nat-instance-id>
```

**Cost:** ~$0.0042/hour (t4g.nano) = 90% savings

**Alternative 2: VPC Endpoints (free for data transfer):**
```bash
# Use VPC endpoints instead of NAT Gateway for AWS services
# No data transfer charges, no hourly charge
```

---

## Troubleshooting

### Instance Can't Reach Internet

**Check:**
1. Route table has default route to IGW
2. Security group allows outbound traffic
3. Network ACL allows outbound traffic
4. Subnet has `MapPublicIpOnLaunch` enabled

```bash
# Verify route table
aws ec2 describe-route-tables --route-table-ids $ROUTE_TABLE

# Check security group
aws ec2 describe-security-groups --group-ids $SG_ID

# Check subnet attribute
aws ec2 describe-subnets --subnet-ids $SUBNET_ID \
  --query 'Subnets[0].MapPublicIpOnLaunch'
```

### Private Instance Can't Reach Internet

**Check:**
1. NAT Gateway in route table
2. NAT Gateway is in public subnet
3. NAT Gateway has Elastic IP

```bash
# Verify NAT Gateway
aws ec2 describe-nat-gateways --nat-gateway-ids $NAT_GW

# Check route
aws ec2 describe-route-tables --route-table-ids $PRIVATE_RT
```

---

## See Also

- [spawn launch](../reference/commands/launch.md) - Network flags
- [AWS VPC Documentation](https://docs.aws.amazon.com/vpc/)
- [How-To: SSH & Connectivity](ssh-connectivity.md) - Access patterns
- [How-To: Security & IAM](security-iam.md) - Network security
