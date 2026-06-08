// Package vconnpool 提供高性能、可复用的网络连接池
package vconnpool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/456vv/vconn"
)

var (
	ErrConnClose         = errors.New("vconnpool: the connection is closed")
	ErrConnPoolClosed    = errors.New("vconnpool: the connection pool has been closed")
	ErrConnRAWRead       = errors.New("vconnpool: the original connection cannot be read repeatedly")
	ErrConnNotAvailable  = errors.New("vconnpool: no available connection in the pool")
	ErrConnPoolMax       = errors.New("vconnpool: the number of connections has reached the maximum limit")
	ErrConnIdleMax       = errors.New("vconnpool: the number of idle connections has reached the maximum")
	ErrConnAlreadyExists = errors.New("vconnpool: the connection already exists in the idle pool")
)

// Dialer 接口定义
type Dialer interface {
	Dial(network, address string) (net.Conn, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Conn 对外暴露的连接接口
type Conn interface {
	net.Conn
	Discard() error
	IsReuseConn() bool
	RawConn() net.Conn
}

// connSingle 连接包装
type connSingle struct {
	*vconn.Conn              // 嵌入包装连接
	mu          sync.RWMutex // 仅保护 Conn 字段在 Close/RawConn 时的状态切换
	cp          *Pool
	addr        net.Addr
	laddr       net.Addr
	raddr       net.Addr
	isPool      bool
	closed      atomic.Bool
	discard     atomic.Bool
	rawRead     atomic.Bool
	activeOps   atomic.Int32
}

// --- connSingle 方法实现 ---
func (t *connSingle) Write(b []byte) (n int, err error) {
	if t.closed.Load() {
		return 0, net.ErrClosed
	}
	// 注意：net.Conn 的 Read/Write 是线程安全的，不需要加锁
	// 锁仅用于防止在读写过程中 Conn 被 Close 清空
	t.mu.RLock()
	vc := t.Conn
	t.mu.RUnlock()

	if vc == nil {
		return 0, net.ErrClosed
	}

	t.activeOps.Add(1)
	defer t.activeOps.Add(-1)

	// Double check to handle race with Close
	if t.closed.Load() {
		return 0, net.ErrClosed
	}

	n, err = vc.Write(b)
	t.errDiscardConnect(err)
	return
}

func (t *connSingle) Read(b []byte) (n int, err error) {
	if t.closed.Load() {
		return 0, net.ErrClosed
	}

	t.mu.RLock()
	vc := t.Conn
	t.mu.RUnlock()

	if vc == nil {
		return 0, net.ErrClosed
	}

	t.activeOps.Add(1)
	defer t.activeOps.Add(-1)

	// Double check to handle race with Close
	if t.closed.Load() {
		return 0, net.ErrClosed
	}

	n, err = vc.Read(b)
	t.errDiscardConnect(err)
	return
}

func (t *connSingle) errDiscardConnect(err error) {
	if err == nil {
		return
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return
	}
	t.discard.Store(true)
}

func (t *connSingle) Close() error {
	if t.closed.Swap(true) {
		return nil // 幂等关闭
	}

	// 原始连接已交出，池不再管理
	if t.rawRead.Load() {
		return nil
	}

	// 如果关闭时有并发的读写操作，必须强行关闭并丢弃连接
	if t.activeOps.Load() > 0 {
		t.discard.Store(true)
	}

	t.mu.Lock()
	vc := t.Conn
	cp := t.cp
	addr := t.addr
	t.Conn = nil // 清空引用，防止内存泄漏
	t.cp = nil
	t.mu.Unlock()

	if vc == nil {
		return nil
	}

	// 读取原连接
	conn := vc.RawConn()

	// 尝试归还连接池
	if !t.discard.Load() && cp != nil {
		err := cp.putPoolConn(conn, addr)
		if err == nil || errors.Is(err, ErrConnAlreadyExists) {
			return nil
		}
		// 若因重复、池满等原因放回失败，则执行物理关闭
	}

	// 物理关闭
	if cp != nil {
		cp.connNum.Add(-1)
		cp.loadPool(addr).totalCount.Add(-1)
	}
	return conn.Close()
}

func (t *connSingle) LocalAddr() net.Addr  { return t.laddr }
func (t *connSingle) RemoteAddr() net.Addr { return t.raddr }

func (t *connSingle) SetDeadline(tm time.Time) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.Conn == nil {
		return net.ErrClosed
	}
	return t.Conn.SetDeadline(tm)
}

func (t *connSingle) SetReadDeadline(tm time.Time) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.Conn == nil {
		return net.ErrClosed
	}
	return t.Conn.SetReadDeadline(tm)
}

