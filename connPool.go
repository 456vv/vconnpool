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
	ErrConnRAWRead       = errors.New("vconnpool: the original connection cannot be read repeatedly or is closed") // 错误信息更明确
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
	Discard()
	IsReuseConn() bool
	RawConn() net.Conn // 修正签名以返回错误
}

// connSingle 连接包装
type connSingle struct {
	mu           sync.RWMutex // 仅保护 Conn 字段在 Close/RawConn 时的状态切换
	Conn         *vconn.Conn  // 嵌入包装连接
	cp           *Pool
	addr         net.Addr
	laddr        net.Addr
	raddr        net.Addr
	isPool       bool
	closed       atomic.Bool // true表示这个connSingle包装器已失效
	discard      atomic.Bool // true表示底层连接不应被复用
	rawConnMoved atomic.Bool // true表示原始net.Conn已通过RawConn()方法移交
	activeOps    atomic.Int32
}

// --- connSingle 方法实现 ---
func (t *connSingle) Write(b []byte) (n int, err error) {
	if t.closed.Load() {
		return 0, net.ErrClosed
	}

	t.activeOps.Add(1)
	defer t.activeOps.Add(-1) // 在函数退出时递减计数

	t.mu.RLock()
	vc := t.Conn
	t.mu.RUnlock()

	// Double check to handle race with Close/RawConn that clears t.Conn
	if vc == nil || t.closed.Load() {
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

	t.activeOps.Add(1)
	defer t.activeOps.Add(-1) // 在函数退出时递减计数

	t.mu.RLock()
	vc := t.Conn
	t.mu.RUnlock()

	// Double check to handle race with Close/RawConn that clears t.Conn
	if vc == nil || t.closed.Load() {
		return 0, net.ErrClosed
	}

	n, err = vc.Read(b)
	t.errDiscardConnect(err)
	return
}

func (t *connSingle) errDiscardConnect(err error) {
	if err != nil {
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.discard.Store(true)
		}
	}
}

