package streaming

import (
	"testing"
)

func TestTransportType_String(t *testing.T) {
	tests := []struct {
		name          string
		transportType TransportType
		expected      string
	}{
		{
			name:          "tcp",
			transportType: TransportTCP,
			expected:      "tcp",
		},
		{
			name:          "quic",
			transportType: TransportQUIC,
			expected:      "quic",
		},
		{
			name:          "rdma",
			transportType: TransportRDMA,
			expected:      "rdma",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.transportType) != tt.expected {
				t.Errorf("TransportType = %v, want %v", tt.transportType, tt.expected)
			}
		})
	}
}

func TestIpToSubnet(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		bits     int
		expected string
	}{
		{
			name:     "valid ipv4 /24",
			ip:       "10.0.1.5",
			bits:     24,
			expected: "10.0.1.0",
		},
		{
			name:     "valid ipv4 /16",
			ip:       "10.0.1.5",
			bits:     16,
			expected: "10.0.0.0",
		},
		{
			name:     "valid ipv4 /8",
			ip:       "10.0.1.5",
			bits:     8,
			expected: "10.0.0.0",
		},
		{
			name:     "different ip /24",
			ip:       "192.168.1.100",
			bits:     24,
			expected: "192.168.1.0",
		},
		{
			name:     "invalid ip",
			ip:       "invalid",
			bits:     24,
			expected: "",
		},
		{
			name:     "empty ip",
			ip:       "",
			bits:     24,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ipToSubnet(tt.ip, tt.bits)
			if result != tt.expected {
				t.Errorf("ipToSubnet(%q, %d) = %q, want %q", tt.ip, tt.bits, result, tt.expected)
			}
		})
	}
}

func TestIsSameAZ(t *testing.T) {
	tests := []struct {
		name     string
		ip1      string
		ip2      string
		expected bool
	}{
		{
			name:     "same /24 subnet",
			ip1:      "10.0.1.5",
			ip2:      "10.0.1.10",
			expected: true,
		},
		{
			name:     "different /24 subnet",
			ip1:      "10.0.1.5",
			ip2:      "10.0.2.5",
			expected: false,
		},
		{
			name:     "same ip",
			ip1:      "10.0.1.5",
			ip2:      "10.0.1.5",
			expected: true,
		},
		{
			name:     "different regions",
			ip1:      "10.0.1.5",
			ip2:      "172.16.1.5",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSameAZ(tt.ip1, tt.ip2)
			if result != tt.expected {
				t.Errorf("isSameAZ(%q, %q) = %v, want %v", tt.ip1, tt.ip2, result, tt.expected)
			}
		})
	}
}

func TestIsSameRegion(t *testing.T) {
	tests := []struct {
		name     string
		ip1      string
		ip2      string
		expected bool
	}{
		{
			name:     "same /16 subnet",
			ip1:      "10.0.1.5",
			ip2:      "10.0.200.10",
			expected: true,
		},
		{
			name:     "different /16 subnet",
			ip1:      "10.0.1.5",
			ip2:      "10.1.1.5",
			expected: false,
		},
		{
			name:     "same /24 implies same /16",
			ip1:      "10.0.1.5",
			ip2:      "10.0.1.10",
			expected: true,
		},
		{
			name:     "completely different networks",
			ip1:      "10.0.1.5",
			ip2:      "192.168.1.5",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSameRegion(tt.ip1, tt.ip2)
			if result != tt.expected {
				t.Errorf("isSameRegion(%q, %q) = %v, want %v", tt.ip1, tt.ip2, result, tt.expected)
			}
		})
	}
}

