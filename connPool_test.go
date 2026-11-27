package vconnpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试基建（合并原三个文件里的重复 helper）
// ---------------------------------------------------------------------------

func testResolver(network, address string) (net.Addr, error) {
	return Addr{Net: network, Name: address}, nil
}

func newTestPool(t *testing.T, d Dialer, opts ...func(*Pool)) *Pool {
	t.Helper()
	p := New(Config{Dialer: d, ResolveAddr: testResolver, IdleTimeout: time.Minute})
	for _, opt := range opts {
		opt(p)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// pipeDialer：每次 Dial 产生一对 net.Pipe，并记录调用次数与对端，便于断言与清理。
type pipeDialer struct {
	mu    sync.Mutex
	calls int
	peers []net.Conn
	errs  []error
}

var _ Dialer = (*pipeDialer)(nil)

func newPipeDialer(t *testing.T) *pipeDialer {
	t.Helper()
	d := &pipeDialer{}
	t.Cleanup(d.closePeers)
	return d
}

func (d *pipeDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *pipeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.calls
	d.calls++
	if i < len(d.errs) && d.errs[i] != nil {
		d.peers = append(d.peers, nil)
		return nil, d.errs[i]
	}
	client, server := net.Pipe()
	d.peers = append(d.peers, server)
	return client, nil
}

func (d *pipeDialer) Calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *pipeDialer) Peer(i int) net.Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i < 0 || i >= len(d.peers) {
		return nil
	}
	return d.peers[i]
}

func (d *pipeDialer) closePeers() {
	d.mu.Lock()
	peers := append([]net.Conn(nil), d.peers...)
	d.mu.Unlock()
	for _, p := range peers {
		if p != nil {
			_ = p.Close()
		}
	}
}

// echoDialer：Dial 得到的连接对端做回显，用于并发读写压测。
type echoDialer struct{}

var _ Dialer = echoDialer{}

func (echoDialer) Dial(network, address string) (net.Conn, error) {
	return echoDialer{}.DialContext(context.Background(), network, address)
}

func (echoDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		_, _ = io.Copy(server, server)
	}()
	return client, nil
}

func assertCounts(t *testing.T, p *Pool, addr net.Addr, num, idle, conn int) {
	t.Helper()
	if got := p.Num(); got != num {
		t.Errorf("Num() = %d, want %d", got, num)
	}
	if got := p.NumIdle(addr); got != idle {
		t.Errorf("NumIdle() = %d, want %d", got, idle)
	}
	if got := p.NumConn(addr); got != conn {
		t.Errorf("NumConn() = %d, want %d", got, conn)
	}
}

// assertPeerClosed 断言对端已观测到关闭（fd 真正释放，而非泄漏）。
func assertPeerClosed(t *testing.T, peer net.Conn) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := peer.Read(b[:]); err == nil {
		t.Fatal("peer observed no close; connection may have leaked")
	}
}

func mustDial(t *testing.T, p *Pool, addr net.Addr) Conn {
	t.Helper()
	c, err := p.Dial(addr.Network(), addr.String())
	if err != nil {
		t.Fatalf("Dial(%q, %q): %v", addr.Network(), addr.String(), err)
	}
	return c
}

// waitStatus 取代旧测试的 waitBool(<-chan bool)：true->WaitSatisfied，
// false->WaitPoolClosed / WaitContextDone。
func waitStatus(t *testing.T, ch <-chan WaitStatus) WaitStatus {
	t.Helper()
	select {
	case st := <-ch:
		return st
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not resolve in time")
		return WaitErrOther
	}
}

func waitFor(t *testing.T, timeout time.Duration, pred func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if pred() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

func putIdlePipe(t *testing.T, p *Pool, addr net.Addr, bs int) (net.Conn, net.Conn) {
	t.Helper()
	server, peer := net.Pipe()
	if err := p.PutFull(server, addr, time.Minute, bs); err != nil {
		_ = peer.Close()
		t.Fatalf("PutFull: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	return server, peer
}

func roundTrip(t *testing.T, c, peer net.Conn, msg string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := peer.Write([]byte(msg)); done <- err }()

	if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != msg {
		t.Fatalf("got %q, want %q", got, msg)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("peer write: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("peer write blocked")
	}
	_ = c.SetReadDeadline(time.Time{})
}

// ---------------------------------------------------------------------------
// 地址解析
// ---------------------------------------------------------------------------

func TestResolveAddr(t *testing.T) {
	tests := []struct {
		network, address, wantType, wantNet string
	}{
		{"tcp", "127.0.0.1:12345", "*net.TCPAddr", "tcp"},
		{"tcp4", "127.0.0.1:12345", "*net.TCPAddr", "tcp"},
		{"udp4", "127.0.0.1:12345", "*net.UDPAddr", "udp"},
		{"unix", "/tmp/vconnpool-test.sock", "*net.UnixAddr", "unix"},
		{"ip", "127.0.0.1", "*net.IPAddr", "ip"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.network, func(t *testing.T) {
			got, err := ResolveAddr(tc.network, tc.address)
			if err != nil {
				t.Fatalf("ResolveAddr: %v", err)
			}
			if typ := fmt.Sprintf("%T", got); typ != tc.wantType {
				t.Fatalf("type = %s, want %s", typ, tc.wantType)
			}
			if got.Network() != tc.wantNet || got.String() != tc.address {
				t.Fatalf("addr = (%q, %q), want (%q, %q)",
					got.Network(), got.String(), tc.wantNet, tc.address)
			}
		})
	}

	t.Run("unsupported", func(t *testing.T) {
		if _, err := ResolveAddr("bogus", "x"); err == nil {
			t.Fatal("unsupported network should error")
		}
	})
	t.Run("Addr", func(t *testing.T) {
		a := Addr{Net: "udp", Name: "example:9"}
		if a.Network() != "udp" || a.String() != "example:9" {
			t.Fatalf("Addr = (%q, %q)", a.Network(), a.String())
		}
	})
}

// ---------------------------------------------------------------------------
// Dial / Close / 复用 / 所有权（合并 TestPoolCountsAndReuse + TestConnLifecycle）
// ---------------------------------------------------------------------------

func TestDialCloseReuse(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "reuse.test:1"}

	t.Run("new then reuse", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd)

		c1 := mustDial(t, p, addr)
		if c1.IsReuseConn() {
			t.Fatal("first Dial should be new")
		}
		assertCounts(t, p, addr, 1, 0, 1)

		if err := c1.Close(); err != nil {
			t.Fatal(err)
		}
		assertCounts(t, p, addr, 1, 1, 1)
		if _, err := c1.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Read after Close = %v, want net.ErrClosed", err)
		}

		c2 := mustDial(t, p, addr)
		if !c2.IsReuseConn() {
			t.Fatal("second Dial should reuse pooled connection")
		}
		if got := pd.Calls(); got != 1 {
			t.Fatalf("dialer calls = %d, want 1", got)
		}
		_ = c2.Close()
	})

	t.Run("Discard prevents pooling", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		c := mustDial(t, p, addr)
		c.Discard()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		assertCounts(t, p, addr, 0, 0, 0)
	})

	t.Run("RawConn transfers ownership once", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd)

		c := mustDial(t, p, addr)
		raw, err := c.RawConn()
		if err != nil || raw == nil {
			t.Fatalf("RawConn = (%v, %v)", raw, err)
		}
		assertCounts(t, p, addr, 0, 0, 0)

		// 二次 RawConn 必须报 ErrConnRAWRead（原测试漏掉了这个断言）。
		if _, err := c.RawConn(); !errors.Is(err, ErrConnRAWRead) {
			t.Fatalf("second RawConn = %v, want ErrConnRAWRead", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close after RawConn: %v", err)
		}
		roundTrip(t, raw, pd.Peer(0), "owned")
		_ = raw.Close()
	})

	t.Run("deadline cleared on checkout", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd)

		c1 := mustDial(t, p, addr)
		peer := pd.Peer(0)
		if err := c1.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := c1.Close(); err != nil {
			t.Fatal(err)
		}

		c2 := mustDial(t, p, addr)
		if !c2.IsReuseConn() {
			t.Fatal("expected reused connection")
		}
		got := make([]byte, 1)
		readDone := make(chan error, 1)
		go func() { _, err := c2.Read(got); readDone <- err }()
		go func() { _, _ = peer.Write([]byte("x")) }()

		select {
		case err := <-readDone:
			if err != nil {
				t.Fatalf("Read after checkout: %v (deadline leaked?)", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Read timed out")
		}
		if string(got) != "x" {
			t.Fatalf("got %q, want x", got)
		}
		_ = c2.Close()
	})
}

