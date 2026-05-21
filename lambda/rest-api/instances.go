package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

func handleListInstances(ctx context.Context, cfg aws.Config, req events.APIGatewayV2HTTPRequest, p *Principal) (events.APIGatewayV2HTTPResponse, error) {
	region := req.QueryStringParameters["region"]
	state := req.QueryStringParameters["state"]
	if state == "" {
		state = "running"
	}

	client := spawnclient.NewClientFromConfig(cfg)
	instances, err := client.ListInstances(ctx, region, state)
	if err != nil {
		return errResp(http.StatusInternalServerError, fmt.Sprintf("list instances: %v", err)), nil
	}

	type instanceOut struct {
		InstanceID       string            `json:"instance_id"`
		Name             string            `json:"name"`
		InstanceType     string            `json:"instance_type"`
		State            string            `json:"state"`
		Region           string            `json:"region"`
		AvailabilityZone string            `json:"availability_zone,omitempty"`
		PublicIP         string            `json:"public_ip,omitempty"`
		DNS              string            `json:"dns,omitempty"`
		LaunchTime       time.Time         `json:"launch_time"`
		TTL              string            `json:"ttl,omitempty"`
		IdleTimeout      string            `json:"idle_timeout,omitempty"`
		Tags             map[string]string `json:"tags,omitempty"`
	}

	out := make([]instanceOut, 0, len(instances))
	for _, inst := range instances {
		dns := inst.Tags["spawn:dns-name"]
		out = append(out, instanceOut{
			InstanceID:       inst.InstanceID,
			Name:             inst.Name,
			InstanceType:     inst.InstanceType,
			State:            inst.State,
			Region:           inst.Region,
			AvailabilityZone: inst.AvailabilityZone,
			PublicIP:         inst.PublicIP,
			DNS:              dns,
			LaunchTime:       inst.LaunchTime,
			TTL:              inst.TTL,
			IdleTimeout:      inst.IdleTimeout,
		})
	}
	return jsonResp(http.StatusOK, map[string]any{"instances": out, "count": len(out)}), nil
}

// LaunchRequest is the JSON body for POST /v1/instances.
// Only InstanceType, Region, and AMI are required; all lifecycle fields are optional.
type LaunchRequest struct {
	Name            string `json:"name"`
	InstanceType    string `json:"instance_type"`
	Region          string `json:"region"`
	AMI             string `json:"ami"`
	KeyName         string `json:"key_name,omitempty"`
	Spot            bool   `json:"spot,omitempty"`
	TTL             string `json:"ttl,omitempty"`
	IdleTimeout     string `json:"idle_timeout,omitempty"`
	OnComplete      string `json:"on_complete,omitempty"`
	PreStop         string `json:"pre_stop,omitempty"`
	CompletionFile  string `json:"completion_file,omitempty"`
	SlackWorkspace  string `json:"slack_workspace,omitempty"`
	ActiveProcesses string `json:"active_processes,omitempty"`
}

func handleLaunch(ctx context.Context, cfg aws.Config, req events.APIGatewayV2HTTPRequest, p *Principal) (events.APIGatewayV2HTTPResponse, error) {
	var body LaunchRequest
	if err := parseJSON(req.Body, &body); err != nil {
		return errResp(http.StatusBadRequest, "invalid JSON body"), nil
	}
	if body.InstanceType == "" || body.Region == "" {
		return errResp(http.StatusBadRequest, "instance_type and region are required"), nil
	}

	lc := spawnclient.LaunchConfig{
		Name:               body.Name,
		InstanceType:       body.InstanceType,
		Region:             body.Region,
		AMI:                body.AMI,
		KeyName:            body.KeyName,
		Spot:               body.Spot,
		TTL:                body.TTL,
		IdleTimeout:        body.IdleTimeout,
		OnComplete:         body.OnComplete,
		PreStop:            body.PreStop,
		CompletionFile:     body.CompletionFile,
		SlackWorkspaceID:   body.SlackWorkspace,
		ActiveProcessesRaw: body.ActiveProcesses,
	}

	// Inject notification URL for hosted spore.host
	if body.SlackWorkspace != "" && lc.NotifyURL == "" {
		lc.NotifyURL = spawnconfig.GetNotifyURL()
		lc.NotifyCommand = "/spore"
	}

	client := spawnclient.NewClientFromConfig(cfg)
	result, err := client.Launch(ctx, lc)
	if err != nil {
		return errResp(http.StatusInternalServerError, fmt.Sprintf("launch failed: %v", err)), nil
	}

	return jsonResp(http.StatusCreated, map[string]any{
		"instance_id":       result.InstanceID,
		"name":              result.Name,
		"public_ip":         result.PublicIP,
		"private_ip":        result.PrivateIP,
		"availability_zone": result.AvailabilityZone,
		"state":             result.State,
		"key_name":          result.KeyName,
		"region":            body.Region,
	}), nil
}

