package vconnpool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		n, err := conn.Read(buf)
		if err != nil && n == 0 {
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
	addr := &Addr{Net: network, Name: targetAddr}

	var serverWG sync.WaitGroup
	// Mock Dialer: Each Dial call provides a fresh pipe connection
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	// Setup a ConnPool
	pool := &Pool{
		Dialer:      dialer,
		MaxConn:     10,
		IdleConn:    5,
		IdleTimeout: time.Second,
	}
	defer pool.Close()

	t.Run("DialNewConnection", func(t *testing.T) {
		conn, err := pool.Dial(network, targetAddr)
		if err != nil {
			t.Fatalf("Dial failed: %v", err)
		}
		if conn.IsReuseConn() {
			t.Error("Expected new connection, but IsReuseConn returned true")
		}
		if pool.Num() != 1 {
			t.Errorf("Expected Num 1, got %d", pool.Num())
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
		if pool.Num() != 1 { // Connection is in idle pool, still counted
			t.Errorf("Expected Num 1 after close, got %d", pool.Num())
		}
		addr := &Addr{Net: network, Name: targetAddr}
		if pool.NumIdle(addr) != 1 {
			t.Errorf("Expected 1 idle connection, got %d", pool.NumIdle(addr))
		}
	})

	t.Run("DialReuseConnection", func(t *testing.T) {
		// Should reuse the connection from the previous test

		conn, err := pool.Dial(network, targetAddr)
		if err != nil {
			t.Fatalf("Dial failed: %v", err)
		}
		if !conn.IsReuseConn() {
			t.Error("Expected reused connection, but IsReuseConn returned false")
		}
		if pool.Num() != 1 { // Still the same connection
			t.Errorf("Expected Num 1, got %d", pool.Num())
		}
		if pool.NumIdle(addr) != 0 { // Connection is now active, not idle
			t.Errorf("Expected 0 idle connections, got %d", pool.NumIdle(addr))
		}

		// Close it again
		err = conn.Close()
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		if pool.NumIdle(addr) != 1 {
			t.Errorf("Expected 1 idle connection after reuse close, got %d", pool.NumIdle(addr))
		}
	})

	t.Run("IdleTimeout", func(t *testing.T) {
		// Wait for the idle timeout to expire
		time.Sleep(1500 * time.Millisecond) // Slightly longer than IdeTimeout

		if pool.NumIdle(addr) != 0 {
			t.Errorf("Expected 0 idle connections after timeout, got %d", pool.NumIdle(addr))
		}
		if pool.Num() != 0 {
			t.Errorf("Expected Num 0 after timeout, got %d", pool.Num())
		}
	})

	serverWG.Wait() // Ensure all echo servers are done
}

func TestConnPool_IdeConnLimit(t *testing.T) {
	targetAddr := "localhost:12347"
	network := "tcp"
	addr := &Addr{Net: network, Name: targetAddr}

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	pool := &Pool{
		Dialer:      dialer,
		MaxConn:     0, // No max total connections
		IdleConn:    1, // Max 1 idle connection
		IdleTimeout: time.Second,
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
	if pool.Num() != 2 {
		t.Errorf("Expected Num 2, got %d", pool.Num())
	}

	// Close conn1 (should go to idle pool)
	conn1.Close()
	if pool.NumIdle(addr) != 1 {
		t.Errorf("Expected 1 idle connection, got %d", pool.NumIdle(addr))
	}

	// Close conn2 (should be discarded due to IdeConn limit)
	conn2.Close()
	if pool.NumIdle(addr) != 1 {
		t.Errorf("Expected still 1 idle connection (conn2 discarded), got %d", pool.NumIdle(addr))
	}
	if pool.Num() != 1 { // One was recycled, one discarded
		t.Errorf("Expected Num 1 (conn2 discarded), got %d", pool.Num())
	}

	serverWG.Wait()
}

func TestConnPool_Discard(t *testing.T) {
	targetAddr := "localhost:12348"
	network := "tcp"
	addr := &Addr{Net: network, Name: targetAddr}

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	pool := &Pool{
		Dialer:   dialer,
		MaxConn:  10,
		IdleConn: 10,
	}
	defer pool.Close()

	conn, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if pool.Num() != 1 {
		t.Errorf("Expected Num 1, got %d", pool.Num())
	}

	// Mark connection for discard
	conn.Discard()

	// Close the connection (should NOT be put back to pool)
	err = conn.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if pool.Num() != 0 { // Should be 0 because it was discarded
		t.Errorf("Expected Num 0 after discard and close, got %d", pool.Num())
	}
	if pool.NumIdle(addr) != 0 {
		t.Errorf("Expected 0 idle connections, got %d", pool.NumIdle(addr))
	}

	serverWG.Wait()
}

func TestConnPool_RawConn(t *testing.T) {
	targetAddr := "localhost:12349"
	network := "tcp"
	addr := &Addr{Net: network, Name: targetAddr}

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	pool := &Pool{
		Dialer:   dialer,
		MaxConn:  10,
		IdleConn: 10,
	}
	defer pool.Close()

	conn, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if pool.Num() != 1 {
		t.Errorf("Expected Num 1, got %d", pool.Num())
	}

	// Get raw connection
	rawConn := conn.RawConn()
	if rawConn == nil {
		t.Fatal("RawConn returned nil")
	}
	if pool.Num() != 0 { // Should be 0 because RawConn removes it from pool management
		t.Errorf("Expected Num 0 after RawConn, got %d", pool.Num())
	}
	if pool.NumIdle(addr) != 0 {
		t.Errorf("Expected 0 idle connections, got %d", pool.NumIdle(addr))
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
}

func TestConnPool_DialContext_Priority(t *testing.T) {
	targetAddr := "localhost:12350"
	network := "tcp"
	addr := &Addr{Net: network, Name: targetAddr}

	var serverWG sync.WaitGroup
	dialer := &mockDialer1{
		wg: &serverWG,
	}

	pool := &Pool{
		Dialer:   dialer,
		MaxConn:  10,
		IdleConn: 10,
	}
	defer pool.Close()

	conn1, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	conn1.Close() // Put to pool

	// Num should be 1, Ide is 1
	if pool.Num() != 1 || pool.NumIdle(addr) != 1 {
		t.Fatalf("Expected Num 1 and Ide 1, got %d, %d", pool.Num(), pool.NumIdle(addr))
	}
	// Dial with priority context: should ignore pool and create new connection
	ctx := context.WithValue(context.Background(), PriorityContextKey, true)
	conn2, err := pool.DialContext(ctx, network, targetAddr)
	if err != nil {
		t.Fatalf("DialContext with priority failed: %v", err)
	}
	if conn2.IsReuseConn() {
		t.Error("Expected new connection (priority), but IsReuseConn returned true")
	}
	if pool.Num() != 2 { // Total connections should now be 2
		t.Errorf("Expected Num 2, got %d", pool.Num())
	}
	if pool.NumIdle(addr) != 1 { // Idle connection should still be 1 (the first one)
		t.Errorf("Expected Ide 1, got %d", pool.NumIdle(addr))
	}

	conn2.Close() // Put to pool
	if pool.Num() != 2 {
		t.Errorf("Expected Num 2 after conn2 close, got %d", pool.Num())
	}
	if pool.NumIdle(addr) != 2 {
		t.Errorf("Expected Ide 2, got %d", pool.NumIdle(addr))
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

	pool := &Pool{
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
	if pool.Num() != 0 {
		t.Errorf("Expected Num 0, got %d", pool.Num())
	}
}

func TestConnPool_Add_Put(t *testing.T) {
	targetAddr := "localhost:12352"
	network := "tcp"

	pool := &Pool{
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
		if pool.Num() != 1 {
			t.Errorf("Expected Num 1, got %d", pool.Num())
		}
		if pool.NumIdle(mockAddr) != 1 {
			t.Errorf("Expected 1 idle connection, got %d", pool.NumIdle(mockAddr))
		}

		conn, err := pool.Get(mockAddr)
		if err != nil {
			t.Fatalf("Get after Add failed: %v", err)
		}
		if pool.NumIdle(mockAddr) != 0 {
			t.Errorf("Expected 0 idle connection after Get, got %d", pool.NumIdle(mockAddr))
		}

		err = pool.Put(conn, mockAddr)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if pool.NumIdle(mockAddr) != 1 { // Should be returned to pool
			t.Errorf("Expected 1 idle connection after Get and Close, got %d", pool.NumIdle(mockAddr))
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
		pool = &Pool{
			MaxConn:  10,
			IdleConn: 10,
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
		if pool.NumIdle(mockAddr) != 1 { // Should still be 1 idle conn
			t.Errorf("Expected 1 idle conn after duplicate put, got %d", pool.NumIdle(mockAddr))
		}
		pool.CloseIdleConnections()
		serverWG.Wait()
	})
}

func TestConnPool_Get(t *testing.T) {
	targetAddr := "localhost:12353"
	network := "tcp"
	addr := &Addr{Net: network, Name: targetAddr}

	pool := &Pool{}
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
		if pool.NumIdle(addr) != 0 {
			t.Errorf("Expected 0 idle connections after Get, got %d", pool.NumIdle(addr))
		}
		if pool.Num() != 0 { // Get removes it from pool's count, calling code should handle it.
			t.Errorf("Expected Num 0 after Get, got %d", pool.Num())
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
	pool := &Pool{}
	targetAddr := "localhost:12354"
	network := "tcp"
	addr := &Addr{Net: network, Name: targetAddr}

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
	if pool.Num() != 1 || pool.NumIdle(addr) != 1 {
		t.Fatalf("Pre-close: Num %d, Ide %d", pool.Num(), pool.NumIdle(addr))
	}

	t.Run("Close", func(t *testing.T) {
		err := pool.Close()
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		if pool.Num() != 0 {
			t.Errorf("Expected Num 0 after Close, got %d", pool.Num())
		}
		if pool.NumIdle(addr) != 0 {
			t.Errorf("Expected 0 idle connections after Close, got %d", pool.NumIdle(addr))
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
		if !errors.Is(err, ErrConnPoolClosed) {
			t.Errorf("Expected errorConnPoolClose on Dial, got %v", err)
		}

		_, err = pool.DialContext(context.Background(), network, targetAddr)
		if !errors.Is(err, ErrConnPoolClosed) {
			t.Errorf("Expected errorConnPoolClose on DialContext, got %v", err)
		}

		_, err = pool.Get(newMockVConn1(network, targetAddr))
		if !errors.Is(err, ErrConnPoolClosed) {
			t.Errorf("Expected errorConnPoolClose on Get, got %v", err)
		}

		err = pool.Add(nil) // Should still return specific nil error first
		if err == nil {
			t.Errorf("Expected errorConnPoolClose on Add, got %v", err)
		}

		err = pool.Put(nil, newMockVConn1(network, targetAddr))
		if err != nil && err.Error() != "vconnpool: nil parameters" {
			t.Errorf("Expected errorConnPoolClose on Put, got %v", err)
		}

		if pool.Num() != 0 { // Should still be 0
			t.Errorf("Num on closed pool expected 0, got %d", pool.Num())
		}
		if pool.NumIdle(addr) != 0 { // Should still be 0
			t.Errorf("ConnNumIde on closed pool expected 0, got %d", pool.NumIdle(addr))
		}
	})
	serverWG.Wait()
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

	pool := &Pool{
		Dialer:   dialer,
		MaxConn:  1, // Only one connection
		IdleConn: 1,
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
		if !conn.IsReuseConn() {
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

	pool := &Pool{
		Dialer:   dialer,
		MaxConn:  0, // No max, always new
		IdleConn: 0, // No idle pool
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
	pool := &Pool{
		Dialer:   dialer,
		MaxConn:  1,
		IdleConn: 1,
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
