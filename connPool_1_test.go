package vconnpool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// ============================================================================
// Mock Objects and Helper Functions
// ============================================================================

// mockNetAddr1 实现 net.Addr 接口
type mockNetAddr1 struct {
	netType string
	addrStr string
}

func (m *mockNetAddr1) Network() string { return m.netType }
func (m *mockNetAddr1) String() string  { return m.addrStr }

// newMockVConn1 创建 mockNetAddr1
func newMockVConn1(network, address string) *mockNetAddr1 {
	return &mockNetAddr1{netType: network, addrStr: address}
}

// mockDialer1 模拟 net.Dialer 接口
type mockDialer1 struct {
	// 用于控制 DialContext 返回的连接和错误
	nextConn        net.Conn
	nextErr         error
	simulateLatency time.Duration                                                        // 模拟拨号延迟
	dial            func(network, address string) (net.Conn, error)                      // 可选的自定义 Dial 实现
	dialContext     func(ctx context.Context, network, address string) (net.Conn, error) // 可选的自定义 DialContext 实现
	wg              *sync.WaitGroup
}

func (m *mockDialer1) Dial(network, address string) (net.Conn, error) {
	if m.dial != nil {
		return m.dial(network, address)
	}
	return m.DialContext(context.Background(), network, address)
}

func (m *mockDialer1) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if m.dialContext != nil {
		return m.dialContext(ctx, network, address)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if m.simulateLatency > 0 {
		time.Sleep(m.simulateLatency)
	}
	if m.nextErr != nil {
		return nil, m.nextErr
	}
	if m.nextConn != nil {
		conn := m.nextConn
		m.nextConn = nil // 每次Dial后清空，避免重复使用同一个conn
		return conn, nil
	}

	c1, c2 := net.Pipe()
	if m.wg != nil {
		m.wg.Add(1)
	}
	go echoServer(c2, m.wg) // 启动一个简单的 echo 服务器模拟对端
	return c1, nil
}

// echoServer 模拟一个简单的 Echo 服务器
// 它会从连接中读取数据并写回
func echoServer(conn net.Conn, wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}
	defer conn.Close()

	buf := make([]byte, 1024)
	for {
		conn.SetReadDeadline(time.Now().Add(time.Second)) // 短暂超时，以便检测连接关闭
		n, err := conn.Read(buf)
		if err != nil {
			if nErr, ok := err.(net.Error); ok && nErr.Timeout() {
				// 超时是正常的，继续循环等待数据
				continue
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return // 客户端关闭连接
			}
			return
		}
		if n > 0 {
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			_, err := conn.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}
}

// ============================================================================
// Tests for Addr
// ============================================================================

func TestAddr_Methods(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "localhost:8080"}

	t.Run("Network", func(t *testing.T) {
		if addr.Network() != "tcp" {
			t.Errorf("Expected network 'tcp', got %s", addr.Network())
		}
	})

	t.Run("String", func(t *testing.T) {
		if addr.String() != "localhost:8080" {
			t.Errorf("Expected string 'localhost:8080', got %s", addr.String())
		}
	})

	t.Run("NilAddr", func(t *testing.T) {
		var nilAddr Addr
		if nilAddr.Network() != "" {
			t.Errorf("Expected nil Addr Network to be '', got %s", nilAddr.Network())
		}
		if nilAddr.String() != "" {
			t.Errorf("Expected nil Addr String to be '', got %s", nilAddr.String())
		}
	})
}

// ============================================================================
// Tests for ResolveAddr
// ============================================================================

