package vconnpool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// mockDialer2 模拟网络拨号
type mockDialer2 struct {
	mu sync.Mutex
}

func (m *mockDialer2) Dial(network, address string) (net.Conn, error) {
	return m.DialContext(context.Background(), network, address)
}

func (m *mockDialer2) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c1, c2 := net.Pipe()
	// 启动一个协程模拟对端处理，防止 Write 阻塞
	go func() {
		buf := make([]byte, 1024)
		for {
			c2.SetReadDeadline(time.Now().Add(time.Second))
			n, err := c2.Read(buf)
			if err != nil {
				c2.Close()
				return
			}
			c2.Write(buf[:n]) // Echo 回去
		}
	}()
	return c1, nil
}

// TestConnPool_Functional 测试连接池的核心功能：创建、复用、限制
func TestConnPool_Functional(t *testing.T) {
	network := "tcp"
	targetAddr := "127.0.0.1:8080"
	addr := &Addr{Net: network, Name: targetAddr}
	pool := &Pool{
		Dialer:      &mockDialer2{},
		MaxConn:     5,
		IdleConn:    2,
		IdleTimeout: time.Second * 2,
	}
	defer pool.Close()

	// 1. 测试 Dial 和复用
	c1, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if pool.Num() != 1 {
		t.Errorf("Expected 1 conn, got %d", pool.Num())
	}

	// 关闭 c1，它应该进入空闲池
	c1.Close()
	if pool.NumIdle(addr) != 1 {
		t.Errorf("Expected 1 idle conn, got %d", pool.NumIdle(addr))
	}

	// 再次 Dial，应该是复用的
	c2, _ := pool.Dial(network, targetAddr)
	if !c2.IsReuseConn() {
		t.Error("Expected reused connection")
	}
	c2.Close()

	// 2. 测试 MaxConn 限制
	var conns []net.Conn
	for i := 0; i < 5; i++ {
		c, err := pool.Dial(network, targetAddr)
		if err != nil {
			t.Errorf("Dial %d failed: %v", i, err)
		}
		conns = append(conns, c)
	}
	_, err = pool.Dial(network, targetAddr)
	if !errors.Is(err, ErrConnPoolMax) {
		t.Errorf("Expected ErrConnPoolMax, got %v", err)
	}

	// 释放所有连接
	for _, c := range conns {
		c.Close()
	}

	// 3. 测试 IdeConn (空闲上限)
	// 虽然开了 5 个，但 IdeConn 是 2，关闭后池里应只有 2 个，其余 3 个应被物理关闭
	time.Sleep(time.Millisecond * 100) // 等待异步回收完成
	numIde := pool.NumIdle(addr)
	if numIde != 2 {
		t.Errorf("Idle connections %d did not match expected 2", numIde)
	}
}

// TestConnPool_RawConn_Discard 测试原始连接获取和废弃逻辑
func TestConnPool_RawConn_Discard(t *testing.T) {
	network := "tcp"
	targetAddr := "127.0.0.1:8080"
	addr := &Addr{Net: network, Name: targetAddr}
	pool := &Pool{Dialer: &mockDialer2{}}
	defer pool.Close()

	// 测试 Discard
	c, err := pool.Dial(network, targetAddr)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}

	c.Discard()
	c.Close() // 标记为 Discard 后，Close 不会放回池，且 Num 应该减少
	if pool.NumIdle(addr) != 0 {
		t.Error("Discarded connection should not be in pool")
	}

	// 测试 RawConn
	c2, _ := pool.Dial(network, targetAddr)
	raw := c2.RawConn()
	if raw == nil {
		t.Fatal("RawConn returned nil")
	}
	defer raw.Close()
	if pool.NumIdle(addr) != 0 {
		t.Error("RawConn should not be in pool")
	}
	c2.Close()
}

// TestConnPool_Priority 测试优先级 Dial
func TestConnPool_Priority(t *testing.T) {
	pool := &Pool{Dialer: &mockDialer2{}}
	defer pool.Close()

	c1, _ := pool.Dial("tcp", "127.0.0.1:0")
	c1.Close() // 入池

	// 使用优先级 Context
	ctx := context.WithValue(context.Background(), PriorityContextKey, true)
	c2, _ := pool.DialContext(ctx, "tcp", "127.0.0.1:0")
	if c2.IsReuseConn() {
		t.Error("Priority Dial should create new connection, not reuse")
	}
	c2.Close()
}

func TestConnPool_Race_CrossCall(t *testing.T) {
	pool := &Pool{
		Dialer:      &mockDialer2{},
		MaxConn:     50,
		IdleConn:    10,
		IdleTimeout: time.Millisecond * 500,
	}

	var wg sync.WaitGroup
	workers := 20
	iterations := 100

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// 随机执行不同操作
				op := j % 5
				addr := fmt.Sprintf("127.0.0.%d:80", j%3) // 模拟多个目标地址

				switch op {
				case 0: // Dial & Read/Write & Close
					c, err := pool.Dial("tcp", addr)
					if err == nil {
						c.Write([]byte("ping"))
						buf := make([]byte, 4)
						c.Read(buf)
						c.Close()
					}
				case 1: // Get & Put (手动管理)
					a, _ := ResolveAddr("tcp", addr)
					c, err := pool.Get(a)
					if err == nil {
						pool.Put(c, a)
					}
				case 2: // 检查状态方法
					pool.Num()
					pool.NumIdle(&Addr{Net: "tcp", Name: addr})
				case 3: // Discard 逻辑
					c, err := pool.Dial("tcp", addr)
					if err == nil {
						if j%2 == 0 {
							c.Discard()
						}
						c.Close()
					}
				case 4: // 清理空闲连接
					if j == iterations-1 {
						pool.CloseIdleConnections()
					}
				}
			}
		}(i)
	}

	wg.Wait()
	t.Logf("Final Num: %d", pool.Num())
	pool.Close()
	if pool.Num() != 0 {
		t.Errorf("Expected 0 connections after close, got %d", pool.Num())
	}
}

// BenchmarkConnPool_DialAndClose 测试池化后的 Dial+Close 吞吐量
func BenchmarkConnPool_DialAndClose(b *testing.B) {
	pool := &Pool{
		Dialer:   &mockDialer2{},
		MaxConn:  1000,
		IdleConn: 500,
	}
	defer pool.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c, err := pool.Dial("tcp", "127.0.0.1:80")
			if err != nil {
				continue
			}
			c.Close() // 回收到池
		}
	})
}

// BenchmarkConnPool_DialNewConnection 测试不使用池（或池满）时的性能
func BenchmarkConnPool_NoPool_Dial(b *testing.B) {
	pool := &Pool{
		Dialer:   &mockDialer2{},
		MaxConn:  0, // 无限制
		IdleConn: 0, // 不缓存
	}
	defer pool.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c, err := pool.Dial("tcp", "127.0.0.1:80")
			if err != nil {
				continue
			}
			conn := c.RawConn()
			if pool.Put(conn, conn.RemoteAddr()) != nil {
				conn.Close() // 无法放回池，直接关闭
			}
			conn1, err := pool.Get(conn.RemoteAddr())
			if err == nil {
				conn1.Close()
			}

		}
	})
}
