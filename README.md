# vconnpool [![Build Status](https://travis-ci.org/456vv/vconnpool.svg?branch=master)](https://travis-ci.org/456vv/vconnpool)
golang vconnpool，可以TCP连接复用，使用方法和 net.Dialer 接口是相同。

# **列表：**
```go
type Dialer interface {                                                 // net.Dialer 接口
    Dial(network, address string) (net.Conn, error)                             // 拨号
    DialContext(ctx context.Context, network, address string) (net.Conn, error) // 拨号（支持上下文）
}
type Conn interface{                                                    // 连接接口
    net.Conn                                                                    // net连接接口
    Discard() error                                                             // 废弃（这条连接不再回收）
    IsReuseConn() bool                                                          // 判断这条连接是否是从池中读取出来的
    RawConn() net.Conn                                                          // 原始连接，这个连接使用 Close 关闭后，不会回收
}
type ConnPool struct {                                                          // 连接池
    Dialer                                                                 // 拨号
    Host        func(oldAddress string) (newAddress string)                     // 拨号地址变更
    IdeConn     int                                                             // 空闲连接数，0为不复用连接
    MaxConn     int                                                             // 最大连接数，0为无限制连接
    IdeTimeout  time.Duration                                                   // 空闲自动超时，0为不超时
}
    func (T *ConnPool) Dial(network, address string) (net.Conn, error)         // 拨号,如果 address 参数是host域名，.Get(...)将无法读取到连接。请再次使用 .Dial(...) 来读取。
    func (T *ConnPool) DialContext(ctx context.Context, network, address string) (net.Conn, error) //拨号（支持上下文）,如果 address 参数是host域名，.Get(...)将无法读取到连接。请再次使用 .Dial(...) 来读取。
    func (T *ConnPool) Add(conn net.Conn) error                                // 增加连接
    func (T *ConnPool) Put(conn net.Conn, addr net.Addr) error                 // 增加连接，支持 addr
    func (T *ConnPool) Get(addr net.Addr) (net.Conn, error)                    // 读取连接，读取出来的连接不会自动回收，需要调用 .Add(...) 收入
    func (T *ConnPool) ConnNum() int                                           // 当前连接数量
    func (T *ConnPool) ConnNumIde(network, address string) int                 // 当前连接数量(空闲)，不是实时的空闲连接数，存在多线程！
    func (T *ConnPool) CloseIdleConnections()                                  // 关闭空闲连接
    func (T *ConnPool) Close() error                                           // 关闭连接池
```