func TestResolveAddr(t *testing.T) {
	testCases := []struct {
		network string
		address string
		expect  string // Expected string representation of resolved address
		wantErr bool
	}{
		{"tcp", "127.0.0.1:80", "127.0.0.1:80", false},
		{"tcp4", "127.0.0.1:80", "127.0.0.1:80", false},
		{"udp", "127.0.0.1:53", "127.0.0.1:53", false},
		{"unix", "/tmp/sock", "/tmp/sock", false},
		{"unknown", "some_addr", "", true},
		{"tcp", "invalid:port", "", true},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s_%s", tc.network, tc.address), func(t *testing.T) {
			addr, err := ResolveAddr(tc.network, tc.address)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ResolveAddr(%s, %s) error = %v, wantErr %v", tc.network, tc.address, err, tc.wantErr)
			}
			if !tc.wantErr && addr.String() != tc.expect {
				t.Errorf("ResolveAddr(%s, %s) = %s, want %s", tc.network, tc.address, addr.String(), tc.expect)
			}
		})
	}
}

// ============================================================================
// Tests for ConnPool and Conn (connSingle)
// ============================================================================

func TestConnPool_DialAndClose(t *testing.T) {
	targetAddr := "localhost:12345"
	network := "tcp"

	var serverWG sync.WaitGroup
	// Mock Dialer: Each Dial call provides a fresh pipe connection
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	// Setup a ConnPool
	pool := &ConnPool{
		Dialer:     dialer,
		MaxConn:    10,
		IdeConn:    5,
		IdeTimeout: time.Second,
	}
	defer pool.Close()

	t.Run("DialNewConnection", func(t *testing.T) {
		conn, err := pool.Dial(network, targetAddr)
		if err != nil {
			t.Fatalf("Dial failed: %v", err)
		}
		if conn.(Conn).IsReuseConn() {
			t.Error("Expected new connection, but IsReuseConn returned true")
		}
		if pool.ConnNum() != 1 {
			t.Errorf("Expected ConnNum 1, got %d", pool.ConnNum())
		}

		// Use the connection
		testData := []byte("hello world")
		n, err := conn.Write(testData)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		if n != len(testData) {
			t.Errorf("Expected to write %d bytes, wrote %d", len(testData), n)
		}

		readBuf := make([]byte, len(testData))
		n, err = conn.Read(readBuf)
		if err != nil {
			t.Fatalf("Read failed: %v", err)
		}
		if n != len(testData) || !bytes.Equal(readBuf, testData) {
			t.Errorf("Expected to read '%s', got '%s'", testData, readBuf[:n])
		}

		// Close the connection (should be put back to pool)
		err = conn.Close()
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		if pool.ConnNum() != 1 { // Connection is in idle pool, still counted
			t.Errorf("Expected ConnNum 1 after close, got %d", pool.ConnNum())
		}
		if pool.ConnNumIde(network, targetAddr) != 1 {
			t.Errorf("Expected 1 idle connection, got %d", pool.ConnNumIde(network, targetAddr))
		}
	})

	t.Run("DialReuseConnection", func(t *testing.T) {
		// Should reuse the connection from the previous test
		conn, err := pool.Dial(network, targetAddr)
		if err != nil {
			t.Fatalf("Dial failed: %v", err)
		}
		if !conn.(Conn).IsReuseConn() {
			t.Error("Expected reused connection, but IsReuseConn returned false")
		}
		if pool.ConnNum() != 1 { // Still the same connection
			t.Errorf("Expected ConnNum 1, got %d", pool.ConnNum())
		}
		if pool.ConnNumIde(network, targetAddr) != 0 { // Connection is now active, not idle
			t.Errorf("Expected 0 idle connections, got %d", pool.ConnNumIde(network, targetAddr))
		}

		// Close it again
		err = conn.Close()
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		if pool.ConnNumIde(network, targetAddr) != 1 {
			t.Errorf("Expected 1 idle connection after reuse close, got %d", pool.ConnNumIde(network, targetAddr))
		}
	})

	t.Run("IdleTimeout", func(t *testing.T) {
		// Wait for the idle timeout to expire
		time.Sleep(1500 * time.Millisecond) // Slightly longer than IdeTimeout

		if pool.ConnNumIde(network, targetAddr) != 0 {
			t.Errorf("Expected 0 idle connections after timeout, got %d", pool.ConnNumIde(network, targetAddr))
		}
		if pool.ConnNum() != 0 {
			t.Errorf("Expected ConnNum 0 after timeout, got %d", pool.ConnNum())
		}
	})

	serverWG.Wait() // Ensure all echo servers are done
}

