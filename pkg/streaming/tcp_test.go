package streaming

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// freePort asks the OS for an available TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	// nosemgrep: go.lang.security.audit.net.bind_all.avoid-bind-to-all-interfaces -- test helper grabbing a free ephemeral port (:0) for a local loopback test; not a real listener.
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startServer launches a TCP server on a free port with the given handler and
// returns the port. The server is shut down when the test ends.
func startServer(t *testing.T, handler func(conn net.Conn)) int {
	t.Helper()
	port := freePort(t)
	srv, err := NewTCPServer(port, handler)
	if err != nil {
		t.Fatalf("NewTCPServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()

	// Wait until the port accepts connections.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return port
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not become ready")
	return 0
}

func TestTCPClientSendReceiveString(t *testing.T) {
	// Echo handler: read a line, write it back.
	port := startServer(t, func(conn net.Conn) {
		ls := NewLineStreamer(conn)
		line, err := ls.ReadLine()
		if err != nil {
			return
		}
		_ = ls.WriteLine("echo:" + line[:len(line)-1]) // strip trailing \n
	})

	client, err := NewTCPClient("127.0.0.1", port)
	if err != nil {
		t.Fatalf("NewTCPClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.SendString("hello"); err != nil {
		t.Fatalf("SendString: %v", err)
	}
	got, err := client.ReceiveString()
	if err != nil {
		t.Fatalf("ReceiveString: %v", err)
	}
	if got != "echo:hello\n" {
		t.Errorf("got %q, want %q", got, "echo:hello\n")
	}
}

func TestTCPClientSendReceiveBytes(t *testing.T) {
	port := startServer(t, func(conn net.Conn) {
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n]) // echo
	})

	client, err := NewTCPClient("127.0.0.1", port)
	if err != nil {
		t.Fatalf("NewTCPClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Send([]byte("raw-bytes")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	buf := make([]byte, 64)
	n, err := client.Receive(buf)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if string(buf[:n]) != "raw-bytes" {
		t.Errorf("got %q, want raw-bytes", buf[:n])
	}
}

func TestStreamWriterReaderChunk(t *testing.T) {
	// Server reads a length-prefixed chunk and echoes it back the same way.
	port := startServer(t, func(conn net.Conn) {
		r := NewStreamReader(conn)
		data, err := r.ReadChunk()
		if err != nil {
			return
		}
		w := NewStreamWriter(conn)
		_ = w.WriteChunk(data)
	})

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	w := NewStreamWriter(conn)
	payload := []byte("chunked payload of some length")
	if err := w.WriteChunk(payload); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	r := NewStreamReader(conn)
	got, err := r.ReadChunk()
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, payload)
	}
}

func TestStreamWriterRawWrite(t *testing.T) {
	port := startServer(t, func(conn net.Conn) {
		buf := make([]byte, 16)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
	})
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	w := NewStreamWriter(conn)
	if _, err := w.Write([]byte("rawwrite")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	r := NewStreamReader(conn)
	buf := make([]byte, 16)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "rawwrite" {
		t.Errorf("got %q, want rawwrite", buf[:n])
	}
}

func TestConnectionPool(t *testing.T) {
	// Echo handler. The readiness probe in startServer also hits this handler,
	// so it must be self-contained (no shared WaitGroup).
	handler := func(conn net.Conn) {
		buf := make([]byte, 32)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n])
	}
	port := startServer(t, handler)

	pool, err := NewConnectionPool([]string{"127.0.0.1", "127.0.0.1"}, port)
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	defer pool.CloseAll()

	if pool.Size() != 2 {
		t.Errorf("pool size = %d, want 2", pool.Size())
	}
	if pool.Get(0) == nil {
		t.Error("Get(0) returned nil")
	}
	if pool.Get(99) != nil {
		t.Error("Get(out-of-range) should return nil")
	}

	if err := pool.Broadcast([]byte("ping")); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	// Confirm each pooled connection echoes back.
	for i := 0; i < pool.Size(); i++ {
		buf := make([]byte, 8)
		n, err := pool.Get(i).Receive(buf)
		if err != nil {
			t.Fatalf("Receive from conn %d: %v", i, err)
		}
		if string(buf[:n]) != "ping" {
			t.Errorf("conn %d echoed %q, want ping", i, buf[:n])
		}
	}
}

func TestNewConnectionPool_DialFailure(t *testing.T) {
	// Port 1 on localhost should refuse — pool creation must fail and clean up.
	_, err := NewConnectionPool([]string{"127.0.0.1"}, 1)
	if err == nil {
		t.Error("expected error connecting to a refused port")
	}
}

func TestNewTCPClient_DialFailure(t *testing.T) {
	if _, err := NewTCPClient("127.0.0.1", 1); err == nil {
		t.Error("expected dial failure to a refused port")
	}
}

func TestReadChunk_TooLarge(t *testing.T) {
	// Server writes an oversized length prefix; reader must reject it.
	port := startServer(t, func(conn net.Conn) {
		// length = 0xFFFFFFFF (~4GB) > 100MB cap
		_, _ = conn.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	})
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	r := NewStreamReader(conn)
	if _, err := r.ReadChunk(); err == nil {
		t.Error("expected error for oversized chunk")
	}
}