func (t *connSingle) Close() error {
	if t.closed.Swap(true) {
		return nil // 幂等关闭
	}

	// 如果原始连接已通过RawConn()移交，则connSingle不再对其负责
	if t.rawConnMoved.Load() {
		return nil
	}

	// 如果关闭时有并发的读写操作，必须强行关闭并丢弃连接
	if t.activeOps.Load() > 0 {
		t.discard.Store(true)
	}

	t.mu.Lock()
	conn := t.Conn.RawConn()
	cp := t.cp
	addr := t.addr // 临时保存 addr，防止在 t.cp = nil 后无法获取
	t.Conn = nil   // 清空引用，防止内存泄漏和后续访问
	t.cp = nil
	t.addr = nil
	t.mu.Unlock()

	// 尝试归还连接池
	if !t.discard.Load() && cp != nil {
		err := cp.putPoolConn(conn, addr)
		if err == nil || errors.Is(err, ErrConnAlreadyExists) {
			return nil // 成功归还或连接已存在（视为成功归还，池已持有）
		}
		// 若因重复、池满等原因放回失败，则继续执行物理关闭
	}

	// 物理关闭
	// 只有在没有成功归还池的情况下，才减少总计数
	cp.connNum.Add(-1)
	cp.loadPool(addr).connTotalCountAdd(-1)
	cp.removeUsedConn(conn)
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

func (t *connSingle) Discard() {
	t.discard.Store(true)
}

func (t *connSingle) IsReuseConn() bool {
	return t.isPool
}

func (t *connSingle) RawConn() net.Conn {
	if t.rawConnMoved.Swap(true) { // 标记原始连接已移交，防止重复移交
		panic(ErrConnRAWRead)
	}
	if t.closed.Swap(true) { // 标记 connSingle 包装器已失效
		panic(net.ErrClosed)
	}

	t.mu.Lock()
	conn := t.Conn.RawConn()
	cp := t.cp
	addr := t.addr
	t.Conn = nil
	t.cp = nil
	t.addr = nil
	t.mu.Unlock()

	cp.connNum.Add(-1)
	cp.loadPool(addr).connTotalCountAdd(-1)
	cp.removeUsedConn(conn)
	return conn // 返回原始连接
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

	ic.pool.remove(ic) // 移除过期或失效连接
}

type pools struct {
	cp             *Pool
	mu             sync.Mutex
	idle           []*idleConn
	present        map[net.Conn]struct{} // 用于 O(1) 查重
	connIdleGeq    map[int][]chan bool
	connIdleLeq    map[int][]chan bool
	connNumGeq     map[int][]chan bool
	connNumLeq     map[int][]chan bool
	connTotalCount atomic.Int64 // 该地址当前持有的总连接数（包含空闲和在用连接）
}

func (p *pools) put(conn net.Conn, timeout time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cp.closed.Load() {
		return ErrConnPoolClosed
	}

	if p.present == nil {
		p.present = make(map[net.Conn]struct{})
	}

	if _, ok := p.present[conn]; ok {
		return ErrConnAlreadyExists
	}

	// 检查该地址的空闲连接数是否达到上限
	if p.cp.IdleConn > 0 && len(p.idle) >= p.cp.IdleConn {
		return ErrConnIdleMax
	}

	vc := vconn.New(conn)
	vc.SetBackgroundReadDiscard(true)

	ic := &idleConn{conn: conn, vc: vc, pool: p}
	p.idle = append(p.idle, ic)
	p.present[conn] = struct{}{}

	l := len(p.idle)
	p.geqConnIdleNotify(l) // 通知等待空闲连接数达到阈值的goroutine

	go ic.wait(timeout) // 启动连接超时/关闭监听
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
		p.leqConnIdleNotify(n) // 通知等待空闲连接数减少的goroutine

		// 检查连接是否依然健康，或者vconn是否已将底层连接移交
		if !ic.vc.CancelNotify(vconn.ErrRawConnAlreadyUsed) {
			// 连接不健康或底层连接已不再被vconn管理，关闭并移除
			ic.vc.Close()
			p.cp.connNum.Add(-1)          // 减少全局总连接数
			l := p.connTotalCount.Add(-1) // 减少该地址的总连接数
			p.leqConnNumNotify(int(l))    // 通知该地址连接数变化
			continue                      // 继续尝试获取下一个连接
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
		return // 池已清空或未初始化
	}
	if _, ok := p.present[ic.conn]; !ok {
		return // 连接不在池中
	}

	// 查找并快速删除切片中的元素
	for i, v := range p.idle {
		if v == ic {
			p.idle[i] = p.idle[len(p.idle)-1]
			p.idle[len(p.idle)-1] = nil
			p.idle = p.idle[:len(p.idle)-1]
			delete(p.present, ic.conn)

			ic.vc.Close()
			p.cp.connNum.Add(-1)          // 减少全局总连接数
			l := p.connTotalCount.Add(-1) // 减少该地址的总连接数
			p.leqConnNumNotify(int(l))    // 通知该地址连接数变化
			break
		}
	}
	p.leqConnIdleNotify(len(p.idle)) // 通知空闲连接数变化
}

// connTotalCountAdd 负责原子更新 totalCount 并发出通知
func (p *pools) connTotalCountAdd(n int64) {
	l := int(p.connTotalCount.Add(n))

	p.mu.Lock() // 保护对 map 的访问
	defer p.mu.Unlock()

	if n > 0 {
		p.geqConnNumNotify(l)
	} else {
		p.leqConnNumNotify(l)
	}
}

// connNotify 统一处理通知逻辑，在锁内调用
func (p *pools) connNotify(mc map[int][]chan bool, l int) {
	if channels, ok := mc[l]; ok {
		for _, ch := range channels {
			select {
			case ch <- !p.cp.closed.Load():
			default: // 防止阻塞
			}
		}
		delete(mc, l)
	}
}

// connWatiClean 统一清理等待队列，在锁内调用
func (p *pools) connWatiClean(mc map[int][]chan bool) {
	for _, channels := range mc {
		for _, ch := range channels {
			select {
			case ch <- !p.cp.closed.Load():
			default: // 防止阻塞
			}
		}
	}
}

func (p *pools) geqConnNumWait(l int) <-chan bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan bool, 1)
	if p.connTotalCount.Load() >= int64(l) || p.cp.closed.Load() {
		ch <- !p.cp.closed.Load()
		return ch
	}

	p.connNumGeq[l] = append(p.connNumGeq[l], ch)
	return ch
}

