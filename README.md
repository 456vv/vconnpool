# vconnpool [![Build Status](https://travis-ci.org/456vv/vconnpool.svg?branch=master)](https://travis-ci.org/456vv/vconnpool)
go/golang TCP connection pool, 可以连接复用，使用方法和 net.Dialer 是相同的，所以比较方便调用
<br/>
最近更新20160912：<a href="/v1/update.txt">update.txt</a>
<br/>
列表：
====================
    var DefaultReadBufSize int = 4096                                               // 默认读取时的缓冲区大小（单位字节）
    type Dialer interface {                                                 // net.Dialer 接口
        Dial(network, address string) (net.Conn, error)                             // 拨号
    }
    type Conn interface{                                                    // 连接接口
        net.Conn                                                                    // net连接接口
        Discard() error                                                             // 废弃（这条连接不再回收）
    }
    type ConnPool struct {                                                  // 连接池
        net.Dialer                                                                  // 拨号
        IdeConn     int                                                             // 空闲连接数，0为不复用连接
        MaxConn     int                                                             // 最大连接数，0为无限制连接
    }
        func (cp *ConnPool) Dial(network, address string) (net.Conn, error)         // 拨号,如果 address 参数是host域名，.Get(...)将无法读取到连接。请再次使用 .Dial(...) 来读取。
        func (cp *ConnPool) DialContext(ctx context.Context, network, address string) (net.Conn, error) //拨号（支持上下文）,如果 address 参数是host域名，.Get(...)将无法读取到连接。请再次使用 .Dial(...) 来读取。
        func (cp *ConnPool) Add(addr net.Addr, conn net.Conn) error                 // 增加连接
        func (cp *ConnPool) Get(addr net.Addr) (net.Conn, error)                    // 读取连接，读取出来的连接不会自动回收，需要调用 .Add(...) 收入
        func (cp *ConnPool) ConnNum() int                                           // 当前连接数量
        func (cp *ConnPool) ConnNumIde(network, address string) int                 // 当前连接数量(空闲)
        func (cp *ConnPool) CloseIdleConnections()                                  // 关闭空闲连接
        func (cp *ConnPool) Close() error                                           // 关闭连接池
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
        err = cp.Add(conn.RemoteAddr(), conn)
        fmt.Println(err)
        conn, err = cp.Get(conn.RemoteAddr())
        fmt.Println(conn, err)
    }

例3：

    func Test_ConnPool_5(t *testing.T){
        cp := &ConnPool{
            IdeConn:5,
            MaxConn:2,
        }
        defer cp.Close()
        conn, err := cp.Dial("tcp", "www.baidu.com:80")
        if err != nil {t.Fatal(err)}
        conn.Close()

        conn, err = cp.Dial("tcp", "www.baidu.com:80")
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

