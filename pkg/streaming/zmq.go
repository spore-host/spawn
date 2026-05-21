//go:build zmq
// +build zmq

package streaming

import (
	"context"
	"fmt"
	"strings"
	"time"

	zmq "github.com/pebbe/zmq4"
)

// ZMQTransport wraps ZeroMQ socket with pipeline-friendly patterns
type ZMQTransport struct {
	socket    *zmq.Socket
	pattern   ZMQPattern
	endpoint  string
	ctx       *zmq.Context
	connected bool
}

// ZMQPattern represents ZeroMQ communication patterns
type ZMQPattern string

const (
	// Push/Pull - Unidirectional pipeline stages
	ZMQPush ZMQPattern = "PUSH" // Sender
	ZMQPull ZMQPattern = "PULL" // Receiver

	// Pub/Sub - Fan-out broadcasting
	ZMQPub ZMQPattern = "PUB" // Publisher
	ZMQSub ZMQPattern = "SUB" // Subscriber

	// Dealer/Router - Load balancing
	ZMQDealer ZMQPattern = "DEALER" // Client
	ZMQRouter ZMQPattern = "ROUTER" // Server
)

// ZMQConfig configures ZeroMQ transport
type ZMQConfig struct {
	Pattern       ZMQPattern
	Endpoint      string // e.g., "tcp://10.0.1.50:5555"
	HighWaterMark int    // Message buffer size (0 = unlimited)
	LingerMs      int    // Time to wait for pending messages on close
	Subscribe     string // For SUB sockets, topic filter ("" = all)
}

// NewZMQTransport creates a new ZeroMQ transport
func NewZMQTransport(config ZMQConfig) (*ZMQTransport, error) {
	ctx, err := zmq.NewContext()
	if err != nil {
		return nil, fmt.Errorf("create zmq context: %w", err)
	}

	var socketType zmq.Type
	switch config.Pattern {
	case ZMQPush:
		socketType = zmq.PUSH
	case ZMQPull:
		socketType = zmq.PULL
	case ZMQPub:
		socketType = zmq.PUB
	case ZMQSub:
		socketType = zmq.SUB
	case ZMQDealer:
		socketType = zmq.DEALER
	case ZMQRouter:
		socketType = zmq.ROUTER
	default:
		return nil, fmt.Errorf("unknown pattern: %s", config.Pattern)
	}

	socket, err := ctx.NewSocket(socketType)
	if err != nil {
		ctx.Term()
		return nil, fmt.Errorf("create socket: %w", err)
	}

	// Set high water mark
	if config.HighWaterMark > 0 {
		socket.SetSndhwm(config.HighWaterMark)
		socket.SetRcvhwm(config.HighWaterMark)
	}

	// Set linger period
	if config.LingerMs >= 0 {
		socket.SetLinger(time.Duration(config.LingerMs) * time.Millisecond)
	}

	// Subscribe to topic for SUB sockets
	if config.Pattern == ZMQSub {
		if err := socket.SetSubscribe(config.Subscribe); err != nil {
			socket.Close()
			ctx.Term()
			return nil, fmt.Errorf("set subscribe: %w", err)
		}
	}

	return &ZMQTransport{
		socket:   socket,
		pattern:  config.Pattern,
		endpoint: config.Endpoint,
		ctx:      ctx,
	}, nil
}

// Connect connects to remote endpoint (for PUSH, SUB, DEALER)
func (t *ZMQTransport) Connect() error {
	if t.connected {
		return nil
	}

	if err := t.socket.Connect(t.endpoint); err != nil {
		return fmt.Errorf("connect to %s: %w", t.endpoint, err)
	}

	t.connected = true

	// Give PUB/SUB time to establish (slow joiner syndrome)
	if t.pattern == ZMQPub || t.pattern == ZMQSub {
		time.Sleep(100 * time.Millisecond)
	}

	return nil
}

// Bind binds to local endpoint (for PULL, PUB, ROUTER)
func (t *ZMQTransport) Bind() error {
	if t.connected {
		return nil
	}

	if err := t.socket.Bind(t.endpoint); err != nil {
		return fmt.Errorf("bind to %s: %w", t.endpoint, err)
	}

	t.connected = true

	// Give PUB/SUB time to establish
	if t.pattern == ZMQPub || t.pattern == ZMQSub {
		time.Sleep(100 * time.Millisecond)
	}

	return nil
}

// Send sends data (for PUSH, PUB, DEALER)
func (t *ZMQTransport) Send(data []byte) error {
	_, err := t.socket.SendBytes(data, 0)
	return err
}

// SendMultipart sends multipart message (for ROUTER, DEALER)
func (t *ZMQTransport) SendMultipart(parts [][]byte) error {
	strParts := make([]string, len(parts))
	for i, part := range parts {
		strParts[i] = string(part)
	}
	_, err := t.socket.SendMessage(strParts)
	return err
}

// Receive receives data (for PULL, SUB, DEALER)
func (t *ZMQTransport) Receive() ([]byte, error) {
	return t.socket.RecvBytes(0)
}

// ReceiveMultipart receives multipart message (for ROUTER, DEALER)
func (t *ZMQTransport) ReceiveMultipart() ([][]byte, error) {
	parts, err := t.socket.RecvMessageBytes(0)
	return parts, err
}

// ReceiveTimeout receives with timeout
func (t *ZMQTransport) ReceiveTimeout(timeout time.Duration) ([]byte, error) {
	t.socket.SetRcvtimeo(timeout)
	defer t.socket.SetRcvtimeo(0) // Reset to blocking
	return t.socket.RecvBytes(0)
}