// ---------------------------------------------------------------------------
// 空闲超时（合并 TestIdleTimeout + 收敛里的一段）
// ---------------------------------------------------------------------------

func TestIdleTimeout(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "idle.test:1"}

	tests := []struct {
		name string
		idle time.Duration
		use  func(t *testing.T, p *Pool, addr net.Addr, timeout time.Duration)
	}{
		{"dial-close uses config timeout", 40 * time.Millisecond, func(t *testing.T, p *Pool, addr net.Addr, _ time.Duration) {
			c := mustDial(t, p, addr)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if got := p.NumIdle(addr); got != 1 {
				t.Fatalf("NumIdle right after Close = %d, want 1", got)
			}
		}},
		{"putfull uses custom timeout", 40 * time.Millisecond, func(t *testing.T, p *Pool, addr net.Addr, timeout time.Duration) {
			server, peer := net.Pipe()
			t.Cleanup(func() { _ = peer.Close() })
			if err := p.PutFull(server, addr, timeout, 0); err != nil {
				t.Fatal(err)
			}
			if got := p.NumIdle(addr); got != 1 {
				t.Fatalf("NumIdle after PutFull = %d, want 1", got)
			}
		}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool(t, newPipeDialer(t), func(p *Pool) {
				p.UpdateConfig(func(c *Config) { c.IdleTimeout = tc.idle })
			})
			tc.use(t, p, addr, tc.idle)

			if !waitFor(t, 5*time.Second, func() bool { return p.NumIdle(addr) == 0 }) {
				t.Fatalf("idle conn not reclaimed (NumIdle=%d)", p.NumIdle(addr))
			}
			if _, _, err := p.GetFull(addr); !errors.Is(err, ErrConnNotAvailable) {
				t.Fatalf("GetFull after timeout = %v, want ErrConnNotAvailable", err)
			}
			assertCounts(t, p, addr, 0, 0, 0)
		})
	}

	t.Run("kept before expiry", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t), func(p *Pool) {
			p.UpdateConfig(func(c *Config) { c.IdleTimeout = 5 * time.Second })
		})
		c := mustDial(t, p, addr)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(100 * time.Millisecond)
		for time.Now().Before(deadline) {
			if got := p.NumIdle(addr); got != 1 {
				t.Fatalf("reclaimed too early: NumIdle=%d, want 1", got)
			}
			time.Sleep(5 * time.Millisecond)
		}
		conn, _, err := p.GetFull(addr)
		if err != nil {
			t.Fatalf("GetFull before expiry: %v", err)
		}
		_ = conn.Close()
	})
}

// ---------------------------------------------------------------------------
// Put 契约（合并 TestPutContracts 的全部子测试 + 新增“失败时 fd 必须真正关闭”）
// ---------------------------------------------------------------------------