func (t *connSingle) SetWriteDeadline(tm time.Time) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.Conn == nil {
		return net.ErrClosed
	}
	return t.Conn.SetWriteDeadline(tm)
}

func (t *connSingle) Discard() error {
	t.discard.Store(true)
	return nil
}

func (t *connSingle) IsReuseConn() bool {
	return t.isPool
}

func (t *connSingle) RawConn() net.Conn {
	if t.rawRead.Swap(true) {
		panic(ErrConnRAWRead)
	}
	if t.closed.Swap(true) {
		panic(net.ErrClosed)
	}

	t.mu.Lock()
	vc := t.Conn
	cp := t.cp
	addr := t.addr
	t.Conn = nil
	t.cp = nil
	t.mu.Unlock()

	if cp != nil {
		cp.connNum.Add(-1)
		cp.loadPool(addr).totalCount.Add(-1)
	}
	return vc.RawConn()
}

// --- 内部池管理 ---

type idleConn struct {
	conn net.Conn
	vc   *vconn.Conn
	pool *pools
}

func (ic *idleConn) wait(timeout time.Duration) {
	var timeoutC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	select {
	case <-ic.vc.CloseNotify():
		// 连接被对端断开或底层异常
	case <-timeoutC:
		// 超时
	}

	ic.pool.remove(ic)
}

type pools struct {
	cp             *Pool
	mu             sync.Mutex
	idle           []*idleConn
	present        map[net.Conn]struct{} // 用于 O(1) 查重
	connExhausted  map[int][]chan struct{}
	connAvailabled map[int][]chan struct{}
	totalCount     atomic.Int32 // 该地址当前持有的总连接数（包含空闲和在用连接）
}

func (p *pools) put(conn net.Conn, timeout time.Duration) error {
	p.mu.Lock()

	if p.cp.closed.Load() {
		p.mu.Unlock()
		return ErrConnPoolClosed
	}

	if p.present == nil {
		p.present = make(map[net.Conn]struct{})
	}

	if _, ok := p.present[conn]; ok {
		p.mu.Unlock()
		return ErrConnAlreadyExists
	}

	if p.cp.IdleConn > 0 && len(p.idle) >= p.cp.IdleConn {
		p.mu.Unlock()
		return ErrConnIdleMax
	}

	vc := vconn.New(conn)
	vc.SetBackgroundReadDiscard(true)

	ic := &idleConn{conn: conn, vc: vc, pool: p}
	p.idle = append(p.idle, ic)
	p.present[conn] = struct{}{}
	l := len(p.idle)
	p.notifyConnAvailabled(l)
	p.mu.Unlock()

	go ic.wait(timeout)
	return nil
}

func (p *pools) get() (net.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for len(p.idle) > 0 {
		n := len(p.idle) - 1
		ic := p.idle[n]
		p.idle[n] = nil
		p.idle = p.idle[:n]
		delete(p.present, ic.conn)
		p.checkConnExhausted(n)

		// 检查连接是否依然健康
		if !ic.vc.CancelNotify(vconn.ErrRawConnAlreadyUsed) {
			ic.conn.Close()
			p.cp.connNum.Add(-1)
			p.totalCount.Add(-1)
			continue
		}
		// 连接健康，返回给调用者
		return ic.conn, nil
	}
	return nil, ErrConnNotAvailable
}