func TestConnPool_IdeConnLimit(t *testing.T) {
	targetAddr := "localhost:12347"
	network := "tcp"

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	pool := &ConnPool{
		Dialer:     dialer,
		MaxConn:    0, // No max total connections
		IdeConn:    1, // Max 1 idle connection
		IdeTimeout: time.Second,
	}
	defer pool.Close()

	conn1, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial 1 failed: %v", err)
	}

	conn2, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial 2 failed: %v", err)
	}
	if pool.ConnNum() != 2 {
		t.Errorf("Expected ConnNum 2, got %d", pool.ConnNum())
	}

	// Close conn1 (should go to idle pool)
	conn1.Close()
	if pool.ConnNumIde(network, targetAddr) != 1 {
		t.Errorf("Expected 1 idle connection, got %d", pool.ConnNumIde(network, targetAddr))
	}

	// Close conn2 (should be discarded due to IdeConn limit)
	conn2.Close()
	if pool.ConnNumIde(network, targetAddr) != 1 {
		t.Errorf("Expected still 1 idle connection (conn2 discarded), got %d", pool.ConnNumIde(network, targetAddr))
	}
	if pool.ConnNum() != 1 { // One was recycled, one discarded
		t.Errorf("Expected ConnNum 1 (conn2 discarded), got %d", pool.ConnNum())
	}

	serverWG.Wait()
}

func TestConnPool_Discard(t *testing.T) {
	targetAddr := "localhost:12348"
	network := "tcp"

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	pool := &ConnPool{
		Dialer:  dialer,
		MaxConn: 10,
		IdeConn: 10,
	}
	defer pool.Close()

	conn, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if pool.ConnNum() != 1 {
		t.Errorf("Expected ConnNum 1, got %d", pool.ConnNum())
	}

	// Mark connection for discard
	conn.(Conn).Discard()

	// Close the connection (should NOT be put back to pool)
	err = conn.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if pool.ConnNum() != 0 { // Should be 0 because it was discarded
		t.Errorf("Expected ConnNum 0 after discard and close, got %d", pool.ConnNum())
	}
	if pool.ConnNumIde(network, targetAddr) != 0 {
		t.Errorf("Expected 0 idle connections, got %d", pool.ConnNumIde(network, targetAddr))
	}

	serverWG.Wait()
}

func TestConnPool_RawConn(t *testing.T) {
	targetAddr := "localhost:12349"
	network := "tcp"

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	pool := &ConnPool{
		Dialer:  dialer,
		MaxConn: 10,
		IdeConn: 10,
	}
	defer pool.Close()

	conn, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if pool.ConnNum() != 1 {
		t.Errorf("Expected ConnNum 1, got %d", pool.ConnNum())
	}

	// Get raw connection
	rawConn := conn.(Conn).RawConn()
	if rawConn == nil {
		t.Fatal("RawConn returned nil")
	}
	if pool.ConnNum() != 0 { // Should be 0 because RawConn removes it from pool management
		t.Errorf("Expected ConnNum 0 after RawConn, got %d", pool.ConnNum())
	}
	if pool.ConnNumIde(network, targetAddr) != 0 {
		t.Errorf("Expected 0 idle connections, got %d", pool.ConnNumIde(network, targetAddr))
	}

	conn.Close()

	// Test writing to raw connection
	testData := []byte("raw hello")
	n, err := rawConn.Write(testData)
	if err != nil {
		t.Fatalf("RawConn Write failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Expected to write %d bytes to rawConn, wrote %d", len(testData), n)
	}

	readBuf := make([]byte, len(testData))
	n, err = rawConn.Read(readBuf)
	if err != nil {
		t.Fatalf("RawConn Read failed: %v", err)
	}
	if n != len(testData) || !bytes.Equal(readBuf, testData) {
		t.Errorf("Expected to read '%s' from rawConn, got '%s'", testData, readBuf[:n])
	}

	// Close the raw connection
	rawConn.Close()
	serverWG.Wait()

	serverWG.Add(1)
	t.Run("RawConn_PanicOnRepeatedCall", func(t *testing.T) {
		defer serverWG.Done()
		conn2, err := pool.Dial(network, targetAddr)
		if err != nil {
			t.Fatalf("Dial failed: %v", err)
		}
		conn := conn2.(Conn).RawConn() // First call is fine
		defer conn.Close()
		defer func() {
			if r := recover(); r == nil {
				t.Error("Expected RawConn to panic on second call, but it did not")
			}
		}()
		conn2.(Conn).RawConn() // Second call should panic
	})
	serverWG.Wait() // Wait for the second echo server if it was started
}

