package vconnpool

import (
	"testing"
    "time"
    "net"
)


func Test_ConnPool_1(t *testing.T){
    cp := &ConnPool{
        IdeConn:5,
        MaxConn:2,
    }
    defer cp.Close()
    netConn1, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }

    netConn2, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn1.Close()

    netConn3, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn2.Close()
    netConn3.Close()

    key := connAddr{"tcp", "news.baidu.com:80"}
    l := len(cp.conns[key])
    if l != 2 {
        t.Fatalf("错误：无法加入空闲连接到池中，池中连接数量：%d", l)
    }
}
func Test_ConnPool_2(t *testing.T){
    cp := &ConnPool{
        Dialer:&net.Dialer{},
        IdeConn:5,
    }
    defer cp.Close()
    netConn4, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn4.Close()

}

func Test_ConnPool_3(t *testing.T){
    cp := &ConnPool{
        Dialer:&net.Dialer{},
        IdeConn:5,
    }
    defer cp.Close()
    netConn1, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn1.Write([]byte("GET / HTTP/1.1\r\nHost:news.baidu.com\r\nConnection:Close\r\n\r\n"))
    p := make([]byte, 1024)
    n, err := netConn1.Read(p)
    if err != nil {
        t.Fatal(err)
    }
    t.Log(n)

    netConn1.Close()

    time.Sleep(time.Second)

    netConn2, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn2.Write([]byte("GET / HTTP/1.1\r\nHost:news.baidu.com\r\nConnection:Close\r\n\r\n"))
    p = make([]byte, 1024)
    n, err = netConn2.Read(p)
    if err != nil {
        t.Fatal(err)
    }
    t.Log(n)

    netConn2.Close()

}

func Test_ConnPool_4(t *testing.T){
    cp := &ConnPool{
        Dialer:&net.Dialer{},
        IdeConn:5,
    }
    defer cp.Close()

    netConn1, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {t.Fatal(err)}
    netConn1.Write([]byte("GET / HTTP/1.1\r\nHost:news.baidu.com\r\nConnection:Close\r\n\r\n"))
    p := make([]byte, 10240)
    n, err := netConn1.Read(p)
    if err != nil {t.Fatal(err)}
    t.Log(n)
    netConn1.Close()

    time.Sleep(time.Second)
    if cp.ConnNum() != 1 {
        t.Fatalf("netConn1:池里的连接数量不符，返回为：%d，预设为：1", cp.ConnNum())
    }

    netConn2, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {t.Fatal(err)}
    netConn2.Write([]byte("GET / HTTP/1.1\r\nHost:news.baidu.com\r\nConnection:Close\r\n\r\n"))
    p = make([]byte, 10240)
    n, err = netConn2.Read(p)
    if err != nil {t.Fatal(err)}
    t.Log(n)
    netConn2.Close()

    time.Sleep(time.Second)
    if cp.ConnNum() != 1 {
        t.Fatalf("netConn2:池里的连接数量不符，返回为：%d，预设为：1", cp.ConnNum())
    }

    netConn3, err := net.Dial("tcp", "news.baidu.com:80")
    if err != nil {t.Fatal(err)}
    defer netConn3.Close()
    err = cp.Add(netConn3.RemoteAddr(), netConn3)
    if err != nil {t.Fatal(err)}
    if cp.ConnNum() != 2 {
        t.Fatalf("netConn3:池里的‘可用’连接数量不符，返回为：%d，预设为：2", cp.ConnNum())
    }

    netConn4, err := net.Dial("tcp", "news.baidu.com:80")
    if err != nil {t.Fatal(err)}
    defer netConn4.Close()
    err = cp.Add(netConn4.RemoteAddr(), netConn4)
    if err != nil {t.Fatal(err)}
    if cp.ConnNum() != 3 {
        t.Fatalf("netConn4:池里的连接数量不符，返回为：%d，预设为：3", cp.ConnNum())
    }

    _, err = cp.Get(netConn3.RemoteAddr())
    if err != nil {t.Fatal(err)}
    if cp.ConnNum() != 2 {
        t.Fatalf("Get:池里的连接数量不符，返回为：%d，预设为：2", cp.ConnNum())
    }

    _, err = cp.Get(netConn4.RemoteAddr())
    if err != nil {t.Fatal(err)}
    if cp.ConnNum() != 1 {
        t.Fatalf("Get:池里的连接数量不符，返回为：%d，预设为：1", cp.ConnNum())
    }

    cp.Close()
    cp.Close()
    cp.CloseIdleConnections()
    cp.CloseIdleConnections()
    cp.Close()
    cp.CloseIdleConnections()
    if cp.ConnNum() != 0 {
        t.Fatalf("Get:池里的连接数量不符，返回为：%d，预设为：0", cp.ConnNum())
    }

}


func Test_ConnPool_5(t *testing.T){
    cp := &ConnPool{
        IdeConn:5,
        MaxConn:2,
    }
    defer cp.Close()
    conn, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {t.Fatal(err)}
    conn.Close()

    conn, err = cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {t.Fatal(err)}
    c, ok := conn.(Conn)
    if !ok {
        t.Fatal("不支持转换为 Conn 接口")
    }
    if cp.ConnNum() != 1 {
        t.Fatalf("池里的连接数量不符，返回为：%d，预设为：1", cp.ConnNum())
    }
    c.Discard()
    c.Close()
    if cp.ConnNum() != 0 {
        t.Fatalf("池里的连接数量不符，返回为：%d，预设为：0", cp.ConnNum())
    }
}

func Test_ConnPool_6(t *testing.T){
   cp := &ConnPool{
        IdeConn:5,
        MaxConn:2,
    }
    defer cp.Close()
    conn, err := cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {t.Fatal(err)}
    conn.Close()

    conn, err = cp.Dial("tcp", "news.baidu.com:80")
    if err != nil {t.Fatal(err)}
    if cp.ConnNum() != 1 {
        t.Fatalf("池里的连接数量不符，返回为：%d，预设为：1", cp.ConnNum())
    }
    conn.Close()

    ideCount := cp.ConnNumIde("tcp", "news.baidu.com:80")
    if ideCount != cp.ConnNum() {
        t.Fatalf("空闲连接和可用连接数量不符，空闲为：%d，可用为：%d", ideCount, cp.ConnNum())
    }
    cp.Close()
    //池被清空，空闲和实用连接都为0
    ideCount = cp.ConnNumIde("tcp", "news.baidu.com:80")
    if ideCount != 0 || cp.ConnNum() !=0 {
        t.Fatalf("空闲连接和可用连接数量不符，空闲为：%d，可用为：%d", ideCount, cp.ConnNum())
    }

}

