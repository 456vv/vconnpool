package vconnpool

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/456vv/vconn"
	"github.com/456vv/x/tcptest"

	"github.com/issue9/assert/v2"
)

// 判断池中空闲连接数量
func Test_ConnPool_1(t *testing.T) {
	as := assert.New(t, true)

	tcptest.D2S("127.0.0.1:0", func(c net.Conn) {
		defer c.Close()
		<-vconn.New(c).CloseNotify()
	}, func(raddr net.Addr) {
		cp := &ConnPool{
			IdeConn: 5,
			MaxConn: 2,
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

		d := cp.ConnNum()
		as.Equal(d, 2)

		time.Sleep(time.Millisecond)
		d = cp.ConnNumIde(raddr.Network(), raddr.String())
		as.Equal(d, 2)

		cp.CloseIdleConnections()

		time.Sleep(time.Millisecond)
		d = cp.ConnNum()
		as.Equal(d, 0)

		d = cp.ConnNumIde(raddr.Network(), raddr.String())
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
		cp := &ConnPool{
			IdeConn: 5,
		}
		defer cp.Close()

		// 创建连接
		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		d := cp.ConnNum()
		as.Equal(d, 1)

		time.Sleep(time.Millisecond)
		d = cp.ConnNumIde(raddr.Network(), raddr.String())
		as.Equal(d, 1)

		d = cp.ConnNum()
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
		cp := &ConnPool{
			IdeConn: 5,
		}
		defer cp.Close()

		// 创建连接
		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		d := cp.ConnNum()
		as.Equal(d, 1)

		time.Sleep(time.Millisecond)
		d = cp.ConnNumIde(raddr.Network(), raddr.String())
		as.Equal(d, 1)

		// 从池中读出
		conn, err = cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)

		// 原始连接
		netConn_2 := conn.(Conn).RawConn()
		netConn_2.Close()

		// 不会被回收，因为已经使用RawConn读取连接
		conn.Close()

		d = cp.ConnNum()
		as.Equal(d, 0)

		time.Sleep(time.Millisecond)
		d = cp.ConnNumIde(raddr.Network(), raddr.String())
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
		cp := &ConnPool{
			IdeConn: 5,
		}
		defer cp.Close()

		// 创建连接
		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		defer conn.Close()

		// 加入池中
		err = cp.Add(conn)
		as.NotError(err)

		d := cp.ConnNum()
		as.Equal(d, 1)

		time.Sleep(time.Millisecond)
		d = cp.ConnNumIde(raddr.Network(), raddr.String())
		as.Equal(d, 1)

		// 从池中读取
		tconn, err := cp.Get(conn.RemoteAddr())
		as.NotError(err)
		tconn.Close()

		d = cp.ConnNum()
		as.Equal(d, 0)

		time.Sleep(time.Millisecond)
		d = cp.ConnNumIde(raddr.Network(), raddr.String())
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
		cp := &ConnPool{
			IdeConn: 5,
			MaxConn: 2,
		}
		defer cp.Close()

		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		conn, err = cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)

		c1, ok := conn.(Conn)
		as.True(ok)

		d := cp.ConnNum()
		as.Equal(d, 1)

		time.Sleep(time.Millisecond)
		d = cp.ConnNumIde(raddr.Network(), raddr.String())
		as.Equal(d, 0)

		// 废弃这个连接，不让他进入池内
		c1.Discard()
		c1.Close()

		d = cp.ConnNum()
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
		cp := &ConnPool{
			IdeConn: 5,
			MaxConn: 2,
		}
		defer cp.Close()

		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		conn, err = cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)

		time.Sleep(5 * time.Millisecond)

		// 正在使用的连接数量
		as.Equal(cp.ConnNum(), 1)

		// 池中的连接数量
		d := cp.ConnNumIde(raddr.Network(), raddr.String())
		as.Equal(d, 0)

		conn.Close()

		as.Equal(cp.ConnNum(), 1)
		d = cp.ConnNumIde(raddr.Network(), raddr.String())
		as.Equal(d, 1)
	})
}

// 判断连接池中的数量是否正确
func Test_ConnPool_8(t *testing.T) {
	as := assert.New(t, true)

	tcptest.D2S("127.0.0.1:0", func(c net.Conn) {
		<-vconn.New(c).CloseNotify()
		c.Close()
	}, func(raddr net.Addr) {
		cp := &ConnPool{
			IdeConn: 5,
			MaxConn: 2,
		}
		defer cp.Close()

		conn, err := cp.Dial(raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		ctx := context.WithValue(context.Background(), PriorityContextKey, true) // 新创建连接
		conn, err = cp.DialContext(ctx, raddr.Network(), raddr.String())
		as.NotError(err)
		conn.Close()

		as.Equal(cp.ConnNum(), 2)
		d := cp.ConnNumIde(raddr.Network(), raddr.String())
		as.Equal(d, 2)

		cp.CloseIdleConnections()
		time.Sleep(time.Millisecond)

		as.Equal(cp.ConnNum(), 0)
		d = cp.ConnNumIde(raddr.Network(), raddr.String())
		as.Equal(d, 0)
	})
}

// 已经关闭的连接禁止加入
func Test_pools_1(t *testing.T) {
	as := assert.New(t, true)

	tcptest.C2S("127.0.0.1:0", func(c net.Conn) {
		time.Sleep(time.Second)
		c.Close()
	}, func(c net.Conn) {
		conn := vconn.New(c)
		ps := &pools{
			cp: &ConnPool{
				IdeConn: 10,
			},
			occupy:  make(map[net.Conn]int),  // 占据位置
			vacancy: make(map[int]struct{}),  // 空缺位置
			conns:   make([]*connMan, 0, 10), // 存在
		}

		conn.Close()
		ps.put(conn, 5*time.Second)

		as.Equal(ps.length(), 0)

		conn1, err := ps.get()
		as.ErrorIs(err, ErrConnNotAvailable).Nil(conn1)
	})
}

// 连接加入池之后，关闭连接
func Test_pools_2(t *testing.T) {
	as := assert.New(t, true)

	tcptest.C2S("127.0.0.1:0", func(c net.Conn) {
		time.Sleep(time.Second)
		c.Close()
	}, func(c net.Conn) {
		conn := vconn.New(c)
		ps := &pools{
			cp: &ConnPool{
				IdeConn: 10,
			},
			occupy:  make(map[net.Conn]int),  // 占据位置
			vacancy: make(map[int]struct{}),  // 空缺位置
			conns:   make([]*connMan, 0, 10), // 存在
		}

		ps.put(conn, 5*time.Second)

		conn.Close()
		time.Sleep(10 * time.Millisecond)

		as.Equal(ps.length(), 0)

		conn1, err := ps.get()
		as.ErrorIs(err, ErrConnNotAvailable).Nil(conn1)
	})
}