func (p *pools) geqConnNumNotify(l int) {
	p.connNotify(p.connNumGeq, l)
}

func (p *pools) geqConnNumClean() {
	p.connWatiClean(p.connNumGeq)
	p.connNumGeq = make(map[int][]chan bool)
}

func (p *pools) leqConnNumWait(l int) <-chan bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan bool, 1)
	if p.connTotalCount.Load() <= int64(l) || p.cp.closed.Load() {
		ch <- !p.cp.closed.Load()
		return ch
	}

	p.connNumLeq[l] = append(p.connNumLeq[l], ch)
	return ch
}

func (p *pools) leqConnNumNotify(l int) {
	p.connNotify(p.connNumLeq, l)
}

func (p *pools) leqConnNumClean() {
	p.connWatiClean(p.connNumLeq)
	p.connNumLeq = make(map[int][]chan bool)
}

func (p *pools) geqConnIdleWait(l int) <-chan bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan bool, 1)
	if len(p.idle) >= l || p.cp.closed.Load() {
		ch <- !p.cp.closed.Load()
		return ch
	}

	p.connIdleGeq[l] = append(p.connIdleGeq[l], ch)
	return ch
}

func (p *pools) geqConnIdleNotify(l int) {
	p.connNotify(p.connIdleGeq, l)
}

func (p *pools) geqConnIdleClean() {
	p.connWatiClean(p.connIdleGeq)
	p.connIdleGeq = make(map[int][]chan bool)
}

func (p *pools) leqConnIdleWait(l int) <-chan bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch := make(chan bool, 1)
	if len(p.idle) <= l || p.cp.closed.Load() {
		ch <- !p.cp.closed.Load()
		return ch
	}

	p.connIdleLeq[l] = append(p.connIdleLeq[l], ch)
	return ch
}

func (p *pools) leqConnIdleNotify(l int) {
	p.connNotify(p.connIdleLeq, l)
}

func (p *pools) leqConnIdleClean() {
	p.connWatiClean(p.connIdleLeq)
	p.connIdleLeq = make(map[int][]chan bool)
}

func (p *pools) clean() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.idle != nil { // 避免重复清理
		for _, ic := range p.idle {
			ic.vc.CancelNotify(net.ErrClosed)
			ic.conn.Close()
			l := p.connTotalCount.Add(-1)
			p.leqConnNumNotify(int(l))
		}

		p.cp.connNum.Add(-int64(len(p.idle)))
	}
	// 如果是池关闭了，关闭接收者
	if p.cp.closed.Load() {
		p.geqConnIdleClean()
		p.geqConnNumClean()
		p.leqConnNumClean()
	}

	// 告诉接收者，连接全部关闭
	p.leqConnIdleClean()

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

	connNum   atomic.Int64
	conns     map[string]*pools // 采用标准 Map + 细粒度 RWMutex 优化并发读写性能
	usedConns map[net.Conn]struct{}
	closed    atomic.Bool
	mu        sync.RWMutex
}

func addrKey(network, address string) string {
	return network + "," + address
}

