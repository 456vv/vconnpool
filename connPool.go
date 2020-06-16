package vconnpool

import (
    "net"
    "errors"
    "sync"
    "io"
    "time"
    "fmt"
    "context"
    "sync/atomic"
    "github.com/456vv/vconn"
)

var (
	errorConnClose 		= errors.New("vconnpool: 连接已经关闭了")
	errorConnPoolClose 	= errors.New("vconnpool: 连接池已经被关闭")
	errorConnPoolMax 	= errors.New("vconnpool: 连接池中的连接数量已经达到最大限制")

)

//响应完成设置
type atomicBool int32
func (T *atomicBool) isTrue() bool 	{ return atomic.LoadInt32((*int32)(T)) != 0 }
func (T *atomicBool) isFalse() bool	{ return atomic.LoadInt32((*int32)(T)) != 1 }
func (T *atomicBool) setTrue() bool	{ return !atomic.CompareAndSwapInt32((*int32)(T), 0, 1)}
func (T *atomicBool) setFalse() bool{ return !atomic.CompareAndSwapInt32((*int32)(T), 1, 0)}


//Dialer 是 net.Dialer 接口
type Dialer interface {
    Dial(network, address string) (net.Conn, error)
    DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

//Conn 连接接口，包含了 net.Conn
type Conn interface{
	net.Conn        	// 连接
	Discard() error 	// 废弃（这条连接不再回收）
	IsReuseConn() bool	// 判断这条连接是否是从池中读取出来的
}

//connAddr 连接地址
type connAddr struct {
    network, address string                                                                 // 类型，地址
}

//connSingle 单连接
type connSingle struct {
    net.Conn                                                                                // 连接
    cp          *ConnPool                                                                   // 池
    key         connAddr                                                                    // 连接地址
    isPool     	bool                                                                        // 连接来源，判断连接是不是从池里读出来的
    closed      atomicBool                                                                  // 连接关闭了
    discard     atomicBool                                                                  // 废弃（这条连接不再回收）
}

//Write 写入
//	b []byte    写入字节
//	n int       成功写入字节的长度
//	err error   错误，超时或暂时性的错误
func (cs *connSingle) Write(b []byte) (n int, err error){
    if cs.closed.isTrue() {
        return 0, io.EOF
    }
    return cs.Conn.Write(b)
}

//Read 读取
//	b []byte    字节写入到b
//	n int       成功读取字节的长度
//	err error   错误，超时或暂时性的错误
func (cs *connSingle) Read(b []byte) (n int, err error){
    if cs.closed.isTrue() {
        return 0, io.EOF
    }
    return cs.Conn.Read(b)
}

//Close 关闭连接
//	error          错误
func (cs *connSingle) Close() error {
    if cs.closed.setTrue() {
        return errorConnClose
    }
    
    notifier, ok := cs.Conn.(vconn.CloseNotifier)
    if ok && cs.discard.isFalse() {
    	select {
    	case <-notifier.CloseNotify():
    		//连接已经关闭
    	default:
    		return cs.cp.put(cs.key, cs.Conn)
    	}
    }
    atomic.AddInt32(&cs.cp.connNum, -1)
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
    if cs.closed.isTrue() {
        return errorConnClose
    }
    return cs.Conn.SetDeadline(t)
}
//SetReadDeadline 设置读取超时时间
func (cs *connSingle) SetReadDeadline(t time.Time) error{
    if cs.closed.isTrue() {
        return errorConnClose
    }
    return cs.Conn.SetReadDeadline(t)
}
//SetWriteDeadline 设置写入超时时间
func (cs *connSingle) SetWriteDeadline(t time.Time) error{
    if cs.closed.isTrue() {
        return errorConnClose
    }
   return cs.Conn.SetWriteDeadline(t)
}

//Discard 废弃（这条连接不再回收）
func (cs *connSingle) Discard() error {
    cs.discard.setTrue()
    return nil
}

//IsReuseConn 是否是重用连接
func (cs *connSingle) IsReuseConn() bool {
	return cs.isPool
}


//ConnPool 连接池
type ConnPool struct {
    *net.Dialer                                                                             // 拨号
    IdeConn     int                                                                         // 空闲连接数，0为不复用连接
    MaxConn     int                                                                         // 最大连接数，0为无限制连接
    connNum     int32                                                                       // 当前连接数
    conns       map[connAddr]chan net.Conn                                              	// 连接集
    m           sync.Mutex                                                                  // 锁
    closed      atomicBool                                                                  // 关闭池
    inited      atomicBool                                                                  // 初始化
}

func (cp *ConnPool) init(){
    if cp.inited.isTrue() {
        return
    }
    if cp.conns ==  nil {
         cp.conns = make(map[connAddr]chan net.Conn)
    }
    if cp.Dialer == nil {
    	cp.Dialer = new(net.Dialer)
    }
    cp.inited.setTrue()
}


func (cp *ConnPool) getConns(key connAddr) chan net.Conn {
    cp.m.Lock()
    defer cp.m.Unlock()
    conns, ok := cp.conns[key]
    if !ok {
        conns = make(chan net.Conn, cp.IdeConn)
        cp.conns[key] = conns
    }
    return conns
}

//Dial 拨号
//	network string      连接类型
//	address string      连接地址
//	net.Conn            连接
//	error               错误
func (cp *ConnPool) Dial(network, address string) (net.Conn, error) {
    return cp.DialContext(context.Background(), network, address)
}

//DialContext 拨号，如果ctx 携带键值是（priority=true）,是创建新连接，否则从池中读取。
//	ctx context.Context 上下文
//	network string      连接类型
//	address string      连接地址
//	net.Conn            连接
//	error               错误
func (cp *ConnPool) DialContext(ctx context.Context, network, address string) (conn net.Conn, err error) {
    if cp.closed.isTrue() {
        return nil, errorConnPoolClose
    }
    cp.init()
    var (
    	pool 	bool
	    key 	= connAddr{network, address}
    )
    priority, ok :=ctx.Value("priority").(bool)
    if ok && priority {
    	conn, err = cp.dial(ctx, network, address)
    }else{
	    conn, pool, err = cp.getConn(ctx, key, true)
    }
    if err != nil {
        return nil, err
    }
    return &connSingle{Conn:conn, cp:cp, key:key, isPool: pool}, nil
}


func (cp *ConnPool) dial(ctx context.Context, network, address string) (conn net.Conn, err error) {
    if cp.MaxConn != 0 &&  int(atomic.LoadInt32(&cp.connNum)) >= cp.MaxConn {
        return nil, errorConnPoolMax
    }
    
    conn, err = cp.Dialer.DialContext(ctx, network, address)
    if err != nil {
        return
    }
    
    if int(atomic.AddInt32(&cp.connNum, 1)) > cp.MaxConn && cp.MaxConn != 0 {
    	atomic.AddInt32(&cp.connNum, -1)
    	conn.Close()
        return nil, errorConnPoolMax
    }
   	
	return vconn.NewConn(conn), nil
}

func (cp *ConnPool) getConn(ctx context.Context, key connAddr, dial bool) (conn net.Conn, pool bool, err error) {
    //读取的时要注意 host 或 ip，如果你使用 .Dialer 访问并回收连接，之后使用 .Get 读取连接。
    //要注意了，.Dialer 是以 hostname 作为 key 存储，而 .Get 读取是以 IP，所以可能、可能、可能会读不出接连。
    G1:
	select {
    case conn = <- cp.getConns(key):
        pool = true
        notifier, ok := conn.(vconn.CloseNotifier)
    	if ok {
    		select {
    		case <-notifier.CloseNotify():
       			atomic.AddInt32(&cp.connNum, -1)
    			goto G1
    		default:
    		}
    	}
    default:
        if !dial {
            //dial设置了不需创建新连接
            err = fmt.Errorf("vconnpool: 读取 %v，没有空闲的连接可以读取。", key)
            return
        }
       	conn, err = cp.dial(ctx, key.network, key.address)
        pool = false
    }
    return
}

//Get 从池中读取一条连接。
//读取出来的连接不会自动回收，如果你.Close() 是真的关闭连接，不是回收。
//需要在不关闭连接的状态下，需要调用 .Add(...) 收入
//而.Dial(...) 读取出来的连接，调用.Close() 之后，是自动收回的。.Get(...) 不是。
//	addr net.Addr   地址
//	conn net.Conn   连接
//	error           错误
func (cp *ConnPool) Get(addr net.Addr) (conn net.Conn, err error) {
	if cp.closed.isTrue() {
	    return nil, errorConnPoolClose
	}
    
	cp.init()
	key := connAddr{addr.Network(), addr.String()}
	conn, _, err = cp.getConn(context.Background(), key, false)
	if err == nil {
		atomic.AddInt32(&cp.connNum, -1)
	}
	return
}

//Add 增加一个连接到池中
//	addr net.Addr   存储位置Key名称
//	conn net.Conn   连接
//	error           错误
func (cp *ConnPool) Add(addr net.Addr, conn net.Conn) error {
    if cp.closed.isTrue() {
        //池被关闭了，由于是空闲连接，就关闭它。
        conn.Close()
        return errors.New("vconnpool: 连接池已经被关闭，当前连接也已经被关闭")
    }
    
    //如果是 *connSingle 类型则关闭，使用自动收回，不重复回收。
    if c, ok := conn.(*connSingle); ok {
    	return c.Close()
    }

    if cp.MaxConn != 0 && int(atomic.LoadInt32(&cp.connNum)) >= cp.MaxConn {
        conn.Close()
        return errors.New("vconnpool: 连接池中的连接数量已经达到最大限制，当前连接也已经被关闭")
    }
   	atomic.AddInt32(&cp.connNum, 1)
	
    cp.init()
    key := connAddr{addr.Network(), addr.String()}
    return cp.put(key, vconn.NewConn(conn))
}

//put 回收连接
func (cp *ConnPool) put(key connAddr, conn net.Conn) error {
    if cp.closed.isTrue() {
        return conn.Close()
     }

    select {
    case cp.getConns(key) <- conn:
    default:
       	atomic.AddInt32(&cp.connNum, -1)
        return conn.Close()
    }
	return nil
}

//ConnNum 当前可用连接数量
//	int     数量
func (cp *ConnPool) ConnNum() int {
    return int(atomic.LoadInt32(&cp.connNum))
}

//ConnNumIde    当前空闲连接数量
//	int     数量
func (cp *ConnPool) ConnNumIde(network, address string) int {
    if cp.closed.isTrue() {
        return 0
    }
    cp.init()
    key := connAddr{network, address}
    conns := cp.getConns(key)
    return len(conns)
}

//CloseIdleConnections 关闭空闲连接池
func (cp *ConnPool) CloseIdleConnections() {
    cp.m.Lock()
    defer cp.m.Unlock()
    cp.init()
     for k, conns := range cp.conns {
        GO:for {
         	select{
                case conn := <- conns:
                	atomic.AddInt32(&cp.connNum, -1)
                    conn.Close()
                default:
                   if cp.closed.isTrue() {
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
    if cp.closed.setTrue() {
        return nil
    }
    cp.CloseIdleConnections()
    return nil
}
