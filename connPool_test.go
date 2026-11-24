package vconnpool

import (
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
// 测试基础设施
// ============================================================================

var testAddr = Addr{Net: "tcp", Name: "pool.test:1"}

func testResolver(network, address string) (net.Addr, error) {
	return Addr{Net: network, Name: address}, nil
}

func newTestPool(t *testing.T, d Dialer, opts ...func(*Pool)) *Pool {
	t.Helper()
	p := New(Config{
		Dialer:      d,
		ResolveAddr: testResolver,
		IdleTimeout: time.Minute,
	})
	for _, opt := range opts {
		opt(p)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// pipeDialer 每次 Dial 产出一条 net.Pipe，记录全部 server 端供断言使用。
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

// echoDialer 每条连接自回显，供并发/基准测试。
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

// ============================================================================
// 断言助手
// ============================================================================

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

func mustDial(t *testing.T, p *Pool, addr net.Addr) Conn {
	t.Helper()
	c, err := p.Dial(addr.Network(), addr.String())
	if err != nil {
		t.Fatalf("Dial(%q, %q): %v", addr.Network(), addr.String(), err)
	}
	return c
}

func waitBool(t *testing.T, ch <-chan bool) bool {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for waiter result")
		return false
	}
}

// waitFor 轮询 pred 直到成立或超时，避免固定 Sleep。
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

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}

// putIdlePipe 直接向池放入一条空闲 Pipe，返回 (池内 conn, 对端 peer)。
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

// roundTrip 双向冒烟：peer 写 msg，c 必须完整读到。
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

// ============================================================================
// 地址解析
// ============================================================================

func TestResolveAddrAndAddr(t *testing.T) {
	tests := []struct {
		name     string
		network  string
		address  string
		wantType string
		wantNet  string
	}{
		{"tcp", "tcp", "127.0.0.1:12345", "*net.TCPAddr", "tcp"},
		{"tcp4", "tcp4", "127.0.0.1:12345", "*net.TCPAddr", "tcp"},
		{"udp4", "udp4", "127.0.0.1:12345", "*net.UDPAddr", "udp"},
		{"unix", "unix", "/tmp/vconnpool-test.sock", "*net.UnixAddr", "unix"},
		{"ip", "ip", "127.0.0.1", "*net.IPAddr", "ip"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
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
			t.Fatal("unsupported network should return error")
		}
	})

	t.Run("Addr", func(t *testing.T) {
		a := Addr{Net: "udp", Name: "example:9"}
		if a.Network() != "udp" || a.String() != "example:9" {
			t.Fatalf("Addr = (%q, %q)", a.Network(), a.String())
		}
	})
}

// ============================================================================
// 生命周期：复用 / 关闭 / RawConn 移交
// ============================================================================

func TestPoolCountsAndReuse(t *testing.T) {
	pd := newPipeDialer(t)
	p := newTestPool(t, pd)
	addr := Addr{Net: "tcp", Name: "counts.test:1"}

	c1, err := p.Dial(addr.Network(), addr.String())
	if err != nil {
		t.Fatal(err)
	}
	if c1.IsReuseConn() {
		t.Fatal("first Dial should be a new connection")
	}
	assertCounts(t, p, addr, 1, 0, 1)

	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, p, addr, 1, 1, 1)
	if _, err := c1.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read after Close = %v, want net.ErrClosed", err)
	}

	c2, err := p.Dial(addr.Network(), addr.String())
	if err != nil {
		t.Fatal(err)
	}
	if !c2.IsReuseConn() {
		t.Fatal("second Dial should reuse pooled connection")
	}
	if got := pd.Calls(); got != 1 {
		t.Fatalf("dialer calls = %d, want 1", got)
	}

	// RawConn 移交所有权后：配额归零、Close 幂等、底层连接仍归调用方。
	raw := c2.RawConn()
	assertCounts(t, p, addr, 0, 0, 0)
	if err := c2.Close(); err != nil {
		t.Fatalf("Close after RawConn: %v", err)
	}
	roundTrip(t, raw, pd.Peer(0), "still-owned")
	_ = raw.Close()
}