func TestConnPool_DialContext_Priority(t *testing.T) {
	targetAddr := "localhost:12350"
	network := "tcp"

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	pool := &ConnPool{
		Dialer:  dialer,
		MaxConn: 10,
		IdeConn: 10,
	}
	defer pool.Close()

	conn1, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	conn1.Close() // Put to pool

	// ConnNum should be 1, Ide is 1
	if pool.ConnNum() != 1 || pool.ConnNumIde(network, targetAddr) != 1 {
		t.Fatalf("Expected ConnNum 1 and Ide 1, got %d, %d", pool.ConnNum(), pool.ConnNumIde(network, targetAddr))
	}
	// Dial with priority context: should ignore pool and create new connection
	ctx := context.WithValue(context.Background(), PriorityContextKey, true)
	conn2, err := pool.DialContext(ctx, network, targetAddr)
	if err != nil {
		t.Fatalf("DialContext with priority failed: %v", err)
	}
	if conn2.(Conn).IsReuseConn() {
		t.Error("Expected new connection (priority), but IsReuseConn returned true")
	}
	if pool.ConnNum() != 2 { // Total connections should now be 2
		t.Errorf("Expected ConnNum 2, got %d", pool.ConnNum())
	}
	if pool.ConnNumIde(network, targetAddr) != 1 { // Idle connection should still be 1 (the first one)
		t.Errorf("Expected Ide 1, got %d", pool.ConnNumIde(network, targetAddr))
	}

	conn2.Close() // Put to pool
	if pool.ConnNum() != 2 {
		t.Errorf("Expected ConnNum 2 after conn2 close, got %d", pool.ConnNum())
	}
	if pool.ConnNumIde(network, targetAddr) != 2 {
		t.Errorf("Expected Ide 2, got %d", pool.ConnNumIde(network, targetAddr))
	}

	pool.CloseIdleConnections()
	serverWG.Wait()
}

func TestConnPool_DialContext_Cancelled(t *testing.T) {
	targetAddr := "localhost:12351"
	network := "tcp"

	dialer := &mockDialer1{
		simulateLatency: 100 * time.Millisecond, // Simulate slow dial
	}

	pool := &ConnPool{
		Dialer: dialer,
	}
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Immediately cancel the context

	_, err := pool.DialContext(ctx, network, targetAddr)
	if err == nil {
		t.Error("Expected DialContext to fail with cancelled context, got nil error")
	}
	// The error type might vary depending on OS and net.Dialer implementation,
	// but it should indicate cancellation.
	if !errors.Is(err, context.Canceled) && err.Error() != "context canceled" {
		t.Errorf("Expected context.Canceled error, got %v", err)
	}
	if pool.ConnNum() != 0 {
		t.Errorf("Expected ConnNum 0, got %d", pool.ConnNum())
	}
}

