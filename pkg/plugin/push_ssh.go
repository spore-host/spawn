package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// SSHTunnelPushClient pushes values to spored's push API via an SSH port-forward.
//
// Flow:
//  1. Read the bearer token via: ssh host sudo cat /var/lib/spored/push.token
//  2. Open SSH tunnel: -L <localPort>:127.0.0.1:7777
//  3. POST /v1/plugins/<name>/push with Bearer auth
type SSHTunnelPushClient struct {
	host    string // user@host or bare host
	keyPath string // optional path to SSH private key
	port    int    // local tunnel port (default 7777)
}

// NewSSHTunnelPushClient creates a push client using an SSH tunnel.
func NewSSHTunnelPushClient(host, keyPath string) *SSHTunnelPushClient {
	return &SSHTunnelPushClient{
		host:    host,
		keyPath: keyPath,
		port:    7777,
	}
}

// Push reads the remote token, opens an SSH tunnel, and POSTs the key/value.
func (c *SSHTunnelPushClient) Push(ctx context.Context, pluginName, key, value string) error {
	token, err := c.readRemoteToken(ctx)
	if err != nil {
		return fmt.Errorf("read push token from %s: %w", c.host, err)
	}
	return c.pushViaTunnel(ctx, token, pluginName, key, value)
}

// readRemoteToken reads /var/lib/spored/push.token from the remote host over SSH.
func (c *SSHTunnelPushClient) readRemoteToken(ctx context.Context) (string, error) {
	args := c.baseSSHArgs()
	args = append(args, c.host, "sudo", "cat", "/var/lib/spored/push.token")

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh: %w — stderr: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// pushViaTunnel opens an SSH port-forward and POSTs to the push API.
func (c *SSHTunnelPushClient) pushViaTunnel(ctx context.Context, token, pluginName, key, value string) error {
	tunnelCtx, cancelTunnel := context.WithCancel(ctx)
	defer cancelTunnel()

	args := c.baseSSHArgs()
	args = append(args,
		"-N",
		"-L", fmt.Sprintf("%d:127.0.0.1:7777", c.port),
		"-o", "ExitOnForwardFailure=yes",
		c.host,
	)

	tunnel := exec.CommandContext(tunnelCtx, "ssh", args...)
	if err := tunnel.Start(); err != nil {
		return fmt.Errorf("start ssh tunnel: %w", err)
	}

	// Wait for the port-forward to become ready (max 5 s).
	tunnelAddr := fmt.Sprintf("127.0.0.1:%d", c.port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", tunnelAddr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	payload, err := json.Marshal(map[string]string{"key": key, "value": value})
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/v1/plugins/%s/push", c.port, pluginName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST push: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("push API returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *SSHTunnelPushClient) baseSSHArgs() []string {
	args := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
	}
	if c.keyPath != "" {
		args = append(args, "-i", c.keyPath)
	}
	return args
}