func TestPutContracts(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "put.test:1"}

	t.Run("nil conn", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		if err := p.PutFull(nil, addr, time.Minute, 0); err == nil {
			t.Fatal("PutFull(nil, ...) should fail")
		}
		if err := p.Add(nil); err == nil {
			t.Fatal("Add(nil) should fail")
		}
	})

	t.Run("nil addr closes conn", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		if err := p.PutFull(server, nil, time.Minute, 0); err == nil {
			t.Fatal("PutFull(conn, nil, ...) should fail")
		}
		assertPeerClosed(t, peer)
	})

	t.Run("closed pool closes conn", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		if err := p.PutFull(server, addr, time.Minute, 0); !errors.Is(err, ErrConnPoolClosed) {
			t.Fatalf("PutFull on closed pool = %v, want ErrConnPoolClosed", err)
		}
		assertPeerClosed(t, peer)
		if got := p.Num(); got != 0 {
			t.Fatalf("Num() = %d, want 0", got)
		}
	})

	t.Run("duplicate put idempotent", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })

		if err := p.PutFull(server, addr, time.Minute, 0); err != nil {
			t.Fatal(err)
		}
		if err := p.PutFull(server, addr, time.Minute, 0); err != nil {
			t.Fatalf("duplicate PutFull = %v, want nil", err)
		}
		assertCounts(t, p, addr, 1, 1, 1)

		out, _, err := p.GetFull(addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = out.Close()
	})

	t.Run("over idle limit", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t), func(p *Pool) {
			p.UpdateConfig(func(c *Config) { c.IdleConn = 1 })
		})
		first, peer1 := net.Pipe()
		t.Cleanup(func() { _ = peer1.Close() })
		if err := p.PutFull(first, addr, time.Minute, 0); err != nil {
			t.Fatal(err)
		}

		second, peer2 := net.Pipe()
		t.Cleanup(func() { _ = peer2.Close() })
		if err := p.PutFull(second, addr, time.Minute, 0); !errors.Is(err, ErrConnIdleMax) {
			t.Fatalf("PutFull over idle limit = %v, want ErrConnIdleMax", err)
		}
		assertCounts(t, p, addr, 1, 1, 1)
		assertPeerClosed(t, peer2) // 超限的连接必须被真正关闭
	})

	t.Run("over max conn", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd, func(p *Pool) {
			p.UpdateConfig(func(c *Config) { c.MaxConn = 1 })
		})
		held := mustDial(t, p, addr)

		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		if err := p.PutFull(server, addr, time.Minute, 0); !errors.Is(err, ErrConnPoolMax) {
			t.Fatalf("PutFull over max conn = %v, want ErrConnPoolMax", err)
		}
		if got := p.Num(); got != 1 {
			t.Fatalf("Num() = %d, want 1", got)
		}
		assertPeerClosed(t, peer)
		_ = held.Close()
	})

	t.Run("Add uses remote addr", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })

		remote := server.RemoteAddr()
		if err := p.Add(server); err != nil {
			t.Fatal(err)
		}
		if got := p.NumConn(remote); got != 1 {
			t.Fatalf("NumConn(remote) = %d, want 1", got)
		}
		if got := p.NumIdle(remote); got != 1 {
			t.Fatalf("NumIdle(remote) = %d, want 1", got)
		}
		conn, err := p.Get(remote)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		if got := p.Num(); got != 0 {
			t.Fatalf("Num() = %d, want 0", got)
		}
	})
}

// ---------------------------------------------------------------------------
// MaxConn 与失败回滚（合并 TestMaxConnAndFailureAccounting）
// ---------------------------------------------------------------------------

func TestMaxConnAndFailure(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "max.test:1"}

	t.Run("max conn enforced", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd, func(p *Pool) {
			p.UpdateConfig(func(c *Config) { c.MaxConn = 1 })
		})

		c1 := mustDial(t, p, addr)
		if _, err := p.Dial(addr.Network(), addr.String()); !errors.Is(err, ErrConnPoolMax) {
			t.Fatalf("second Dial = %v, want ErrConnPoolMax", err)
		}
		if got := pd.Calls(); got != 1 {
			t.Fatalf("dialer calls = %d, want 1", got)
		}
		if err := c1.Close(); err != nil {
			t.Fatal(err)
		}

		c2 := mustDial(t, p, addr)
		if !c2.IsReuseConn() {
			t.Fatal("expected reuse after first connection returned")
		}
		if _, err := p.Dial(addr.Network(), addr.String()); !errors.Is(err, ErrConnPoolMax) {
			t.Fatalf("Dial while max conn in use = %v, want ErrConnPoolMax", err)
		}
		_ = c2.Close()
	})

	t.Run("failed dial rolls back quota", func(t *testing.T) {
		errBoom := errors.New("dial boom")
		pd := &pipeDialer{errs: []error{errBoom}}
		t.Cleanup(pd.closePeers)
		p := newTestPool(t, pd, func(p *Pool) {
			p.UpdateConfig(func(c *Config) { c.MaxConn = 1 })
		})

		if _, err := p.Dial(addr.Network(), addr.String()); !errors.Is(err, errBoom) {
			t.Fatalf("first Dial = %v, want %v", err, errBoom)
		}
		if got := p.Num(); got != 0 {
			t.Fatalf("Num after failed dial = %d, want 0", got)
		}

		c, err := p.Dial(addr.Network(), addr.String())
		if err != nil {
			t.Fatalf("second Dial: %v", err)
		}
		if c.IsReuseConn() {
			t.Fatal("connection after failed dial should be new")
		}
		if got := p.Num(); got != 1 {
			t.Fatalf("Num = %d, want 1", got)
		}
		_ = c.Close()
	})
}

// ---------------------------------------------------------------------------
// DialContext 策略（合并 TestDialContextPolicy）
// ---------------------------------------------------------------------------

