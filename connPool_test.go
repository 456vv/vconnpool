package vconnpool

import (
	"testing"
    "time"
    "net"
    "io"
    "io/ioutil"
    "github.com/456vv/vconn"
    "bytes"
   // "reflect"
    "context"
)

func server(t *testing.T, c chan net.Listener){
    bl, err := net.Listen("tcp", "127.0.0.1:0")
    fatal(t, err)
    defer bl.Close()
    c <- bl
    for {
	    conn, err := bl.Accept()
	    if err != nil {
	    	return
	    }
		go func(conn net.Conn){
			defer conn.Close()
			p := make([]byte, 1024)
			exit := []byte("Close")
			for {
				n, err := conn.Read(p)
				conn.Write(p[:n])
				if err != nil || bytes.Contains(p[:n], exit) {
					//t.Log("server", err)
					return
				}
			}
		}(conn)
    }
}

func fatal(t *testing.T, err error){
    if err != nil {
    	t.Fatal(err)
    }
}

func Test_ConnPool_1(t *testing.T){
	c := make(chan net.Listener, 1)
	go server(t, c)
	listen := <-c
	defer listen.Close()
	
 	addr := ParseAddr(listen.Addr().Network(), listen.Addr().String())
	
    cp := &ConnPool{
        IdeConn:5,
        MaxConn:2,
    }
    defer cp.Close()
    
    netConn1, err := cp.Dial(addr.Network(), addr.String())
    fatal(t, err)
    
    netConn2, err := cp.Dial(addr.Network(), addr.String())
    fatal(t, err)

    netConn1.Close()
    
    netConn3, err := cp.Dial(addr.Network(), addr.String())
    fatal(t, err)
    
	if netConn1.LocalAddr() != netConn3.LocalAddr() {
		t.Fatal("error")
	}
	
    netConn2.Close()
    netConn3.Close()
    
    if d := cp.ConnNum(); d != 2 {
    	t.Fatalf("error %d", d)
    }
    
    time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 2 {
    	t.Fatalf("error %d", d)
    }
    cp.CloseIdleConnections()
	time.Sleep(time.Second)

	if d := cp.ConnNum(); d != 0 {
    	t.Fatalf("error %d", d)
    }
    
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }
    cp.Close()
}

func Test_ConnPool_2(t *testing.T){
	c := make(chan net.Listener, 1)
	go server(t, c)
	listen := <-c
	defer listen.Close()
	
 	addr := ParseAddr(listen.Addr().Network(), listen.Addr().String())

    cp := &ConnPool{
        Dialer:&net.Dialer{},
        IdeConn:0,
    }
    defer cp.Close()
    netConn4, err := cp.Dial(addr.Network(), addr.String())
    fatal(t, err)
    
    netConn4.Close()
    if d := cp.ConnNum(); d != 0 {
    	t.Fatalf("error %d", d)
    }

	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }

}

func Test_ConnPool_3(t *testing.T){
	
 	addr := ParseAddr("tcp", "www.baidu.com:80")

    cp := &ConnPool{
        Dialer:&net.Dialer{},
        IdeConn:5,
    }
    defer cp.Close()
    
    netConn1, err := cp.Dial(addr.Network(), addr.String())
    fatal(t, err)
    
    netConn1.Write([]byte("GET / HTTP/1.1\r\nHost:www.baidu.com\r\nConnection:close\r\n\r\n"))
    n64, err := io.Copy(ioutil.Discard, netConn1)
    fatal(t, err)

    t.Log(n64)
	
    netConn1.Close()
	
    if d := cp.ConnNum(); d != 0 {
    	t.Fatalf("error %d", d)
    }
    
	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }
    
    netConn2, err := cp.Dial(addr.Network(), addr.String())
    fatal(t, err)
    
    netConn2.Write([]byte("GET / HTTP/1.1\r\nHost:www.baidu.com\r\nConnection:close\r\n\r\n"))
    n64, err = io.Copy(ioutil.Discard, netConn2)
    fatal(t, err)
    
    t.Log(n64)

    netConn2.Close()
    
    if d := cp.ConnNum(); d != 0 {
    	t.Fatalf("error %d", d)
    }
    
	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }
}

