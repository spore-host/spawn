package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// AWS regions to query
var awsRegions = []string{
	"us-east-1",
	"us-east-2",
	"us-west-1",
	"us-west-2",
	"eu-west-1",
	"eu-west-2",
	"eu-central-1",
	"ap-southeast-1",
	"ap-southeast-2",
	"ap-northeast-1",
}

// listInstances queries all regions in parallel and returns spawn-managed instances.
// When teamID is non-empty, instances tagged with spawn:team-id=teamID are merged with
// the caller's personal instances (deduplicated by instance ID).
// callerCreds is the caller's own credentials, threaded through so cross-account
// EC2 reads are made AS the caller rather than as this Lambda (#815). nil when
// the request authenticated without usable credentials, in which case
// getEC2ClientForRegion refuses rather than falling back to this Lambda's role —
// which the member-account trust does not name, and which would be a standing
// cross-tenant read path if it did.
func listInstances(ctx context.Context, cfg aws.Config, callerCreds aws.CredentialsProvider, cliIamArn, teamID string) ([]InstanceInfo, error) {
	var wg sync.WaitGroup
	// Buffer twice the regions to hold both personal and team results per region
	bufSize := len(awsRegions)
	if teamID != "" {
		bufSize *= 2
	}
	instancesChan := make(chan []InstanceInfo, bufSize)
	errorsChan := make(chan error, bufSize)

	// Query all regions in parallel
	for _, region := range awsRegions {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()

			// Get EC2 client for region with cross-account credentials
			ec2Client, err := getEC2ClientForRegion(ctx, cfg, callerCreds, r)
			if err != nil {
				errorsChan <- fmt.Errorf("region %s: %w", r, err)
				return
			}

			// Personal instances: filter by spawn:managed and spawn:iam-user
			personalFilters := []types.Filter{
				{Name: aws.String("tag:spawn:managed"), Values: []string{"true"}},
				{Name: aws.String("tag:spawn:iam-user"), Values: []string{cliIamArn}},
			}
			result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
				Filters: personalFilters,
			})
			if err != nil {
				errorsChan <- fmt.Errorf("region %s: %w", r, err)
				return
			}
			instances := convertReservationsToInstances(result.Reservations, r, "")
			if len(instances) > 0 {
				instancesChan <- instances
			}

			// Team instances: additionally filter by spawn:team-id
			if teamID != "" {
				teamFilters := []types.Filter{
					{Name: aws.String("tag:spawn:managed"), Values: []string{"true"}},
					{Name: aws.String("tag:spawn:team-id"), Values: []string{teamID}},
				}
				teamResult, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
					Filters: teamFilters,
				})
				if err == nil {
					teamInstances := convertReservationsToInstances(teamResult.Reservations, r, "")
					if len(teamInstances) > 0 {
						instancesChan <- teamInstances
					}
				}
			}
		}(region)
	}

	// Wait for all goroutines
	go func() {
		wg.Wait()
		close(instancesChan)
		close(errorsChan)
	}()

	// Collect results, deduplicating by instance ID
	seen := make(map[string]struct{})
	var allInstances []InstanceInfo
	for instances := range instancesChan {
		for _, inst := range instances {
			if _, dup := seen[inst.InstanceID]; dup {
				continue
			}
			seen[inst.InstanceID] = struct{}{}
			allInstances = append(allInstances, inst)
		}
	}

	// Collect errors (don't fail on region errors, just log)
	for err := range errorsChan {
		log.Printf("warning: %v", err)
	}

	// Sort by launch time (newest first)
	sort.Slice(allInstances, func(i, j int) bool {
		return allInstances[i].LaunchTime.After(allInstances[j].LaunchTime)
	})

	return allInstances, nil
}

