package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// PeerInfo contains information about a pipeline peer instance
type PeerInfo struct {
	StageID    string `json:"stage_id"`
	StageIndex int    `json:"stage_index"`
	InstanceID string `json:"instance_id"`
	PrivateIP  string `json:"private_ip"`
	PublicIP   string `json:"public_ip,omitempty"`
	DNSName    string `json:"dns_name"`
	State      string `json:"state"`
	Index      int    `json:"index"` // Index within stage (0, 1, 2, ...)
}

// PipelinePeerFile is the peer discovery file format
type PipelinePeerFile struct {
	PipelineID       string                `json:"pipeline_id"`
	StageID          string                `json:"stage_id"`
	StageIndex       int                   `json:"stage_index"`
	InstanceIndex    int                   `json:"instance_index"`
	StagePeers       []PeerInfo            `json:"stage_peers"`       // Peers in same stage
	UpstreamStages   map[string][]PeerInfo `json:"upstream_stages"`   // Stages this depends on
	DownstreamStages map[string][]PeerInfo `json:"downstream_stages"` // Stages that depend on this
	AllStages        map[string][]PeerInfo `json:"all_stages"`        // All stages in pipeline
}

// GeneratePeerDiscoveryFile generates the peer discovery file for a pipeline instance
// using the default AWS credential chain.
func GeneratePeerDiscoveryFile(ctx context.Context, pipelineID, stageID string, stageIndex, instanceIndex int, pipelineDef *Pipeline) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	return GeneratePeerDiscoveryFileWithAWSConfig(ctx, pipelineID, stageID, stageIndex, instanceIndex, pipelineDef, cfg)
}

// GeneratePeerDiscoveryFileWithAWSConfig generates the peer discovery file using
// an injected AWS config. Use this in tests to point at a Substrate emulator.
func GeneratePeerDiscoveryFileWithAWSConfig(ctx context.Context, pipelineID, stageID string, stageIndex, instanceIndex int, pipelineDef *Pipeline, awsCfg aws.Config) error {
	log.Printf("Generating peer discovery file for %s/%s (index %d)", pipelineID, stageID, instanceIndex)

	ec2Client := ec2.NewFromConfig(awsCfg)

	// Query all instances in the pipeline
	allInstances, err := queryPipelineInstances(ctx, ec2Client, pipelineID)
	if err != nil {
		return fmt.Errorf("query pipeline instances: %w", err)
	}

	// Group instances by stage
	stageMap := make(map[string][]PeerInfo)
	for _, peer := range allInstances {
		stageMap[peer.StageID] = append(stageMap[peer.StageID], peer)
	}

	// Sort peers within each stage by index
	for _, peers := range stageMap {
		sort.Slice(peers, func(i, j int) bool {
			return peers[i].Index < peers[j].Index
		})
	}

	// Get current stage definition
	stageDef := pipelineDef.GetStage(stageID)
	if stageDef == nil {
		return fmt.Errorf("stage %s not found in pipeline definition", stageID)
	}

	// Build upstream stages map
	upstreamStages := make(map[string][]PeerInfo)
	for _, depID := range stageDef.DependsOn {
		if peers, ok := stageMap[depID]; ok {
			upstreamStages[depID] = peers
		}
	}

	// Build downstream stages map (stages that depend on this stage)
	downstreamStages := make(map[string][]PeerInfo)
	for _, otherStage := range pipelineDef.Stages {
		for _, depID := range otherStage.DependsOn {
			if depID == stageID {
				if peers, ok := stageMap[otherStage.StageID]; ok {
					downstreamStages[otherStage.StageID] = peers
				}
				break
			}
		}
	}

	// Build peer file
	peerFile := PipelinePeerFile{
		PipelineID:       pipelineID,
		StageID:          stageID,
		StageIndex:       stageIndex,
		InstanceIndex:    instanceIndex,
		StagePeers:       stageMap[stageID],
		UpstreamStages:   upstreamStages,
		DownstreamStages: downstreamStages,
		AllStages:        stageMap,
	}

	// Write to file
	return WritePeerDiscoveryFile(&peerFile, "/etc/spawn/pipeline-peers.json")
}

func queryPipelineInstances(ctx context.Context, ec2Client *ec2.Client, pipelineID string) ([]PeerInfo, error) {
	// Query EC2 for all instances with the pipeline tag
	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String(tagprefix.FilterTag("pipeline-id")),
				Values: []string{pipelineID},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running"},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe instances: %w", err)
	}

	var peers []PeerInfo
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			peer := PeerInfo{
				InstanceID: *instance.InstanceId,
				State:      string(instance.State.Name),
			}

			if instance.PrivateIpAddress != nil {
				peer.PrivateIP = *instance.PrivateIpAddress
			}
			if instance.PublicIpAddress != nil {
				peer.PublicIP = *instance.PublicIpAddress
			}

			// Extract tags
			for _, tag := range instance.Tags {
				if tag.Key == nil || tag.Value == nil {
					continue
				}
				switch *tag.Key {
				case tagprefix.Tag("stage-id"):
					peer.StageID = *tag.Value
				case tagprefix.Tag("stage-index"):
					_, _ = fmt.Sscanf(*tag.Value, "%d", &peer.StageIndex)
				case tagprefix.Tag("instance-index"):
					_, _ = fmt.Sscanf(*tag.Value, "%d", &peer.Index)
				case tagprefix.Tag("dns-name"):
					peer.DNSName = *tag.Value
				}
			}

			peers = append(peers, peer)
		}
	}

	log.Printf("Found %d instances in pipeline %s", len(peers), pipelineID)
	return peers, nil
}