func Test_ConnPool_4(t *testing.T){
	c := make(chan net.Listener, 1)
	go server(t, c)
	listen := <-c
	defer listen.Close()
	
 	addr := ParseAddr(listen.Addr().Network(), listen.Addr().String())

    cp := &ConnPool{
        Dialer:&net.Dialer{},
        IdeConn:5,
    }
    defer cp.Close()

    netConn1, err := cp.Dial(addr.Network(), addr.String())
    fatal(t, err)
    
    netConn1.Write([]byte("GET / HTTP/1.1\r\nHost:www.baidu.com\r\nUser-Agent: Mozilla/5.0\r\n\r\n"))
    netConn1.SetReadDeadline(time.Now().Add(time.Second*2))
    n64, err := io.Copy(ioutil.Discard, netConn1)
    t.Log(n64)
    if n64 == 0 {fatal(t, err)}
	
    netConn1.Close()

    if d := cp.ConnNum(); d != 1 {
    	t.Fatalf("error %d", d)
    }
    
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 1 {
    	t.Fatalf("error %d", d)
    }
    

    netConn2, err := cp.Dial(addr.Network(), addr.String())
   	fatal(t, err)
    netConn2.Write([]byte("GET / HTTP/1.1\r\nHost:www.baidu.com\r\nConnection: Close\r\n\r\n"))
    netConn2.SetReadDeadline(time.Now().Add(time.Second*2))
    n64, err = io.Copy(ioutil.Discard, netConn2)
    t.Log(n64)
    if n64 == 0 {fatal(t, err)}
    
    time.Sleep(time.Second)
    netConn2_1 := netConn2.(Conn)
    netConn2_2 := netConn2_1.RawConn()
    notify, ok := netConn2_2.(vconn.CloseNotifier)
    if !ok {
    	t.Fatal("error")
    }
    select {
    case err = <- notify.CloseNotify():
    default:
    }
    if err != io.EOF {
    	t.Fatal(err)
    }
    netConn2_2.Close()
    
    if d := cp.ConnNum(); d != 0 {
    	t.Fatalf("error %d", d)
    }
    
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }
	
	cp.Close()
	time.Sleep(time.Second)
}

func Test_ConnPool_5(t *testing.T){
	c := make(chan net.Listener, 1)
	go server(t, c)
	listen := <-c
	defer listen.Close()
	
 	addr := ParseAddr(listen.Addr().Network(), listen.Addr().String())

    cp := &ConnPool{
        Dialer:&net.Dialer{},
        IdeConn:5,
    }
    defer cp.Close()

    netConn3, err := net.Dial(addr.Network(), addr.String())
   	fatal(t, err)
    defer netConn3.Close()
    err = cp.Add(netConn3)
   	fatal(t, err)
    

    if d := cp.ConnNum(); d != 1 {
    	t.Fatalf("error %d", d)
    }
    
	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 1 {
    	t.Fatalf("error %d", d)
    }
	
    netConn4, err := net.Dial(addr.Network(), addr.String())
   	fatal(t, err)
    defer netConn4.Close()
    
    err = cp.Add(netConn4)
   	fatal(t, err)
   	
    if d := cp.ConnNum(); d != 2 {
    	t.Fatalf("error %d", d)
    }
    
	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 2 {
    	t.Fatalf("error %d", d)
    }
	
    tconn, err := cp.Get(netConn4.RemoteAddr())
   	fatal(t, err)
   	defer tconn.Close()
   	
    if d := cp.ConnNum(); d != 1 {
    	t.Fatalf("error %d", d)
    }
    
	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 1 {
    	t.Fatalf("error %d", d)
    }

    tconn, err = cp.Get(netConn4.RemoteAddr())
   	fatal(t, err)
   	defer tconn.Close()
   	
    if d := cp.ConnNum(); d != 0 {
    	t.Fatalf("error %d", d)
    }
    
	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }


	cp.Close()
    cp.Close()
    cp.CloseIdleConnections()
    cp.CloseIdleConnections()
    cp.Close()
    cp.CloseIdleConnections()
	time.Sleep(time.Second)

}


