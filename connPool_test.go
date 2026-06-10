package vconnpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/456vv/vconn"
	"github.com/456vv/x/tcptest"

	"github.com/issue9/assert/v4"
)

// 判断池中空闲连接数量
func Test_ConnPool_1(t *testing.T) {
	as := assert.New(t, true)

	tcptest.D2S("127.0.0.1:0", func(c net.Conn) {
		defer c.Close()
		<-vconn.New(c).CloseNotify()
	}, func(raddr net.Addr) {
		cp := &Pool{
			IdleConn: 5,
			MaxConn:  2,
		}
		defer cp.Close()

		// 创建连接
		conn1, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn2, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)

		// 回收到池里
		conn1.Close()
		conn2.Close()

		d := cp.Num()
		as.Equal(d, 2)

		time.Sleep(100 * time.Millisecond)
		d = cp.NumIdle(raddr)
		as.Equal(d, 2)

		cp.CloseIdleConnections()

		time.Sleep(100 * time.Millisecond)
		d = cp.Num()
		as.Equal(d, 0)

		d = cp.NumIdle(raddr)
		as.Equal(d, 0)
	})
}

// 检查池中的数量
func Test_ConnPool_2(t *testing.T) {
	as := assert.New(t, true)

	tcptest.D2S("127.0.0.1:0", func(c net.Conn) {
		<-vconn.New(c).CloseNotify()
		c.Close()
	}, func(raddr net.Addr) {
		cp := &Pool{
			IdleConn: 5,
		}
		defer cp.Close()

		// 创建连接
		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		d := cp.Num()
		as.Equal(d, 1)

		time.Sleep(100 * time.Millisecond)
		d = cp.NumIdle(raddr)
		as.Equal(d, 1)

		d = cp.Num()
		as.Equal(d, 1)
	})
}

// 读取原始连接，并关闭
func Test_ConnPool_4(t *testing.T) {
	as := assert.New(t, true)

	tcptest.D2S("127.0.0.1:0", func(c net.Conn) {
		<-vconn.New(c).CloseNotify()
		c.Close()
	}, func(raddr net.Addr) {
		cp := &Pool{
			IdleConn: 5,
		}
		defer cp.Close()

		// 创建连接
		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		d := cp.Num()
		as.Equal(d, 1)

		time.Sleep(100 * time.Millisecond)
		d = cp.NumIdle(raddr)
		as.Equal(d, 1)

		// 从池中读出
		conn1, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)

		// 原始连接
		netConn := conn1.RawConn()
		netConn.Close()

		// 不会被回收，因为已经使用RawConn读取连接
		if err := conn1.Close(); err != nil {
			as.Error(err)
		}

		d = cp.Num()
		as.Equal(d, 0)

		time.Sleep(100 * time.Millisecond)
		d = cp.NumIdle(raddr)
		as.Equal(d, 0)
	})
}

// 使用GET读取连接，池中的当前连接数量有变化
func Test_ConnPool_5(t *testing.T) {
	as := assert.New(t, true)

	tcptest.D2S("127.0.0.1:0", func(c net.Conn) {
		<-vconn.New(c).CloseNotify()
		c.Close()
	}, func(raddr net.Addr) {
		cp := &Pool{
			IdleConn: 5,
		}
		defer cp.Close()

		// 创建连接
		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		defer conn.Close()

		// 加入池中
		err = cp.Add(conn)
		as.NotError(err)

		d := cp.Num()
		as.Equal(d, 1)

		time.Sleep(100 * time.Millisecond)
		d = cp.NumIdle(raddr)
		as.Equal(d, 1)

		// 从池中读取
		tconn, err := cp.Get(conn.RemoteAddr())
		as.NotError(err)
		tconn.Close()

		d = cp.Num()
		as.Equal(d, 0)

		time.Sleep(100 * time.Millisecond)
		d = cp.NumIdle(raddr)
		as.Equal(d, 0)

		cp.Close()
		cp.Close()
		cp.CloseIdleConnections()
		cp.CloseIdleConnections()
		cp.Close()
	})
}