// Close closes the transport
func (t *ZMQTransport) Close() error {
	if err := t.socket.Close(); err != nil {
		return err
	}
	return t.ctx.Term()
}

// ZMQPipeline provides high-level pipeline stage abstraction
type ZMQPipeline struct {
	upstream   *ZMQTransport
	downstream *ZMQTransport
}

// NewZMQPipelineStage creates a pipeline stage with upstream/downstream
func NewZMQPipelineStage(upstreamEndpoint, downstreamEndpoint string) (*ZMQPipeline, error) {
	// Create PULL socket for upstream
	upstream, err := NewZMQTransport(ZMQConfig{
		Pattern:  ZMQPull,
		Endpoint: upstreamEndpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("create upstream: %w", err)
	}

	if err := upstream.Bind(); err != nil {
		upstream.Close()
		return nil, fmt.Errorf("bind upstream: %w", err)
	}

	// Create PUSH socket for downstream
	downstream, err := NewZMQTransport(ZMQConfig{
		Pattern:  ZMQPush,
		Endpoint: downstreamEndpoint,
	})
	if err != nil {
		upstream.Close()
		return nil, fmt.Errorf("create downstream: %w", err)
	}

	if err := downstream.Connect(); err != nil {
		upstream.Close()
		downstream.Close()
		return nil, fmt.Errorf("connect downstream: %w", err)
	}

	return &ZMQPipeline{
		upstream:   upstream,
		downstream: downstream,
	}, nil
}

// Process receives from upstream, processes, and sends to downstream
func (p *ZMQPipeline) Process(ctx context.Context, processor func([]byte) ([]byte, error)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Receive from upstream
			data, err := p.upstream.ReceiveTimeout(1 * time.Second)
			if err != nil {
				// Check if timeout
				errMsg := err.Error()
				if strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "EAGAIN") {
					continue // Timeout, check context
				}
				return fmt.Errorf("receive: %w", err)
			}

			// Process
			result, err := processor(data)
			if err != nil {
				return fmt.Errorf("process: %w", err)
			}

			// Send to downstream
			if err := p.downstream.Send(result); err != nil {
				return fmt.Errorf("send: %w", err)
			}
		}
	}
}

// Close closes both upstream and downstream
func (p *ZMQPipeline) Close() error {
	err1 := p.upstream.Close()
	err2 := p.downstream.Close()

	if err1 != nil {
		return err1
	}
	return err2
}

// ZMQFanOut broadcasts to multiple downstream stages
type ZMQFanOut struct {
	publisher *ZMQTransport
}

// NewZMQFanOut creates a fan-out broadcaster
func NewZMQFanOut(endpoint string) (*ZMQFanOut, error) {
	pub, err := NewZMQTransport(ZMQConfig{
		Pattern:  ZMQPub,
		Endpoint: endpoint,
	})
	if err != nil {
		return nil, err
	}

	if err := pub.Bind(); err != nil {
		pub.Close()
		return nil, err
	}

	return &ZMQFanOut{publisher: pub}, nil
}

// Broadcast sends data to all subscribers
func (f *ZMQFanOut) Broadcast(data []byte) error {
	return f.publisher.Send(data)
}

// BroadcastTopic sends data with topic prefix
func (f *ZMQFanOut) BroadcastTopic(topic string, data []byte) error {
	msg := append([]byte(topic+" "), data...)
	return f.publisher.Send(msg)
}

// Close closes the publisher
func (f *ZMQFanOut) Close() error {
	return f.publisher.Close()
}

// ZMQFanIn collects from multiple upstream stages
type ZMQFanIn struct {
	subscribers []*ZMQTransport
}

// NewZMQFanIn creates a fan-in collector
func NewZMQFanIn(endpoints []string, topic string) (*ZMQFanIn, error) {
	subs := make([]*ZMQTransport, len(endpoints))

	for i, endpoint := range endpoints {
		sub, err := NewZMQTransport(ZMQConfig{
			Pattern:   ZMQSub,
			Endpoint:  endpoint,
			Subscribe: topic,
		})
		if err != nil {
			// Cleanup previously created sockets
			for j := 0; j < i; j++ {
				subs[j].Close()
			}
			return nil, err
		}

		if err := sub.Connect(); err != nil {
			sub.Close()
			for j := 0; j < i; j++ {
				subs[j].Close()
			}
			return nil, err
		}

		subs[i] = sub
	}

	return &ZMQFanIn{subscribers: subs}, nil
}

// Collect receives from any upstream (round-robin)
func (f *ZMQFanIn) Collect(timeout time.Duration) ([]byte, int, error) {
	// Use poller for efficient multi-socket receive
	poller := zmq.NewPoller()
	for _, sub := range f.subscribers {
		poller.Add(sub.socket, zmq.POLLIN)
	}

	polled, err := poller.Poll(timeout)
	if err != nil {
		return nil, -1, err
	}

	if len(polled) == 0 {
		return nil, -1, fmt.Errorf("timeout")
	}

	// Receive from first available socket
	for i, sub := range f.subscribers {
		if polled[i].Events&zmq.POLLIN != 0 {
			data, err := sub.Receive()
			return data, i, err
		}
	}

	return nil, -1, fmt.Errorf("no data available")
}

// Close closes all subscribers
func (f *ZMQFanIn) Close() error {
	for _, sub := range f.subscribers {
		if err := sub.Close(); err != nil {
			return err
		}
	}
	return nil
}