func handleGetInstance(ctx context.Context, cfg aws.Config, id string, p *Principal) (events.APIGatewayV2HTTPResponse, error) {
	client := spawnclient.NewClientFromConfig(cfg)
	instances, err := client.ListInstances(ctx, "", "")
	if err != nil {
		return errResp(http.StatusInternalServerError, fmt.Sprintf("list: %v", err)), nil
	}
	for _, inst := range instances {
		if inst.InstanceID == id || strings.EqualFold(inst.Name, id) {
			return jsonResp(http.StatusOK, inst), nil
		}
	}
	return errResp(http.StatusNotFound, fmt.Sprintf("instance %q not found", id)), nil
}

func handleInstanceAction(ctx context.Context, cfg aws.Config, id, action string, req events.APIGatewayV2HTTPRequest, p *Principal) (events.APIGatewayV2HTTPResponse, error) {
	client := spawnclient.NewClientFromConfig(cfg)

	// Resolve instance
	instances, err := client.ListInstances(ctx, "", "")
	if err != nil {
		return errResp(http.StatusInternalServerError, "list instances failed"), nil
	}
	var target *spawnclient.InstanceInfo
	for i := range instances {
		if instances[i].InstanceID == id || strings.EqualFold(instances[i].Name, id) {
			target = &instances[i]
			break
		}
	}
	if target == nil {
		return errResp(http.StatusNotFound, fmt.Sprintf("instance %q not found", id)), nil
	}

	switch action {
	case "stop":
		if err := client.StopInstance(ctx, target.Region, target.InstanceID, false); err != nil {
			return errResp(http.StatusInternalServerError, fmt.Sprintf("stop failed: %v", err)), nil
		}
		return jsonResp(http.StatusOK, map[string]string{"status": "stopped", "instance_id": target.InstanceID}), nil

	case "hibernate":
		if err := client.StopInstance(ctx, target.Region, target.InstanceID, true); err != nil {
			return errResp(http.StatusInternalServerError, fmt.Sprintf("hibernate failed: %v", err)), nil
		}
		return jsonResp(http.StatusOK, map[string]string{"status": "hibernating", "instance_id": target.InstanceID}), nil

	case "start":
		if err := client.StartInstance(ctx, target.Region, target.InstanceID); err != nil {
			return errResp(http.StatusInternalServerError, fmt.Sprintf("start failed: %v", err)), nil
		}
		return jsonResp(http.StatusOK, map[string]string{"status": "starting", "instance_id": target.InstanceID}), nil

	case "terminate":
		if err := client.Terminate(ctx, target.Region, target.InstanceID); err != nil {
			return errResp(http.StatusInternalServerError, fmt.Sprintf("terminate failed: %v", err)), nil
		}
		return jsonResp(http.StatusOK, map[string]string{"status": "terminating", "instance_id": target.InstanceID}), nil

	case "extend":
		var body struct {
			Duration string `json:"duration"`
		}
		if req.Body != "" {
			if err := parseJSON(req.Body, &body); err != nil || body.Duration == "" {
				return errResp(http.StatusBadRequest, "body must include {\"duration\": \"2h\"}"), nil
			}
		} else {
			body.Duration = req.QueryStringParameters["duration"]
		}
		if body.Duration == "" {
			return errResp(http.StatusBadRequest, "duration required"), nil
		}
		if err := client.UpdateInstanceTags(ctx, target.Region, target.InstanceID, map[string]string{
			"spawn:ttl": body.Duration,
		}); err != nil {
			return errResp(http.StatusInternalServerError, fmt.Sprintf("extend failed: %v", err)), nil
		}
		return jsonResp(http.StatusOK, map[string]string{"status": "extended", "ttl": body.Duration, "instance_id": target.InstanceID}), nil

	default:
		return errResp(http.StatusBadRequest, fmt.Sprintf("unknown action %q — valid: stop, start, hibernate, terminate, extend", action)), nil
	}
}
