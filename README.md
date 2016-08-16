# vconnpool
go/golang TCP/UDP connection pool, 可以连接复用，使用方法和 net.Dialer 是相同的，所以比较方便调用
<br/>
最近更新20160816：<a href="/v1/update.txt">update.txt</a>
<br/>
列表：
====================
    var DefaultReadBufSize int = 4096                                               // 默认读取时的缓冲区大小（单位字节）
    type Dialer interface {                                                 // net.Dialer 接口
        Dial(network, address string) (net.Conn, error)                             // 拨号
    }
    type ConnPool struct {                                                  // 连接池
        net.Dialer                                                                  // 拨号
        IdeConn     int                                                             // 空闲连接数，0为不复用连接
        MaxConn     int                                                             // 最大连接数，0为无限制连接
    }
        func (cp *ConnPool) Dial(network, address string) (net.Conn, error)         // 拨号,如果 address 参数是host域名，.Get(...)将无法读取到连接。请再次使用 .Dial(...) 来读取。
        func (cp *ConnPool) Put(conn net.Conn)                                      // 增加连接
        func (cp *ConnPool) Get(add net.Addr) (net.Conn, error)                     // 读取连接
        func (cp *ConnPool) ConnNum() int                                           // 当前连接数量
        func (cp *ConnPool) CloseIdleConnections()                                  // 关闭空闲连接
        func (cp *ConnPool) Close()                                                 // 关闭连接池
<br/>
使用方法：
====================
例1：

    func main(){
        cp := &ConnPool{
            IdeConn:5,
            MaxConn:2,
        }
        defer cp.Close()
        conn, err := cp.Dial("tcp", "www.baidu.com:80")
        fmt.Println(conn, err)
    }

例2：

    func main(){
        cp := &ConnPool{
            IdeConn:5,
            MaxConn:2,
        }
        defer cp.Close()
        conn, err := net.Dial("tcp", "www.baidu.com:80")
        fmt.Println(conn, err)
        err = cp.Put(conn)
        fmt.Println(err)
        conn, err = cp.Get(conn.RemoteAddr())
        fmt.Println(conn, err)
    }