// loadPool 获取或创建指定地址的 pools 实例
func (p *Pool) loadPool(addr net.Addr) *pools {
	key := addrKey(addr.Network(), addr.String())
	p.mu.RLock()
	ps, ok := p.conns[key]
	p.mu.RUnlock()
	if ok {
		return ps
	}

	p.mu.Lock() // 升级为写锁以创建或初始化
	defer p.mu.Unlock()
	if p.conns == nil {
		p.conns = make(map[string]*pools)
	}
	// Double check, in case another goroutine created it while upgrading lock
	ps, ok = p.conns[key]
	if !ok {
		ps = &pools{
			cp:          p,
			present:     make(map[net.Conn]struct{}),
			connIdleLeq: make(map[int][]chan bool),
			connIdleGeq: make(map[int][]chan bool),
			connNumGeq:  make(map[int][]chan bool),
			connNumLeq:  make(map[int][]chan bool),
		}
		p.conns[key] = ps
	}
	return ps
}

// removeUsedConn 从 usedConns 集合中移除一个连接
func (p *Pool) removeUsedConn(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.usedConns != nil {
		delete(p.usedConns, conn)
	}
}

func (p *Pool) WaitConnNumGeq(addr net.Addr, l int) <-chan bool {
	return p.loadPool(addr).geqConnNumWait(l)
}

func (p *Pool) WaitConnNumLeq(addr net.Addr, l int) <-chan bool {
	return p.loadPool(addr).leqConnNumWait(l)
}

func (p *Pool) WaitConnIdleGeq(addr net.Addr, l int) <-chan bool {
	return p.loadPool(addr).geqConnIdleWait(l)
}

func (p *Pool) WaitConnIdleLeq(addr net.Addr, l int) <-chan bool {
	return p.loadPool(addr).leqConnIdleWait(l)
}

func (p *Pool) getPoolConn(network, address string) (net.Conn, error) {
	key := addrKey(network, address)
	p.mu.RLock()
	ps, ok := p.conns[key]
	p.mu.RUnlock()
	if !ok {
		return nil, ErrConnNotAvailable
	}

	return ps.get()
}

func (p *Pool) getPoolConnCount(network, address string) int {
	key := addrKey(network, address)
	p.mu.RLock()
	ps, ok := p.conns[key]
	p.mu.RUnlock()
	if !ok {
		return 0
	}
	return int(ps.connTotalCount.Load())
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
		conn.Close() // 池已关闭，直接关闭连接
		return ErrConnPoolClosed
	}

	return p.loadPool(addr).put(conn, p.IdleTimeout)
}