// getInstance gets a single instance by ID
// callerCreds is the caller's own credentials, threaded through so cross-account
// EC2 reads are made AS the caller rather than as this Lambda (#815). nil when
// the request authenticated without usable credentials, in which case
// getEC2ClientForRegion refuses rather than falling back to this Lambda's role —
// which the member-account trust does not name, and which would be a standing
// cross-tenant read path if it did.
func getInstance(ctx context.Context, cfg aws.Config, callerCreds aws.CredentialsProvider, instanceID, cliIamArn string) (*InstanceInfo, error) {
	// Try all regions in parallel until we find the instance
	var wg sync.WaitGroup
	instanceChan := make(chan *InstanceInfo, 1)
	foundMutex := sync.Mutex{}
	found := false

	for _, region := range awsRegions {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()

			// Check if already found
			foundMutex.Lock()
			if found {
				foundMutex.Unlock()
				return
			}
			foundMutex.Unlock()

			// Get EC2 client for region
			ec2Client, err := getEC2ClientForRegion(ctx, cfg, callerCreds, r)
			if err != nil {
				return
			}

			// Query EC2 for specific instance
			result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
				InstanceIds: []string{instanceID},
			})
			if err != nil {
				// Instance not in this region, continue
				return
			}

			if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
				return
			}

			// Found instance, verify it belongs to the calling user (per-user isolation)
			instance := result.Reservations[0].Instances[0]
			instanceUserID := getTagValue(instance.Tags, "spawn:iam-user")

			if instanceUserID != cliIamArn {
				// Instance exists but doesn't belong to this user
				return
			}

			// Convert to InstanceInfo
			instances := convertReservationsToInstances(result.Reservations, r, "")
			if len(instances) > 0 {
				foundMutex.Lock()
				if !found {
					found = true
					instanceChan <- &instances[0]
				}
				foundMutex.Unlock()
			}
		}(region)
	}

	// Wait for all goroutines
	go func() {
		wg.Wait()
		close(instanceChan)
	}()

	// Return first found instance
	if instance := <-instanceChan; instance != nil {
		return instance, nil
	}

	return nil, fmt.Errorf("instance not found or access denied")
}

// convertReservationsToInstances converts EC2 Reservations to InstanceInfo
func convertReservationsToInstances(reservations []types.Reservation, region, accountBase36 string) []InstanceInfo {
	var instances []InstanceInfo

	for _, reservation := range reservations {
		for _, instance := range reservation.Instances {
			info := InstanceInfo{
				InstanceID:       aws.ToString(instance.InstanceId),
				InstanceType:     string(instance.InstanceType),
				State:            string(instance.State.Name),
				Region:           region,
				AvailabilityZone: aws.ToString(instance.Placement.AvailabilityZone),
				PublicIP:         aws.ToString(instance.PublicIpAddress),
				PrivateIP:        aws.ToString(instance.PrivateIpAddress),
				LaunchTime:       aws.ToTime(instance.LaunchTime),
				SpotInstance:     instance.InstanceLifecycle == types.InstanceLifecycleTypeSpot,
				KeyName:          aws.ToString(instance.KeyName),
				Tags:             make(map[string]string),
			}

			// Extract name from tags
			info.Name = getTagValue(instance.Tags, "Name")

			// Extract spawn-specific tags
			info.TTL = getTagValue(instance.Tags, "spawn:ttl")
			info.IdleTimeout = getTagValue(instance.Tags, "spawn:idle-timeout")

			// Calculate TTL remaining (if TTL tag exists)
			if info.TTL != "" {
				ttlDuration, err := time.ParseDuration(info.TTL)
				if err == nil {
					elapsed := time.Since(info.LaunchTime)
					remaining := ttlDuration - elapsed
					if remaining > 0 {
						info.TTLRemainingSeconds = int(remaining.Seconds())
					} else {
						info.TTLRemainingSeconds = 0
					}
				}
			}

			// Construct DNS name
			dnsName := getTagValue(instance.Tags, "spawn:dns-name")
			if dnsName != "" {
				info.DNSName = getFullDNSName(dnsName, accountBase36)
			}

			// Copy all tags
			for _, tag := range instance.Tags {
				if tag.Key != nil && tag.Value != nil {
					info.Tags[*tag.Key] = *tag.Value
				}
			}

			instances = append(instances, info)
		}
	}

	return instances
}