func TestAutoDetectTransport(t *testing.T) {
	tests := []struct {
		name     string
		config   TransportConfig
		expected TransportType
	}{
		{
			name: "default to tcp for cross-region",
			config: TransportConfig{
				LocalIP:  "10.0.1.5",
				RemoteIP: "172.16.1.5",
			},
			expected: TransportTCP,
		},
		{
			name: "tcp when efa disabled",
			config: TransportConfig{
				LocalIP:   "10.0.1.5",
				RemoteIP:  "10.0.1.10",
				EnableEFA: false,
			},
			expected: TransportTCP,
		},
		{
			name: "tcp for same region different az",
			config: TransportConfig{
				LocalIP:   "10.0.1.5",
				RemoteIP:  "10.0.2.5",
				EnableEFA: false,
			},
			expected: TransportQUIC, // Same region, different AZ
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := autoDetectTransport(tt.config)
			if result != tt.expected {
				t.Errorf("autoDetectTransport() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestCreateTransport(t *testing.T) {
	tests := []struct {
		name          string
		transportType TransportType
		wantErr       bool
		errContains   string
	}{
		{
			name:          "create tcp transport",
			transportType: TransportTCP,
			wantErr:       false,
		},
		{
			name:          "create quic transport fails (not implemented)",
			transportType: TransportQUIC,
			wantErr:       true,
			errContains:   "not yet implemented",
		},
		{
			name:          "create rdma transport fails (not implemented)",
			transportType: TransportRDMA,
			wantErr:       true,
			errContains:   "not yet implemented",
		},
		{
			name:          "unknown transport type",
			transportType: TransportType("unknown"),
			wantErr:       true,
			errContains:   "unknown transport type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := TransportConfig{
				LocalIP:  "10.0.1.5",
				RemoteIP: "10.0.1.10",
				Port:     8080,
			}

			transport, err := createTransport(tt.transportType, config)
			if (err != nil) != tt.wantErr {
				t.Errorf("createTransport() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil {
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Errorf("createTransport() error = %v, want error containing %q", err, tt.errContains)
				}
			}

			if !tt.wantErr && transport == nil {
				t.Error("createTransport() returned nil transport without error")
			}

			if !tt.wantErr && transport != nil {
				if transport.Type() != tt.transportType {
					t.Errorf("transport.Type() = %v, want %v", transport.Type(), tt.transportType)
				}
			}
		})
	}
}

func TestTCPTransport_Type(t *testing.T) {
	transport := NewTCPTransport(TransportConfig{})
	if transport.Type() != TransportTCP {
		t.Errorf("TCPTransport.Type() = %v, want %v", transport.Type(), TransportTCP)
	}
}

func TestTCPTransport_SendReceive_NotConnected(t *testing.T) {
	transport := NewTCPTransport(TransportConfig{})

	// Test Send without connection
	err := transport.Send([]byte("test"))
	if err == nil {
		t.Error("Send() should fail when not connected")
	}
	if !contains(err.Error(), "not connected") {
		t.Errorf("Send() error = %v, want error containing 'not connected'", err)
	}

	// Test Receive without connection
	_, err = transport.Receive()
	if err == nil {
		t.Error("Receive() should fail when not connected")
	}
	if !contains(err.Error(), "not connected") {
		t.Errorf("Receive() error = %v, want error containing 'not connected'", err)
	}
}

func TestTCPTransport_Close_WhenNotConnected(t *testing.T) {
	transport := NewTCPTransport(TransportConfig{})

	// Closing when not connected should not error
	err := transport.Close()
	if err != nil {
		t.Errorf("Close() error = %v, want nil when not connected", err)
	}
}

func TestTransportConfig_Defaults(t *testing.T) {
	config := TransportConfig{
		LocalIP:  "10.0.1.5",
		RemoteIP: "10.0.1.10",
	}

	// Test that zero values work
	if config.Port != 0 {
		t.Errorf("Default Port = %d, want 0", config.Port)
	}
	if config.MaxRetries != 0 {
		t.Errorf("Default MaxRetries = %d, want 0", config.MaxRetries)
	}
	if config.EnableEFA {
		t.Error("Default EnableEFA should be false")
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
