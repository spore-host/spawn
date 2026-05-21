package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// cmdNotify subscribes the calling user to lifecycle DM notifications for an instance.
// Usage: /spore notify <instance-name-or-id>
// The instance must be registered in the workspace by an admin first.
func cmdNotify(ctx context.Context, reg *Registry, action *BotAction) (string, error) {
	target := strings.TrimSpace(action.Nickname)
	if target == "" {
		return fmt.Sprintf("Usage: `%s notify <instance-name>`\n"+
			"Subscribe to DM notifications when an instance changes state.\n"+
			"The instance must be registered in this workspace first.\n"+
			"Use `%s unnotify <name>` to stop.",
			action.slashCmd(), action.slashCmd()), nil
	}

	// Find the instance across all workspace registrations
	instanceID, canonicalName, err := findWorkspaceInstance(ctx, reg, action.Platform, action.WorkspaceID, target)
	if err != nil {
		return "", err
	}
	if instanceID == "" {
		return fmt.Sprintf("❌ No instance named `%s` found in this workspace.\n"+
			"Instances must be registered first — ask your workspace admin to run `spawn bot register`.",
			target), nil
	}

	// If user already has a full (control) registration for this instance, they
	// already receive DMs — no need for a separate notify subscription.
	userRegs, err := reg.ListUserInstances(ctx, action.Platform, action.WorkspaceID, action.UserID)
	if err != nil {
		return "", err
	}
	for _, r := range userRegs {
		if r.InstanceID == instanceID && !r.NotifyOnly {
			return fmt.Sprintf("🔔 You already receive notifications for *%s* — you have control access to it.\nUse `%s list` to see your instances.", canonicalName, action.slashCmd()), nil
		}
		if r.InstanceID == instanceID && r.NotifyOnly {
			return fmt.Sprintf("🔔 You're already subscribed to notifications for *%s*.\nUse `%s unnotify %s` to stop.", canonicalName, action.slashCmd(), canonicalName), nil
		}
	}

	// Create a notify-only registration
	sub := &BotRegistration{
		UserKey:        userKey(action.Platform, action.WorkspaceID, action.UserID),
		Nickname:       "notify::" + canonicalName,
		InstanceID:     instanceID,
		Platform:       action.Platform,
		Enabled:        false,
		AllowedActions: []string{},
		NotifyOnly:     true,
		RegisteredBy:   action.UserID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if err := reg.PutRegistration(ctx, sub); err != nil {
		return "", fmt.Errorf("save notification subscription: %w", err)
	}

	return fmt.Sprintf("🔔 You'll receive DMs when *%s* changes state.\nUse `%s unnotify %s` to stop.",
		canonicalName, action.slashCmd(), canonicalName), nil
}

// cmdUnnotify removes a notification subscription.
// Usage: /spore unnotify <instance-name>
func cmdUnnotify(ctx context.Context, reg *Registry, action *BotAction) (string, error) {
	target := strings.TrimSpace(action.Nickname)
	if target == "" {
		return fmt.Sprintf("Usage: `%s unnotify <instance-name>`", action.slashCmd()), nil
	}

	// Nicknames for notify-only subscriptions are stored as "notify::<name>"
	nickname := "notify::" + target
	existing, _ := reg.GetInstance(ctx, action.Platform, action.WorkspaceID, action.UserID, nickname)
	if existing == nil {
		return fmt.Sprintf("❌ No notification subscription found for `%s`.\nUse `%s list` to see your subscriptions.", target, action.slashCmd()), nil
	}

	if err := reg.DeleteRegistration(ctx, action.Platform, action.WorkspaceID, action.UserID, nickname); err != nil {
		return "", fmt.Errorf("remove subscription: %w", err)
	}

	return fmt.Sprintf("🔕 Stopped notifications for *%s*.", target), nil
}

// findWorkspaceInstance searches all workspace registrations for an instance
// matching the given name, nickname, or instance ID.
// Returns the instance_id and canonical nickname (from the existing registration).
// Skips notify-only entries to avoid circular lookups.
func findWorkspaceInstance(ctx context.Context, reg *Registry, platform, workspaceID, target string) (instanceID, name string, err error) {
	regs, err := reg.ListWorkspaceRegistrations(ctx, platform, workspaceID)
	if err != nil {
		return "", "", fmt.Errorf("search workspace: %w", err)
	}
	for _, r := range regs {
		if r.NotifyOnly {
			continue
		}
		if strings.EqualFold(r.Nickname, target) || strings.EqualFold(r.InstanceID, target) {
			return r.InstanceID, r.Nickname, nil
		}
	}
	return "", "", nil
}
