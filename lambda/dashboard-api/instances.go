package main

import (
	"context"
	"fmt"
	"log"
	"sort"
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
// callerCreds is the caller's own credentials: EC2 is read with them, in the
// caller's own account, never with this Lambda's role (#815, #1585). nil when
// the request carried none that can read EC2, which refuses with
// errNoCallerCredentials before any call.
func listInstances(ctx context.Context, callerCreds aws.CredentialsProvider, cliIamArn, teamID string) ([]InstanceInfo, error) {
	if callerCreds == nil {
		return nil, errNoCallerCredentials
	}
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

			// EC2 client for the region, on the caller's credentials
			ec2Client, err := getEC2ClientForRegion(ctx, callerCreds, r)
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
// callerCreds is the caller's own credentials: EC2 is read with them, in the
// caller's own account, never with this Lambda's role (#815, #1585). nil when
// the request carried none that can read EC2, which refuses with
// errNoCallerCredentials before any call.
func getInstance(ctx context.Context, callerCreds aws.CredentialsProvider, instanceID, cliIamArn string) (*InstanceInfo, error) {
	if callerCreds == nil {
		return nil, errNoCallerCredentials
	}
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
			ec2Client, err := getEC2ClientForRegion(ctx, callerCreds, r)
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

			// Copy the tags, less the ones an older spored put a session
			// token in (withheldTags).
			for _, tag := range instance.Tags {
				if tag.Key != nil && tag.Value != nil && !withheldTag(*tag.Key) {
					info.Tags[*tag.Key] = *tag.Value
				}
			}

			instances = append(instances, info)
		}
	}

	return instances
}

// handleTerminateInstance handles DELETE /api/instances/{id}.
//
// The dashboard does not terminate instances (owner decision, spawn#1349): the
// 403 names `spawn terminate <id>`, which runs as the user. It used to look the
// instance up and call TerminateInstances on SpawnDashboardCrossAccountReadRole,
// which was always denied by design; with that role retired (#1585) and EC2
// read on the caller's own credentials, the same call could now succeed for a
// caller whose credentials allow it, so it is not made at all.
func handleTerminateInstance(instanceID string) (events.APIGatewayProxyResponse, error) {
	return errorResponse(403, terminateDeniedMessage(instanceID)), nil
}

// terminateDeniedMessage is what the dashboard says to a terminate request.
func terminateDeniedMessage(instanceID string) string {
	return fmt.Sprintf("Terminating from the dashboard is not available (spawn#1349). "+
		"Terminate it from the CLI: spawn terminate %s", instanceID)
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

// withheldTag reports whether a tag is left out of the API response. spored
// before session tokens moved off tags (pkg/accesstoken) published the DCV or
// web-app session token in spawn:ready-token and inside spawn:ready-url; an
// instance still running one keeps them until it restarts. A current spored's
// ready-url carries no token, but it is withheld too: the dashboard does not
// open sessions, and an old one is indistinguishable without parsing it.
func withheldTag(key string) bool {
	return key == "spawn:ready-token" || key == "spawn:ready-url"
}
