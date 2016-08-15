# vconnpool
go/golang TCP/UDP connection pool, 可以连接复用
<br/>
列表：
====================
    const defaultBufSize = 4096                                                     // 默认缓冲
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
    }
        func (cs *connSingle) Write(b []byte) (n int, err error)                    // 写入
        func (cs *connSingle) Read(b []byte) (n int, err error)                     // 读取
        func (cs *connSingle) Close() error                                         // 关闭连接
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
        IdeConn     int                                                             // 空连接数
        conns       map[connAddr]chan *connStorage                                  // 连接集
        m           *sync.Mutex                                                     // 锁
        exited      bool                                                            // 关闭池
    }
        func (cp *ConnPool) Dial(network, address string) (net.Conn, error)         // 拨号
        func (cp *ConnPool) put(conn net.Conn, key connAddr) error                  // 回收连接
        func (cp *ConnPool) CloseIdleConnections()                                  // 关闭空闲连接
        func (cp *ConnPool) Close()                                                 // 关闭连接池