func (p *pools) remove(ic *idleConn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.present == nil {
		return
	}
	if _, ok := p.present[ic.conn]; !ok {
		return
	}

	// 快速删除
	for i, v := range p.idle {
		if v == ic {
			p.idle[i] = p.idle[len(p.idle)-1]
			p.idle[len(p.idle)-1] = nil
			p.idle = p.idle[:len(p.idle)-1]
			delete(p.present, ic.conn)

			ic.conn.Close()
			p.cp.connNum.Add(-1)
			p.totalCount.Add(-1)
			break
		}
	}
	p.checkConnExhausted(len(p.idle))
}

func (p *pools) waitConnAvailabled(l int) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := make(chan struct{}, 1)
	if len(p.idle) >= l || p.cp.closed.Load() {
		if !p.cp.closed.Load() {
			ch <- struct{}{}
		}
		close(ch)
		return ch
	}

	p.connAvailabled[l] = append(p.connAvailabled[l], ch)
	return ch
}

func (p *pools) cleanNotifyConnAvailabled() {
	for l := range p.connAvailabled {
		p.notifyConnAvailabled(l)
	}
}

func (p *pools) notifyConnAvailabled(l int) {
	for _, ch := range p.connAvailabled[l] {
		if !p.cp.closed.Load() {
			ch <- struct{}{}
		}
		close(ch)
	}
	delete(p.connAvailabled, l)
}

func (p *pools) waitConnExhausted(l int) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := make(chan struct{}, 1)

	if len(p.idle) <= l || p.cp.closed.Load() {
		if !p.cp.closed.Load() {
			ch <- struct{}{}
		}
		close(ch)
		return ch
	}

	p.connExhausted[l] = append(p.connExhausted[l], ch)
	return ch
}

func (p *pools) checkConnExhausted(l int) {
	for _, ch := range p.connExhausted[l] {
		if !p.cp.closed.Load() {
			ch <- struct{}{}
		}
		close(ch)
	}
	delete(p.connExhausted, l)
}

func (p *pools) cleanCheckConnExhausted() {
	for l := range p.connExhausted {
		p.checkConnExhausted(l)
	}
}

func (p *pools) clean() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, ic := range p.idle {
		ic.vc.CancelNotify(net.ErrClosed)
		ic.conn.Close()
	}

	p.cp.connNum.Add(-int32(len(p.idle)))
	p.totalCount.Add(-int32(len(p.idle)))

	// 如果是池关闭了，关闭接收者
	if p.cp.closed.Load() {
		p.cleanNotifyConnAvailabled()
	}

	// 告诉接收者，连接全部关闭
	p.cleanCheckConnExhausted()

	p.idle = nil
	p.present = nil
}

// --- ConnPool 主体 ---

var defaultDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

type Pool struct {
	Dialer      Dialer
	ResolveAddr func(network, address string) (net.Addr, error)
	IdleConn    int
	IdleTimeout time.Duration
	MaxConn     int

	connNum atomic.Int32
	conns   map[string]*pools // 采用标准 Map + 细粒度 RWMutex 优化并发读写性能
	closed  atomic.Bool
	mu      sync.RWMutex
}

func addrKey(network, address string) string {
	return network + "," + address
}

func (p *Pool) loadPool(addr net.Addr) *pools {
	key := addrKey(addr.Network(), addr.String())
	p.mu.RLock()
	ps, ok := p.conns[key]
	p.mu.RUnlock()
	if ok {
		return ps
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		p.conns = make(map[string]*pools)
	}
	ps, ok = p.conns[key]
	if !ok {
		ps = &pools{
			cp:             p,
			present:        make(map[net.Conn]struct{}),
			connExhausted:  make(map[int][]chan struct{}),
			connAvailabled: make(map[int][]chan struct{}),
		}
		p.conns[key] = ps
	}
	return ps
}

func (p *Pool) WaitConnAvailabled(addr net.Addr, l int) <-chan struct{} {
	return p.loadPool(addr).waitConnAvailabled(l)
}

func (p *Pool) WaitConnExhausted(addr net.Addr, l int) <-chan struct{} {
	return p.loadPool(addr).waitConnExhausted(l)
}

