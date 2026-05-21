package streaming

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// Transport represents a network transport abstraction
type Transport interface {
	// Connect establishes connection to remote endpoint
	Connect(ctx context.Context, addr string) error

	// Send sends data to remote
	Send(data []byte) error

	// Receive receives data from remote
	Receive() ([]byte, error)

	// Close closes the transport
	Close() error

	// Type returns the transport type
	Type() TransportType
}

// TransportType identifies the underlying transport protocol
type TransportType string

const (
	TransportTCP  TransportType = "tcp"  // Standard TCP
	TransportQUIC TransportType = "quic" // QUIC for WAN
	TransportRDMA TransportType = "rdma" // RDMA/EFA for LAN
)

// TransportConfig configures transport selection and behavior
type TransportConfig struct {
	// LocalIP is the local IP address for peer detection
	LocalIP string

	// RemoteIP is the remote peer IP address
	RemoteIP string

	// Port is the connection port
	Port int

	// PreferredTransport overrides auto-detection
	PreferredTransport TransportType

	// EnableEFA enables RDMA/EFA if available
	EnableEFA bool

	// MaxRetries for connection attempts
	MaxRetries int
}

// SelectTransport automatically selects the best transport based on network topology
func SelectTransport(config TransportConfig) (Transport, error) {
	// If user specified transport, use it
	if config.PreferredTransport != "" {
		return createTransport(config.PreferredTransport, config)
	}

	// Auto-select based on topology
	transportType := autoDetectTransport(config)
	return createTransport(transportType, config)
}

func autoDetectTransport(config TransportConfig) TransportType {
	// Check if EFA is available and peers are in same AZ
	if config.EnableEFA && isEFAAvailable() && isSameAZ(config.LocalIP, config.RemoteIP) {
		return TransportRDMA
	}

	// Check if peers are in same region but different AZ
	if isSameRegion(config.LocalIP, config.RemoteIP) && !isSameAZ(config.LocalIP, config.RemoteIP) {
		return TransportQUIC // Better for intra-region WAN
	}

	// Default to TCP for cross-region or unknown topology
	return TransportTCP
}

func createTransport(transportType TransportType, config TransportConfig) (Transport, error) {
	switch transportType {
	case TransportTCP:
		return NewTCPTransport(config), nil
	case TransportQUIC:
		return NewQUICTransport(config)
	case TransportRDMA:
		return NewRDMATransport(config)
	default:
		return nil, fmt.Errorf("unknown transport type: %s", transportType)
	}
}

// TCPTransport implements Transport using standard TCP
type TCPTransport struct {
	config TransportConfig
	client *TCPClient
}

// NewTCPTransport creates a TCP transport
func NewTCPTransport(config TransportConfig) *TCPTransport {
	return &TCPTransport{config: config}
}

func (t *TCPTransport) Connect(ctx context.Context, addr string) error {
	// Parse address
	parts := strings.Split(addr, ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid address format: %s", addr)
	}

	host := parts[0]
	port := t.config.Port

	client, err := NewTCPClient(host, port)
	if err != nil {
		return fmt.Errorf("tcp connect: %w", err)
	}

	t.client = client
	return nil
}

func (t *TCPTransport) Send(data []byte) error {
	if t.client == nil {
		return fmt.Errorf("not connected")
	}
	return t.client.Send(data)
}

