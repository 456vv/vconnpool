package vconnpool

import (
    "net"
    "errors"
    "sync"
    "io"
    "bufio"
    "time"
)

var (
	errorConnClose = errors.New("vconnpool: 连接已经关闭了")
	errorConnPoolClose = errors.New("vconnpool: 连接池已经被关闭")
)

var DefaultReadBufSize int = 4096                                                                 // 默认读取时的缓冲区大小（单位字节）

//Dialer 是 net.Dialer 接口
type Dialer interface {
    Dial(network, address string) (net.Conn, error)
}

//Conn 连接接口，包含了 net.Conn
type Conn interface{
	net.Conn        // 连接
	Discard() error // 废弃（这条连接不再回收）
}

//connAddr 连接地址
type connAddr struct {
    network, address string                                                                 // 类型，地址
}

//connSingle 单连接
type connSingle struct {
    net.Conn                                                                                // 连接
    cs          *connStorage                                                                // 连接存储
    cp          *ConnPool                                                                   // 池
    key         connAddr                                                                    // 连接地址
    err         error                                                                       // 错误
    count       int64                                                                       // 计数，判断还有没有数据正在读写
    poolsrc     bool                                                                        // 连接来源，判断连接是不是从池里读出来的
    once        sync.Once                                                                   // 一次调用，如果从池里读出的连接是已经被远程关闭的。则新创建一条连接
    done        bool                                                                        // 判断本次读取是否完成
    closed      bool                                                                        // 连接关闭了
    discard     bool                                                                        // 废弃（这条连接不再回收）
}

//Write 写入
//  参：
//      b []byte    写入字节
//  返：
//      n int       成功写入字节的长度
//      err error   错误，超时或暂时性的错误
func (cs *connSingle) Write(b []byte) (n int, err error){
    if cs.closed {
        return 0, io.EOF
    }
    n, err = cs.Conn.Write(b)
    cs.err=err
    return
}

//Read 读取
//  参：
//      b []byte    字节写入到b
//  返：
//      n int       成功读取字节的长度
//      err error   错误，超时或暂时性的错误
func (cs *connSingle) Read(b []byte) (n int, err error){
    if cs.closed {
        return 0, io.EOF
    }
    cs.count++
    cs.done = false
    G0:

    //从缓冲中读取
    n, err = cs.cs.bufr.Read(b)

    //由于从池里读出连接，无法知道远程是否已经关闭连接。
    //从第一次读取中判断是不是EOF，如果是，则新创建一条连接。
    var rest bool
    cs.once.Do(func(){
        if cs.poolsrc && err == io.EOF {
            //池里的连接被远程关闭了
            //重新创建一个连接
            conn, e := cs.cp.Dialer.Dial(cs.key.network, cs.key.address)
            if e != nil {
                return
            }
            cs.Conn.Close()
            cs.Conn = conn
            cs.cs.conn = conn
            rest = true
        }
    })
    if rest {
        goto G0
    }
    cs.err=err
    cs.count--

    //如果数据没有装满，说明本次接收完成
    if len(b) != n {
        cs.done = true
    }
    return
}

//Close 关闭连接
//  返：
//      error          错误
func (cs *connSingle) Close() error {
    if cs.closed {
        return errorConnClose
    }
    cs.closed = true

    //符合这几个条件，才回收到池中，其它关闭连接。
    //1，没有错误
    //2，没有正在等待读取
    //3，本次读取完成
    //4，没有被废弃
    if cs.err == nil && cs.count == 0  && cs.done && !cs.discard {
        return cs.cp.put(cs.cs, cs.key, false)
    }
    cs.cp.connNum--
    cs.cp = nil
    cs.cs = nil
    return cs.Conn.Close()
}

//LocalAddr 返回本地网络地址
func (cs *connSingle) LocalAddr() net.Addr{
    return cs.Conn.LocalAddr()
}
//RemoteAddr 返回远端网络地址
func (cs *connSingle) RemoteAddr() net.Addr{
    return cs.Conn.RemoteAddr()
}
//SetDeadline 设置读写超时时间
func (cs *connSingle) SetDeadline(t time.Time) error{
    if cs.closed {
        return errorConnClose
    }
    return cs.Conn.SetDeadline(t)
}
//SetReadDeadline 设置读取超时时间
func (cs *connSingle) SetReadDeadline(t time.Time) error{
    if cs.closed {
        return errorConnClose
    }
    return cs.Conn.SetReadDeadline(t)
}
//SetWriteDeadline 设置写入超时时间
func (cs *connSingle) SetWriteDeadline(t time.Time) error{
    if cs.closed {
        return errorConnClose
    }
    return cs.Conn.SetWriteDeadline(t)
}