// handleTerminateInstance handles DELETE /api/instances/{id}
// callerCreds is the caller's own credentials, threaded through so cross-account
// EC2 reads are made AS the caller rather than as this Lambda (#815). nil when
// the request authenticated without usable credentials, in which case
// getEC2ClientForRegion refuses rather than falling back to this Lambda's role —
// which the member-account trust does not name, and which would be a standing
// cross-tenant read path if it did.
func handleTerminateInstance(ctx context.Context, cfg aws.Config, callerCreds aws.CredentialsProvider, instanceID, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	// Find the instance across regions, verify ownership, then terminate
	var foundRegion string
	for _, region := range awsRegions {
		ec2Client, err := getEC2ClientForRegion(ctx, cfg, callerCreds, region)
		if err != nil {
			continue
		}

		result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			continue
		}

		if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
			continue
		}

		instance := result.Reservations[0].Instances[0]
		ownerTag := getTagValue(instance.Tags, "spawn:iam-user")
		if ownerTag != cliIamArn {
			return errorResponse(403, "Access denied"), nil
		}

		foundRegion = region
		if err := terminateCancellingSpot(ctx, ec2Client, instance); err != nil {
			return errorResponse(500, fmt.Sprintf("Failed to terminate instance: %v", err)), nil
		}
		break
	}

	if foundRegion == "" {
		return errorResponse(404, "Instance not found or access denied"), nil
	}

	return successResponse(map[string]interface{}{
		"success":     true,
		"message":     fmt.Sprintf("Instance %s terminated", instanceID),
		"instance_id": instanceID,
		"region":      foundRegion,
	})
}

// terminateAPI is the slice of the EC2 client terminateCancellingSpot uses.
type terminateAPI interface {
	CancelSpotInstanceRequests(ctx context.Context, in *ec2.CancelSpotInstanceRequestsInput, optFns ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// terminateCancellingSpot terminates the instance, cancelling its Spot request
// first. A --spot launch with a stop/hibernate lifecycle gets a PERSISTENT
// request (spawn#922), which reopens when its instance is terminated and makes
// EC2 launch a replacement. Cancelling never terminates anything, and cancelling
// a one-time request is harmless, so any request id the instance carries is
// cancelled. As in `spawn terminate`, an instance whose PERSISTENT request
// (spawn:spot-request-type=persistent) cannot be cancelled is not terminated —
// that would launch a replacement — and the error names the request to cancel.
// A failed cancel of a one-time request is only logged: it closes with its
// instance.
func terminateCancellingSpot(ctx context.Context, api terminateAPI, instance types.Instance) error {
	instanceID := aws.ToString(instance.InstanceId)
	if reqID := aws.ToString(instance.SpotInstanceRequestId); reqID != "" {
		if _, err := api.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
			SpotInstanceRequestIds: []string{reqID},
		}); err != nil && !strings.Contains(err.Error(), "InvalidSpotInstanceRequestID.NotFound") {
			if getTagValue(instance.Tags, "spawn:spot-request-type") == "persistent" {
				return fmt.Errorf("not terminating %s: its persistent spot request %s could not be "+
					"cancelled, and terminating would launch a replacement — cancel the request, then retry: %w",
					instanceID, reqID, err)
			}
			log.Printf("warning: could not cancel %s's one-time spot request %s: %v", instanceID, reqID, err)
		}
	}
	_, err := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	return err
}

// getTagValue extracts a tag value by key
func getTagValue(tags []types.Tag, key string) string {
	for _, tag := range tags {
		if tag.Key != nil && *tag.Key == key && tag.Value != nil {
			return *tag.Value
		}
	}
	return ""
}
