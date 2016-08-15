package vconnpool

import (
    "net"
    "errors"
    "sync"
    "io"
    "bufio"
)

const defaultBufSize = 4096

type Dialer interface {
    Dial(network, address string) (net.Conn, error)
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
}

//Write 写入
//  参：
//      b []byte    写入字节
//  返：
//      n int       成功写入字节的长度
//      err error   错误，超时或暂时性的错误
func (cs *connSingle) Write(b []byte) (n int, err error){
    if cs.Conn == nil {
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
    if cs.Conn == nil {
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
    //符合这几个条件，才回收到池中，其它关闭连接。
    //1，没有错误
    //2，没有正在等待读取
    //3，本次读取完成
    if cs.err == nil && cs.count == 0  && cs.done {
        cs.Conn = nil
        return cs.cp.put(cs.cs, cs.key)
    }
    return cs.Conn.Close()
}

type connStorage struct{
    conn    net.Conn
    bufr    *bufio.Reader
    bufw    *bufio.Writer
    use     bool
    closed  bool
}

//连接收回收，并检测有没有不请自来的数量。
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
            cs.conn.Close()
            cs.closed = true
            break
        }
    }
}

func (cs *connStorage) Close() error {
    cs.use = true
    cs.closed = true
    return cs.conn.Close()
}

//ConnPool 连接池
type ConnPool struct {
    net.Dialer                                                                              // 拨号
    IdeConn     int                                                                         // 空连接数
    conns       map[connAddr]chan *connStorage                                              // 连接集
    m           *sync.Mutex                                                                 // 锁
    exited      bool                                                                        // 关闭池
}

//Dial 拨号
//  参：
//      network string      连接类型
//      address string      连接地址
//  返：
//      net.Conn            连接
//      error               错误
func (cp *ConnPool) Dial(network, address string) (net.Conn, error) {
    if cp.exited {
        return nil, errors.New("vconnpool.ConnPool.Dial: 连接池已经被关闭")
    }
    if cp.conns ==  nil {
         cp.conns = make(map[connAddr]chan *connStorage)
    }
    if cp.m == nil {
        cp.m = new(sync.Mutex)
    }

    cp.m.Lock()
    defer cp.m.Unlock()

    key := connAddr{network, address}
    conns, ok := cp.conns[key]
    if !ok {
        conns = make(chan *connStorage, cp.IdeConn)
        cp.conns[key] = conns
    }

    var conn net.Conn
    var connStore  *connStorage
    var err error
    var pool bool

    G0:
    select {
        case connStore = <- conns:
            if connStore.closed {
                goto G0
            }
            conn = connStore.conn
            pool = true
        default:
            conn, err = cp.Dialer.Dial(network, address)
            if err != nil {
                return nil, err
            }
            connStore = &connStorage{
                conn: conn,
            }
            //这里这么写，是方便调用 connStore.conn
            //connSingle.Read 调用失败时候会新创建连接，这样就不影响 bufr 和 bufw 这两个使用。
            connStore.bufr= bufio.NewReaderSize(connStore.conn, defaultBufSize)
            connStore.bufw= bufio.NewWriterSize(connStore.conn, defaultBufSize)
            pool = false
    }

    //设置连接在使用状态
    connStore.use = true

    return &connSingle{Conn:conn, cs:connStore, cp:cp, key:key, poolsrc: pool, done: true}, nil
}

//put 回收连接
//  参：
//      conn net.Conn   连接
//      key connAddr    地址
//  返：
//      error           错误
func (cp *ConnPool) put(connStore *connStorage, key connAddr) error {
    if cp.exited {
        return connStore.Close()
     }

    cp.m.Lock()
    defer cp.m.Unlock()

    //设置不在使用，并检查远程有没有数据发来
    connStore.use=false
    go connStore.loopReadUnknownData()

    select {
        case  cp.conns[key] <- connStore:
            return nil
        default:
            return connStore.Close()
    }
}

//CloseIdleConnections 关闭空闲连接池
func (cp *ConnPool) CloseIdleConnections() {
    if cp.m != nil {
        cp.m.Lock()
        defer cp.m.Unlock()
    }

    for _, conns := range cp.conns {
        if cp.exited {close(conns)}
        for conn := range conns {
            conn.conn.Close()
        }
    }
}
// Close 关闭连接池
func (cp *ConnPool) Close() {
    cp.exited = true
    cp.CloseIdleConnections()
}