//Discard 废弃（这条连接不再回收）
func (cs *connSingle) Discard() error {
    cs.discard = true
    return nil
}
//connStorage 连接存储
type connStorage struct{
    conn    net.Conn                // 实时连接
    bufr    *bufio.Reader           // 缓冲读取
    bufw    *bufio.Writer           // 缓冲写入，要记得写入之后调用 .Flush()
    use     bool                    // 为true,正在使用这个连接
    closed  bool                    // 连接已经关闭
}

func newConnStorage(conn net.Conn) *connStorage{
    //这里这么写，是方便调用 connStore.conn
    //connSingle.Read 调用失败时候会新创建连接，这样就不影响 bufr 和 bufw 这两个使用。
    connStore := &connStorage{
        conn: conn,
    }
    connStore.bufr= bufio.NewReaderSize(connStore.conn, DefaultReadBufSize)
    connStore.bufw= bufio.NewWriterSize(connStore.conn, DefaultReadBufSize)
    return connStore
}

//连接收回收，并检测有没有不请自来的数据。
//如果有，这个连接不稳定，可能会造成下次读取到错误的数据。
//这样只能关闭这个连接
func (cs *connStorage) loopReadUnknownData(){

    //先清空缓冲区还没读出的数据。
    n := cs.bufr.Buffered()
    cs.bufr.Discard(n)

    //没有在使用
    for !cs.use {
        _, err := cs.bufr.Peek(1)
        //再次判断没有在使用
        if !cs.use && err != io.EOF {
            cs.closed = true
            cs.conn.Close()
            break
        }
    }
}

func (cs *connStorage) Close() error {
    //设置use为true，是让 .loopReadUnknownData() 可以退出
    cs.use = true
    cs.closed = true
    return cs.conn.Close()
}

//ConnPool 连接池
type ConnPool struct {
    net.Dialer                                                                              // 拨号
    IdeConn     int                                                                         // 空闲连接数，0为不复用连接
    MaxConn     int                                                                         // 最大连接数，0为无限制连接
    connNum     int                                                                         // 当前连接数
    conns       map[connAddr]chan *connStorage                                              // 连接集
    m           *sync.Mutex                                                                 // 锁
    closed      bool                                                                        // 关闭池
    inited      bool                                                                        // 初始化
}

func (cp *ConnPool) init(){
    if cp.inited {
        return
    }
    if cp.conns ==  nil {
         cp.conns = make(map[connAddr]chan *connStorage)
    }
    if cp.m == nil {
        cp.m = new(sync.Mutex)
    }
    cp.inited = true
}

func (cp *ConnPool) getConns(key connAddr) chan *connStorage {
    cp.m.Lock()
    defer cp.m.Unlock()
    conns, ok := cp.conns[key]
    if !ok {
        conns = make(chan *connStorage, cp.IdeConn)
        cp.conns[key] = conns
    }
    return conns
}

//Dial 拨号
//  参：
//      network string      连接类型
//      address string      连接地址
//  返：
//      net.Conn            连接
//      error               错误
func (cp *ConnPool) Dial(network, address string) (net.Conn, error) {
    if cp.closed {
        return nil, errorConnPoolClose
    }
    cp.init()

    key := connAddr{network, address}
    connStore, conn, pool, err := cp.getConn(key, true)
    if err != nil {
        return nil, err
    }
    return &connSingle{Conn:conn, cs:connStore, cp:cp, key:key, poolsrc: pool, done: true}, nil
}

