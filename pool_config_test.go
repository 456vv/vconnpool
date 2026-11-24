package vconnpool

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestPoolConfigBootstrap(t *testing.T) {
	t.Run("New publishes the given config", func(t *testing.T) {
		p := New(Config{IdleTimeout: time.Minute, IdleConn: 2, MaxConn: 4})
		defer p.Close()

		if got := p.Config(); got.IdleTimeout != time.Minute || got.IdleConn != 2 || got.MaxConn != 4 {
			t.Fatalf("bootstrap conf = %+v", got)
		}
		if a, b := p.conf(), p.conf(); a != b {
			t.Fatal("conf() must return the same snapshot")
		}
	})

	// 零值 Pool 仍然可用：首次 conf() 发布一份全零（= 全默认）快照。
	t.Run("zero value pool", func(t *testing.T) {
		p := &Pool{}
		c := p.conf()
		if c == nil {
			t.Fatal("conf() must never be nil")
		}
		if c.IdleTimeout != 0 || c.IdleConn != 0 || c.MaxConn != 0 || c.Dialer != nil || c.ResolveAddr != nil {
			t.Fatalf("zero value conf = %+v, want all-zero config", *c)
		}
		if p.conf() != c {
			t.Fatal("conf() must publish exactly one snapshot on first call")
		}
	})
}

func TestPoolConfigNormalizeAndHotReload(t *testing.T) {
	p := New(Config{IdleTimeout: time.Minute, MaxConn: 4})
	defer p.Close()

	// SetConfig 是整份替换：负值归一为 0，未填写的字段回落零值。
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

	// 配置只能经发布入口生效：Config() 返回的是副本，改它不影响池内快照。
	c.IdleTimeout = time.Hour
	if got := p.conf().IdleTimeout; got != time.Millisecond {
		t.Fatalf("Config() must return a copy, snapshot = %v", got)
	}

	// UpdateConfig 基于当前快照增量修改，未触及字段保持不变。
	if err := p.UpdateConfig(func(c *Config) { c.IdleTimeout = 2 * time.Millisecond }); err != nil {
		t.Fatal(err)
	}
	if got := p.conf().IdleTimeout; got != 2*time.Millisecond {
		t.Fatalf("UpdateConfig did not take effect: %v", got)
	}

	// mutate == nil：无操作，不发布新快照。
	before := p.conf()
	if err := p.UpdateConfig(nil); err != nil {
		t.Fatal(err)
	}
	if p.conf() != before {
		t.Fatal("UpdateConfig(nil) must not publish a new snapshot")
	}

	// 池未关闭：SetConfig 成功（整份替换回零值 = 全部默认）。
	if err := p.SetConfig(Config{}); err != nil {
		t.Fatalf("SetConfig on open pool = %v, want nil", err)
	}
	if got := p.Config(); got.IdleTimeout != 0 || got.IdleConn != 0 || got.MaxConn != 0 {
		t.Fatalf("SetConfig(Config{}) = %+v, want zero config", got)
	}

	_ = p.Close()

	if err := p.SetConfig(Config{}); !errors.Is(err, ErrConnPoolClosed) {
		t.Fatalf("after Close want ErrConnPoolClosed, got %v", err)
	}
	mutated := false
	if err := p.UpdateConfig(func(c *Config) { mutated = true; c.MaxConn = 1 }); !errors.Is(err, ErrConnPoolClosed) {
		t.Fatalf("UpdateConfig after Close = %v, want ErrConnPoolClosed", err)
	}
	if mutated {
		t.Fatal("mutate must not run after Close")
	}
	if p.conf() == nil {
		t.Fatal("conf() must never be nil after Close")
	}
}