func TestConnPool_Add_Put(t *testing.T) {
	targetAddr := "localhost:12352"
	network := "tcp"

	pool := &ConnPool{
		Dialer: &mockDialer1{},
	}
	defer pool.Close()

	mockAddr := newMockVConn1(network, targetAddr)

	t.Run("Add_NewConn", func(t *testing.T) {
		conn1, err := pool.Dial(network, targetAddr)
		if err != nil {
			t.Fatal(err)
		}

		err = pool.Add(conn1) // Add a raw net.Conn
		if err != nil {
			conn1.Close()
			t.Fatalf("Add failed: %v", err)
		}
		if pool.ConnNum() != 1 {
			t.Errorf("Expected ConnNum 1, got %d", pool.ConnNum())
		}
		if pool.ConnNumIde(network, targetAddr) != 1 {
			t.Errorf("Expected 1 idle connection, got %d", pool.ConnNumIde(network, targetAddr))
		}

		conn, err := pool.Get(mockAddr)
		if err != nil {
			t.Fatalf("Get after Add failed: %v", err)
		}
		if pool.ConnNumIde(network, targetAddr) != 0 {
			t.Errorf("Expected 0 idle connection after Get, got %d", pool.ConnNumIde(network, targetAddr))
		}

		err = pool.Put(conn, mockAddr)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if pool.ConnNumIde(network, targetAddr) != 1 { // Should be returned to pool
			t.Errorf("Expected 1 idle connection after Get and Close, got %d", pool.ConnNumIde(network, targetAddr))
		}
		pool.CloseIdleConnections()
	})

	t.Run("Add_NilConn", func(t *testing.T) {
		err := pool.Add(nil)
		if err == nil {
			t.Error("Expected error for adding nil connection, got nil")
		}
	})

	t.Run("Put_NilConn", func(t *testing.T) {
		err := pool.Put(nil, mockAddr)
		if err == nil {
			t.Error("Expected error for putting nil connection, got nil")
		}
	})

	t.Run("Put_NilAddr", func(t *testing.T) {
		clientPipe, serverPipe := net.Pipe()
		serverPipe.Close() // No need for echo server
		err := pool.Put(clientPipe, nil)
		if err == nil {
			t.Error("Expected error for putting with nil address, got nil")
		}
		clientPipe.Close()
	})

	t.Run("Put_ErrConnAlreadyExists", func(t *testing.T) {
		pool.CloseIdleConnections()
		pool.Close()
		pool = &ConnPool{
			MaxConn: 10,
			IdeConn: 10,
		}
		defer pool.Close()

		clientPipe, serverPipe := net.Pipe()
		var serverWG sync.WaitGroup
		serverWG.Add(1)
		go echoServer(serverPipe, &serverWG)

		err := pool.Put(clientPipe, mockAddr) // First put
		if err != nil {
			t.Fatalf("First Put failed: %v", err)
		}

		err = pool.Put(clientPipe, mockAddr) // Second put of the same underlying connection
		if err != nil {                      // The code handles ErrConnAlreadyExists gracefully by returning nil
			t.Errorf("Expected nil error for duplicate put due to graceful handling, got %v", err)
		}
		if pool.ConnNumIde(network, targetAddr) != 1 { // Should still be 1 idle conn
			t.Errorf("Expected 1 idle conn after duplicate put, got %d", pool.ConnNumIde(network, targetAddr))
		}
		pool.CloseIdleConnections()
		serverWG.Wait()
	})
}