func TestDialContextPolicy(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "dial.ctx:1"}
	prepare := func(t *testing.T, p *Pool) {
		c := mustDial(t, p, addr)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name      string
		ctx       context.Context
		wantReuse bool
	}{
		{"true reuses idle", context.WithValue(context.Background(), PoolConnContextKey, true), true},
		{"false forces new", context.WithValue(context.Background(), PoolConnContextKey, false), false},
		{"non-bool behaves as unset", context.WithValue(context.Background(), PoolConnContextKey, "not-bool"), true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pd := newPipeDialer(t)
			p := newTestPool(t, pd)
			prepare(t, p)

			c2, err := p.DialContext(tc.ctx, addr.Network(), addr.String())
			if err != nil {
				t.Fatal(err)
			}
			if c2.IsReuseConn() != tc.wantReuse {
				t.Fatalf("IsReuseConn() = %v, want %v", c2.IsReuseConn(), tc.wantReuse)
			}
			wantCalls := 1
			if !tc.wantReuse {
				wantCalls = 2
			}
			if got := pd.Calls(); got != wantCalls {
				t.Fatalf("dialer calls = %d, want %d", got, wantCalls)
			}
			_ = c2.Close()
		})
	}

	t.Run("true never dials on empty pool", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd)
		ctxTrue := context.WithValue(context.Background(), PoolConnContextKey, true)

		if _, err := p.DialContext(ctxTrue, addr.Network(), addr.String()); !errors.Is(err, ErrConnNotAvailable) {
			t.Fatalf("DialContext = %v, want ErrConnNotAvailable", err)
		}
		if got := pd.Calls(); got != 0 {
			t.Fatalf("dialer calls = %d, want 0", got)
		}

		c1 := mustDial(t, p, addr)
		if _, err := p.DialContext(ctxTrue, addr.Network(), addr.String()); !errors.Is(err, ErrConnNotAvailable) {
			t.Fatalf("DialContext while conn in use = %v, want ErrConnNotAvailable", err)
		}
		if got := pd.Calls(); got != 1 {
			t.Fatalf("dialer calls = %d, want 1", got)
		}
		if err := c1.Close(); err != nil {
			t.Fatal(err)
		}
		c2, err := p.DialContext(ctxTrue, addr.Network(), addr.String())
		if err != nil {
			t.Fatal(err)
		}
		_ = c2.Close()
	})

	t.Run("canceled after checkout releases conn", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd)

		c := mustDial(t, p, addr)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := p.DialContext(ctx, addr.Network(), addr.String()); !errors.Is(err, context.Canceled) {
			t.Fatalf("DialContext with canceled ctx = %v, want context.Canceled", err)
		}
		assertCounts(t, p, addr, 0, 0, 0)
		if got := pd.Calls(); got != 1 {
			t.Fatalf("dialer calls = %d, want 1", got)
		}
	})
}

// ---------------------------------------------------------------------------
// 关池与所有权（合并 TestPoolCloseOwnership）
// ---------------------------------------------------------------------------

func TestPoolClose(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "close.test:1"}

	t.Run("closes owned, idempotent", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd)

		idle := mustDial(t, p, addr)
		if err := idle.Close(); err != nil {
			t.Fatal(err)
		}
		used := mustDial(t, p, addr)
		assertCounts(t, p, addr, 1, 0, 1)

		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		assertCounts(t, p, addr, 0, 0, 0)

		if _, err := used.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Write after Pool.Close = %v, want net.ErrClosed", err)
		}
		if _, err := idle.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Read after Pool.Close = %v, want net.ErrClosed", err)
		}
		if _, err := p.Dial(addr.Network(), addr.String()); !errors.Is(err, ErrConnPoolClosed) {
			t.Fatalf("Dial after Close = %v, want ErrConnPoolClosed", err)
		}
		if _, err := p.Get(addr); !errors.Is(err, ErrConnPoolClosed) {
			t.Fatalf("Get after Close = %v, want ErrConnPoolClosed", err)
		}
		if _, _, err := p.GetFull(addr); !errors.Is(err, ErrConnPoolClosed) {
			t.Fatalf("GetFull after Close = %v, want ErrConnPoolClosed", err)
		}
		if err := p.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		if err := p.Put(server, addr); !errors.Is(err, ErrConnPoolClosed) {
			t.Fatalf("Put after Close = %v, want ErrConnPoolClosed", err)
		}
	})

	t.Run("GetFull transfers ownership", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		if err := p.PutFull(server, addr, time.Minute, 0); err != nil {
			t.Fatal(err)
		}

		out, pending, err := p.GetFull(addr)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("pending = %q, want empty", pending)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		roundTrip(t, out, peer, "outlives-pool")
		_ = out.Close()
	})

	t.Run("close idle scopes", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd)
		addr1 := Addr{Net: "tcp", Name: "idle.scope:1"}
		addr2 := Addr{Net: "tcp", Name: "idle.scope:2"}

		a := mustDial(t, p, addr1)
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		b := mustDial(t, p, addr2)
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		c := mustDial(t, p, addr1) // 仍在用，不能被 CloseIdleConnection 关闭

		p.CloseIdleConnection(addr1)
		if got := p.NumIdle(addr1); got != 0 {
			t.Fatalf("NumIdle(addr1) = %d, want 0", got)
		}
		if got := p.NumConn(addr1); got != 1 {
			t.Fatalf("NumConn(addr1) = %d, want 1", got)
		}
		if got := p.NumIdle(addr2); got != 1 {
			t.Fatalf("NumIdle(addr2) = %d, want 1", got)
		}
		if got := p.Num(); got != 2 {
			t.Fatalf("Num() = %d, want 2", got)
		}

		raw, _, err := p.GetFull(addr2)
		if err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		if got := p.Num(); got != 1 {
			t.Fatalf("Num() after GetFull = %d, want 1", got)
		}

		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		p.CloseIdleConnections()
		if got := p.Num(); got != 0 {
			t.Fatalf("Num() after CloseIdleConnections = %d, want 0", got)
		}
	})
}

// ---------------------------------------------------------------------------
// nil 地址契约（合并 TestNilAddrContracts）
// ---------------------------------------------------------------------------

func TestNilAddr(t *testing.T) {
	p := newTestPool(t, newPipeDialer(t))

	if _, err := p.Get(nil); err == nil {
		t.Fatal("Get(nil) should fail")
	}
	if _, _, err := p.GetFull(nil); err == nil {
		t.Fatal("GetFull(nil) should fail")
	}
	if got := p.NumIdle(nil); got != 0 {
		t.Fatalf("NumIdle(nil) = %d, want 0", got)
	}
	if got := p.NumConn(nil); got != 0 {
		t.Fatalf("NumConn(nil) = %d, want 0", got)
	}
	p.CloseIdleConnection(nil) // 不应 panic
}

// ---------------------------------------------------------------------------
// 预读数据（合并 TestPendingData）
// ---------------------------------------------------------------------------