// 废弃连接，不入池
func Test_ConnPool_6(t *testing.T) {
	as := assert.New(t, true)

	tcptest.D2S("127.0.0.1:0", func(c net.Conn) {
		<-vconn.New(c).CloseNotify()
		c.Close()
	}, func(raddr net.Addr) {
		cp := &Pool{
			IdleConn: 5,
			MaxConn:  2,
		}
		defer cp.Close()

		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		conn, err = cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)

		d := cp.Num()
		as.Equal(d, 1)

		time.Sleep(100 * time.Millisecond)
		d = cp.NumIdle(raddr)
		as.Equal(d, 0)

		// 废弃这个连接，不让他进入池内
		conn.Discard()
		conn.Close()

		d = cp.Num()
		as.Equal(d, 0)
	})
}

// 检查连接数量和空闲数量
func Test_ConnPool_7(t *testing.T) {
	as := assert.New(t, true)

	tcptest.D2S("127.0.0.1:0", func(c net.Conn) {
		<-vconn.New(c).CloseNotify()
		c.Close()
	}, func(raddr net.Addr) {
		cp := &Pool{
			IdleConn: 5,
			MaxConn:  2,
		}
		defer cp.Close()

		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close() // 回池

		// 上面回池之后，池中应该有一个空闲连接
		d := cp.NumIdle(raddr)
		as.Equal(d, 1)
		as.Equal(cp.Num(), 1)

		// 池中有空闲连接，从池中读取连接
		conn1, err := cp.Get(conn.RemoteAddr())
		as.NotError(err)
		as.Equal(cp.Num(), 0)
		defer conn1.Close()

		// 连接再次回池
		err = cp.Put(conn1, conn1.RemoteAddr())
		as.NotError(err)
		err = cp.Put(conn1, conn1.RemoteAddr()) // 重复回池，应该被忽略
		as.NotError(err)
		err = cp.Put(conn1, conn1.RemoteAddr()) // 重复回池，应该被忽略
		as.NotError(err)

		// 上面回池之后，池中应该有一个空闲连接
		d = cp.NumIdle(raddr)
		as.Equal(d, 1)
		as.Equal(cp.Num(), 1)

		// 从池中读取连接
		conn, err = cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		// 判断连接数量和空闲数量
		d = cp.NumIdle(raddr)
		as.Equal(d, 0)
		as.Equal(cp.Num(), 1)

		// 关闭连接
		conn.Discard() // 废弃连接，不让他进入池内
		conn.Close()
		// 真正关闭连接之后，池中应该没有空闲连接了
		d = cp.NumIdle(raddr)
		as.Equal(d, 0)
		as.Equal(cp.Num(), 0)
	})
}

// 判断连接池中的数量是否正确
func Test_ConnPool_8(t *testing.T) {
	as := assert.New(t, true)

	tcptest.D2S("127.0.0.1:0", func(c net.Conn) {
		<-vconn.New(c).CloseNotify()
		c.Close()
	}, func(raddr net.Addr) {
		cp := &Pool{
			IdleConn: 5,
			MaxConn:  2,
		}
		defer cp.Close()

		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		ctx := context.WithValue(context.Background(), PriorityContextKey, true) // 新创建连接
		conn, err = cp.DialContext(ctx, raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		d := cp.NumIdle(raddr)
		as.Equal(d, 2)
		as.Equal(cp.Num(), 2)

		cp.CloseIdleConnections()
		time.Sleep(100 * time.Millisecond)

		d = cp.NumIdle(raddr)
		as.Equal(d, 0)
		as.Equal(cp.Num(), 0)
	})
}

// ==================== 测试辅助工具 ====================

// mockConn1 模拟网络连接
type mockConn1 struct {
	net.Conn
	closed     atomic.Bool
	readData   []byte
	writeData  []byte
	readErr    error
	writeErr   error
	localAddr  net.Addr
	remoteAddr net.Addr
	readDelay  time.Duration
	writeDelay time.Duration
	closeFunc  func() error
	mu         sync.Mutex
}

func newMockConn1(local, remote net.Addr) *mockConn1 {
	return &mockConn1{
		localAddr:  local,
		remoteAddr: remote,
		readData:   []byte("test data"),
	}
}

func (m *mockConn1) Read(b []byte) (n int, err error) {
	if m.readDelay > 0 {
		time.Sleep(m.readDelay)
	}
	if m.closed.Load() {
		return 0, net.ErrClosed
	}
	if m.readErr != nil {
		return 0, m.readErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n = copy(b, m.readData)
	return n, nil
}

func (m *mockConn1) Write(b []byte) (n int, err error) {
	if m.writeDelay > 0 {
		time.Sleep(m.writeDelay)
	}
	if m.closed.Load() {
		return 0, net.ErrClosed
	}
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	m.mu.Lock()
	m.writeData = append(m.writeData, b...)
	m.mu.Unlock()
	return len(b), nil
}

func (m *mockConn1) Close() error {
	if m.closed.Swap(true) {
		return net.ErrClosed
	}
	if m.closeFunc != nil {
		return m.closeFunc()
	}
	return nil
}

func (m *mockConn1) LocalAddr() net.Addr  { return m.localAddr }
func (m *mockConn1) RemoteAddr() net.Addr { return m.remoteAddr }

func (m *mockConn1) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn1) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn1) SetWriteDeadline(t time.Time) error { return nil }