func TestConnPool_Get(t *testing.T) {
	targetAddr := "localhost:12353"
	network := "tcp"

	pool := &ConnPool{}
	defer pool.Close()

	mockAddr := newMockVConn1(network, targetAddr)

	t.Run("Get_NoAvailable", func(t *testing.T) {
		_, err := pool.Get(mockAddr)
		if !errors.Is(err, ErrConnNotAvailable) {
			t.Errorf("Expected ErrConnNotAvailable, got %v", err)
		}
	})

	t.Run("Get_AfterAdd", func(t *testing.T) {
		clientPipe, serverPipe := net.Pipe()
		var serverWG sync.WaitGroup
		serverWG.Add(1)
		go echoServer(serverPipe, &serverWG)

		err := pool.Put(clientPipe, mockAddr)
		if err != nil {
			t.Fatalf("Add failed: %v", err)
		}

		conn, err := pool.Get(mockAddr)
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if conn == nil {
			t.Fatal("Got nil connection")
		}
		if pool.ConnNumIde(network, targetAddr) != 0 {
			t.Errorf("Expected 0 idle connections after Get, got %d", pool.ConnNumIde(network, targetAddr))
		}
		if pool.ConnNum() != 0 { // Get removes it from pool's count, calling code should handle it.
			t.Errorf("Expected ConnNum 0 after Get, got %d", pool.ConnNum())
		}
		conn.Close() // Close the obtained conn
		pool.CloseIdleConnections()
		serverWG.Wait()
	})

	t.Run("Get_NilAddr", func(t *testing.T) {
		_, err := pool.Get(nil)
		if err == nil {
			t.Error("Expected error for Get with nil address, got nil")
		}
	})
}

func TestConnPool_CloseAndClosedState(t *testing.T) {
	pool := &ConnPool{}
	targetAddr := "localhost:12354"
	network := "tcp"

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}
	pool.Dialer = dialer

	conn, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	conn.Close() // Put it to pool
	if pool.ConnNum() != 1 || pool.ConnNumIde(network, targetAddr) != 1 {
		t.Fatalf("Pre-close: ConnNum %d, Ide %d", pool.ConnNum(), pool.ConnNumIde(network, targetAddr))
	}

	t.Run("Close", func(t *testing.T) {
		err := pool.Close()
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		if pool.ConnNum() != 0 {
			t.Errorf("Expected ConnNum 0 after Close, got %d", pool.ConnNum())
		}
		if pool.ConnNumIde(network, targetAddr) != 0 {
			t.Errorf("Expected 0 idle connections after Close, got %d", pool.ConnNumIde(network, targetAddr))
		}
	})

	t.Run("Close_Idempotent", func(t *testing.T) {
		err := pool.Close() // Calling Close again should be fine
		if err != nil {
			t.Errorf("Second Close failed: %v", err)
		}
	})

	t.Run("OperationsOnClosedPool", func(t *testing.T) {
		_, err := pool.Dial(network, targetAddr)
		if !errors.Is(err, ErrConnPoolClose) {
			t.Errorf("Expected errorConnPoolClose on Dial, got %v", err)
		}

		_, err = pool.DialContext(context.Background(), network, targetAddr)
		if !errors.Is(err, ErrConnPoolClose) {
			t.Errorf("Expected errorConnPoolClose on DialContext, got %v", err)
		}

		_, err = pool.Get(newMockVConn1(network, targetAddr))
		if !errors.Is(err, ErrConnPoolClose) {
			t.Errorf("Expected errorConnPoolClose on Get, got %v", err)
		}

		err = pool.Add(nil) // Should still return specific nil error first
		if err == nil {
			t.Errorf("Expected errorConnPoolClose on Add, got %v", err)
		}

		err = pool.Put(nil, newMockVConn1(network, targetAddr))
		if !errors.Is(err, ErrConnPoolClose) {
			t.Errorf("Expected errorConnPoolClose on Put, got %v", err)
		}

		if pool.ConnNum() != 0 { // Should still be 0
			t.Errorf("ConnNum on closed pool expected 0, got %d", pool.ConnNum())
		}
		if pool.ConnNumIde(network, targetAddr) != 0 { // Should still be 0
			t.Errorf("ConnNumIde on closed pool expected 0, got %d", pool.ConnNumIde(network, targetAddr))
		}
	})
	serverWG.Wait()
}