func (p *Pool) checkAndIncConnNum() error {
	if p.MaxConn <= 0 { // MaxConn <= 0 表示不限制连接数
		p.connNum.Add(1)
		return nil
	}
	for {
		current := p.connNum.Load()
		if int(current) >= p.MaxConn {
			return ErrConnPoolMax
		}
		// 尝试原子地增加计数
		if p.connNum.CompareAndSwap(current, current+1) {
			return nil
		}
		// 如果 CAS 失败，说明有其他 goroutine 修改了 current，重试
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
		pool bool // 标记连接是否来自连接池
	)

	isPriority, exist := ctx.Value(PriorityContextKey).(bool)
	if !isPriority {
		conn, err = p.getPoolConn(network, address)
		if err == nil {
			// 成功从池中获取连接
			if ctx.Err() != nil { // 如果Context已取消，则立即关闭获取到的连接并返回Context错误
				// 尝试归还连接，归还失败则关闭
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
		conn, err = p.dialNew(ctx, network, address)
	}
	if err != nil {
		return nil, err
	}

	// 构建 connSingle 包装器
	addr := &Addr{Net: network, Name: address}
	p.mu.Lock()
	if p.closed.Load() {
		p.connNum.Add(-1) // 回滚计数，防止资源泄露
		p.loadPool(addr).connTotalCountAdd(-1)
		conn.Close()
		return nil, ErrConnPoolClosed
	}
	if p.usedConns == nil {
		p.usedConns = make(map[net.Conn]struct{})
	}
	p.usedConns[conn] = struct{}{}
	p.mu.Unlock()

	return &connSingle{
		Conn:   vconn.New(conn),
		cp:     p,
		isPool: pool,
		addr:   addr,
		laddr:  conn.LocalAddr(),
		raddr:  conn.RemoteAddr(),
	}, nil
}

func (p *Pool) dialNew(ctx context.Context, network, address string) (net.Conn, error) {
	// 1. 检查并预占全局连接配额
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
		p.connNum.Add(-1) // 解析失败，回滚配额
		return nil, err
	}

	// 3. 执行物理拨号
	dialer := p.Dialer
	if dialer == nil {
		dialer = defaultDialer
	}

	conn, err := dialer.DialContext(ctx, network, addr.String())
	if err != nil {
		p.connNum.Add(-1) // 拨号失败，回滚配额
		return nil, err
	}

	p.loadPool(addr).connTotalCountAdd(1)
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

	// 连接所有权转移给调用者，减少全局连接计数 (connNum)
	p.connNum.Add(-1)
	p.loadPool(addr).connTotalCountAdd(-1)
	return conn, nil
}

func (p *Pool) Add(conn net.Conn) error {
	if conn == nil {
		return errors.New("vconnpool: cannot add nil connection")
	}
	return p.Put(conn, conn.RemoteAddr())
}

func (p *Pool) Put(conn net.Conn, addr net.Addr) error {
	if conn == nil || addr == nil {
		return errors.New("vconnpool: nil parameters")
	}

	if p.closed.Load() {
		conn.Close() // 池已关闭，直接关闭外部连接
		return ErrConnPoolClosed
	}
	// 如果是包装连接，走自身的回收逻辑
	if cs, ok := conn.(*connSingle); ok {
		return cs.Close()
	}

	// 1. 检查并尝试增加全局连接配额。这对应于将一个外部连接纳入池的管理。
	if err := p.checkAndIncConnNum(); err != nil {
		conn.Close() // 如果达到最大连接数，直接关闭此外部连接
		return err
	}

	if vc, ok := conn.(*vconn.Conn); ok {
		conn = vc.RawConn()
	}

	if err := p.putPoolConn(conn, addr); err != nil {
		p.connNum.Add(-1)
		if errors.Is(err, ErrConnAlreadyExists) {
			return nil
		}
		conn.Close() // 因其他错误（如空闲池满、池已关闭），关闭连接
		return err
	}
	p.loadPool(addr).connTotalCountAdd(1)
	return nil
}

// CloseIdleConnection 关闭指定地址的空闲连接
func (p *Pool) CloseIdleConnection(addr net.Addr) {
	key := addrKey(addr.Network(), addr.String())
	p.mu.RLock()
	ps, ok := p.conns[key]
	p.mu.RUnlock()
	if ok {
		ps.clean()
	}
}

// CloseIdleConnections 关闭所有地址的空闲连接
func (p *Pool) CloseIdleConnections() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, ps := range p.conns {
		ps.clean()
	}
}

func (p *Pool) Close() error {
	if p.closed.Swap(true) { // 幂等关闭整个连接池
		return nil
	}
	p.CloseIdleConnections()

	p.mu.Lock()
	for conn := range p.usedConns {
		conn.Close()
	}
	p.usedConns = make(map[net.Conn]struct{})
	p.mu.Unlock()
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
func (a *Addr) String() string  { return a.Name } // 确保返回完整的地址字符串

// contextKey 用于在 context 中存储连接池相关的值
type contextKey struct{ name string }

func (p *contextKey) String() string { return "connpool context value " + p.name }

// PriorityContextKey 上下文键，用于标记是否优先创建新连接（不使用连接池）
var PriorityContextKey = &contextKey{"priority"}