func (p *Pool) getPoolConn(network, address string) (net.Conn, error) {
	key := addrKey(network, address)
	p.mu.RLock()
	ps, ok := p.conns[key]
	p.mu.RUnlock()
	if !ok {
		return nil, ErrConnNotAvailable
	}

	conn, err := ps.get()
	// 动态清理不再使用的 pool 结构体，保证安全且防止泄露
	if err != nil {
		p.mu.Lock()
		ps.mu.Lock()
		if len(ps.idle) == 0 && len(ps.connAvailabled) == 0 && len(ps.connExhausted) == 0 {
			if p.conns[key] == ps {
				delete(p.conns, key)
			}
		}
		ps.mu.Unlock()
		p.mu.Unlock()
	}
	return conn, err
}

func (p *Pool) getPoolConnCount(network, address string) int {
	key := addrKey(network, address)
	p.mu.RLock()
	ps, ok := p.conns[key]
	p.mu.RUnlock()
	if !ok {
		return 0
	}

	return int(ps.totalCount.Load())
}

func (p *Pool) getIdleConnCount(network, address string) int {
	key := addrKey(network, address)
	p.mu.RLock()
	ps, ok := p.conns[key]
	p.mu.RUnlock()
	if !ok {
		return 0
	}

	ps.mu.Lock()
	count := len(ps.idle)
	ps.mu.Unlock()
	return count
}

func (p *Pool) putPoolConn(conn net.Conn, addr net.Addr) error {
	if conn == nil || addr == nil {
		return errors.New("vconnpool: nil conn or addr")
	}

	if p.closed.Load() {
		return ErrConnPoolClosed
	}

	return p.loadPool(addr).put(conn, p.IdleTimeout)
}

func (p *Pool) checkAndIncConnNum() error {
	if p.MaxConn <= 0 {
		p.connNum.Add(1)
		return nil
	}
	for {
		current := p.connNum.Load()
		if int(current) >= p.MaxConn {
			return ErrConnPoolMax
		}
		if p.connNum.CompareAndSwap(current, current+1) {
			return nil
		}
	}
}

func (p *Pool) Dial(network, address string) (Conn, error) {
	return p.DialContext(context.Background(), network, address)
}

func (p *Pool) DialContext(ctx context.Context, network, address string) (Conn, error) {
	if p.closed.Load() {
		return nil, ErrConnPoolClosed
	}

	var (
		conn net.Conn
		err  error
		pool bool
	)

	// 检查优先级模式
	isPriority, exist := ctx.Value(PriorityContextKey).(bool)

	if !isPriority {
		conn, err = p.getPoolConn(network, address)
		if err == nil {
			if ctx.Err() != nil {
				if err := p.putPoolConn(conn, &Addr{Net: network, Name: address}); err != nil {
					if !errors.Is(err, ErrConnAlreadyExists) {
						conn.Close()
					}
				}
				return nil, ctx.Err()
			}
			pool = true
		}
	}

	// 设置优先级模式，创建连接。
	if exist == isPriority && conn == nil {
		// 池中无连接或强制新建
		conn, err = p.dialNew(ctx, network, address)
	}
	if err != nil {
		return nil, err
	}

	return &connSingle{
		Conn:   vconn.New(conn),
		cp:     p,
		isPool: pool,
		addr:   &Addr{Net: network, Name: address},
		laddr:  conn.LocalAddr(),
		raddr:  conn.RemoteAddr(),
	}, nil
}

func (p *Pool) dialNew(ctx context.Context, network, address string) (net.Conn, error) {
	// 1. 检查并预占配额
	if err := p.checkAndIncConnNum(); err != nil {
		return nil, err
	}

	// 2. 解析地址（支持 Context）
	var addr net.Addr
	var err error
	if p.ResolveAddr != nil {
		addr, err = p.ResolveAddr(network, address)
	} else {
		// 默认解析不支持 Context，这里做一层简单的封装
		addr, err = ResolveAddr(network, address)
	}

	if err != nil {
		p.connNum.Add(-1)
		return nil, err
	}

	// 3. 执行物理拨号
	dialer := p.Dialer
	if dialer == nil {
		dialer = defaultDialer
	}

	conn, err := dialer.DialContext(ctx, network, addr.String())
	if err != nil {
		p.connNum.Add(-1)
		return nil, err
	}

	p.loadPool(addr).totalCount.Add(1)
	return conn, nil
}