func TestConnPool_ConcurrentOperations(t *testing.T) {
	targetAddr := "localhost:12355"
	network := "tcp"
	numClients := 10
	numOperationsPerClient := 10

	var dialerWG sync.WaitGroup     // To manage mock server lifecycles
	var poolClientWG sync.WaitGroup // To manage client goroutines

	dialer := &mockDialer1{
		simulateLatency: 5 * time.Millisecond,
	}
	// Custom DialContext for the mockDialer1 to take connections from the channel
	dialer.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		clientPipe, serverPipe := net.Pipe()
		dialerWG.Add(1)
		go echoServer(serverPipe, &dialerWG)
		return clientPipe, nil
	}

	pool := &ConnPool{
		Dialer:  dialer,
		MaxConn: 50, // Max 50 active/idle connections
		IdeConn: 20, // Max 20 idle connections
	}
	defer pool.Close()

	testData := []byte("ping")

	for i := 0; i < numClients; i++ {
		poolClientWG.Add(1)
		go func(clientID int) {
			defer poolClientWG.Done()

			for j := 0; j < numOperationsPerClient; j++ {
				opType := clientID % 4
				switch opType {
				case 0: // Dial, Read/Write, Close (recycle)
					c1, err := pool.Dial(network, targetAddr)
					if err != nil {
						continue
					}
					conn := c1.(Conn)
					_, err = conn.Write(testData)
					if err != nil {
						conn.Discard() // Mark bad connection
						conn.Close()
						t.Errorf("Client %d: Write failed: %v", clientID, err)
						continue
					}
					readBuf := make([]byte, len(testData))
					_, err = conn.Read(readBuf)
					if err != nil {
						conn.Discard() // Mark bad connection
						conn.Close()
						t.Errorf("Client %d: Read failed: %v", clientID, err)
						continue
					}
					conn.Close()
				case 1: // Dial, Read/Write, Discard, Close
					c1, err := pool.Dial(network, targetAddr)
					if err != nil {
						if errors.Is(err, ErrConnPoolMax) {
							continue
						}
						t.Errorf("Client %d: Dial failed: %v", clientID, err)
						continue
					}
					conn := c1.(Conn)
					conn.Discard() // Always discard this one
					_, err = conn.Write(testData)
					if err != nil {
						t.Errorf("Client %d: Write failed on discarded conn: %v", clientID, err)
					}
					conn.Close()
				case 2: // Dial, RawConn, Close raw
					c1, err := pool.Dial(network, targetAddr)
					if err != nil {
						if errors.Is(err, ErrConnPoolMax) {
							continue
						}
						t.Errorf("Client %d: Dial failed: %v", clientID, err)
						continue
					}
					raw := c1.(Conn).RawConn() // Get raw connection
					_, err = raw.Write(testData)
					if err != nil {
						t.Errorf("Client %d: RawConn Write failed: %v", clientID, err)
					}
					raw.Close() // Must close raw connection manually
				case 3: // Get, Use, Put (simulating external management)
					mockAddr := newMockVConn1(network, targetAddr)
					// Try to get from pool, if not available, create new and Add it
					conn, getErr := pool.Get(mockAddr)
					if getErr == nil { // Wrap it as Conn for Discard/IsReuse
						_, err := conn.Write(testData)
						if err != nil {
							t.Errorf("Client %d: Get/Write failed: %v", clientID, err)
						}
						conn.Close() // Put back to pool via Conn.Close
					}
				}

				time.Sleep(time.Duration(clientID%5) * 5 * time.Millisecond) // Simulate some work time
			}
		}(i)
	}

	poolClientWG.Wait()
	time.Sleep(2 * time.Second)

	t.Logf("Final ConnNum: %d", pool.ConnNum())
	t.Logf("Final Idle ConnNum: %d", pool.ConnNumIde(network, targetAddr))

	// Basic assertion: no negative counts, no panics (checked by -race)
	if pool.ConnNum() < 0 || pool.ConnNumIde(network, targetAddr) < 0 {
		t.Errorf("Negative connection count: ConnNum %d, Idle %d", pool.ConnNum(), pool.ConnNumIde(network, targetAddr))
	}
	// Expect some connections to remain idle or be cleaned up
	// Exact count is hard to predict due to concurrency and timeouts, but should be reasonable.
	if pool.ConnNum() > pool.MaxConn && pool.MaxConn > 0 {
		t.Errorf("ConnNum (%d) exceeded MaxConn (%d)", pool.ConnNum(), pool.MaxConn)
	}

	// Make sure all mock servers eventually close
	pool.CloseIdleConnections() // Force close all idle connections
	pool.Close()                // Ensure pool is fully shut down before dialerWG.Wait()

	dialerWG.Wait() // Wait for all echo servers to complete their work
}