func TestConnLifecycle(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "lifecycle.test:1"}

	t.Run("Discard prevents pooling", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		c := mustDial(t, p, addr)
		c.Discard()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		assertCounts(t, p, addr, 0, 0, 0)
	})

	t.Run("RawConn is single use", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd)
		c := mustDial(t, p, addr)

		raw := c.RawConn()
		if raw == nil {
			t.Fatal("RawConn returned nil")
		}
		assertCounts(t, p, addr, 0, 0, 0)
		mustPanic(t, func() { _ = c.RawConn() }) // 二次移交必须 panic
		if err := c.Close(); err != nil {
			t.Fatalf("Close after RawConn: %v", err)
		}
		_ = raw.Close()
	})

	// 出借前必须清掉上一个使用者的 deadline，否则污染下一个使用者。
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
		writeDone := make(chan error, 1)
		go func() { _, err := peer.Write([]byte("x")); writeDone <- err }()

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
		select {
		case err := <-writeDone:
			if err != nil {
				t.Fatalf("peer write: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("peer write timed out")
		}
		_ = c2.Close()
	})
}

// ============================================================================
// 空闲超时
// ============================================================================

func TestIdleTimeout(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "idle.timeout.test:1"}

	// 到期回收：Dial 归还路径与 PutFull 自定义超时共用同一套过期判定。
	t.Run("expired conn reclaimed", func(t *testing.T) {
		tests := []struct {
			name string
			use  func(t *testing.T, p *Pool, timeout time.Duration) net.Addr
			idle time.Duration
		}{
			{"dial close default timeout", func(t *testing.T, p *Pool, _ time.Duration) net.Addr {
				c := mustDial(t, p, addr)
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				if got := p.NumIdle(addr); got != 1 {
					t.Fatalf("NumIdle() right after Close = %d, want 1", got)
				}
				return addr
			}, 50 * time.Millisecond},
			{"putfull custom timeout", func(t *testing.T, p *Pool, timeout time.Duration) net.Addr {
				server, peer := net.Pipe()
				t.Cleanup(func() { _ = peer.Close() })
				if err := p.PutFull(server, addr, timeout, 0); err != nil {
					t.Fatal(err)
				}
				if got := p.NumIdle(addr); got != 1 {
					t.Fatalf("NumIdle() after PutFull = %d, want 1", got)
				}
				return addr
			}, 30 * time.Millisecond},
		}
		for _, tc := range tests {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				p := newTestPool(t, newPipeDialer(t), func(p *Pool) {
					p.UpdateConfig(func(c *Config) {
						c.IdleTimeout = tc.idle
					})
				})
				a := tc.use(t, p, tc.idle)

				// 轮询等扫描器回收，而不是固定睡眠。
				if !waitFor(t, 5*time.Second, func() bool { return p.NumIdle(a) == 0 }) {
					t.Fatalf("idle conn not reclaimed within 5s (NumIdle=%d)", p.NumIdle(a))
				}
				if _, _, err := p.GetFull(a); !errors.Is(err, ErrConnNotAvailable) {
					t.Fatalf("GetFull after timeout = %v, want ErrConnNotAvailable", err)
				}
				assertCounts(t, p, a, 0, 0, 0)
			})
		}
	})

	// 远未到期的窗口内：不提前回收、取出仍可用。
	t.Run("not reclaimed before timeout", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t), func(p *Pool) {
			p.UpdateConfig(func(c *Config) {
				c.IdleTimeout = 5 * time.Second
			})
		})

		c := mustDial(t, p, addr)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}

		// 采样窗口 ≈ IdleTimeout/33，远离边界，避免时序抖动。
		deadline := time.Now().Add(150 * time.Millisecond)
		for time.Now().Before(deadline) {
			if got := p.NumIdle(addr); got != 1 {
				t.Fatalf("idle conn reclaimed too early: NumIdle() = %d, want 1", got)
			}
			time.Sleep(5 * time.Millisecond)
		}
		conn, _, err := p.GetFull(addr)
		if err != nil {
			t.Fatalf("GetFull before timeout: %v", err)
		}
		_ = conn.Close()
	})
}