// 收敛：IdleConn 收紧立即裁剪最老空闲连接；IdleTimeout 热改立即重算过期。
func TestPoolConfigConvergence(t *testing.T) {
	srv1, peer1 := net.Pipe()
	srv2, peer2 := net.Pipe()
	t.Cleanup(func() {
		_ = peer1.Close()
		_ = peer2.Close()
	})

	p := New(Config{IdleTimeout: 0, IdleConn: 4}) // 空闲不过期；单地址上限 4
	defer p.Close()

	addr := srv1.RemoteAddr()
	if err := p.Add(srv1); err != nil {
		t.Fatal(err)
	}
	if err := p.Add(srv2); err != nil {
		t.Fatal(err)
	}
	if n := p.NumIdle(addr); n != 2 {
		t.Fatalf("NumIdle() = %d, want 2", n)
	}

	// IdleConn == 0 是「不限」而不是「零条」：4 → 0 不是收紧，不得裁剪。
	if err := p.SetConfig(Config{IdleConn: 0}); err != nil {
		t.Fatal(err)
	}
	if n := p.NumIdle(addr); n != 2 {
		t.Fatalf("IdleConn=0 must mean unlimited, idle = %d, want 2", n)
	}

	// 1) IdleConn 收紧（不限 → 1）→ 立即裁剪最老（FIFO）的空闲连接。
	if err := p.SetConfig(Config{IdleConn: 1}); err != nil {
		t.Fatal(err)
	}
	if n := p.NumIdle(addr); n != 1 {
		t.Fatalf("trimIdle failed, still %d idle", n)
	}
	if n := p.Num(); n != 1 {
		t.Fatalf("conn count not released: %d", n)
	}
	// 先入池的 srv1 被裁掉：它的对端应该读到 EOF。
	_ = peer1.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer1.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("oldest idle conn should be closed by trimIdle, read err = %v", err)
	}

	// 2) IdleTimeout 热改 → 存量空闲连接（srv2）立即获得新过期时间。
	//    SetConfig 是整份替换，这里要把 IdleConn 一并写全。
	if err := p.SetConfig(Config{IdleConn: 4, IdleTimeout: 30 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	// sweepExpired 先在锁内摘除、锁外再归还配额，所以 Num 可能比 NumIdle 晚一拍。
	deadline := time.Now().Add(time.Second)
	for (p.NumIdle(addr) > 0 || p.Num() > 0) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := p.NumIdle(addr); n != 0 {
		t.Fatalf("reapplyTimeout failed, still %d idle", n)
	}
	if n := p.Num(); n != 0 {
		t.Fatalf("conn count not released after idle timeout: %d", n)
	}
}

// 只修改与空闲策略无关的配置（如 Dialer）时，publish 不得重置存量空闲连接的归还超时。
func TestPoolConfigUnrelatedChangeKeepsIdleTimeout(t *testing.T) {
	srv, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })

	p := New(Config{IdleTimeout: time.Minute})
	defer p.Close()
	addr := srv.RemoteAddr()

	// PutFull 显式指定 10 分钟，与池级 IdleTimeout（1 分钟）不同。
	if err := p.PutFull(srv, addr, 10*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	ps := p.getPoolAddr(addr)
	if ps == nil {
		t.Fatal("bucket missing")
	}

	snapshot := func() (time.Duration, time.Time) {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if ps.head >= len(ps.idle) {
			return 0, time.Time{}
		}
		ic := ps.idle[ps.head]
		return ic.timeout, ic.expireAt
	}

	timeoutBefore, expireBefore := snapshot()
	if timeoutBefore != 10*time.Minute || expireBefore.IsZero() {
		t.Fatalf("unexpected idle snapshot: timeout=%v expireAt=%v", timeoutBefore, expireBefore)
	}

	if err := p.UpdateConfig(func(c *Config) { c.Dialer = defaultDialer }); err != nil {
		t.Fatal(err)
	}

	timeoutAfter, expireAfter := snapshot()
	if timeoutAfter != timeoutBefore || !expireAfter.Equal(expireBefore) {
		t.Fatalf("unrelated change reset idle timeout: %v -> %v, expireAt %v -> %v",
			timeoutBefore, timeoutAfter, expireBefore, expireAfter)
	}
	if got := p.NumIdle(addr); got != 1 {
		t.Fatalf("NumIdle() = %d, want 1", got)
	}
}

func TestPoolConfigHotReloadRace(t *testing.T) {
	p := New(Config{IdleTimeout: 10 * time.Millisecond, IdleConn: 8, MaxConn: 32})
	defer p.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = p.SetConfig(Config{
				IdleTimeout: time.Duration(i%7) * time.Millisecond,
				IdleConn:    i % 5,
				MaxConn:     i % 17,
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			switch i % 6 {
			case 0:
				_ = p.conf()
			case 1:
				_ = p.Config()
			case 2:
				_ = p.sweepInterval()
			case 3:
				// 配额借还必须成对：达到上限（借失败）时不归还。
				if err := p.checkAndIncConnNum(p.conf().MaxConn); err == nil {
					p.releaseCount(nil, 1)
				}
			case 4:
				p.wakeSweeper()
			case 5:
				_ = p.Num()
			}
		}
	}()
	wg.Wait()
	if n := p.Num(); n != 0 {
		t.Fatalf("connNum leak: %d", n)
	}
}