func TestPendingData(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "pending.test:1"}
	ctxTrue := context.WithValue(context.Background(), PoolConnContextKey, true)

	writeAsync := func(t *testing.T, peer net.Conn, s string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { _, err := peer.Write([]byte(s)); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("peer write: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("peer write blocked; background read did not consume")
		}
	}

	t.Run("GetFull returns idle bytes", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		_, peer := putIdlePipe(t, p, addr, 64)
		writeAsync(t, peer, "hello")

		conn, pending, err := p.GetFull(addr)
		if err != nil {
			t.Fatal(err)
		}
		if string(pending) != "hello" {
			t.Fatalf("pending = %q, want hello", pending)
		}
		_ = conn.Close()
		assertCounts(t, p, addr, 0, 0, 0)
	})

	t.Run("Get prepends idle bytes", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		_, peer := putIdlePipe(t, p, addr, 64)
		writeAsync(t, peer, "world")

		conn, err := p.Get(addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 5)
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != "world" {
			t.Fatalf("got %q, want world", got)
		}
		_ = conn.Close()
	})

	t.Run("Buffered tracks pending", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		_, peer := putIdlePipe(t, p, addr, 64)
		writeAsync(t, peer, "abc")

		c, err := p.DialContext(ctxTrue, addr.Network(), addr.String())
		if err != nil {
			t.Fatal(err)
		}
		bc, ok := c.(interface{ Buffered() int })
		if !ok {
			t.Fatal("connection does not expose Buffered")
		}
		if got := bc.Buffered(); got != 3 {
			t.Fatalf("Buffered() = %d, want 3", got)
		}
		for _, st := range []struct{ read, want int }{{2, 1}, {1, 0}} {
			if _, err := io.ReadFull(c, make([]byte, st.read)); err != nil {
				t.Fatal(err)
			}
			if got := bc.Buffered(); got != st.want {
				t.Fatalf("Buffered() after %d bytes = %d, want %d", st.read, got, st.want)
			}
		}
		_ = c.Close()
	})

	t.Run("RawConnFull preserves pending", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		_, peer := putIdlePipe(t, p, addr, 64)
		writeAsync(t, peer, "raw")

		c, err := p.DialContext(ctxTrue, addr.Network(), addr.String())
		if err != nil {
			t.Fatal(err)
		}
		rf, ok := c.(interface {
			RawConnFull() (net.Conn, []byte, error)
		})
		if !ok {
			t.Fatal("connection does not expose RawConnFull")
		}
		raw, pending, _ := rf.RawConnFull()
		if string(pending) != "raw" {
			t.Fatalf("pending = %q, want raw", pending)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close after RawConnFull: %v", err)
		}
		_ = raw.Close()
		assertCounts(t, p, addr, 0, 0, 0)
	})
}

// ---------------------------------------------------------------------------
// 死连接不复用（合并 TestDeadConnNotReused）
// ---------------------------------------------------------------------------

func TestDeadConnNotReused(t *testing.T) {
	pd := newPipeDialer(t)
	p := newTestPool(t, pd)
	addr := Addr{Net: "tcp", Name: "dead.test:1"}

	c := mustDial(t, p, addr)
	peer := pd.Peer(0)
	if peer == nil {
		t.Fatal("dialer peer missing")
	}

	_ = peer.Close() // 对端挂断
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read after peer close: want an error, got nil")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close dead conn: %v", err)
	}
	assertCounts(t, p, addr, 0, 0, 0)

	c2 := mustDial(t, p, addr)
	if c2.IsReuseConn() {
		t.Fatal("dead connection must not be reused from pool")
	}
	if got := pd.Calls(); got != 2 {
		t.Fatalf("dialer calls = %d, want 2", got)
	}
	_ = c2.Close()
}

// ---------------------------------------------------------------------------
// 等待接口（合并 TestWaitersTransitions / TestWaitersEmptyBucketContract /
//           connPool_wait_test.go 的全部用例；统一改为 WaitStatus 语义）
// ---------------------------------------------------------------------------

func TestWaiterImmediate(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "wait.immediate:1"}
	tests := []struct {
		name string
		mk   func(*Pool, net.Addr) <-chan WaitStatus
	}{
		{"idle>=0", func(p *Pool, a net.Addr) <-chan WaitStatus { return p.WaitConnIdleGeq(nil, a, 0) }},
		{"idle<=0", func(p *Pool, a net.Addr) <-chan WaitStatus { return p.WaitConnIdleLeq(nil, a, 0) }},
		{"num>=0", func(p *Pool, a net.Addr) <-chan WaitStatus { return p.WaitConnNumGeq(nil, a, 0) }},
		{"num<=0", func(p *Pool, a net.Addr) <-chan WaitStatus { return p.WaitConnNumLeq(nil, a, 0) }},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool(t, newPipeDialer(t))
			if st := waitStatus(t, tc.mk(p, addr)); st != WaitSatisfied {
				t.Fatalf("status = %v, want WaitSatisfied", st)
			}
		})
	}
}

func TestWaiterErrOther(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "wait.err:1"}
	tests := []struct {
		name    string
		maxConn int
		mk      func(*Pool, net.Addr) <-chan WaitStatus
	}{
		{"idle>= exceeds MaxConn", 2, func(p *Pool, a net.Addr) <-chan WaitStatus { return p.WaitConnIdleGeq(nil, a, 5) }},
		{"num>= exceeds MaxConn", 2, func(p *Pool, a net.Addr) <-chan WaitStatus { return p.WaitConnNumGeq(nil, a, 5) }},
		{"num<= negative", 0, func(p *Pool, a net.Addr) <-chan WaitStatus { return p.WaitConnNumLeq(nil, a, -1) }},
		{"idle<= negative", 0, func(p *Pool, a net.Addr) <-chan WaitStatus { return p.WaitConnIdleLeq(nil, a, -1) }},
		{"nil addr", 0, func(p *Pool, a net.Addr) <-chan WaitStatus { return p.WaitConnIdleGeq(nil, nil, 1) }},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool(t, newPipeDialer(t), func(p *Pool) {
				p.UpdateConfig(func(c *Config) { c.MaxConn = tc.maxConn })
			})
			if st := waitStatus(t, tc.mk(p, addr)); st != WaitErrOther {
				t.Fatalf("status = %v, want WaitErrOther", st)
			}
		})
	}
}