// ============================================================================
// Put / PutFull / Add 契约（含失败路径的所有权承诺）
// ============================================================================

func TestPutContracts(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "put.contracts:1"}

	t.Run("nil conn or nil addr", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		if err := p.PutFull(nil, testAddr, time.Minute, 0); err == nil {
			t.Fatal("PutFull(nil, ...) should fail")
		}
		if err := p.Add(nil); err == nil {
			t.Fatal("Add(nil) should fail")
		}
		server, _ := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		if err := p.PutFull(server, nil, time.Minute, 0); err == nil {
			t.Fatal("PutFull(conn, nil, ...) should fail")
		}
	})

	// 所有权承诺：返回错误 ⇒ 连接已被关闭，调用方不应再 Close。
	// 用对端读到 EOF/重置确认关闭，避免假阳性。
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
		if got := p.Num(); got != 0 {
			t.Fatalf("Num() = %d, want 0", got)
		}
	})

	t.Run("duplicate put is idempotent", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })

		if err := p.PutFull(server, addr, time.Minute, 0); err != nil {
			t.Fatal(err)
		}
		if err := p.PutFull(server, addr, time.Minute, 0); err != nil {
			t.Fatalf("duplicate PutFull returned %v, want nil", err)
		}
		assertCounts(t, p, addr, 1, 1, 1)

		out, _, err := p.GetFull(addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = out.Close()
	})

	t.Run("over idle limit fails and closes conn", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t), func(p *Pool) {
			p.UpdateConfig(func(c *Config) {
				c.IdleConn = 1
			})
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
	})

	t.Run("over max conn fails and closes conn", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd, func(p *Pool) {
			p.UpdateConfig(func(c *Config) {
				c.MaxConn = 1
			})
		})
		held := mustDial(t, p, addr)

		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		if err := p.PutFull(server, addr, time.Minute, 0); !errors.Is(err, ErrConnPoolMax) {
			t.Fatalf("PutFull over max conn = %v, want ErrConnPoolMax", err)
		}
		if got := p.Num(); got != 1 {
			t.Fatalf("Num() = %d, want 1 (held conn still in use)", got)
		}
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

// ============================================================================
// Dial / DialContext 策略 / MaxConn
// ============================================================================