func (t *TCPTransport) Receive() ([]byte, error) {
	if t.client == nil {
		return nil, fmt.Errorf("not connected")
	}
	buf := make([]byte, 65536)
	n, err := t.client.Receive(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (t *TCPTransport) Close() error {
	if t.client != nil {
		return t.client.Close()
	}
	return nil
}

func (t *TCPTransport) Type() TransportType {
	return TransportTCP
}

// QUICTransport implements Transport using QUIC (placeholder)
type QUICTransport struct {
	// TODO: Add quic-go connection
}

// NewQUICTransport creates a QUIC transport
func NewQUICTransport(config TransportConfig) (*QUICTransport, error) {
	// TODO: Implement QUIC using quic-go library
	return nil, fmt.Errorf("QUIC transport not yet implemented")
}

func (t *QUICTransport) Connect(ctx context.Context, addr string) error {
	return fmt.Errorf("not implemented")
}

func (t *QUICTransport) Send(data []byte) error {
	return fmt.Errorf("not implemented")
}

func (t *QUICTransport) Receive() ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

func (t *QUICTransport) Close() error {
	return fmt.Errorf("not implemented")
}

func (t *QUICTransport) Type() TransportType {
	return TransportQUIC
}

// RDMATransport implements Transport using RDMA/EFA (placeholder)
type RDMATransport struct {
	// TODO: Add libfabric bindings
}

// NewRDMATransport creates an RDMA transport
func NewRDMATransport(config TransportConfig) (*RDMATransport, error) {
	// TODO: Implement RDMA using libfabric
	return nil, fmt.Errorf("RDMA transport not yet implemented")
}

func (t *RDMATransport) Connect(ctx context.Context, addr string) error {
	return fmt.Errorf("not implemented")
}

func (t *RDMATransport) Send(data []byte) error {
	return fmt.Errorf("not implemented")
}

func (t *RDMATransport) Receive() ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

func (t *RDMATransport) Close() error {
	return fmt.Errorf("not implemented")
}

func (t *RDMATransport) Type() TransportType {
	return TransportRDMA
}

// Helper functions for topology detection

func isEFAAvailable() bool {
	// Check if EFA device is present
	// TODO: Read from /sys/class/infiniband/*/device/vendor
	return false
}

func isSameAZ(ip1, ip2 string) bool {
	// AWS IPs in same AZ typically have low RTT (<1ms)
	// Simple heuristic: same /24 subnet
	subnet1 := ipToSubnet(ip1, 24)
	subnet2 := ipToSubnet(ip2, 24)
	return subnet1 == subnet2
}

func isSameRegion(ip1, ip2 string) bool {
	// AWS IPs in same region typically have low RTT (<5ms)
	// Heuristic: same /16 subnet (not perfect but reasonable)
	subnet1 := ipToSubnet(ip1, 16)
	subnet2 := ipToSubnet(ip2, 16)
	return subnet1 == subnet2
}

func ipToSubnet(ip string, bits int) string {
	addr := net.ParseIP(ip)
	if addr == nil {
		return ""
	}

	ipv4 := addr.To4()
	if ipv4 == nil {
		return ""
	}

	mask := net.CIDRMask(bits, 32)
	subnet := ipv4.Mask(mask)
	return subnet.String()
}

// TransportPool manages multiple transport connections for fan-out/fan-in
type TransportPool struct {
	transports []Transport
	config     TransportConfig
}

// NewTransportPool creates a pool of transports to multiple peers
func NewTransportPool(config TransportConfig, peerIPs []string) (*TransportPool, error) {
	pool := &TransportPool{
		transports: make([]Transport, 0, len(peerIPs)),
		config:     config,
	}

	for _, peerIP := range peerIPs {
		peerConfig := config
		peerConfig.RemoteIP = peerIP

		transport, err := SelectTransport(peerConfig)
		if err != nil {
			_ = pool.Close()
			return nil, fmt.Errorf("create transport for %s: %w", peerIP, err)
		}

		pool.transports = append(pool.transports, transport)
	}

	return pool, nil
}

// Broadcast sends data to all transports in the pool
func (p *TransportPool) Broadcast(data []byte) error {
	for i, transport := range p.transports {
		if err := transport.Send(data); err != nil {
			return fmt.Errorf("send to transport %d: %w", i, err)
		}
	}
	return nil
}

// Close closes all transports in the pool
func (p *TransportPool) Close() error {
	for _, transport := range p.transports {
		if err := transport.Close(); err != nil {
			return err
		}
	}
	return nil
}

// GetTransportTypes returns the types of all transports in the pool
func (p *TransportPool) GetTransportTypes() []TransportType {
	types := make([]TransportType, len(p.transports))
	for i, transport := range p.transports {
		types[i] = transport.Type()
	}
	return types
}