func TestWaiterTransitions(t *testing.T) {
	pd := newPipeDialer(t)
	p := newTestPool(t, pd)
	addr := Addr{Net: "tcp", Name: "wait.trans:1"}

	c1 := mustDial(t, p, addr)
	chNum2 := p.WaitConnNumGeq(nil, addr, 2)
	c2 := mustDial(t, p, addr)
	if st := waitStatus(t, chNum2); st != WaitSatisfied {
		t.Fatalf("num>=2 status = %v", st)
	}

	chIdle1 := p.WaitConnIdleGeq(nil, addr, 1)
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	if st := waitStatus(t, chIdle1); st != WaitSatisfied {
		t.Fatalf("idle>=1 status = %v", st)
	}

	chIdle0 := p.WaitConnIdleLeq(nil, addr, 0)
	raw, _, err := p.GetFull(addr)
	if err != nil {
		t.Fatal(err)
	}
	if st := waitStatus(t, chIdle0); st != WaitSatisfied {
		t.Fatalf("idle<=0 status = %v", st)
	}
	_ = raw.Close()

	chNum0 := p.WaitConnNumLeq(nil, addr, 0)
	raw2, _ := c2.RawConn()
	if st := waitStatus(t, chNum0); st != WaitSatisfied {
		t.Fatalf("num<=0 status = %v", st)
	}
	_ = raw2.Close()
}

