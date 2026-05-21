package streaming

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

// TCPServer represents a TCP streaming server
type TCPServer struct {
	listener net.Listener
	port     int
	handler  func(conn net.Conn)
}

// NewTCPServer creates a new TCP server
func NewTCPServer(port int, handler func(conn net.Conn)) (*TCPServer, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	log.Printf("TCP server listening on port %d", port)

	return &TCPServer{
		listener: listener,
		port:     port,
		handler:  handler,
	}, nil
}

// Serve starts accepting connections
func (s *TCPServer) Serve(ctx context.Context) error {
	defer func() { _ = s.listener.Close() }()

	for {
		select {
		case <-ctx.Done():
			log.Println("TCP server shutting down")
			return ctx.Err()
		default:
			conn, err := s.listener.Accept()
			if err != nil {
				log.Printf("Accept error: %v", err)
				continue
			}

			log.Printf("Accepted connection from %s", conn.RemoteAddr())
			go s.handleConnection(conn)
		}
	}
}

func (s *TCPServer) handleConnection(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	s.handler(conn)
}

// TCPClient represents a TCP streaming client
type TCPClient struct {
	conn    net.Conn
	address string
}

// NewTCPClient creates a new TCP client and connects to the server
func NewTCPClient(address string, port int) (*TCPClient, error) {
	fullAddr := net.JoinHostPort(address, fmt.Sprintf("%d", port))

	log.Printf("Connecting to TCP server at %s", fullAddr)

	conn, err := net.DialTimeout("tcp", fullAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	log.Printf("Connected to %s", fullAddr)

	return &TCPClient{
		conn:    conn,
		address: fullAddr,
	}, nil
}

// Close closes the connection
func (c *TCPClient) Close() error {
	return c.conn.Close()
}

// Send sends data to the server
func (c *TCPClient) Send(data []byte) error {
	_, err := c.conn.Write(data)
	return err
}

// SendString sends a string to the server
func (c *TCPClient) SendString(s string) error {
	_, err := fmt.Fprintf(c.conn, "%s\n", s)
	return err
}

// Receive receives data from the server
func (c *TCPClient) Receive(buf []byte) (int, error) {
	return c.conn.Read(buf)
}

// ReceiveString receives a line from the server
func (c *TCPClient) ReceiveString() (string, error) {
	reader := bufio.NewReader(c.conn)
	return reader.ReadString('\n')
}

// StreamWriter provides a writer interface for streaming
type StreamWriter struct {
	conn net.Conn
}

// NewStreamWriter creates a new stream writer
func NewStreamWriter(conn net.Conn) *StreamWriter {
	return &StreamWriter{conn: conn}
}

// Write writes data to the stream
func (w *StreamWriter) Write(p []byte) (int, error) {
	return w.conn.Write(p)
}

// WriteChunk writes a length-prefixed chunk
func (w *StreamWriter) WriteChunk(data []byte) error {
	// Write length (4 bytes)
	length := uint32(len(data))
	lengthBytes := []byte{
		byte(length >> 24),
		byte(length >> 16),
		byte(length >> 8),
		byte(length),
	}

	if _, err := w.conn.Write(lengthBytes); err != nil {
		return err
	}

	// Write data
	if _, err := w.conn.Write(data); err != nil {
		return err
	}

	return nil
}

// StreamReader provides a reader interface for streaming
type StreamReader struct {
	conn net.Conn
}

// NewStreamReader creates a new stream reader
func NewStreamReader(conn net.Conn) *StreamReader {
	return &StreamReader{conn: conn}
}

// Read reads data from the stream
func (r *StreamReader) Read(p []byte) (int, error) {
	return r.conn.Read(p)
}

// ReadChunk reads a length-prefixed chunk
func (r *StreamReader) ReadChunk() ([]byte, error) {
	// Read length (4 bytes)
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(r.conn, lengthBytes); err != nil {
		return nil, err
	}

	length := uint32(lengthBytes[0])<<24 |
		uint32(lengthBytes[1])<<16 |
		uint32(lengthBytes[2])<<8 |
		uint32(lengthBytes[3])

	// Sanity check
	if length > 100*1024*1024 { // 100MB max
		return nil, fmt.Errorf("chunk too large: %d bytes", length)
	}

	// Read data
	data := make([]byte, length)
	if _, err := io.ReadFull(r.conn, data); err != nil {
		return nil, err
	}

	return data, nil
}

// LineStreamer provides line-by-line streaming
type LineStreamer struct {
	writer *bufio.Writer
	reader *bufio.Reader
}

// NewLineStreamer creates a new line streamer
func NewLineStreamer(conn net.Conn) *LineStreamer {
	return &LineStreamer{
		writer: bufio.NewWriter(conn),
		reader: bufio.NewReader(conn),
	}
}

// WriteLine writes a line to the stream
func (s *LineStreamer) WriteLine(line string) error {
	if _, err := s.writer.WriteString(line + "\n"); err != nil {
		return err
	}
	return s.writer.Flush()
}

// ReadLine reads a line from the stream
func (s *LineStreamer) ReadLine() (string, error) {
	return s.reader.ReadString('\n')
}

// ConnectionPool manages multiple TCP connections
type ConnectionPool struct {
	connections []*TCPClient
	addresses   []string
}

// NewConnectionPool creates a connection pool to multiple addresses
func NewConnectionPool(addresses []string, port int) (*ConnectionPool, error) {
	pool := &ConnectionPool{
		connections: make([]*TCPClient, 0, len(addresses)),
		addresses:   addresses,
	}

	for _, addr := range addresses {
		client, err := NewTCPClient(addr, port)
		if err != nil {
			// Close already opened connections
			pool.CloseAll()
			return nil, fmt.Errorf("connect to %s: %w", addr, err)
		}
		pool.connections = append(pool.connections, client)
	}

	log.Printf("Connection pool created with %d connections", len(pool.connections))
	return pool, nil
}

// Get returns the connection at the specified index
func (p *ConnectionPool) Get(index int) *TCPClient {
	if index < 0 || index >= len(p.connections) {
		return nil
	}
	return p.connections[index]
}

// Broadcast sends data to all connections
func (p *ConnectionPool) Broadcast(data []byte) error {
	for i, conn := range p.connections {
		if err := conn.Send(data); err != nil {
			return fmt.Errorf("send to connection %d (%s): %w", i, p.addresses[i], err)
		}
	}
	return nil
}

// CloseAll closes all connections in the pool
func (p *ConnectionPool) CloseAll() {
	for _, conn := range p.connections {
		_ = conn.Close()
	}
}

// Size returns the number of connections in the pool
func (p *ConnectionPool) Size() int {
	return len(p.connections)
}
