package vconnpool

import (
	"context"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

func startTestServer(t *testing.T) (net.Addr, func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				io.Copy(io.Discard, c)
				c.Close()
			}(c)
		}
	}()
	return ln.Addr(), func() { ln.Close() }
}

func dialTo(t *testing.T, addr net.Addr) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

// 条件被满足 → 投递 true，且注册项被移除
func TestWaitConnIdleGeqContext_Wake(t *testing.T) {
	addr, stop := startTestServer(t)
	defer stop()

	p := New(Config{})
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := p.WaitConnIdleGeq(ctx, addr, 1)
	select {
	case ok := <-ch:
		t.Fatalf("提前唤醒：%v", ok)
	case <-time.After(50 * time.Millisecond):
	}

	if err := p.Add(dialTo(t, addr)); err != nil {
		t.Fatalf("add: %v", err)
	}
	select {
	case ok := <-ch:
		if !ok {
			t.Fatal("期望 true")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待唤醒超时")
	}
}

// ctx 取消 → 投递 false，且注册项被摘除（本轮核心目标）
func TestWaitContextCancel_Unregisters(t *testing.T) {
	addr, stop := startTestServer(t)
	defer stop()

	p := New(Config{})
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch := p.WaitConnIdleGeq(ctx, addr, 5)
	cancel()

	select {
	case ok := <-ch:
		if ok {
			t.Fatal("取消后应收到 false")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后未投递 false")
	}
}

// 池关闭 → 所有等待者立刻收到 false
func TestWaitPoolClose_WakesFalse(t *testing.T) {
	addr, stop := startTestServer(t)
	defer stop()

	p := New(Config{})
	ch := p.WaitConnNumGeq(nil, addr, 10) // 老 API：零协程路径
	p.Close()

	select {
	case ok := <-ch:
		if ok {
			t.Fatal("关池应收到 false")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关池未唤醒")
	}
}

// 唤醒 / 取消 / 入池 三方竞争：注册项必须全部回收
func TestWaitStress_RaceAndReclaim(t *testing.T) {
	addr, stop := startTestServer(t)
	defer stop()

	p := New(Config{})
	defer p.Close()

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%5+1)*time.Millisecond)
			defer cancel()
			ch := p.WaitConnIdleGeq(ctx, addr, 100)
			select {
			case <-ch:
			case <-ctx.Done():
			}
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				c, err := net.Dial("tcp", addr.String())
				if err != nil {
					return
				}
				if err := p.Add(c); err != nil {
					c.Close()
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
}

// 监视线程必须随唤醒/取消回收（无 socket 依赖，稳定不 flaky）
func TestWaitWatcherGoroutineReclaimed(t *testing.T) {
	p := New(Config{})
	defer p.Close()
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}

	runtime.GC()
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	chs := make([]<-chan bool, 0, 64)
	for i := 0; i < 64; i++ {
		chs = append(chs, p.WaitConnIdleGeq(ctx, addr, 1000))
	}
	cancel()

	for _, ch := range chs {
		select {
		case ok := <-ch:
			if ok {
				t.Fatal("不该满足")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("取消未唤醒")
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 {
		if time.Now().After(deadline) {
			t.Fatalf("监视线程未回收：base=%d now=%d", base, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
