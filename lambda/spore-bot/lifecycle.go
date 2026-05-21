package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// extendTTL adds duration to the instance's current TTL by updating the spawn:ttl EC2 tag.
// Usage: /spore extend <name> <duration>  e.g. /spore extend rstudio 2h
func extendTTL(ctx context.Context, client *ec2.Client, reg *BotRegistration, durationStr, slashCmd string) (string, error) {
	if durationStr == "" {
		return fmt.Sprintf("Usage: `%s extend <name> <duration>`\nExample: `%s extend %s 2h`",
			slashCmd, slashCmd, reg.Nickname), nil
	}

	extension, err := time.ParseDuration(durationStr)
	if err != nil || extension <= 0 {
		return fmt.Sprintf("❌ Invalid duration `%s`. Use a format like `2h`, `30m`, or `1h30m`.", durationStr), nil
	}

	// Get current TTL tag
	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{reg.InstanceID},
	})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("describe instance: %w", err)
	}
	inst := out.Reservations[0].Instances[0]

	var currentTTL time.Duration
	var launchTime time.Time
	if inst.LaunchTime != nil {
		launchTime = *inst.LaunchTime
	}
	for _, tag := range inst.Tags {
		if tag.Key == nil || tag.Value == nil {
			continue
		}
		if *tag.Key == reg.TagPrefix+":ttl" {
			currentTTL, _ = time.ParseDuration(*tag.Value)
		}
	}

	// New TTL = existing TTL + extension (relative to launch time)
	newTTL := currentTTL + extension
	newTTLStr := formatTTLDuration(newTTL)

	_, err = client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{reg.InstanceID},
		Tags: []ec2types.Tag{
			{Key: aws.String(reg.TagPrefix + ":ttl"), Value: aws.String(newTTLStr)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("update TTL tag: %w", err)
	}

	// Calculate new termination time
	newTerminateAt := launchTime.Add(newTTL)
	remaining := time.Until(newTerminateAt)

	return fmt.Sprintf("⏱️ Extended *%s* TTL by %s.\n  New deadline: %s (%s remaining)",
		reg.Nickname,
		durationStr,
		newTerminateAt.UTC().Format("2 Jan 15:04 UTC"),
		formatHMS(remaining)), nil
}

// setIdleTimeout updates (or removes) the spawn:idle-timeout EC2 tag.
// Usage: /spore idle <name> <duration|off>  e.g. /spore idle rstudio 30m
func setIdleTimeout(ctx context.Context, client *ec2.Client, reg *BotRegistration, durationStr, slashCmd string) (string, error) {
	if durationStr == "" {
		return fmt.Sprintf("Usage: `%s idle <name> <duration|off>`\nExamples: `%s idle %s 30m` or `%s idle %s off`",
			slashCmd, slashCmd, reg.Nickname, slashCmd, reg.Nickname), nil
	}

	if durationStr == "off" || durationStr == "none" || durationStr == "disable" {
		// Remove the idle timeout tag
		_, err := client.DeleteTags(ctx, &ec2.DeleteTagsInput{
			Resources: []string{reg.InstanceID},
			Tags: []ec2types.Tag{
				{Key: aws.String(reg.TagPrefix + ":idle-timeout")},
			},
		})
		if err != nil {
			return "", fmt.Errorf("remove idle timeout tag: %w", err)
		}
		return fmt.Sprintf("💤 Idle timeout disabled for *%s*.", reg.Nickname), nil
	}

	d, err := time.ParseDuration(durationStr)
	if err != nil || d <= 0 {
		return fmt.Sprintf("❌ Invalid duration `%s`. Use a format like `30m`, `1h`, or `off` to disable.", durationStr), nil
	}

	_, err = client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{reg.InstanceID},
		Tags: []ec2types.Tag{
			{Key: aws.String(reg.TagPrefix + ":idle-timeout"), Value: aws.String(durationStr)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("update idle timeout tag: %w", err)
	}

	return fmt.Sprintf("💤 *%s* will stop after %s of inactivity.", reg.Nickname, durationStr), nil
}

// formatTTLDuration formats a duration as a clean string for the spawn:ttl tag.
func formatTTLDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d.Hours() >= 1 && d == d.Round(time.Hour) {
		return fmt.Sprintf("%.0fh", d.Hours())
	}
	if d.Minutes() >= 1 && d == d.Round(time.Minute) {
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 && m > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", m)
}