func Test_ConnPool_6(t *testing.T){
	c := make(chan net.Listener, 1)
	go server(t, c)
	listen := <-c
	defer listen.Close()
	
 	addr := ParseAddr(listen.Addr().Network(), listen.Addr().String())

    cp := &ConnPool{
        IdeConn:5,
        MaxConn:2,
    }
    defer cp.Close()
    
    //=====================================================
    conn, err := cp.Dial(addr.Network(), addr.String())
   	fatal(t, err)
    conn.Close()
    
    if d := cp.ConnNum(); d != 1 {
    	t.Fatalf("error %d", d)
    }
    time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 1 {
    	t.Fatalf("error %d", d)
    }
    //=====================================================
    conn, err = cp.Dial(addr.Network(), addr.String())
   	fatal(t, err)
   	
    c1, ok := conn.(Conn)
    if !ok {
        t.Fatal("不支持转换为 Conn 接口")
    }
    if d := cp.ConnNum(); d != 1 {
    	t.Fatalf("error %d", d)
    }
    
    time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }
    //=====================================================
    c1.Discard()
    c1.Close()

    if d := cp.ConnNum(); d != 0 {
    	t.Fatalf("error %d", d)
    }
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }
	cp.Close()
}



func Test_ConnPool_7(t *testing.T){
	c := make(chan net.Listener, 1)
	go server(t, c)
	listen := <-c
	defer listen.Close()
	
 	addr := ParseAddr(listen.Addr().Network(), listen.Addr().String())

   cp := &ConnPool{
        IdeConn:5,
        MaxConn:2,
    }
    defer cp.Close()
    conn, err := cp.Dial(addr.Network(), addr.String())
   	fatal(t, err)
    conn.Close()

    conn, err = cp.Dial(addr.Network(), addr.String())
   	fatal(t, err)
   	
    if d := cp.ConnNum(); d != 1 {
    	t.Fatalf("error %d", d)
    }
    
	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }

    conn.Close()

    if d := cp.ConnNum(); d != 1 {
    	t.Fatalf("error %d", d)
    }
    
	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 1 {
    	t.Fatalf("error %d", d)
    }

	cp.Close()
   	time.Sleep(time.Second)

}

func Test_ConnPool_8(t *testing.T){
	c := make(chan net.Listener, 1)
	go server(t, c)
	listen := <-c
	defer listen.Close()
	
 	addr := ParseAddr(listen.Addr().Network(), listen.Addr().String())

   cp := &ConnPool{
        IdeConn:5,
        MaxConn:2,
    }
    defer cp.Close()
    conn, err := cp.Dial(addr.Network(), addr.String())
   	fatal(t, err)
    conn.Close()
    
    ctx := context.WithValue(context.Background(), "priority", true) //新创建连接
    conn, err = cp.DialContext(ctx, addr.Network(), addr.String())
   	fatal(t, err)
    addr = conn.RemoteAddr()
    conn.Close()
    
    conn, err = cp.Get(addr)
   	fatal(t, err)
   	defer conn.Close()

    err = cp.Add(conn)
   	fatal(t, err)
   	
    if d := cp.ConnNum(); d != 2 {
    	t.Fatalf("error %d", d)
    }
    
	time.Sleep(time.Second)
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 2 {
    	t.Fatalf("error %d", d)
    }
 
    cp.CloseIdleConnections()
   	time.Sleep(time.Second)
   	
    if d := cp.ConnNum(); d != 0 {
    	t.Fatalf("error %d", d)
    }
    
    if d := cp.ConnNumIde(addr.Network(), addr.String()); d != 0 {
    	t.Fatalf("error %d", d)
    }

	cp.Close()
   	time.Sleep(time.Second)

}