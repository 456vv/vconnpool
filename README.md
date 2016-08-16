# vconnpool
go/golang TCP/UDP connection pool, 可以连接复用，使用方法和 net.Dialer 是相同的，所以比较方便调用
<br/>
更新：
====================
    时间：2016-08-16
    内容：1, 增加一个最大连接限制 ConnPool.MaxConn int
          2, 增加一个可控的连接在读取数据时的缓冲区大小 var DefaultReadBufSize int = 4096
          3, 增加8个方法：
                func (cp *ConnPool) Put(conn net.Conn)// 增加连接
                func (cp *ConnPool) Get(add net.Addr) (net.Conn, error)// 读取连接
                func (cp *ConnPool) ConnNum() int// 当前连接数量
                func (cs *connSingle) LocalAddr() net.Addr// 返回本地网络地址
                func (cs *connSingle) RemoteAddr() net.Addr// 返回远端网络地址
                func (cs *connSingle) SetDeadline(t time.Time) error// 设置读写超时时间
                func (cs *connSingle) SetReadDeadline(t time.Time) error// 设置读取超时时间
                func (cs *connSingle) SetWriteDeadline(t time.Time) error// 设置写入超时时间
    时间：2016-08-15
    内容：TCP连接池，已经完成。正在投入使用...
<br/>
列表：
====================
    var DefaultReadBufSize int = 4096                                               // 默认读取时的缓冲区大小（单位字节）
    type Dialer interface {                                                 // net.Dialer 接口
        Dial(network, address string) (net.Conn, error)                             // 拨号
    }
    type connAddr struct {                                                  // 连接地址
        network, address string                                                     // 类型，地址
    }
    type connSingle struct {                                                // 单连接
        net.Conn                                                                    // 连接
        cs          *connStorage                                                    // 连接存储
        cp          *ConnPool                                                       // 池
        key         connAddr                                                        // 连接地址
        err         error                                                           // 错误
        count       int64                                                           // 计数，判断还有没有数据正在读写
        poolsrc     bool                                                            // 连接来源，判断连接是不是从池里读出来的
        once        sync.Once                                                       // 一次调用，如果从池里读出的连接是已经被远程关闭的。则新创建一条连接
        done        bool                                                            // 判断本次读取是否完成
        closed      bool                                                            // 连接关闭了
    }
        func (cs *connSingle) Write(b []byte) (n int, err error)                    // 写入
        func (cs *connSingle) Read(b []byte) (n int, err error)                     // 读取
        func (cs *connSingle) Close() error                                         // 关闭连接
        func (cs *connSingle) LocalAddr() net.Addr                                  // 返回本地网络地址
        func (cs *connSingle) RemoteAddr() net.Addr                                 // 返回远端网络地址
        func (cs *connSingle) SetDeadline(t time.Time) error                        // 设置读写超时时间
        func (cs *connSingle) SetReadDeadline(t time.Time) error                    // 设置读取超时时间
        func (cs *connSingle) SetWriteDeadline(t time.Time) error                   // 设置写入超时时间
    type connStorage struct{                                                // 连接存储
        conn    net.Conn                                                            // 实时连接
        bufr    *bufio.Reader                                                       // 缓冲读取
        bufw    *bufio.Writer                                                       // 缓冲写入，要记得写入之后调用 .Flush()
        use     bool                                                                // 为true,正在使用这个连接
        closed  bool                                                                // 连接已经关闭
    }
        func (cs *connStorage) loopReadUnknownData()                                // 连接收回收，并检测有没有不请自来的数据。
        func (cs *connStorage) Close() error                                        // 连接关闭
    type ConnPool struct {                                                  // 连接池
        net.Dialer                                                                  // 拨号
        IdeConn     int                                                             // 空闲连接数，0为不复用连接
        MaxConn     int                                                             // 最大连接数，0为无限制连接
        connNum     int                                                             // 当前连接数
        conns       map[connAddr]chan *connStorage                                  // 连接集
        m           *sync.Mutex                                                     // 锁
        closed      bool                                                            // 关闭池
        inited      bool                                                            // 初始化
    }
        func (cp *ConnPool) init()                                                  // 初始化
        func (cp *ConnPool) getConns(key connAddr) chan *connStorage                // 读取池连接
        func (cp *ConnPool) Dial(network, address string) (net.Conn, error)         // 拨号,如果 address 参数是host域名，.Get(...)将无法读取到连接。请再次使用 .Dial(...) 来读取。
        func (cp *ConnPool) getConn(key connAddr, dial bool) (connStore *connStorage, conn net.Conn, pool bool, err error) // 读取连接
        func (cp *ConnPool) Put(conn net.Conn)                                      // 增加连接
        func (cp *ConnPool) Get(add net.Addr) (net.Conn, error)                     // 读取连接
        func (cp *ConnPool) ConnNum() int                                           // 当前连接数量
        func (cp *ConnPool) put(conn net.Conn, key connAddr) error                  // 回收连接
        func (cp *ConnPool) CloseIdleConnections()                                  // 关闭空闲连接
        func (cp *ConnPool) Close()                                                 // 关闭连接池
<br/>
使用方法：
====================
例1：<br/>
    func main(){
        cp := &ConnPool{
            IdeConn:5,
            MaxConn:2,
        }
        defer cp.Close()
        conn, err := cp.Dial("tcp", "www.baidu.com:80")
        fmt.Println(conn, err)
    }
例2：<br/>
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