func TestWaiterWakeOnPut(t *testing.T) {
	p := newTestPool(t, newPipeDialer(t))
	addr := Addr{Net: "tcp", Name: "wait.put:1"}

	ch := p.WaitConnIdleGeq(nil, addr, 1)
	select {
	case st := <-ch:
		t.Fatalf("resolved early: %v", st)
	case <-time.After(50 * time.Millisecond):
	}

	server, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	if err := p.PutFull(server, addr, time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	if st := waitStatus(t, ch); st != WaitSatisfied {
		t.Fatalf("status = %v, want WaitSatisfied", st)
	}
}

func TestWaiterContextCancel(t *testing.T) {
	p := newTestPool(t, newPipeDialer(t))
	addr := Addr{Net: "tcp", Name: "wait.cancel:1"}

	ctx, cancel := context.WithCancel(context.Background())
	ch := p.WaitConnIdleGeq(ctx, addr, 5)
	cancel()
	if st := waitStatus(t, ch); st != WaitContextDone {
		t.Fatalf("status = %v, want WaitContextDone", st)
	}
}

func TestWaiterPoolClose(t *testing.T) {
	p := newTestPool(t, newPipeDialer(t))
	addr := Addr{Net: "tcp", Name: "wait.close:1"}

	ch := p.WaitConnNumGeq(nil, addr, 10) // 老 API：零协程路径
	_ = p.Close()
	if st := waitStatus(t, ch); st != WaitPoolClosed {
		t.Fatalf("status = %v, want WaitPoolClosed", st)
	}
	// 关池后的新等待应立即返回 WaitPoolClosed
	if st := waitStatus(t, p.WaitConnIdleGeq(nil, addr, 1)); st != WaitPoolClosed {
		t.Fatalf("post-close status = %v, want WaitPoolClosed", st)
	}
}

func TestWaiterGoroutineReclaimed(t *testing.T) {
	p := newTestPool(t, newPipeDialer(t))
	addr := Addr{Net: "tcp", Name: "wait.gor:1"}

	runtime.GC()
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	chs := make([]<-chan WaitStatus, 0, 64)
	for i := 0; i < 64; i++ {
		chs = append(chs, p.WaitConnIdleGeq(ctx, addr, 1000))
	}
	cancel()

	for _, ch := range chs {
		if st := waitStatus(t, ch); st != WaitContextDone {
			t.Fatalf("status = %v, want WaitContextDone", st)
		}
	}

	// sweeper 常驻 +1，故容忍 +2。
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 {
		if time.Now().After(deadline) {
			t.Fatalf("watcher goroutines not reclaimed: base=%d now=%d", base, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWaiterStress(t *testing.T) {
	pd := newPipeDialer(t)
	p := newTestPool(t, pd)
	addr := Addr{Net: "tcp", Name: "wait.stress:1"}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%5+1)*time.Millisecond)
			defer cancel()
			ch := p.WaitConnIdleGeq(ctx, addr, 100) // 阈值 100，永远不会被满足
			select {
			case <-ch:
			case <-ctx.Done():
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				c, err := p.Dial(addr.Network(), addr.String())
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}
	wg.Wait()

	p.CloseIdleConnections()
	if n := p.Num(); n != 0 {
		t.Fatalf("Num = %d, want 0 after drain", n)
	}
}

func TestWaitStatusString(t *testing.T) {
	for _, tc := range []struct {
		st   WaitStatus
		want string
	}{
		{WaitSatisfied, "wait: satisfied"},
		{WaitAborted, "wait: aborted, condition not met"},
		{WaitContextDone, "wait: context done"},
		{WaitPoolClosed, "wait: pool closed"},
		{WaitErrOther, "wait: condition can never be satisfied"},
		{WaitStatus(99), "wait: unknown"},
	} {
		if got := tc.st.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.st, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 配置（合并 pool_config_test.go）
// ---------------------------------------------------------------------------

func TestConfigBootstrap(t *testing.T) {
	t.Run("New publishes given config", func(t *testing.T) {
		p := New(Config{IdleTimeout: time.Minute, IdleConn: 2, MaxConn: 4})
		defer p.Close()

		got := p.Config()
		if got.IdleTimeout != time.Minute || got.IdleConn != 2 || got.MaxConn != 4 {
			t.Fatalf("bootstrap conf = %+v", got)
		}
	})

	t.Run("zero value pool", func(t *testing.T) {
		p := &Pool{}
		got := p.Config()
		if got.IdleTimeout != 0 || got.IdleConn != 0 || got.MaxConn != 0 || got.Dialer != nil || got.ResolveAddr != nil {
			t.Fatalf("zero conf = %+v, want all-zero", got)
		}
		_ = p.Close()
	})
}

func TestConfigNormalizeAndReload(t *testing.T) {
	p := New(Config{IdleTimeout: time.Minute, MaxConn: 4})
	defer p.Close()

	if err := p.SetConfig(Config{IdleConn: -5, MaxConn: -1, IdleTimeout: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	c := p.Config()
	if c.IdleConn != 0 || c.MaxConn != 0 { // 负值归一
		t.Fatalf("normalize failed: %+v", c)
	}
	if c.IdleTimeout != time.Millisecond {
		t.Fatalf("IdleTimeout = %v, want %v", c.IdleTimeout, time.Millisecond)
	}
	if c.Dialer != nil || c.ResolveAddr != nil {
		t.Fatalf("SetConfig must clear omitted fields: %+v", c)
	}

	// Config() 返回副本：改副本不影响内部快照
	c.IdleTimeout = time.Hour
	if got := p.Config().IdleTimeout; got != time.Millisecond {
		t.Fatalf("Config() must be a copy, snapshot = %v", got)
	}

	if err := p.UpdateConfig(func(c *Config) { c.IdleTimeout = 2 * time.Millisecond }); err != nil {
		t.Fatal(err)
	}
	if got := p.Config().IdleTimeout; got != 2*time.Millisecond {
		t.Fatalf("UpdateConfig not applied: %v", got)
	}

	// UpdateConfig(nil) 不改动任何可比较字段（“不发布新快照”的等价黑盒断言）
	before := p.Config()
	if err := p.UpdateConfig(nil); err != nil {
		t.Fatal(err)
	}
	after := p.Config()
	if after.IdleTimeout != before.IdleTimeout || after.IdleConn != before.IdleConn || after.MaxConn != before.MaxConn {
		t.Fatal("UpdateConfig(nil) changed config")
	}

	if err := p.SetConfig(Config{}); err != nil {
		t.Fatalf("SetConfig on open pool = %v, want nil", err)
	}
	if got := p.Config(); got.IdleTimeout != 0 || got.IdleConn != 0 || got.MaxConn != 0 {
		t.Fatalf("SetConfig(Config{}) = %+v, want zero", got)
	}

	_ = p.Close()
	if err := p.SetConfig(Config{}); !errors.Is(err, ErrConnPoolClosed) {
		t.Fatalf("after Close = %v, want ErrConnPoolClosed", err)
	}
	mutated := false
	if err := p.UpdateConfig(func(c *Config) { mutated = true; c.MaxConn = 1 }); !errors.Is(err, ErrConnPoolClosed) {
		t.Fatalf("UpdateConfig after Close = %v, want ErrConnPoolClosed", err)
	}
	if mutated {
		t.Fatal("mutate must not run after Close")
	}
}

func TestConfigConvergence(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "cfg.conv:1"}
	p := New(Config{IdleTimeout: 0, IdleConn: 4}) // 空闲不过期；单地址上限 4
	defer p.Close()

	srv1, peer1 := net.Pipe()
	srv2, peer2 := net.Pipe()
	t.Cleanup(func() { _ = peer1.Close(); _ = peer2.Close() })

	if err := p.PutFull(srv1, addr, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.PutFull(srv2, addr, 0, 0); err != nil {
		t.Fatal(err)
	}
	if n := p.NumIdle(addr); n != 2 {
		t.Fatalf("NumIdle = %d, want 2", n)
	}

	if err := p.SetConfig(Config{IdleConn: 0}); err != nil { // 0 = 不限
		t.Fatal(err)
	}
	if n := p.NumIdle(addr); n != 2 {
		t.Fatalf("IdleConn=0 must be unlimited, idle = %d", n)
	}

	if err := p.SetConfig(Config{IdleConn: 1}); err != nil { // 收紧到 1：淘汰最旧
		t.Fatal(err)
	}
	if n := p.NumIdle(addr); n != 1 {
		t.Fatalf("trimIdle failed, idle = %d", n)
	}
	if n := p.Num(); n != 1 {
		t.Fatalf("count not released: %d", n)
	}
	assertPeerClosed(t, peer1) // 最旧的 srv1 应被关闭

	if err := p.SetConfig(Config{IdleConn: 4, IdleTimeout: 30 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return p.NumIdle(addr) == 0 && p.Num() == 0 }) {
		t.Fatalf("reapplyTimeout failed: idle=%d num=%d", p.NumIdle(addr), p.Num())
	}
}

// 关键回归：修改“无关配置”（Dialer）不得重置空闲连接的自定义过期时间。
func TestConfigUnrelatedChangeKeepsTimeout(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "cfg.unrelated:1"}
	// 池默认 IdleTimeout 很长，只有 PutFull 的自定义超时（60ms）会触发回收。
	p := New(Config{IdleTimeout: time.Hour, ResolveAddr: testResolver})
	defer p.Close()

	srv, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	if err := p.PutFull(srv, addr, 60*time.Millisecond, 0); err != nil {
		t.Fatal(err)
	}
	if n := p.NumIdle(addr); n != 1 {
		t.Fatalf("NumIdle = %d, want 1", n)
	}

	// 无关字段变更：不应触发 reapplyTimeout。
	if err := p.UpdateConfig(func(c *Config) { c.Dialer = echoDialer{} }); err != nil {
		t.Fatal(err)
	}

	// 若过期时间被错误重置为 now+1h，则永远不会在此窗口内被回收。
	if !waitFor(t, 2*time.Second, func() bool { return p.NumIdle(addr) == 0 }) {
		t.Fatal("unrelated config change reset idle timeout; conn never expired")
	}
	if n := p.Num(); n != 0 {
		t.Fatalf("Num = %d, want 0", n)
	}
}

func TestConfigHotReloadRace(t *testing.T) {
	pd := newPipeDialer(t)
	addr := Addr{Net: "tcp", Name: "cfg.race:1"}
	p := newTestPool(t, pd, func(p *Pool) {
		p.UpdateConfig(func(c *Config) {
			c.IdleTimeout = 10 * time.Millisecond
			c.IdleConn = 8
			c.MaxConn = 64
		})
	})

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // 持续热更新
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = p.SetConfig(Config{
				Dialer:      pd,
				ResolveAddr: testResolver,
				IdleTimeout: time.Duration(i%7) * time.Millisecond,
				IdleConn:    i % 5,
				MaxConn:     64,
			})
		}
	}()
	go func() { // 持续读配置/计数
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = p.Config()
			_ = p.Num()
			_ = p.NumIdle(addr)
			_ = p.NumConn(addr)
		}
	}()
	go func() { // 持续拨号/归还
		defer wg.Done()
		for i := 0; i < 100; i++ {
			c, err := p.Dial(addr.Network(), addr.String())
			if err == nil {
				_ = c.Close()
			}
		}
	}()
	wg.Wait()

	p.CloseIdleConnections()
	if !waitFor(t, time.Second, func() bool { return p.Num() == 0 }) {
		t.Fatalf("connNum leak: %d", p.Num())
	}
}

// ---------------------------------------------------------------------------
// 并发压测（合并 TestConcurrentDialWriteReadClose）
// ---------------------------------------------------------------------------

func TestConcurrentDialWriteReadClose(t *testing.T) {
	const (
		workers    = 8
		iterations = 20
		idleMax    = 8
	)
	addr := Addr{Net: "tcp", Name: "concurrent.test:1"}
	p := newTestPool(t, echoDialer{}, func(p *Pool) {
		p.UpdateConfig(func(c *Config) { c.IdleConn = idleMax })
	})

	stopStats := make(chan struct{})
	var statsWg sync.WaitGroup
	for i := 0; i < 4; i++ {
		statsWg.Add(1)
		go func() {
			defer statsWg.Done()
			for {
				select {
				case <-stopStats:
					return
				default:
					_ = p.Num()
					_ = p.NumIdle(addr)
					_ = p.NumConn(addr)
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c, err := p.Dial(addr.Network(), addr.String())
				if err != nil {
					errCh <- err
					return
				}
				if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
					_ = c.Close()
					errCh <- err
					return
				}
				var b [1]byte
				if _, err := c.Write(b[:]); err != nil {
					_ = c.Close()
					errCh <- err
					return
				}
				if _, err := io.ReadFull(c, b[:]); err != nil {
					_ = c.Close()
					errCh <- err
					return
				}
				if err := c.Close(); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(stopStats)
	statsWg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	if num, idle := p.Num(), p.NumIdle(addr); num != idle {
		t.Fatalf("Num()=%d, NumIdle()=%d; want equal", num, idle)
	}
	if idle := p.NumIdle(addr); idle > idleMax {
		t.Fatalf("NumIdle()=%d exceeds IdleConn=%d", idle, idleMax)
	}
	if conn := p.NumConn(addr); conn != p.NumIdle(addr) {
		t.Fatalf("NumConn()=%d, want == NumIdle()=%d", conn, p.NumIdle(addr))
	}

	idleBefore := p.NumIdle(addr)
	drained := 0
	for {
		c, _, err := p.GetFull(addr)
		if errors.Is(err, ErrConnNotAvailable) {
			break
		}
		if err != nil {
			t.Fatalf("GetFull during drain: %v", err)
		}
		drained++
		_ = c.Close()
	}
	if drained != idleBefore {
		t.Fatalf("drained %d conns, but NumIdle was %d", drained, idleBefore)
	}
	if got := p.Num(); got != 0 {
		t.Fatalf("Num() after drain = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 基准测试（保留原有 4 个，非重复）
// ---------------------------------------------------------------------------

func BenchmarkPoolDialClose(b *testing.B) {
	pd := &pipeDialer{}
	p := New(Config{Dialer: pd, ResolveAddr: testResolver, IdleTimeout: time.Minute})
	defer func() { _ = p.Close(); pd.closePeers() }()

	addr := Addr{Net: "tcp", Name: "bench.dial:1"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := p.Dial(addr.Network(), addr.String())
		if err != nil {
			b.Fatal(err)
		}
		if err := c.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPoolDialWriteReadClose(b *testing.B) {
	p := New(Config{Dialer: echoDialer{}, ResolveAddr: testResolver, IdleTimeout: time.Minute})
	defer func() { _ = p.Close() }()

	addr := Addr{Net: "tcp", Name: "bench.io:1"}
	var one [1]byte
	b.ReportAllocs()
	b.SetBytes(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := p.Dial(addr.Network(), addr.String())
		if err != nil {
			b.Fatal(err)
		}
		if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
			_ = c.Close()
			b.Fatal(err)
		}
		if _, err := c.Write(one[:]); err != nil {
			_ = c.Close()
			b.Fatal(err)
		}
		if _, err := io.ReadFull(c, one[:]); err != nil {
			_ = c.Close()
			b.Fatal(err)
		}
		if err := c.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPoolParallelDialWriteReadClose(b *testing.B) {
	p := New(Config{Dialer: echoDialer{}, ResolveAddr: testResolver, IdleTimeout: time.Minute})
	defer func() { _ = p.Close() }()

	addr := Addr{Net: "tcp", Name: "bench.parallel:1"}
	b.ReportAllocs()
	b.SetBytes(1)
	b.RunParallel(func(pb *testing.PB) {
		var one [1]byte
		for pb.Next() {
			c, err := p.Dial(addr.Network(), addr.String())
			if err != nil {
				b.Error(err)
				return
			}
			if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
				_ = c.Close()
				b.Error(err)
				return
			}
			if _, err := c.Write(one[:]); err != nil {
				_ = c.Close()
				b.Error(err)
				return
			}
			if _, err := io.ReadFull(c, one[:]); err != nil {
				_ = c.Close()
				b.Error(err)
				return
			}
			if err := c.Close(); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkPoolGetFullPutFull(b *testing.B) {
	p := New(Config{IdleTimeout: time.Minute})
	addr := Addr{Net: "tcp", Name: "bench.getput:1"}

	server, peer := net.Pipe()
	defer func() { _ = p.Close(); _ = peer.Close() }()
	if err := p.PutFull(server, addr, time.Minute, 0); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, _, err := p.GetFull(addr)
		if err != nil {
			b.Fatal(err)
		}
		if err := p.PutFull(conn, addr, time.Minute, 0); err != nil {
			b.Fatal(err)
		}
	}
}