// Get 从连接池获取指定地址的连接（不创建新连接）
func (p *Pool) Get(addr net.Addr) (conn net.Conn, err error) {
	if p.closed.Load() {
		return nil, ErrConnPoolClosed
	}

	if addr == nil {
		return nil, errors.New("vconnpool: address cannot be nil")
	}

	conn, err = p.getPoolConn(addr.Network(), addr.String())
	if err != nil {
		return nil, err
	}

	// 连接所有权转移给调用者，减少计数
	p.connNum.Add(-1)
	p.loadPool(addr).totalCount.Add(-1)
	return conn, nil
}

func (p *Pool) Add(conn net.Conn) error {
	if conn == nil {
		return errors.New("vconnpool: cannot add nil connection")
	}
	return p.Put(conn, conn.RemoteAddr())
}

func (p *Pool) Put(conn net.Conn, addr net.Addr) error {
	if p.closed.Load() {
		return ErrConnPoolClosed
	}
	if conn == nil || addr == nil {
		return errors.New("vconnpool: nil parameters")
	}

	// 如果是包装连接，走自身的回收逻辑
	if cs, ok := conn.(*connSingle); ok {
		return cs.Close()
	}

	if err := p.checkAndIncConnNum(); err != nil {
		return err
	}

	if vc, ok := conn.(*vconn.Conn); ok {
		conn = vc.RawConn()
	}

	ps := p.loadPool(addr)
	ps.totalCount.Add(1)

	if err := p.putPoolConn(conn, addr); err != nil {
		p.connNum.Add(-1)
		ps.totalCount.Add(-1)
		if errors.Is(err, ErrConnAlreadyExists) {
			return nil
		}
		return err
	}
	return nil
}

// CloseIdleConnection 关闭空闲连接池
func (p *Pool) CloseIdleConnection(addr net.Addr) {
	key := addrKey(addr.Network(), addr.String())
	p.mu.RLock()
	ps, ok := p.conns[key]
	p.mu.RUnlock()
	if ok {
		ps.clean()
	}
}

func (p *Pool) CloseIdleConnections() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, ps := range p.conns {
		ps.clean()
	}
}

func (p *Pool) Close() error {
	if p.closed.Swap(true) {
		return nil
	}
	p.CloseIdleConnections()
	p.connNum.Store(0)
	return nil
}

func (p *Pool) Num() int {
	return int(p.connNum.Load())
}

// NumIdle 当前空闲连接数量
func (p *Pool) NumIdle(addr net.Addr) int {
	if p.closed.Load() {
		return 0
	}

	return p.getIdleConnCount(addr.Network(), addr.String())
}

// NumConn 当前地址连接数量
func (p *Pool) NumConn(addr net.Addr) int {
	if p.closed.Load() {
		return 0
	}

	return p.getPoolConnCount(addr.Network(), addr.String())
}

func ResolveAddr(network, address string) (net.Addr, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return net.ResolveTCPAddr(network, address)
	case "udp", "udp4", "udp6":
		return net.ResolveUDPAddr(network, address)
	case "unix", "unixgram", "unixpacket":
		return net.ResolveUnixAddr(network, address)
	case "ip", "ip4", "ip6":
		return net.ResolveIPAddr(network, address)
	default:
		return nil, fmt.Errorf("vconnpool: unsupported network %s", network)
	}
}

type Addr struct {
	Name string
	Net  string
}

func (a *Addr) Network() string { return a.Net }
func (a *Addr) String() string  { return a.Name }

// contextKey 用于在 context 中存储连接池相关的值
type contextKey struct{ name string }

func (p *contextKey) String() string { return "connpool context value " + p.name }

// PriorityContextKey 上下文键，用于标记是否优先创建新连接（不使用连接池）
var PriorityContextKey = &contextKey{"priority"}