func (cp *ConnPool) getConn(key connAddr, dial bool) (connStore *connStorage, conn net.Conn, pool bool, err error) {
    //读取的时要注意 host 或 ip，如果你使用 .Dialer 访问并回收连接，之后使用 .Get 读取连接。
    //要注意了，.Dialer 是以 hostname 作为 key 存储，而 .Get 读取是以 IP，所以可能、可能、可能会读不出接连。
    G0:
    select {
        case connStore = <- cp.getConns(key):
            if connStore.closed {
                //存储里的连接关闭，需要减少当前连接计数
                cp.connNum--
                goto G0
            }
            conn = connStore.conn
            pool = true
        default:
            if !dial {
                //dial设置了不需创建新连接
                err = errors.New("vconnpool: 没有空闲的连接可以读取。或者也有可能是池中的连接失败的，不能使用。")
                return
            }
            if cp.MaxConn != 0 && cp.connNum >= cp.MaxConn {
                err = errors.New("vconnpool: 连接池中的连接数量已经达到最大限制")
                return
            }
            conn, err = cp.Dialer.Dial(key.network, key.address)
            if err != nil {
                return
            }
            connStore = newConnStorage(conn)
            pool = false
            cp.connNum++
    }
    //设置连接在使用状态
    connStore.use = true
    return
}

//Get 从池中读取一条连接。
//读取出来的连接不会自动回收，如果你.Close() 是真的关闭连接，不是回收。
//需要在不关闭连接的状态下，需要调用 .Put(...) 收入
//而.Dial(...) 读取出来的连接，调用.Close() 之后，是自动收回的。.Get(...) 不是。
//  参：
//      addr net.Addr   地址
//  返：
//      conn net.Conn   连接
//      error           错误
func (cp *ConnPool) Get(addr net.Addr) (conn net.Conn, err error) {
    if cp.closed {
        return nil, errorConnPoolClose
    }
    cp.init()
    key := connAddr{addr.Network(), addr.String()}
    _, conn, _, err = cp.getConn(key, false)
    if err == nil {
        cp.connNum--
    }
    return
}

//Put 增加一个连接到池中
//  参：
//      conn net.Conn   连接
//  返：
//      error           错误
func (cp *ConnPool) Put(conn net.Conn) error {
    if cp.closed {
        //池被关闭了，由于是空闲连接，就关闭它。
        conn.Close()
        return errors.New("vconnpool: 连接池已经被关闭，当前连接也已经被关闭")
    }

    //如果是 *connSingle 类型则关闭，使用自动收回，不重复回收。
    if c, ok := conn.(*connSingle); ok {
        return c.Close()
    }

    if cp.MaxConn != 0 && cp.connNum >= cp.MaxConn {
        conn.Close()
        return errors.New("vconnpool: 连接池中的连接数量已经达到最大限制，当前连接也已经被关闭")
    }

    cp.init()
    connStore := newConnStorage(conn)
    addr := conn.RemoteAddr()
    key := connAddr{addr.Network(), addr.String()}
    cp.connNum++
    return cp.put(connStore, key, true)
}

//put 回收连接
func (cp *ConnPool) put(connStore *connStorage, key connAddr, use bool) error {
    if cp.closed {
        return connStore.Close()
     }

    //设置不在使用，并检查远程有没有数据发来
    connStore.use=use
    go connStore.loopReadUnknownData()

    select {
        case  cp.getConns(key) <- connStore:
            return nil
        default:
            cp.connNum--
            return connStore.Close()
    }
}

//ConnNum 当前可用连接数量
//  返：
//      int     数量
func (cp *ConnPool) ConnNum() int {
    return cp.connNum
}

//ConnNumIde    当前空闲连接数量
//  返：
//      int     数量
func (cp *ConnPool) ConnNumIde(network, address string) int {
    if cp.closed {
        return 0
    }
    cp.init()
    key := connAddr{network, address}
    conns := cp.getConns(key)
    return len(conns)
}

//CloseIdleConnections 关闭空闲连接池
func (cp *ConnPool) CloseIdleConnections() {
    cp.init()
    cp.m.Lock()
    defer cp.m.Unlock()
     for k, conns := range cp.conns {
        GO:for {
         	select{
                case connstore := <- conns:
                    cp.connNum--
                    connstore.Close()
                default:
                   if cp.closed {
                        close(conns)
                        delete(cp.conns, k)
                    }
                    break GO
            }
        }
    }
}
// Close 关闭连接池
func (cp *ConnPool) Close() error {
    if cp.closed {
        return nil
    }
    cp.closed = true
    cp.CloseIdleConnections()
    return nil
}