// mockDialer 模拟拨号器
type mockDialer struct {
	dialFunc   func(network, address string) (net.Conn, error)
	dialCount  atomic.Int32
	dialDelay  time.Duration
	shouldFail atomic.Bool
	failAfter  int32
}

func (m *mockDialer) Dial(network, address string) (net.Conn, error) {
	return m.DialContext(context.Background(), network, address)
}

func (m *mockDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	count := m.dialCount.Add(1)

	if m.dialDelay > 0 {
		select {
		case <-time.After(m.dialDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if m.shouldFail.Load() || (m.failAfter > 0 && count > m.failAfter) {
		return nil, errors.New("mock dial failed")
	}

	if m.dialFunc != nil {
		return m.dialFunc(network, address)
	}

	addr, _ := ResolveAddr(network, address)
	return newMockConn1(&Addr{Net: network, Name: "local"}, addr), nil
}

// ==================== 单元测试 ====================

// TestConnSingle_BasicOperations 测试 connSingle 基本操作
func TestConnSingle_BasicOperations(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T, cs *connSingle)
	}{
		{
			name: "Read操作",
			fn: func(t *testing.T, cs *connSingle) {
				buf := make([]byte, 100)
				n, err := cs.Read(buf)
				if err != nil {
					t.Errorf("Read failed: %v", err)
				}
				if n == 0 {
					t.Error("Read returned 0 bytes")
				}
			},
		},
		{
			name: "Write操作",
			fn: func(t *testing.T, cs *connSingle) {
				data := []byte("test write")
				n, err := cs.Write(data)
				if err != nil {
					t.Errorf("Write failed: %v", err)
				}
				if n != len(data) {
					t.Errorf("Write returned %d, expected %d", n, len(data))
				}
			},
		},
		{
			name: "LocalAddr操作",
			fn: func(t *testing.T, cs *connSingle) {
				addr := cs.LocalAddr()
				if addr == nil {
					t.Error("LocalAddr returned nil")
				}
			},
		},
		{
			name: "RemoteAddr操作",
			fn: func(t *testing.T, cs *connSingle) {
				addr := cs.RemoteAddr()
				if addr == nil {
					t.Error("RemoteAddr returned nil")
				}
			},
		},
		{
			name: "SetDeadline操作",
			fn: func(t *testing.T, cs *connSingle) {
				err := cs.SetDeadline(time.Now().Add(time.Second))
				if err != nil {
					t.Errorf("SetDeadline failed: %v", err)
				}
			},
		},
		{
			name: "IsReuseConn检查",
			fn: func(t *testing.T, cs *connSingle) {
				isReuse := cs.IsReuseConn()
				if isReuse != cs.isPool {
					t.Errorf("IsReuseConn returned %v, expected %v", isReuse, cs.isPool)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &Pool{}
			mockConn1 := newMockConn1(
				&Addr{Net: "tcp", Name: "127.0.0.1:8080"},
				&Addr{Net: "tcp", Name: "127.0.0.1:9090"},
			)

			cs := &connSingle{
				Conn:   nil, // 会在需要时创建
				cp:     cp,
				addr:   &Addr{Net: "tcp", Name: "127.0.0.1:9090"},
				laddr:  mockConn1.LocalAddr(),
				raddr:  mockConn1.RemoteAddr(),
				isPool: false,
			}

			// 手动设置底层连接
			cs.mu.Lock()
			cs.Conn = vconn.New(mockConn1)
			cs.mu.Unlock()

			tt.fn(t, cs)
		})
	}
}

// TestConnSingle_ErrorHandling 测试 connSingle 错误处理
func TestConnSingle_ErrorHandling(t *testing.T) {
	tests := []struct {
		name      string
		setupErr  func(*mockConn1)
		operation func(*connSingle) error
		wantErr   bool
	}{
		{
			name: "Read遇到EOF",
			setupErr: func(mc *mockConn1) {
				mc.readErr = io.EOF
			},
			operation: func(cs *connSingle) error {
				buf := make([]byte, 10)
				_, err := cs.Read(buf)
				return err
			},
			wantErr: true,
		},
		{
			name: "Write遇到连接关闭",
			setupErr: func(mc *mockConn1) {
				mc.writeErr = net.ErrClosed
			},
			operation: func(cs *connSingle) error {
				_, err := cs.Write([]byte("test"))
				return err
			},
			wantErr: true,
		},
		{
			name: "操作已关闭的连接",
			setupErr: func(mc *mockConn1) {
				mc.closed.Store(true)
			},
			operation: func(cs *connSingle) error {
				buf := make([]byte, 10)
				_, err := cs.Read(buf)
				return err
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &Pool{}
			mockConn1 := newMockConn1(
				&Addr{Net: "tcp", Name: "127.0.0.1:8080"},
				&Addr{Net: "tcp", Name: "127.0.0.1:9090"},
			)

			if tt.setupErr != nil {
				tt.setupErr(mockConn1)
			}

			cs := &connSingle{
				cp:    cp,
				addr:  &Addr{Net: "tcp", Name: "127.0.0.1:9090"},
				laddr: mockConn1.LocalAddr(),
				raddr: mockConn1.RemoteAddr(),
			}

			cs.mu.Lock()
			cs.Conn = vconn.New(mockConn1)
			cs.mu.Unlock()

			err := tt.operation(cs)
			if (err != nil) != tt.wantErr {
				t.Errorf("operation() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestConnSingle_Discard 测试连接废弃功能
func TestConnSingle_Discard(t *testing.T) {
	cp := &Pool{}
	cp.connNum.Store(1)

	mockConn1 := newMockConn1(
		&Addr{Net: "tcp", Name: "127.0.0.1:8080"},
		&Addr{Net: "tcp", Name: "127.0.0.1:9090"},
	)

	cs := &connSingle{
		cp:    cp,
		addr:  &Addr{Net: "tcp", Name: "127.0.0.1:9090"},
		laddr: mockConn1.LocalAddr(),
		raddr: mockConn1.RemoteAddr(),
	}

	cs.mu.Lock()
	cs.Conn = vconn.New(mockConn1)
	cs.mu.Unlock()

	cs.Discard()
	if !cs.discard.Load() {
		t.Error("Connection not marked as discarded")
	}

	// 关闭连接，应该不会回收到池中
	err := cs.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	// 验证连接数减少
	if cp.connNum.Load() != 0 {
		t.Errorf("connNum = %d, expected 0", cp.connNum.Load())
	}
}

// TestConnSingle_RawConn 测试获取原始连接
func TestConnSingle_RawConn(t *testing.T) {
	cp := &Pool{}
	cp.connNum.Store(1)

	mockConn1 := newMockConn1(
		&Addr{Net: "tcp", Name: "127.0.0.1:8080"},
		&Addr{Net: "tcp", Name: "127.0.0.1:9090"},
	)

	cs := &connSingle{
		cp:    cp,
		addr:  &Addr{Net: "tcp", Name: "127.0.0.1:9090"},
		laddr: mockConn1.LocalAddr(),
		raddr: mockConn1.RemoteAddr(),
	}

	cs.mu.Lock()
	cs.Conn = vconn.New(mockConn1)
	cs.mu.Unlock()

	// 获取原始连接
	rawConn := cs.RawConn()
	if rawConn == nil {
		t.Fatal("RawConn returned nil")
	}

	// 验证连接数减少
	if cp.connNum.Load() != 0 {
		t.Errorf("connNum = %d, expected 0", cp.connNum.Load())
	}

	// 再次获取应该 panic
	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected panic when calling RawConn twice")
		}
	}()
	cs.RawConn()
}

// TestConnPool_BasicOperations 测试连接池基本操作
func TestConnPool_BasicOperations(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T, cp *Pool)
	}{
		{
			name: "Dial创建连接",
			fn: func(t *testing.T, cp *Pool) {
				conn, err := cp.Dial("tcp", "127.0.0.1:8080")
				if err != nil {
					t.Fatalf("Dial failed: %v", err)
				}
				defer conn.Close()

				if conn == nil {
					t.Fatal("Dial returned nil connection")
				}
			},
		},
		{
			name: "DialContext创建连接",
			fn: func(t *testing.T, cp *Pool) {
				ctx := context.Background()
				conn, err := cp.DialContext(ctx, "tcp", "127.0.0.1:8080")
				if err != nil {
					t.Fatalf("DialContext failed: %v", err)
				}
				defer conn.Close()

				if conn == nil {
					t.Fatal("DialContext returned nil connection")
				}
			},
		},
		{
			name: "连接复用",
			fn: func(t *testing.T, cp *Pool) {
				// 创建第一个连接
				conn1, err := cp.Dial("tcp", "127.0.0.1:8080")
				if err != nil {
					t.Fatalf("First Dial failed: %v", err)
				}

				// 关闭连接（应该回收到池中）
				conn1.Close()

				// 创建第二个连接（应该从池中获取）
				conn2, err := cp.Dial("tcp", "127.0.0.1:8080")
				if err != nil {
					t.Fatalf("Second Dial failed: %v", err)
				}
				defer conn2.Close()

				// 验证是复用的连接
				if cs, ok := conn2.(*connSingle); ok {
					if !cs.IsReuseConn() {
						t.Error("Expected reused connection")
					}
				}
			},
		},
		{
			name: "ConnNum统计",
			fn: func(t *testing.T, cp *Pool) {
				initialCount := cp.Num()

				conn, err := cp.Dial("tcp", "127.0.0.1:8080")
				if err != nil {
					t.Fatalf("Dial failed: %v", err)
				}

				if cp.Num() != initialCount+1 {
					t.Errorf("Num = %d, expected %d", cp.Num(), initialCount+1)
				}

				conn.Close()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialer := &mockDialer{}
			cp := &Pool{
				Dialer: dialer,
			}
			defer cp.Close()

			tt.fn(t, cp)
		})
	}
}

// TestConnPool_MaxConnLimit 测试最大连接数限制
func TestConnPool_MaxConnLimit(t *testing.T) {
	maxConn := 5
	dialer := &mockDialer{}
	cp := &Pool{
		Dialer:  dialer,
		MaxConn: maxConn,
	}
	defer cp.Close()

	var conns []net.Conn

	// 创建最大数量的连接
	for i := 0; i < maxConn; i++ {
		conn, err := cp.Dial("tcp", "127.0.0.1:9999")
		if err != nil {
			t.Fatalf("Dial %d failed: %v", i, err)
		}
		conns = append(conns, conn)
	}

	// 尝试创建超出限制的连接
	_, err := cp.Dial("tcp", "127.0.0.1:9999")
	if !errors.Is(err, ErrConnPoolMax) {
		t.Errorf("Expected ErrConnPoolMax, got %v", err)
	}

	// 关闭一个连接
	conns[0].Close()

	// 现在应该可以创建新连接
	conn, err := cp.Dial("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Errorf("Dial after close failed: %v", err)
	}
	if conn != nil {
		conn.Close()
	}

	// 清理
	for _, c := range conns[1:] {
		c.Close()
	}
}

// TestConnPool_IdleConnLimit 测试空闲连接数限制
func TestConnPool_IdleConnLimit(t *testing.T) {
	ideConn := 3
	dialer := &mockDialer{}
	cp := &Pool{
		Dialer:   dialer,
		IdleConn: ideConn,
	}
	defer cp.Close()

	network := "tcp"
	targetAddr := "127.0.0.1:8080"
	addr := &Addr{Net: network, Name: targetAddr}

	// 创建并关闭多个连接
	for i := 0; i < ideConn+2; i++ {
		conn, err := cp.Dial("tcp", targetAddr)
		if err != nil {
			t.Fatalf("Dial %d failed: %v", i, err)
		}
		conn.Close()
	}

	// 验证空闲连接数不超过限制
	idleCount := cp.NumIdle(addr)
	if idleCount > ideConn {
		t.Errorf("Idle connections = %d, expected <= %d", idleCount, ideConn)
	}
}

// TestConnPool_IdleTimeout 测试空闲超时
func TestConnPool_IdleTimeout(t *testing.T) {
	timeout := 100 * time.Millisecond
	dialer := &mockDialer{}
	cp := &Pool{
		Dialer:      dialer,
		IdleTimeout: timeout,
	}
	defer cp.Close()

	network := "tcp"
	targetAddr := "127.0.0.1:8080"
	addr := &Addr{Net: network, Name: targetAddr}

	// 创建并关闭连接
	conn, err := cp.Dial("tcp", targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	conn.Close()

	// 验证空闲连接存在
	if cp.NumIdle(addr) == 0 {
		t.Error("No idle connections after close")
	}

	// 等待超时
	time.Sleep(timeout + 50*time.Millisecond)

	// 验证空闲连接被清理
	if cp.NumIdle(addr) != 0 {
		t.Error("Idle connections not cleaned up after timeout")
	}
}

// TestConnPool_PriorityContext 测试优先级上下文
func TestConnPool_PriorityContext(t *testing.T) {
	dialer := &mockDialer{}
	cp := &Pool{
		Dialer: dialer,
	}
	defer cp.Close()

	address := "127.0.0.1:8080"

	// 创建并关闭一个连接（放入池中）
	conn1, err := cp.Dial("tcp", address)
	if err != nil {
		t.Fatalf("First Dial failed: %v", err)
	}
	conn1.Close()

	// 使用优先级上下文创建新连接
	ctx := context.WithValue(context.Background(), PriorityContextKey, true)
	conn2, err := cp.DialContext(ctx, "tcp", address)
	if err != nil {
		t.Fatalf("Priority Dial failed: %v", err)
	}
	defer conn2.Close()

	// 验证不是复用的连接
	if cs, ok := conn2.(*connSingle); ok {
		if cs.IsReuseConn() {
			t.Error("Expected new connection with priority context")
		}
	}
}

// TestConnPool_AddAndGet 测试 Add 和 Get 方法
func TestConnPool_AddAndGet(t *testing.T) {
	cp := &Pool{}
	defer cp.Close()

	addr := &Addr{Net: "tcp", Name: "127.0.0.1:8080"}
	mockConn1 := newMockConn1(addr, addr)

	// 添加连接
	err := cp.Put(mockConn1, addr)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// 获取连接
	conn, err := cp.Get(addr)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if conn == nil {
		t.Fatal("Get returned nil connection")
	}

	conn.Close()
}

// TestConnPool_CloseIdleConnections 测试关闭空闲连接
func TestConnPool_CloseIdleConnections(t *testing.T) {
	dialer := &mockDialer{}
	cp := &Pool{
		Dialer: dialer,
	}
	defer cp.Close()

	// 创建多个连接并关闭
	for i := 0; i < 5; i++ {
		conn, err := cp.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", 8080+i))
		if err != nil {
			t.Fatalf("Dial %d failed: %v", i, err)
		}
		conn.Close()
	}

	// 验证有空闲连接
	totalIdle := 0
	for i := 0; i < 5; i++ {
		totalIdle += cp.NumIdle(&Addr{Net: "tcp", Name: fmt.Sprintf("127.0.0.1:%d", 8080+i)})
	}

	if totalIdle != 5 {
		t.Error("No idle connections before CloseIdleConnections")
	}

	// 关闭所有空闲连接
	cp.CloseIdleConnections()

	// 验证空闲连接被清理
	totalIdle = 0
	for i := 0; i < 5; i++ {
		totalIdle += cp.NumIdle(&Addr{Net: "tcp", Name: fmt.Sprintf("127.0.0.1:%d", 8080+i)})
	}

	if totalIdle != 0 {
		t.Errorf("Idle connections = %d after CloseIdleConnections, expected 0", totalIdle)
	}
}

// TestConnPool_Close 测试关闭连接池
func TestConnPool_Close(t *testing.T) {
	dialer := &mockDialer{}
	cp := &Pool{
		Dialer: dialer,
	}

	// 创建一些连接
	conn, err := cp.Dial("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	conn.Close()

	// 关闭连接池
	err = cp.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	// 验证连接池已关闭
	_, err = cp.Dial("tcp", "127.0.0.1:8080")
	if !errors.Is(err, ErrConnPoolClosed) {
		t.Errorf("Expected errorConnPoolClose, got %v", err)
	}

	// 重复关闭应该不报错
	err = cp.Close()
	if err != nil {
		t.Errorf("Second Close failed: %v", err)
	}
}