// WritePeerDiscoveryFile writes the peer discovery file to disk
func WritePeerDiscoveryFile(peerFile *PipelinePeerFile, path string) error {
	// Ensure directory exists
	dir := "/etc/spawn"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(peerFile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	// Write to file
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	log.Printf("Wrote peer discovery file to %s", path)
	return nil
}

// LoadPeerDiscoveryFile loads the peer discovery file from disk
func LoadPeerDiscoveryFile(path string) (*PipelinePeerFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	var peerFile PipelinePeerFile
	if err := json.Unmarshal(data, &peerFile); err != nil {
		return nil, fmt.Errorf("unmarshal JSON: %w", err)
	}

	return &peerFile, nil
}

// GetUpstreamPeers returns peers from a specific upstream stage
func (p *PipelinePeerFile) GetUpstreamPeers(stageID string) []PeerInfo {
	return p.UpstreamStages[stageID]
}

// GetDownstreamPeers returns peers from a specific downstream stage
func (p *PipelinePeerFile) GetDownstreamPeers(stageID string) []PeerInfo {
	return p.DownstreamStages[stageID]
}

// GetFirstUpstreamPeer returns the first peer from the first upstream stage (for simple pipelines)
func (p *PipelinePeerFile) GetFirstUpstreamPeer() *PeerInfo {
	for _, peers := range p.UpstreamStages {
		if len(peers) > 0 {
			return &peers[0]
		}
	}
	return nil
}

// GetMyStagePeers returns all peers in the same stage as this instance
func (p *PipelinePeerFile) GetMyStagePeers() []PeerInfo {
	return p.StagePeers
}

// GenerateHostfile generates an MPI-style hostfile from peer information
func (p *PipelinePeerFile) GenerateHostfile(path string, slotsPerHost int) error {
	var lines []string

	// Add all peers in the current stage
	for _, peer := range p.StagePeers {
		ip := peer.PrivateIP
		if ip == "" {
			ip = peer.DNSName
		}
		line := fmt.Sprintf("%s slots=%d", ip, slotsPerHost)
		lines = append(lines, line)
	}

	content := ""
	for _, line := range lines {
		content += line + "\n"
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write hostfile: %w", err)
	}

	log.Printf("Generated MPI hostfile: %s (%d hosts)", path, len(lines))
	return nil
}

// GetEnvironmentVariables returns environment variables for upstream peer access
func (p *PipelinePeerFile) GetEnvironmentVariables() map[string]string {
	env := make(map[string]string)

	// Add upstream stage variables
	for stageID, peers := range p.UpstreamStages {
		if len(peers) > 0 {
			// First peer (for simple cases)
			first := peers[0]
			envKey := fmt.Sprintf("UPSTREAM_%s", toEnvVarName(stageID))
			env[envKey] = first.PrivateIP
			env[envKey+"_DNS"] = first.DNSName

			// All peers (comma-separated)
			var ips []string
			var dnsNames []string
			for _, peer := range peers {
				ips = append(ips, peer.PrivateIP)
				dnsNames = append(dnsNames, peer.DNSName)
			}
			env[envKey+"_ALL"] = join(ips, ",")
			env[envKey+"_DNS_ALL"] = join(dnsNames, ",")
		}
	}

	// Add stage peer variables
	if len(p.StagePeers) > 0 {
		var ips []string
		var dnsNames []string
		for _, peer := range p.StagePeers {
			ips = append(ips, peer.PrivateIP)
			dnsNames = append(dnsNames, peer.DNSName)
		}
		env["STAGE_PEERS"] = join(ips, ",")
		env["STAGE_PEERS_DNS"] = join(dnsNames, ",")
		env["STAGE_PEER_COUNT"] = fmt.Sprintf("%d", len(p.StagePeers))
	}

	return env
}

func toEnvVarName(s string) string {
	// Convert stage ID to valid environment variable name
	result := ""
	for _, ch := range s {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			result += string(ch)
		} else {
			result += "_"
		}
	}
	// Convert to uppercase
	return strings.ToUpper(result)
}

func join(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for i := 1; i < len(strs); i++ {
		result += sep + strs[i]
	}
	return result
}
