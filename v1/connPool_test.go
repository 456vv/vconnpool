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
    netConn1, err := cp.Dial("tcp", "www.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }

    netConn2, err := cp.Dial("tcp", "www.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn1.Close()

    netConn3, err := cp.Dial("tcp", "www.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn2.Close()
    netConn3.Close()

    key := connAddr{"tcp", "www.baidu.com:80"}
    l := len(cp.conns[key])
    if l != 2 {
        t.Fatalf("错误：无法加入空闲连接到池中，池中连接数量：%d", l)
    }
}
func Test_ConnPool_2(t *testing.T){
    cp := &ConnPool{
        Dialer:net.Dialer{},
        IdeConn:5,
    }
    defer cp.Close()
    cp.Timeout = 1*time.Second
    cp.KeepAlive = 1*time.Second
    netConn4, err := cp.Dial("tcp", "www.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn4.Close()


}

func Test_ConnPool_3(t *testing.T){
    cp := &ConnPool{
        Dialer:net.Dialer{},
        IdeConn:5,
    }
    defer cp.Close()
    cp.Timeout = 1*time.Minute
    cp.KeepAlive = 1*time.Minute
    netConn1, err := cp.Dial("tcp", "www.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn1.Write([]byte("GET / HTTP/1.1\r\nHost:www.baidu.com\r\nConnection:Close\r\n\r\n"))
    p := make([]byte, 1024)
    n, err := netConn1.Read(p)
    if err != nil {
        t.Fatal(err)
    }
    t.Log(n)

    netConn1.Close()

    time.Sleep(time.Second)

    netConn2, err := cp.Dial("tcp", "www.baidu.com:80")
    if err != nil {
    	t.Fatal(err)
    }
    netConn2.Write([]byte("GET / HTTP/1.1\r\nHost:www.baidu.com\r\nConnection:Close\r\n\r\n"))
    p = make([]byte, 1024)
    n, err = netConn2.Read(p)
    if err != nil {
        t.Fatal(err)
    }
    t.Log(n)

    netConn1.Close()

}