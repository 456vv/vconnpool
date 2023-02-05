package vconnpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/456vv/vconn"
)

var (
	errorConnClose     = errors.New("vconnpool: the connection is closed")
	errorConnPoolClose = errors.New("vconnpool: the connection pool has been closed")
	ErrConnPoolMax     = errors.New("vconnpool: the number of connections in the connection pool has reached the maximum limit")
	errorConnRAWRead   = errors.New("vconnpool: the original connection cannot be read repeatedly")

	ErrConnNotAvailable = errors.New("vconnpool: no connections available in the pool")
	ErrPoolFull         = errors.New("vconnpool: the number of idle connections has reached the maximum")
)

type atomicBool int32

func (T *atomicBool) isTrue() bool   { return atomic.LoadInt32((*int32)(T)) != 0 }
func (T *atomicBool) isFalse() bool  { return atomic.LoadInt32((*int32)(T)) != 1 }
func (T *atomicBool) setTrue() bool  { return !atomic.CompareAndSwapInt32((*int32)(T), 0, 1) }
func (T *atomicBool) setFalse() bool { return !atomic.CompareAndSwapInt32((*int32)(T), 1, 0) }

// Dialer 是 net.Dialer 接口
type Dialer interface {
	Dial(network, address string) (net.Conn, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Conn 连接接口，包含了 net.Conn
type Conn interface {
	net.Conn           // 连接
	Discard() error    // 废弃（这条连接不再回收）
	IsReuseConn() bool // 判断这条连接是否是从池中读取出来的
	RawConn() net.Conn // 原始连接，这个连接使用 Close 关闭后，不会回收
}

// connSingle 单连接
type connSingle struct {
	net.Conn            // 连接
	addr     net.Addr   // 地址用于回收识别
	cp       *ConnPool  // 池
	isPool   bool       // 连接来源，判断连接是不是从池里读出来的
	closed   atomicBool // 连接关闭了
	discard  atomicBool // 废弃（这条连接不再回收）
	rawRead  atomicBool
}

// Write 写入
//
//	b []byte    写入字节
//	n int       成功写入字节的长度
//	err error   错误，超时或暂时性的错误
func (T *connSingle) Write(b []byte) (n int, err error) {
	if T.closed.isTrue() {
		return 0, io.EOF
	}
	n, err = T.Conn.Write(b)
	if ne, ok := err.(net.Error); ok && !ne.Timeout() {
		T.discard.setTrue()
	}
	return
}

// Read 读取
//
//	b []byte    字节写入到b
//	n int       成功读取字节的长度
//	err error   错误，超时或暂时性的错误
func (T *connSingle) Read(b []byte) (n int, err error) {
	if T.closed.isTrue() {
		return 0, io.EOF
	}
	n, err = T.Conn.Read(b)
	if ne, ok := err.(net.Error); ok && !ne.Timeout() {
		T.discard.setTrue()
	}
	return
}

// Close 关闭连接
//
//	error          错误
func (T *connSingle) Close() error {
	if T.rawRead.isTrue() {
		return nil
	}
	if T.closed.setTrue() {
		return errorConnClose
	}

	notifier, ok := T.Conn.(vconn.CloseNotifier)
	if ok && T.discard.isFalse() {
		select {
		case <-notifier.CloseNotify():
			// 连接已经关闭
		default:
			if err := T.cp.putPoolConn(T.Conn, T.addr); err == nil {
				// 回收成功
				return nil
			}
		}
	}
	atomic.AddInt32(&T.cp.connNum, -1)
	return T.Conn.Close()
}

// LocalAddr 返回本地网络地址
func (T *connSingle) LocalAddr() net.Addr {
	return T.Conn.LocalAddr()
}

// RemoteAddr 返回远端网络地址
func (T *connSingle) RemoteAddr() net.Addr {
	return T.Conn.RemoteAddr()
}

// SetDeadline 设置读写超时时间
func (T *connSingle) SetDeadline(t time.Time) error {
	if T.closed.isTrue() {
		return errorConnClose
	}
	return T.Conn.SetDeadline(t)
}

// SetReadDeadline 设置读取超时时间
func (T *connSingle) SetReadDeadline(t time.Time) error {
	if T.closed.isTrue() {
		return errorConnClose
	}
	return T.Conn.SetReadDeadline(t)
}

// SetWriteDeadline 设置写入超时时间
func (T *connSingle) SetWriteDeadline(t time.Time) error {
	if T.closed.isTrue() {
		return errorConnClose
	}
	return T.Conn.SetWriteDeadline(t)
}

// Discard 废弃（这条连接不再回收）
func (T *connSingle) Discard() error {
	T.discard.setTrue()
	return nil
}

// IsReuseConn 是否是重用连接
func (T *connSingle) IsReuseConn() bool {
	return T.isPool
}

func (T *connSingle) RawConn() net.Conn {
	if T.rawRead.setTrue() {
		panic(errorConnRAWRead)
	}
	if T.closed.setTrue() {
		panic(errorConnClose)
	}

	atomic.AddInt32(&T.cp.connNum, -1)
	conn := T.Conn
	T.Conn = nil
	T.cp = nil
	T.addr = nil
	if conn, ok := conn.(*vconn.Conn); ok {
		return conn.RawConn()
	}
	return conn
}

type connMan struct {
	pools       *pools
	conn        net.Conn
	ctx         context.Context
	ctxCancel   context.CancelFunc
	unavailable atomicBool // 不可用
	readyed     chan struct{}
}

func (T *connMan) notifyYield() {
	defer T.ctxCancel()
	T.readyed <- struct{}{}
	notify, ok := T.conn.(vconn.CloseNotifier)
	if ok {
		select {
		case <-notify.CloseNotify():
		case <-T.ctx.Done():
		}
	} else {
		<-T.ctx.Done()
	}
	//有三种行为：
	//1，用户取消
	//2，空闲超时
	//3，连接关闭
	//
	//可能：
	//1
	//设置 unavailable 为true，表示不可用状态。这样就不会在 get 中读取出来
	//由于在多线程里面，要保证用户没有"正在" get 读取该连接
	//2
	//如果用户已经 get 读出，并设置 unavailable 为 true
	//这里再次设置 unavailable 为 true，相同的值返回true
	//!true 等于 false, 跳过

	if !T.unavailable.setTrue() {
		T.conn.Close()
		// 减少连接总数量
		atomic.AddInt32(&T.pools.cp.connNum, -1)
	}
	T.pools.yield(T.conn)
}

type pools struct {
	occupy    map[net.Conn]int // 占用的位置
	vacancy   map[int]struct{} // 空缺的位置
	conns     []*connMan       // 存放的列表
	connsSize int              // 增长的位置
	mu        sync.Mutex
	cp        *ConnPool
	ctx       context.Context
	ctxCancel context.CancelFunc
}

func connClosed(conn net.Conn) bool {
	if notify, ok := conn.(vconn.CloseNotifier); ok {
		select {
		case <-notify.CloseNotify():
			return true
		default:
		}
	}
	return false
}

// 连接被读出后，该连接应该用池中让位。
func (T *pools) yield(conn net.Conn) {
	T.mu.Lock()
	defer T.mu.Unlock()

	if pos, ok := T.occupy[conn]; ok {
		delete(T.occupy, conn)

		T.conns[pos] = nil
		T.vacancy[pos] = struct{}{}
	}
}

func (T *pools) put(conn net.Conn, idleTImeout time.Duration) error {
	T.mu.Lock()
	defer T.mu.Unlock()

	// 加入连接池之前，先判断该连接是否已经关闭
	if connClosed(conn) {
		return errorConnClose
	}

	// 重复回收跳过
	for {
		if pos, ok := T.occupy[conn]; ok {
			cm := T.conns[pos]
			if cm.unavailable.isTrue() {
				// 连接不可用，等待让位中....
				T.mu.Unlock()
				T.mu.Lock()
				continue
			}
			// 连接已经存在
			return nil
		}
		break
	}

	cm := &connMan{
		pools:   T,
		conn:    conn,
		readyed: make(chan struct{}),
	}

	// 上下文
	if T.ctx == nil {
		T.ctx, T.ctxCancel = context.WithCancel(context.Background())
	}
	if idleTImeout != 0 {
		// 负责处理空闲超时
		cm.ctx, cm.ctxCancel = context.WithTimeout(T.ctx, idleTImeout)
	} else {
		// 负责处理读出取消
		cm.ctx, cm.ctxCancel = context.WithCancel(T.ctx)
	}

	// 在空缺位置安放
	for pos := range T.vacancy {

		delete(T.vacancy, pos)

		// 超出最大的空闲连接
		if T.cp.IdeConn != 0 && pos >= T.cp.IdeConn {
			continue
		}

		T.conns[pos] = cm
		T.occupy[conn] = pos
		go cm.notifyYield()
		<-cm.readyed
		return nil
	}

	// 池中的连接等于或超出最大限制连接
	if T.cp.IdeConn != 0 && T.connsSize >= T.cp.IdeConn {
		return ErrPoolFull
	}

	// 正常收回
	T.conns = append(T.conns, cm)
	T.occupy[conn] = T.connsSize
	T.connsSize++
	go cm.notifyYield()
	<-cm.readyed
	return nil
}

func (T *pools) get() (conn net.Conn, err error) {
	T.mu.Lock()
	defer T.mu.Unlock()
	for _, pos := range T.occupy {
		connMan := T.conns[pos]
		if connMan.unavailable.setTrue() {
			//1，读取出来后设置该连接为不可用
			//2，该连接已经失效
			//
			//调用上下文取消，notifyYield() 是在另一个线程里面。
			//并没有及时调用 yield 方法从 occupy 里删除
			//若在再次调用 get 读取，可能会读取到相同的连接。
			continue
		}
		connMan.ctxCancel()
		return connMan.conn, nil
	}
	return nil, ErrConnNotAvailable
}

func (T *pools) length() int {
	T.mu.Lock()
	defer T.mu.Unlock()
	return len(T.occupy)
}

func (T *pools) clear() {
	if T.ctxCancel != nil {
		T.ctxCancel()
	}
}

func ResolveAddr(network, address string) (net.Addr, error) {
	switch network {
	case "tcp":
		return net.ResolveTCPAddr(network, address)
	case "udp":
		return net.ResolveUDPAddr(network, address)
	case "ip":
		return net.ResolveIPAddr(network, address)
	case "unix":
		return net.ResolveUnixAddr(network, address)
	}
	return nil, fmt.Errorf("the network type %s not support", network)
}

func parseKey(network, address string) string {
	return network + "," + address
}

// ConnPool 连接池
type ConnPool struct {
	Dialer                                                      // 拨号
	ResolveAddr func(network, address string) (net.Addr, error) // 拨号地址变更
	IdeConn     int                                             // 空闲连接数，0为不支持连接入池
	IdeTimeout  time.Duration                                   // 空闲自动超时，0为不超时
	MaxConn     int                                             // 最大连接数，0为无限制连接
	connNum     int32                                           // 当前连接数
	conns       map[string]*pools                               // 连接集
	m           sync.Mutex                                      // 锁
	closed      atomicBool                                      // 关闭池
	inited      atomicBool                                      // 初始化
	pool        sync.Pool                                       // 临时存在，存在空闲的池对象
}

func (T *ConnPool) init() {
	if T.inited.setTrue() {
		return
	}
	if T.conns == nil {
		T.conns = make(map[string]*pools)
	}
	if T.Dialer == nil {
		T.Dialer = new(net.Dialer)
	}
}

func (T *ConnPool) getPoolConn(network, address string) (conn net.Conn, err error) {
	T.m.Lock()
	defer T.m.Unlock()
	T.init()

	key := parseKey(network, address)
	ps, ok := T.conns[key]
	if !ok {
		return nil, ErrConnNotAvailable
	}
	conn, err = ps.get()
	if err != nil {
		// 池中没有空闲连接，删除该池
		delete(T.conns, key)
		T.pool.Put(ps)
	}
	return
}

func (T *ConnPool) putPoolConn(conn net.Conn, addr net.Addr) error {
	// 空闲连接限制
	if T.IdeConn == 0 {
		return ErrPoolFull
	}

	T.m.Lock()
	defer T.m.Unlock()
	T.init()

	key := parseKey(addr.Network(), addr.String())
	ps, ok := T.conns[key]
	if !ok {
		if inf := T.pool.Get(); inf != nil {
			ps = inf.(*pools)
		} else {
			ps = &pools{
				cp:      T,
				occupy:  make(map[net.Conn]int),         // 占据位置
				vacancy: make(map[int]struct{}),         // 空缺位置
				conns:   make([]*connMan, 0, T.IdeConn), // 存在
			}
		}
		T.conns[key] = ps
	}
	return ps.put(conn, T.IdeTimeout)
}

func (T *ConnPool) getPoolConnCount(network, address string) int {
	T.m.Lock()
	defer T.m.Unlock()

	key := parseKey(network, address)
	pools, ok := T.conns[key]
	if !ok {
		return 0
	}
	return pools.length()
}

func (T *ConnPool) clearPoolConn() {
	T.m.Lock()
	defer T.m.Unlock()
	for key, pools := range T.conns {
		pools.clear()
		delete(T.conns, key)
		T.pool.Put(pools)
	}
}

func (T *ConnPool) parseAddr(network, address string) (net.Addr, error) {
	if T.ResolveAddr != nil {
		return T.ResolveAddr(network, address)
	}
	return ResolveAddr(network, address)
}

// Dial 见 DialContext
//
//	network string      连接类型
//	address string      连接地址
//	net.Conn            连接
//	error               错误
func (T *ConnPool) Dial(network, address string) (net.Conn, error) {
	return T.DialContext(context.Background(), network, address)
}

// DialContext 拨号，如果ctx 携带键值是（priority=true）,是创建新连接，否则从池中读取。
// 注意：远程地址支持 host 或 ip，一个 host 会有多个 ip 地址，所以无法用 host 的 ip 做为存储地址。
// DialContext 支持 hsot 和 ip 读取或创建连接。 而.Get 仅支持 ip 读取池中连接。
// DialContext 创建的连接，调用 Close 关闭后，自动收回。
//
//	ctx context.Context 上下文
//	network string      连接类型
//	address string      连接地址
//	net.Conn            连接
//	error               错误
func (T *ConnPool) DialContext(ctx context.Context, network, address string) (conn net.Conn, err error) {
	if T.closed.isTrue() {
		return nil, errorConnPoolClose
	}

	addr, err := T.parseAddr(network, address)
	if err != nil {
		return nil, err
	}

	T.init()

	var pool bool
	priority, ok := ctx.Value(PriorityContextKey).(bool)
	if ok && priority {
		// 新建拨号
		conn, err = T.dialCtx(ctx, network, addr.String())
	} else {
		// 读取不存在，新建拨号
		conn, pool, err = T.getConn(ctx, network, addr.String())
	}
	if err != nil {
		return nil, err
	}
	return &connSingle{Conn: conn, cp: T, isPool: pool, addr: addr}, nil
}

func (T *ConnPool) dialCtx(ctx context.Context, network, address string) (conn net.Conn, err error) {
	if T.MaxConn != 0 && int(atomic.LoadInt32(&T.connNum)) >= T.MaxConn {
		return nil, ErrConnPoolMax
	}

	conn, err = T.Dialer.DialContext(ctx, network, address)
	if err != nil {
		return
	}

	// 支持多线程拨号，防止网络阻塞，无法继续创建
	// 再次判断连接数是否已经超出
	if int(atomic.AddInt32(&T.connNum, 1)) > T.MaxConn && T.MaxConn != 0 { // 注意：判断位置不要交换
		atomic.AddInt32(&T.connNum, -1)
		conn.Close()
		return nil, ErrConnPoolMax
	}

	return vconn.New(conn), nil
}

func (T *ConnPool) getConn(ctx context.Context, network, address string) (conn net.Conn, pool bool, err error) {
	if T.getPoolConnCount(network, address) > 0 {
		if conn, err = T.getPoolConn(network, address); err == nil {
			pool = true
			return
		}
	}
	conn, err = T.dialCtx(ctx, network, address)
	return
}

// Get 从池中读取一条连接。读取出来的连接不会自动回收，如果你.Close() 是真的关闭连接，不是回收。
//
//	addr net.Addr   地址，为远程地址RemoteAddr
//	error           错误
func (T *ConnPool) Get(addr net.Addr) (conn net.Conn, err error) {
	if T.closed.isTrue() {
		return nil, errorConnPoolClose
	}

	conn, err = T.getPoolConn(addr.Network(), addr.String())
	if err != nil {
		return nil, err
	}
	atomic.AddInt32(&T.connNum, -1)
	if conn, ok := conn.(*vconn.Conn); ok {
		return conn.RawConn(), nil
	}
	return conn, nil
}

// Add 增加一个连接到池中，适用于 Dial 的连接。默认使用 RemoteAddr 作为 key 存放在池中。
// 如果你的需求特殊的，请使用 Put 方法。
//
//	conn net.Conn   连接
//	error           错误
func (T *ConnPool) Add(conn net.Conn) error {
	return T.Put(vconn.New(conn), conn.RemoteAddr())
}

// Put 增加一个连接到池中，适用于 Dial 和 listen 的连接。
// Dial 连接使用RemoteAddr，listen 连接使用LocalAddr 为做 addr
//
//	conn net.Conn   连接
//	addr net.Addr	地址，作为池的 key 存放
//	error           错误
func (T *ConnPool) Put(conn net.Conn, addr net.Addr) error {
	if T.closed.isTrue() {
		return errorConnPoolClose
	}

	if T.MaxConn != 0 && int(atomic.LoadInt32(&T.connNum)) >= T.MaxConn {
		return ErrConnPoolMax
	}

	// 如果是 *connSingle 类型则关闭，使用自动收回，不重复回收。
	if c, ok := conn.(*connSingle); ok {
		return c.Close()
	}

	atomic.AddInt32(&T.connNum, 1)
	if err := T.putPoolConn(conn, addr); err != nil {
		atomic.AddInt32(&T.connNum, -1)
		return err
	}
	return nil
}

// ConnNum 当前可用连接数量
//
//	int     数量
func (T *ConnPool) ConnNum() int {
	if T.closed.isTrue() {
		return 0
	}
	return int(atomic.LoadInt32(&T.connNum))
}

// ConnNumIde 当前空闲连接数量。这不是实时的空闲连接数量。
// 入池后读取，得到真实数量。出池后读取，得到的不真实数量，因为存在多线程处理。
//
//	int     数量
func (T *ConnPool) ConnNumIde(network, address string) int {
	if T.closed.isTrue() {
		return 0
	}
	addr, err := T.parseAddr(network, address)
	if err != nil {
		return 0
	}
	return T.getPoolConnCount(network, addr.String())
}

// CloseIdleConnections 关闭空闲连接池
func (T *ConnPool) CloseIdleConnections() {
	T.clearPoolConn()
}

// Close 关闭连接池
func (cp *ConnPool) Close() error {
	if cp.closed.setTrue() {
		return nil
	}
	cp.CloseIdleConnections()
	return nil
}

// 上下文的Key，在请求中可以使用
type contextKey struct {
	name string
}

func (T *contextKey) String() string { return "connpool context value " + T.name }

var PriorityContextKey = &contextKey{"priority"}