// ============================================================================
// Benchmarks
// ============================================================================

func BenchmarkConnPool_DialRecycle(b *testing.B) {
	targetAddr := "localhost:12356"
	network := "tcp"

	// Create a single pipe pair that will be reused
	clientPipe, serverPipe := net.Pipe()
	defer clientPipe.Close()
	defer serverPipe.Close()

	var serverWG sync.WaitGroup
	serverWG.Add(1)
	go echoServer(serverPipe, &serverWG)
	defer serverWG.Wait()

	// Mock Dialer always returns the same clientPipe
	dialer := &mockDialer1{
		nextConn: clientPipe, // This will be consumed once
	}

	pool := &ConnPool{
		Dialer:  dialer,
		MaxConn: 1, // Only one connection
		IdeConn: 1,
	}
	defer pool.Close()

	// Initial Dial to populate the pool
	conn, err := pool.Dial(network, targetAddr)
	if err != nil {
		b.Fatalf("Initial Dial failed: %v", err)
	}
	conn.Close() // Put it into the pool

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		conn, err := pool.Dial(network, targetAddr)
		if err != nil {
			b.Fatalf("Dial failed: %v", err)
		}
		if !conn.(Conn).IsReuseConn() {
			b.Fatalf("Expected reused connection, got new on iteration %d", i)
		}
		conn.Close()
	}
}

func BenchmarkConnPool_DialNew(b *testing.B) {
	targetAddr := "localhost:12357"
	network := "tcp"

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	pool := &ConnPool{
		Dialer:  dialer,
		MaxConn: 0, // No max, always new
		IdeConn: 0, // No idle pool
	}
	defer pool.Close()

	ctx := context.WithValue(context.Background(), PriorityContextKey, true) // Always force new

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		conn, err := pool.DialContext(ctx, network, targetAddr)
		if err != nil {
			b.Fatalf("Dial failed: %v", err)
		}
		conn.Close()
	}
	pool.CloseIdleConnections()
	serverWG.Wait()
}

func BenchmarkConnPool_ReadWrite(b *testing.B) {
	targetAddr := "localhost:12358"
	network := "tcp"
	messageSize := 1024

	// Create a pipe pair for the benchmark
	clientPipe, serverPipe := net.Pipe()
	defer clientPipe.Close()
	defer serverPipe.Close()

	var serverWG sync.WaitGroup
	serverWG.Add(1)
	go echoServer(serverPipe, &serverWG)
	defer serverWG.Wait()

	dialer := &mockDialer1{
		nextConn: clientPipe,
	}
	pool := &ConnPool{
		Dialer:  dialer,
		MaxConn: 1,
		IdeConn: 1,
	}
	defer pool.Close()

	// Get a connection from the pool
	conn, err := pool.Dial(network, targetAddr)
	if err != nil {
		b.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()

	// Prepare data
	sendData := bytes.Repeat([]byte("a"), messageSize)
	receiveBuffer := make([]byte, messageSize)

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(messageSize))

	for i := 0; i < b.N; i++ {
		_, err := conn.Write(sendData)
		if err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		_, err = conn.Read(receiveBuffer)
		if err != nil {
			b.Fatalf("Read failed: %v", err)
		}
		if !bytes.Equal(sendData, receiveBuffer) {
			b.Fatalf("Data mismatch: sent %v, received %v", sendData, receiveBuffer)
		}
	}
}