func TestMaxConnAndFailureAccounting(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "max.conn.test:1"}

	t.Run("max conn enforced", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd, func(p *Pool) {
			p.UpdateConfig(func(c *Config) {
				c.MaxConn = 1
			})
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
			p.UpdateConfig(func(c *Config) {
				c.MaxConn = 1
			})
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

func TestDialContextPolicy(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "dial.context:1"}

	// putIdleClose：建 1 条在用连接再归还，返回累计拨号次数基准。
	prepare := func(t *testing.T, p *Pool) {
		c1 := mustDial(t, p, addr)
		if err := c1.Close(); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name    string
		ctx     context.Context
		wantNet bool // 是否期望复用池内连接
	}{
		{"true reuses idle", context.WithValue(context.Background(), PoolConnContextKey, true), true},
		{"false forces new dial", context.WithValue(context.Background(), PoolConnContextKey, false), false},
		{"non-bool behaves as unset", context.WithValue(context.Background(), PoolConnContextKey, "not-bool"), true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pd := newPipeDialer(t)
			p := newTestPool(t, pd)
			prepare(t, p) // 拨号 1 次 + 归还入池

			c2, err := p.DialContext(tc.ctx, addr.Network(), addr.String())
			if err != nil {
				t.Fatal(err)
			}
			if c2.IsReuseConn() != tc.wantNet {
				t.Fatalf("IsReuseConn() = %v, want %v", c2.IsReuseConn(), tc.wantNet)
			}
			wantCalls := 1
			if !tc.wantNet {
				wantCalls = 2
			}
			if got := pd.Calls(); got != wantCalls {
				t.Fatalf("dialer calls = %d, want %d", got, wantCalls)
			}
			_ = c2.Close()
		})
	}

	t.Run("true with empty pool never dials", func(t *testing.T) {
		pd := newPipeDialer(t)
		p := newTestPool(t, pd)
		ctxTrue := context.WithValue(context.Background(), PoolConnContextKey, true)

		if _, err := p.DialContext(ctxTrue, addr.Network(), addr.String()); !errors.Is(err, ErrConnNotAvailable) {
			t.Fatalf("DialContext = %v, want ErrConnNotAvailable", err)
		}
		if got := pd.Calls(); got != 0 {
			t.Fatalf("dialer calls = %d, want 0", got)
		}

		// 池非空但已被取走时同样拒绝，绝不拨号。
		c1 := mustDial(t, p, addr)
		if _, err := p.DialContext(ctxTrue, addr.Network(), addr.String()); !errors.Is(err, ErrConnNotAvailable) {
			t.Fatalf("DialContext while pool empty = %v, want ErrConnNotAvailable", err)
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

	// 从池里取到连接后发现 ctx 已取消：连接必须被丢弃并归还配额。
	t.Run("canceled after pooled checkout", func(t *testing.T) {
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

// ============================================================================
// Close 语义 / 所有权
// ============================================================================

func TestPoolCloseOwnership(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "pool.close:1"}

	t.Run("closes owned connections and is idempotent", func(t *testing.T) {
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

		// Close 后所有路径都拒绝，且已出借/已归还的连接不可再用。
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

	t.Run("GetFull transfers ownership out of pool", func(t *testing.T) {
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
		// 所有权已转给调用方：Pool.Close 不应关闭 out。
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

func TestNilAddrContracts(t *testing.T) {
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

// ============================================================================
// 等待器
// ============================================================================

func TestWaitersTransitions(t *testing.T) {
	pd := newPipeDialer(t)
	p := newTestPool(t, pd)
	addr := Addr{Net: "tcp", Name: "wait.transition:1"}

	// Num >= 2：第二次 Dial 后成立。
	c1 := mustDial(t, p, addr)
	chNum2 := p.WaitConnNumGeq(addr, 2)
	c2 := mustDial(t, p, addr)
	if got := waitBool(t, chNum2); !got {
		t.Fatal("WaitConnNumGeq(2) should become true")
	}

	// Idle >= 1：归还后成立。
	chIdle1 := p.WaitConnIdleGeq(addr, 1)
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	if got := waitBool(t, chIdle1); !got {
		t.Fatal("WaitConnIdleGeq(1) should become true")
	}

	// Idle <= 0：GetFull pop 掉唯一空闲连接后成立。
	chIdle0 := p.WaitConnIdleLeq(addr, 0)
	raw, _, err := p.GetFull(addr)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitBool(t, chIdle0); !got {
		t.Fatal("WaitConnIdleLeq(0) should become true after pop")
	}
	_ = raw.Close()

	// Num <= 0：RawConn 移交所有权后成立。
	chNum0 := p.WaitConnNumLeq(addr, 0)
	raw2 := c2.RawConn()
	if got := waitBool(t, chNum0); !got {
		t.Fatal("WaitConnNumLeq(0) should become true after ownership transfer")
	}
	_ = raw2.Close()

	// 池关闭必须向未满足的等待者投递 false。
	p2 := newTestPool(t, newPipeDialer(t))
	c3 := mustDial(t, p2, addr)
	chAbort := p2.WaitConnIdleGeq(addr, 1)
	if err := p2.Close(); err != nil {
		t.Fatal(err)
	}
	if got := waitBool(t, chAbort); got {
		t.Fatal("waiter should receive false when pool closes")
	}
	_ = c3.Close()
}

func TestWaitersEmptyBucketContract(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "wait.empty:1"}

	// 空桶在计数语义上等价于 0：非正阈值应立即成立。
	tests := []struct {
		name string
		ch   <-chan bool
	}{
		{"idle>=0", newTestPool(t, newPipeDialer(t)).WaitConnIdleGeq(addr, 0)},
		{"idle<=0", newTestPool(t, newPipeDialer(t)).WaitConnIdleLeq(addr, 0)},
		{"num>=0", newTestPool(t, newPipeDialer(t)).WaitConnNumGeq(addr, 0)},
		{"num<=0", newTestPool(t, newPipeDialer(t)).WaitConnNumLeq(addr, 0)},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			select {
			case got := <-tc.ch:
				if !got {
					t.Fatal("empty bucket with non-positive threshold should be immediately true")
				}
			case <-time.After(time.Second):
				t.Fatal("empty bucket waiter did not resolve")
			}
		})
	}

	// 未知地址 + 正阈值：此刻不成立，Dial 后必须被唤醒。
	t.Run("unknown bucket with positive threshold fires after dial", func(t *testing.T) {
		p := newTestPool(t, newPipeDialer(t))
		a := Addr{Net: "tcp", Name: "wait.empty.geq:1"}
		ch := p.WaitConnIdleGeq(a, 1)

		select {
		case got := <-ch:
			t.Fatalf("resolved early: %v", got)
		case <-time.After(50 * time.Millisecond):
		}
		c := mustDial(t, p, a)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if got := waitBool(t, ch); !got {
			t.Fatal("WaitConnIdleGeq(1) should fire after connection returned")
		}
	})
}

// ============================================================================
// 预读数据 / Conn 扩展
// ============================================================================

func TestPendingData(t *testing.T) {
	addr := Addr{Net: "tcp", Name: "pending.test:1"}
	ctxTrue := context.WithValue(context.Background(), PoolConnContextKey, true)

	// 空闲期预读：GetFull 以 pending 字节交付，Get 回灌进 Read 流。
	t.Run("delivered via GetFull and Get", func(t *testing.T) {
		t.Run("GetFull returns idle bytes", func(t *testing.T) {
			p := newTestPool(t, newPipeDialer(t))
			_, peer := putIdlePipe(t, p, addr, 64)
			done := make(chan error, 1)
			go func() { _, err := peer.Write([]byte("hello")); done <- err }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("peer write: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("peer write blocked; background read did not consume")
			}

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

		t.Run("Get prepends idle bytes to Read stream", func(t *testing.T) {
			p := newTestPool(t, newPipeDialer(t))
			_, peer := putIdlePipe(t, p, addr, 64)
			done := make(chan error, 1)
			go func() { _, err := peer.Write([]byte("world")); done <- err }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("peer write: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("peer write blocked; background read did not consume")
			}

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
	})

	// Buffered 随读扣减到 0；RawConnFull 交出 pending 且零丢失。
	t.Run("Buffered and RawConnFull", func(t *testing.T) {
		t.Run("Buffered tracks pending bytes", func(t *testing.T) {
			p := newTestPool(t, newPipeDialer(t))
			_, peer := putIdlePipe(t, p, addr, 64)
			done := make(chan error, 1)
			go func() { _, err := peer.Write([]byte("abc")); done <- err }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("peer write: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("peer write blocked")
			}

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

			steps := []struct {
				read int
				want int
			}{{2, 1}, {1, 0}}
			for _, st := range steps {
				if _, err := io.ReadFull(c, make([]byte, st.read)); err != nil {
					t.Fatal(err)
				}
				if got := bc.Buffered(); got != st.want {
					t.Fatalf("Buffered() after %d bytes = %d, want %d", st.read, got, st.want)
				}
			}
			_ = c.Close()
		})

		t.Run("RawConnFull preserves pending bytes", func(t *testing.T) {
			p := newTestPool(t, newPipeDialer(t))
			_, peer := putIdlePipe(t, p, addr, 64)
			done := make(chan error, 1)
			go func() { _, err := peer.Write([]byte("raw")); done <- err }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("peer write: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("peer write blocked")
			}

			c, err := p.DialContext(ctxTrue, addr.Network(), addr.String())
			if err != nil {
				t.Fatal(err)
			}
			rf, ok := c.(interface{ RawConnFull() (net.Conn, []byte) })
			if !ok {
				t.Fatal("connection does not expose RawConnFull")
			}
			raw, pending := rf.RawConnFull()
			if string(pending) != "raw" {
				t.Fatalf("pending = %q, want raw", pending)
			}
			if err := c.Close(); err != nil {
				t.Fatalf("Close after RawConnFull: %v", err)
			}
			_ = raw.Close()
			assertCounts(t, p, addr, 0, 0, 0)
		})
	})
}

// ============================================================================
// 死连接不得入池 / 不得复用
// ============================================================================

// TestDeadConnNotReused：对端挂断（EOF）后归还，必须丢弃而非入池。
// 采用「读到对端结束 → 归还」的真实序列触发观测，不手动 Discard。
func TestDeadConnNotReused(t *testing.T) {
	pd := newPipeDialer(t)
	p := newTestPool(t, pd)
	addr := Addr{Net: "tcp", Name: "dead.notreused:1"}

	c := mustDial(t, p, addr)
	peer := pd.Peer(0)
	if peer == nil {
		t.Fatal("dialer peer missing")
	}

	_ = peer.Close() // 对端挂断，连接变死
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read after peer close: want an error, got nil")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close dead conn: %v", err)
	}
	assertCounts(t, p, addr, 0, 0, 0)

	// 再次 Dial 必须新建，证明死连接未被复用。
	c2 := mustDial(t, p, addr)
	if c2.IsReuseConn() {
		t.Fatal("dead connection must not be reused from pool")
	}
	if got := pd.Calls(); got != 2 {
		t.Fatalf("dialer calls = %d, want 2 (dead conn must not be reused)", got)
	}
	_ = c2.Close()
}

// ============================================================================
// 并发冒烟
// ============================================================================

func TestConcurrentDialWriteReadClose(t *testing.T) {
	const (
		workers    = 16
		iterations = 25
		idleMax    = 8
	)
	addr := Addr{Net: "tcp", Name: "concurrent.test:1"}

	p := newTestPool(t, echoDialer{}, func(p *Pool) {
		p.UpdateConfig(func(c *Config) {
			c.IdleConn = idleMax
		})
	})

	// 后台并发读统计接口：锁竞争下做冒烟，不参与正确性判定。
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
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
					_ = c.Close()
					select {
					case errCh <- err:
					default:
					}
					return
				}
				var b [1]byte
				if _, err := c.Write(b[:]); err != nil {
					_ = c.Close()
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if _, err := io.ReadFull(c, b[:]); err != nil {
					_ = c.Close()
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if err := c.Close(); err != nil {
					select {
					case errCh <- err:
					default:
					}
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

	// 收尾不变量：无在用连接；IdleConn 上限不被突破。
	if num, idle := p.Num(), p.NumIdle(addr); num != idle {
		t.Fatalf("after workers: Num()=%d, NumIdle()=%d; want equal (no conns in use)", num, idle)
	}
	if idle := p.NumIdle(addr); idle > idleMax {
		t.Fatalf("NumIdle()=%d exceeds IdleConn=%d", idle, idleMax)
	}
	if conn := p.NumConn(addr); conn != p.NumIdle(addr) {
		t.Fatalf("NumConn()=%d, want == NumIdle()=%d", conn, p.NumIdle(addr))
	}

	// 逐个 GetFull 转移所有权排空，池应彻底清零。
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

// ============================================================================
// 基准测试
// ============================================================================

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
