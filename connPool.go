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

	"github.com/456vv/vconn/v2"
)

var (
	ErrConnPoolClosed   = errors.New("vconnpool: the connection pool has been closed")
	ErrConnRAWRead      = errors.New("vconnpool: the original connection cannot be read repeatedly or is closed")
	ErrConnNotAvailable = errors.New("vconnpool: no available connection in the pool")
	ErrConnPoolMax      = errors.New("vconnpool: the number of connections has reached the maximum limit")
	ErrConnIdleMax      = errors.New("vconnpool: the number of idle connections has reached the maximum")
	ErrConnExists       = errors.New("vconnpool: the connection already exists in the idle pool")
)

type Dialer interface {
	Dial(network, address string) (net.Conn, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type Conn interface {
	net.Conn
	Discard()
	IsReuseConn() bool
	RawConn() (net.Conn, error)
}

const (
	stAlive  int32 = iota // 可用
	stClosed              // 已关闭（幂等）
	stMoved               // 原始连接已移交
)

type connSingle struct {
	mu sync.Mutex

	vc     *vconn.Conn
	cp     *Pool
	ps     *pools
	addr   net.Addr
	laddr  net.Addr
	raddr  net.Addr
	isPool bool

	putTimeout time.Duration
	putBS      int

	bufSize   atomic.Int64 // 尚未被读走的预读字节数（永不为负，见 readBufSize）
	state     atomic.Int32 // stAlive / stClosed / stMoved
	discard   atomic.Bool  // 底层连接不可复用
	activeOps atomic.Int32 // 在途的 Read/Write/SetDeadline 数量

	raw net.Conn

	partialWrite atomic.Bool

	opsMu sync.RWMutex
}

func (t *connSingle) beginOp() (*vconn.Conn, error) {
	if t.state.Load() != stAlive {
		return nil, net.ErrClosed
	}
	t.mu.Lock()
	vc := t.vc
	t.mu.Unlock()
	if vc == nil {
		return nil, net.ErrClosed
	}
	return vc, nil
}

func (t *connSingle) readBufSize(n int) {
	if n <= 0 {
		return
	}
	for {
		cur := t.bufSize.Load()
		if cur <= 0 {
			return
		}
		next := cur - int64(n)
		if next < 0 {
			next = 0
		}
		if t.bufSize.CompareAndSwap(cur, next) {
			return
		}
	}
}

func (t *connSingle) Read(b []byte) (int, error) {
	t.opsMu.RLock()
	defer t.opsMu.RUnlock()
	vc, err := t.beginOp()
	if err != nil {
		return 0, err
	}

	n, err := vc.Read(b)
	if n > 0 {
		t.readBufSize(n)
	}
	if err != nil {
		t.errDiscard(err)
	}
	return n, err
}

func (t *connSingle) Write(b []byte) (int, error) {
	t.opsMu.RLock()
	defer t.opsMu.RUnlock()
	vc, err := t.beginOp()
	if err != nil {
		return 0, err
	}

	n, err := vc.Write(b)
	if err != nil {
		t.errDiscard(err)
		if t.state.Load() != stAlive {
			t.partialWrite.Store(true)
		}
	}
	return n, err
}

func (t *connSingle) errDiscard(err error) {
	if err == nil {
		return
	}
	switch vconn.StatusOf(err).Class() {
	case vconn.StatusAlive, vconn.StatusTimeout, vconn.StatusBusy, vconn.StatusUnverified, vconn.StatusLatched:
		return // 连接未必坏
	}
	t.discard.Store(true)
}

func (t *connSingle) close(res drainResult) error {
	if res.interrupted || res.killed {
		t.discard.Store(true)
	}

	t.mu.Lock()
	putTimeout, putBS := t.putTimeout, t.putBS
	vc, cp, ps := t.vc, t.cp, t.ps
	t.vc, t.cp, t.ps, t.addr = nil, nil, nil, nil
	t.mu.Unlock()

	if vc == nil || cp == nil {
		return nil // 已被其它路径收尾，避免空指针
	}

	if !t.discard.Load() {
		if err := vc.CheckReusable(); reusablePut(vc, err) {
			raw, pending, err := vc.RawConnFull()
			if err == nil && ps != nil {
				err = ps.put(raw, pending, putTimeout, putBS, 0)
				if err == nil {
					cp.ensureSweeper()
					if putTimeout > 0 {
						cp.wakeSweeperAt(time.Now().Add(putTimeout))
					}
					cp.removeUsedConn(t)
					return nil
				}
			}
			cp.releaseCount(ps, 1)
			cp.removeUsedConn(t)
			if errors.Is(err, ErrConnExists) {
				return nil
			}
			if raw != nil {
				if cerr := raw.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
					return cerr
				}
				return nil
			}
			// raw == nil：fd 还在 vc 里，不能直接返回。
			if cerr := vc.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
				return cerr
			}
			return nil
		}
	}

	cp.releaseCount(ps, 1)
	cp.removeUsedConn(t)
	if err := vc.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func (t *connSingle) Close() error {
	if !t.state.CompareAndSwap(stAlive, stClosed) {
		return nil
	}
	return t.withExclusive(closeDrainGrace, t.close)
}

func (t *connSingle) LocalAddr() net.Addr  { return t.laddr }
func (t *connSingle) RemoteAddr() net.Addr { return t.raddr }

func (t *connSingle) doDeadline(fn func(*vconn.Conn) error) error {
	t.opsMu.RLock()
	defer t.opsMu.RUnlock()
	vc, err := t.beginOp()
	if err != nil {
		return err
	}
	return fn(vc)
}

func (t *connSingle) SetDeadline(tm time.Time) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetDeadline(tm) })
}

func (t *connSingle) SetReadDeadline(tm time.Time) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetReadDeadline(tm) })
}

func (t *connSingle) SetWriteDeadline(tm time.Time) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetWriteDeadline(tm) })
}

func (t *connSingle) SetReadBuffer(bytes int) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetReadBuffer(bytes) })
}

func (t *connSingle) SetWriteBuffer(bytes int) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetWriteBuffer(bytes) })
}

func (t *connSingle) SetNoDelay(enable bool) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetNoDelay(enable) })
}

func (t *connSingle) SetKeepAlive(enable bool) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetKeepAlive(enable) })
}

func (t *connSingle) SetKeepAlivePeriod(d time.Duration) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetKeepAlivePeriod(d) })
}

func (t *connSingle) SetLinger(sec int) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetLinger(sec) })
}

func (t *connSingle) CloseWrite() error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.CloseWrite() })
}

func (t *connSingle) CloseRead() error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.CloseRead() })
}

func (t *connSingle) Discard() { t.discard.Store(true) }

func (t *connSingle) IsReuseConn() bool { return t.isPool }

func (t *connSingle) Buffered() int {
	if n := t.bufSize.Load(); n > 0 {
		return int(n)
	}
	return 0
}

const (
	moveOutDrainGrace = 300 * time.Millisecond
	closeDrainGrace   = 100 * time.Millisecond
)

func (t *connSingle) RawConn() (net.Conn, error) {
	conn, _, err := t.moveOut()
	return conn, err
}

func (t *connSingle) RawConnFull() (net.Conn, []byte, error) {
	return t.moveOut()
}

type drainResult struct {
	interrupted bool // 打断过在途 I/O（被中断方看到的是伪 i/o timeout）
	killed      bool // 已强杀底层 fd，连接彻底不可用
}

func (t *connSingle) interrupt() {
	now := time.Now()
	if raw := t.raw; raw != nil {
		raw.SetReadDeadline(now)
		raw.SetWriteDeadline(now)
	}
}

func (t *connSingle) enterExclusive(grace time.Duration) (res drainResult) {
	if t.opsMu.TryLock() {
		return drainResult{}
	}
	res.interrupted = true

	done := make(chan struct{})
	go func() {
		t.opsMu.Lock() // 写优先：同时挡住后续新读，避免读把写锁饿死
		close(done)
	}()

	t.interrupt() // ① 打断

	tm := time.NewTimer(grace)
	defer tm.Stop()
	select {
	case <-done:
		return res
	case <-tm.C:
	}

	if raw := t.raw; raw != nil { // ② 兜底强杀
		res.killed = true
		raw.Close()
	}
	<-done // fd 已关，在途调用必错返回；这里必然拿到独占
	return res
}

func (t *connSingle) withExclusive(grace time.Duration, fn func(res drainResult) error) error {
	res := t.enterExclusive(grace)
	defer t.opsMu.Unlock()
	return fn(res)
}

func (t *connSingle) moveOut() (net.Conn, []byte, error) {
	if !t.state.CompareAndSwap(stAlive, stMoved) {
		if t.state.Load() == stMoved {
			return nil, nil, ErrConnRAWRead
		}
		return nil, nil, net.ErrClosed
	}
	var (
		outRaw  net.Conn
		outData []byte
	)
	err := t.withExclusive(moveOutDrainGrace, func(res drainResult) error {
		t.mu.Lock()
		vc, cp, ps := t.vc, t.cp, t.ps
		t.vc, t.cp, t.ps, t.addr = nil, nil, nil, nil
		t.mu.Unlock()

		if cp != nil {
			cp.releaseCount(ps, 1)
			cp.removeUsedConn(t)
		}
		if vc == nil {
			return net.ErrClosed
		}
		defer vc.Close()
		if res.killed || t.partialWrite.Load() || res.interrupted {
			return errors.New("vconnpool: connection has in-flight i/o and cannot be handed out")
		}
		raw, data, rerr := vc.RawConnFull()
		if rerr != nil {
			if raw != nil {
				raw.Close()
			}
			return rerr
		}
		outRaw, outData = raw, data
		return nil
	})
	return outRaw, outData, err
}

type idleConn struct {
	conn     net.Conn
	vc       *vconn.Conn
	putAt    time.Time
	expireAt time.Time // 零值 = 永不过期

	timeout time.Duration
	bs      int
}

func (ic *idleConn) expired(now time.Time) bool {
	return !ic.expireAt.IsZero() && !now.Before(ic.expireAt)
}

type pools struct {
	cp  *Pool
	key string // 创建后只读，等于 addrKey。空则 dropBucket 退回扫描

	mu      sync.Mutex
	idle    []*idleConn // 环形队列：head 之前是已弹出的空位
	head    int
	present map[net.Conn]struct{} // O(1) 查重

	wait map[waitKey][]*waiter

	connTotalCount atomic.Int64 // 该地址当前持有的总连接数（空闲 + 在用）

	cleanSeq uint64

	minExpire atomic.Int64
}

func (p *pools) idleCountLocked() int { return len(p.idle) - p.head }

// emptyLocked 调用方必须持有 ps.mu。
// 用 == 0 而不是 <= 0：负数说明记账已经偏了，桶上可能还有在用连接，不能摘。
func (p *pools) emptyLocked() bool {
	return p.connTotalCount.Load() == 0 && p.idleCountLocked() == 0 && len(p.wait) == 0
}

func (p *pools) pushLocked(ic *idleConn) {
	if p.head > 0 && p.head*2 >= len(p.idle) {
		n := copy(p.idle, p.idle[p.head:])
		for i := n; i < len(p.idle); i++ {
			p.idle[i] = nil
		}
		p.idle = p.idle[:n]
		p.head = 0
	}
	p.idle = append(p.idle, ic)
	p.noteExpireLocked(ic.expireAt)
}

func (p *pools) noteExpireLocked(at time.Time) {
	if at.IsZero() {
		return
	}
	v := at.UnixNano()
	for {
		cur := p.minExpire.Load()
		if cur != 0 && cur <= v {
			return
		}
		if p.minExpire.CompareAndSwap(cur, v) {
			return
		}
	}
}

func (p *pools) recomputeMinLocked() {
	var min int64
	for i := p.head; i < len(p.idle); i++ {
		if e := p.idle[i].expireAt; !e.IsZero() {
			if v := e.UnixNano(); min == 0 || v < min {
				min = v
			}
		}
	}
	p.minExpire.Store(min)
}

func (p *pools) refreshMinLocked(gone time.Time) {
	if gone.IsZero() || p.minExpire.Load() != gone.UnixNano() {
		return
	}
	p.recomputeMinLocked()
}

func (p *pools) popLocked() *idleConn {
	if p.head >= len(p.idle) {
		p.idle, p.head = nil, 0
		p.minExpire.Store(0)
		return nil
	}
	ic := p.idle[p.head]
	p.idle[p.head] = nil
	p.head++
	if p.head >= len(p.idle) {
		p.idle, p.head = nil, 0
	}
	p.refreshMinLocked(ic.expireAt)
	return ic
}

func (p *pools) removeLocked(ic *idleConn) bool {
	for i := p.head; i < len(p.idle); i++ {
		if p.idle[i] == ic {
			copy(p.idle[i:], p.idle[i+1:])
			p.idle[len(p.idle)-1] = nil
			p.idle = p.idle[:len(p.idle)-1]
			if p.head >= len(p.idle) {
				p.idle, p.head = nil, 0
			}
			p.refreshMinLocked(ic.expireAt)
			return true
		}
	}
	return false
}

func wakeLocked(ws []*waiter, st WaitStatus) {
	for _, w := range ws {
		w.wake(st)
	}
}

func (p *pools) notifyAllLocked() {
	p.finishWaitersLocked(false)
}

// wakeAllLocked 只给 clean 用。
// 非关闭时不再把「条件已成立」报成 WaitAborted；关闭才全部 WaitPoolClosed。
func (p *pools) wakeAllLocked(abort bool) {
	p.finishWaitersLocked(abort)
}

func (p *pools) connTotalCountAdd(n int64) {
	p.connTotalCount.Add(n)
	p.mu.Lock()
	p.notifyAllLocked()
	p.mu.Unlock()
}

func unverifiedUsable(vc *vconn.Conn) bool {
	if vc.IsPeerClosed() {
		return false
	}
	if vc.Snapshot() == vconn.StatusLocalClosed {
		return false
	}
	return true
}

func reusableIdle(vc *vconn.Conn, err error) bool {
	switch st := vconn.StatusOf(err); st.Class() {
	case vconn.StatusAlive, vconn.StatusDataPending:
		return true
	case vconn.StatusUnverified:
		return unverifiedUsable(vc)
	}
	return false
}

func reusablePut(vc *vconn.Conn, err error) bool {
	switch st := vconn.StatusOf(err); st.Class() {
	case vconn.StatusAlive, vconn.StatusDataInitial:
		return true
	case vconn.StatusUnverified:
		return unverifiedUsable(vc)
	}
	return false
}

func (p *pools) precheck(conn net.Conn) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cp.closed.Load() {
		return ErrConnPoolClosed
	}
	if p.present != nil {
		if _, ok := p.present[conn]; ok {
			return ErrConnExists
		}
	}
	maxIdle := p.cp.conf().IdleConn
	if maxIdle > 0 && p.idleCountLocked() >= maxIdle {
		return ErrConnIdleMax
	}
	return nil
}

func (p *pools) put(conn net.Conn, pending []byte, timeout time.Duration, bs int, countDelta int64) error {
	if conn == nil {
		return errors.New("vconnpool: nil conn")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cp.closed.Load() {
		return ErrConnPoolClosed
	}
	if p.present == nil {
		p.present = make(map[net.Conn]struct{}, 8)
	}
	if _, ok := p.present[conn]; ok {
		return ErrConnExists
	}
	maxIdle := p.cp.conf().IdleConn
	if maxIdle > 0 && p.idleCountLocked() >= maxIdle {
		return ErrConnIdleMax
	}

	vc := vconn.NewData(conn, pending)
	if bs != 0 {
		vc.SetBackgroundReadBuffer(bs)
		vc.CloseNotify()
	}

	ic := &idleConn{
		conn:    conn,
		vc:      vc,
		putAt:   time.Now(),
		timeout: timeout,
		bs:      bs,
	}
	if timeout > 0 {
		ic.expireAt = ic.putAt.Add(timeout)
	}

	p.pushLocked(ic)
	p.present[conn] = struct{}{}
	if countDelta != 0 {
		p.connTotalCount.Add(countDelta)
	}
	p.notifyAllLocked()
	return nil
}

func (p *pools) get() (net.Conn, []byte, time.Duration, int, error) {
	for {
		if p.cp.closed.Load() {
			return nil, nil, 0, 0, ErrConnPoolClosed
		}

		p.mu.Lock()
		if p.cp.closed.Load() {
			p.mu.Unlock()
			return nil, nil, 0, 0, ErrConnPoolClosed
		}
		ic := p.popLocked()
		if ic != nil {
			delete(p.present, ic.conn)
			p.notifyAllLocked()
		}
		p.mu.Unlock()

		if ic == nil {
			return nil, nil, 0, 0, ErrConnNotAvailable
		}

		if ic.expired(time.Now()) {
			p.discardOwned(ic)
			continue
		}

		err := ic.vc.Check()
		if reusableIdle(ic.vc, err) {
			conn, b, rerr := ic.vc.RawConnFull()
			if rerr != nil {
				if conn != nil {
					conn.Close()
				}
				p.discardOwned(ic)
				continue
			}
			if derr := conn.SetDeadline(time.Time{}); derr != nil {
				conn.Close()
				p.cp.releaseCount(p, 1)
				continue
			}
			return conn, b, ic.timeout, ic.bs, nil
		}
		p.discardOwned(ic)
	}
}

func (p *pools) removeIdle(ic *idleConn) (seq uint64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.removeLocked(ic) {
		return p.cleanSeq, false
	}
	delete(p.present, ic.conn)
	return p.cleanSeq, true
}

func (p *pools) discardOwned(ic *idleConn) {
	ic.vc.Close()
	p.cp.releaseCount(p, 1)
}

func (p *pools) snapshotIdle() []*idleConn {
	p.mu.Lock()
	n := p.idleCountLocked()
	if n == 0 {
		p.mu.Unlock()
		return nil
	}
	snap := make([]*idleConn, n)
	copy(snap, p.idle[p.head:])
	p.mu.Unlock()
	return snap
}

func (p *pools) sweep(now time.Time, health bool) {
	p.sweepExpired(now)
	if !health {
		return
	}
	for _, ic := range p.snapshotIdle() {
		seq, ok := p.removeIdle(ic)
		if !ok {
			continue // 已被 get 取走 / 已被 sweepExpired 回收
		}
		if err := ic.vc.Check(); !reusableIdle(ic.vc, err) {
			p.discardOwned(ic) // 已独占：关闭 + 归还配额
			continue
		}
		if ok, closeConn := p.restoreIdle(ic, seq); !ok {
			if closeConn {
				p.discardOwned(ic) // 池已关闭 / 代际已变 / 队列已被 clean 清空
			} else {
				p.cp.releaseCount(p, 1)
			}
		}
	}
}

func (p *pools) restoreIdle(ic *idleConn, seq uint64) (ok bool, closeConn bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cp.closed.Load() || p.present == nil || p.cleanSeq != seq {
		return false, true
	}
	if _, dup := p.present[ic.conn]; dup {
		return false, false
	}
	p.pushLocked(ic)
	p.present[ic.conn] = struct{}{}
	p.notifyAllLocked()
	return true, false
}

func (p *pools) sweepExpired(now time.Time) {
	p.mu.Lock()
	if p.idleCountLocked() == 0 {
		p.mu.Unlock()
		return
	}

	var dead []*idleConn
	w := p.head
	for i := p.head; i < len(p.idle); i++ {
		ic := p.idle[i]
		if ic.expired(now) {
			dead = append(dead, ic)
			delete(p.present, ic.conn)
			continue
		}
		p.idle[w] = ic
		w++
	}
	if len(dead) == 0 {
		p.mu.Unlock()
		return
	}
	for i := w; i < len(p.idle); i++ {
		p.idle[i] = nil
	}
	p.idle = p.idle[:w]
	if p.head >= len(p.idle) {
		p.idle, p.head = nil, 0
	}
	p.recomputeMinLocked()
	p.notifyAllLocked()
	p.mu.Unlock()

	for _, ic := range dead {
		p.discardOwned(ic)
	}
}

func (p *pools) clean() {
	p.mu.Lock()
	idle := p.idle[p.head:]
	p.idle, p.head = nil, 0
	p.present = nil
	p.minExpire.Store(0)
	p.cleanSeq++
	n := int64(len(idle))
	p.connTotalCount.Add(-n)
	p.wakeAllLocked(p.cp.closed.Load())
	p.mu.Unlock()

	if n == 0 {
		return
	}
	p.cp.connNum.Add(-n)
	for _, ic := range idle {
		ic.vc.Close()
	}
}

var defaultDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

type Pool struct {
	cfg atomic.Pointer[Config]

	connNum atomic.Int64
	conns   map[string]*pools
	mu      sync.RWMutex

	usedMu    sync.Mutex
	usedConns map[*connSingle]struct{}

	closed atomic.Bool

	sweepOnce sync.Once
	sweepMu   sync.Mutex
	sweepStop chan struct{}

	sweepWake atomic.Pointer[chan struct{}]

	sweepNext atomic.Int64

	sweepWg sync.WaitGroup
}

var (
	_ Conn      = (*connSingle)(nil)
	_ net.Conn  = (*connSingle)(nil)
	_ io.Closer = (*Pool)(nil)
)

func (p *pools) reapplyTimeout(timeout time.Duration) (done bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cp.closed.Load() || p.idleCountLocked() == 0 {
		return false
	}

	now := time.Now()
	for i := p.head; i < len(p.idle); i++ {
		ic := p.idle[i]
		ic.timeout = timeout // 取出时原样交回 connSingle，与新配置对齐
		if timeout > 0 {
			at := ic.putAt.Add(timeout)
			if at.Before(now) {
				at = now.Add(-time.Nanosecond)
			}
			ic.expireAt = at
		} else {
			ic.expireAt = time.Time{} // 永不过期
		}
		done = true
	}
	p.recomputeMinLocked()
	p.cp.wakeSweeper()
	return done
}

func (p *pools) trimIdle(maxIdle int) {
	p.mu.Lock()
	if p.cp.closed.Load() || p.idleCountLocked() <= maxIdle {
		p.mu.Unlock()
		return
	}
	n := p.idleCountLocked() - maxIdle
	dead := make([]*idleConn, n)
	copy(dead, p.idle[p.head:p.head+n])

	rest := p.idle[p.head+n:]
	copy(p.idle[p.head:], rest)
	for i := p.head + len(rest); i < len(p.idle); i++ {
		p.idle[i] = nil
	}
	p.idle = p.idle[:p.head+len(rest)]
	if p.head >= len(p.idle) {
		p.idle, p.head = nil, 0
	}
	for _, ic := range dead {
		delete(p.present, ic.conn)
	}
	p.recomputeMinLocked()
	p.notifyAllLocked()
	p.mu.Unlock()

	for _, ic := range dead {
		p.discardOwned(ic) // 关闭 socket + 归还全局配额与桶计数
	}
}

func addrKey(network, address string) string {
	switch network {
	case "tcp4", "tcp6":
		network = "tcp"
	case "udp4", "udp6":
		network = "udp"
	case "ip4", "ip6":
		network = "ip"
	}
	return network + "," + address
}

func (p *Pool) newBucket(key string) *pools {
	return &pools{
		cp:      p,
		key:     key,
		present: make(map[net.Conn]struct{}, 8),
	}
}

func (p *Pool) withBucket(key string, fn func(*pools) error) error {
	p.ensureSweeper()

	p.mu.RLock()
	if ps := p.conns[key]; ps != nil && !p.closed.Load() {
		defer p.mu.RUnlock()
		return fn(ps)
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return ErrConnPoolClosed
	}
	if p.conns == nil {
		p.conns = make(map[string]*pools)
	}
	ps := p.conns[key]
	if ps == nil {
		ps = p.newBucket(key)
		p.conns[key] = ps
	}
	err := fn(ps)
	if err != nil {
		p.dropIfEmpty(key, ps) // 建出来又没放进东西：别留到下一轮清扫
	}
	return err
}

// dropIfEmpty 调用方必须持有 Pool.mu 写锁。检查和 delete 都在桶锁内完成。
func (p *Pool) dropIfEmpty(key string, ps *pools) {
	if ps == nil || p.conns[key] != ps {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.emptyLocked() && p.conns[key] == ps {
		delete(p.conns, key)
		if len(p.conns) == 0 {
			p.conns = nil
		}
	}
}

// reapEmpty 摘掉所有空桶。写锁与「读锁下的写入」互斥；
// count != 0 的桶连桶锁都不拿。桶锁拿不到就留到下一轮，避免写锁被健康检查堵住。
func (p *Pool) reapEmpty() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() || len(p.conns) == 0 {
		return
	}
	for k, ps := range p.conns {
		if ps.connTotalCount.Load() != 0 {
			continue
		}
		if !ps.mu.TryLock() {
			continue
		}
		if ps.emptyLocked() {
			delete(p.conns, k)
		}
		ps.mu.Unlock()
	}
	if len(p.conns) == 0 {
		p.conns = nil
	}
}

// putIdle 把一条池外连接放进地址桶，countDelta 与 idle 一起提交。
// 归还已计入总数的连接不要走这里，直接 ps.put(..., 0)。
func (p *Pool) putIdle(addr net.Addr, conn net.Conn, pending []byte, timeout time.Duration, bs int, countDelta int64) error {
	if conn == nil {
		return errors.New("vconnpool: nil conn")
	}
	if addr == nil {
		return errors.New("vconnpool: nil addr")
	}
	if p.closed.Load() {
		return ErrConnPoolClosed
	}
	err := p.withBucket(addrKey(addr.Network(), addr.String()), func(ps *pools) error {
		return ps.put(conn, pending, timeout, bs, countDelta)
	})
	if err != nil {
		return err
	}
	if timeout > 0 {
		p.wakeSweeperAt(time.Now().Add(timeout))
	}
	return nil
}

func (p *Pool) getPoolAddr(addr net.Addr) *pools {
	if addr == nil {
		return nil
	}
	return p.getPool(addr.Network(), addr.String())
}

func (p *Pool) getPool(network, address string) *pools {
	key := addrKey(network, address)
	p.mu.RLock()
	ps := p.conns[key]
	p.mu.RUnlock()
	return ps
}

const (
	sweepMinSleep = 5 * time.Millisecond
)

func (p *Pool) ensureSweeper() {
	if p.sweepWake.Load() != nil || p.closed.Load() {
		return
	}
	p.sweepMu.Lock()
	defer p.sweepMu.Unlock()
	// 先发布 stop/wake 再 go。Close 持着 sweepMu 时，要么看见 stop，要么这里看见 closed。
	if p.closed.Load() || p.sweepWake.Load() != nil {
		return
	}
	wake := make(chan struct{}, 1)
	p.sweepStop = make(chan struct{})
	p.sweepWake.Store(&wake)
	p.sweepWg.Add(1)
	go p.sweepLoop(wake)
}

func (p *Pool) wakeSweeperAt(deadline time.Time) {
	if deadline.IsZero() {
		return
	}
	if n := p.sweepNext.Load(); n != 0 && deadline.UnixNano() >= n {
		return
	}
	if pw := p.sweepWake.Load(); pw != nil {
		select {
		case *pw <- struct{}{}:
		default:
		}
	}
}

func (p *Pool) nextExpiry() (time.Time, bool) {
	p.mu.RLock()
	var min int64
	for _, ps := range p.conns {
		if v := ps.minExpire.Load(); v != 0 && (min == 0 || v < min) {
			min = v
		}
	}
	p.mu.RUnlock()
	if min == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, min), true
}

func (p *Pool) sweepInterval() time.Duration {
	iv := 5 * time.Second
	if it := p.conf().IdleTimeout; it > 0 {
		iv = it / 2
	}
	if iv < 100*time.Millisecond {
		iv = 100 * time.Millisecond
	}
	if iv > 30*time.Second {
		iv = 30 * time.Second
	}
	return iv
}

func (p *Pool) sweepLoop(wake <-chan struct{}) {
	defer p.sweepWg.Done()

	tm := time.NewTimer(time.Hour)
	if !tm.Stop() {
		select {
		case <-tm.C:
		default:
		}
	}
	defer tm.Stop()

	var lastReap time.Time
	lastHealth := time.Now()

	for {
		if p.closed.Load() {
			return
		}
		// 放在定时器之前：wake 很勤时也不会把空桶回收饿死。
		if time.Since(lastReap) >= p.sweepInterval() {
			p.reapEmpty()
			lastReap = time.Now()
		}

		p.sweepNext.Store(0)

		d := time.Until(lastHealth.Add(p.sweepInterval()))
		if at, ok := p.nextExpiry(); ok {
			if w := time.Until(at); w < d {
				d = w
			}
		}
		if d < sweepMinSleep {
			d = sweepMinSleep
		}
		p.sweepNext.Store(time.Now().Add(d).UnixNano())
		tm.Reset(d)

		select {
		case <-p.sweepStop:
			return
		case <-wake:
			if !tm.Stop() {
				select {
				case <-tm.C:
				default:
				}
			}
		case <-tm.C:
			if p.closed.Load() {
				return
			}
			now := time.Now()
			health := now.Sub(lastHealth) >= p.sweepInterval()
			p.sweepAll(now, health)
			if health {
				lastHealth = now
			}
		}
	}
}

func (p *Pool) sweepAll(now time.Time, health bool) {
	p.mu.RLock()
	list := make([]*pools, 0, len(p.conns))
	for _, ps := range p.conns {
		list = append(list, ps)
	}
	p.mu.RUnlock()

	for _, ps := range list {
		ps.sweep(now, health)
	}
}

func (p *Pool) checkAndIncConnNum(max int) error {
	if max <= 0 {
		p.connNum.Add(1)
		return nil
	}
	for {
		cur := p.connNum.Load()
		if int(cur) >= max {
			return ErrConnPoolMax
		}
		if p.connNum.CompareAndSwap(cur, cur+1) {
			return nil
		}
	}
}

func (p *Pool) releaseCount(ps *pools, n int64) {
	if n == 0 {
		return
	}
	p.connNum.Add(-n)
	if ps != nil {
		ps.connTotalCountAdd(-n)
	}
}

func (p *Pool) removeUsedConn(t *connSingle) {
	p.usedMu.Lock()
	delete(p.usedConns, t)
	p.usedMu.Unlock()
}

type WaitStatus int8

const (
	WaitSatisfied   WaitStatus = iota // 判定瞬间谓词为真；动作前必须重读 NumConn/NumIdle
	WaitAborted                       // 兼容保留；池不再发出
	WaitContextDone                   // ctx 取消或超时
	WaitPoolClosed                    // 池已关闭
	WaitErrOther                      // 参数非法，或超过 MaxConn/IdleConn 硬上限
)

func (s WaitStatus) String() string {
	switch s {
	case WaitSatisfied:
		return "wait: satisfied"
	case WaitAborted:
		return "wait: aborted, condition not met"
	case WaitContextDone:
		return "wait: context done"
	case WaitPoolClosed:
		return "wait: pool closed"
	case WaitErrOther:
		return "wait: condition can never be satisfied"
	}
	return "wait: unknown"
}

const (
	waitIdleGeq uint8 = iota
	waitIdleLeq
	waitNumGeq
	waitNumLeq
)

type waitKey struct {
	kind uint8
	l    int
}

type waiter struct {
	ch   chan WaitStatus
	stop chan struct{} // 仅当 ctx 可取消（含派生的超时 ctx）时非 nil
}

func (w *waiter) wake(st WaitStatus) {
	select {
	case w.ch <- st:
	default:
	}
	if w.stop != nil {
		close(w.stop) // 投递权唯一，无需 sync.Once 防重复关闭
	}
}

// waitLimits 是等待谓词可见的硬上限。<=0 表示不限制，与 Config 一致。
type waitLimits struct {
	maxConn  int
	idleConn int
}

func (p *Pool) limits() waitLimits {
	c := p.conf()
	return waitLimits{maxConn: c.MaxConn, idleConn: c.IdleConn}
}

func condMet(kind uint8, l, idle, num int) bool {
	switch kind {
	case waitIdleGeq:
		return idle >= l
	case waitIdleLeq:
		return idle <= l
	case waitNumGeq:
		return num >= l
	case waitNumLeq:
		return num <= l
	default:
		return false
	}
}

// evalWait 是无桶、有桶、清扫、改配置共用的完成判定。
// done 为 false 时 st 无意义，调用方必须继续等。st 的零值是 WaitSatisfied，不能单独使用。
// 优先级：未知 kind / Leq 的 l < 0 > 此刻谓词为真 > 硬上限下已不可能 > 继续等。
// 池关闭和 ctx 取消不是快照属性，由调用方先处理。
func evalWait(kind uint8, l, idle, num int, lim waitLimits) (st WaitStatus, done bool) {
	switch kind {
	case waitIdleGeq, waitIdleLeq, waitNumGeq, waitNumLeq:
	default:
		return WaitErrOther, true
	}
	if num < 0 {
		num = 0 // 与 NumConn 一致：漂移的负数不当成「还有连接」
	}
	if (kind == waitIdleLeq || kind == waitNumLeq) && l < 0 {
		return WaitErrOther, true
	}
	if condMet(kind, l, idle, num) {
		return WaitSatisfied, true
	}
	switch kind {
	case waitIdleGeq:
		// 空闲数不会超过总连接数，所以 MaxConn 也是 IdleGeq 的硬上限。
		if (lim.idleConn > 0 && l > lim.idleConn) || (lim.maxConn > 0 && l > lim.maxConn) {
			return WaitErrOther, true
		}
	case waitNumGeq:
		if lim.maxConn > 0 && l > lim.maxConn {
			return WaitErrOther, true
		}
	}
	return 0, false
}

func (p *pools) evalLocked(k waitKey) (WaitStatus, bool) {
	return evalWait(k.kind, k.l, p.idleCountLocked(), int(p.connTotalCount.Load()), p.cp.limits())
}

// finishWaitersLocked 的调用方必须持有 ps.mu。
// 关闭时全部 WaitPoolClosed；否则只结束「已成立」或「已不可能」的等待。
func (p *pools) finishWaitersLocked(abort bool) {
	if abort || p.cp.closed.Load() {
		for _, ws := range p.wait {
			wakeLocked(ws, WaitPoolClosed)
		}
		p.wait = nil
		return
	}
	if len(p.wait) == 0 {
		return
	}
	idle := p.idleCountLocked()
	num := int(p.connTotalCount.Load())
	lim := p.cp.limits()
	for k, ws := range p.wait {
		if p.cp.closed.Load() {
			wakeLocked(ws, WaitPoolClosed)
			delete(p.wait, k)
			continue
		}
		st, done := evalWait(k.kind, k.l, idle, num, lim)
		if !done {
			continue
		}
		wakeLocked(ws, st)
		delete(p.wait, k)
	}
	if len(p.wait) == 0 {
		p.wait = nil
	}
}

// recheckWaiters 在 SetConfig 替换快照之后调用。调用方不得持有 Pool.mu 或 ps.mu。
func (p *pools) recheckWaiters() {
	p.mu.Lock()
	p.finishWaitersLocked(false)
	drop := p.emptyLocked()
	p.mu.Unlock()
	if drop {
		p.cp.dropBucket(p)
	}
}

// dropBucket 摘掉已经没有连接、没有等待者的桶。
// 禁止在持有 ps.mu 时调用：内部要拿 Pool.mu，锁序只能是 Pool.mu → ps.mu。
func (p *Pool) dropBucket(ps *pools) {
	if ps == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		return
	}
	if key := ps.key; key != "" {
		if p.conns[key] == ps {
			p.dropIfEmpty(key, ps)
		}
		return
	}
	for k, cur := range p.conns {
		if cur == ps {
			p.dropIfEmpty(k, ps)
			return
		}
	}
}

func (p *pools) addWaitLocked(k waitKey, w *waiter) {
	if p.wait == nil {
		p.wait = make(map[waitKey][]*waiter, 4)
	}
	p.wait[k] = append(p.wait[k], w)
}

func (p *pools) delWaitLocked(k waitKey, w *waiter) bool {
	list := p.wait[k]
	for i, x := range list {
		if x != w {
			continue
		}
		last := len(list) - 1
		list[i] = list[last]
		list[last] = nil
		list = list[:last]
		if last == 0 {
			delete(p.wait, k) // 空 key 一并清掉，避免 map 底层 table 只增不减
		} else {
			p.wait[k] = list
		}
		return true
	}
	return false
}

func (p *pools) watchWait(ctx context.Context, k waitKey, w *waiter) {
	select {
	case <-w.stop:
		return
	case <-ctx.Done():
	}

	p.mu.Lock()
	removed := p.delWaitLocked(k, w)
	drop := removed && p.emptyLocked()
	p.mu.Unlock() // 必须先放开 ps.mu，下面的 dropBucket 要拿 Pool.mu
	if !removed {
		return // 唤醒方已投递，不能再发 ContextDone
	}
	select {
	case w.ch <- WaitContextDone:
	default:
	}
	if drop {
		p.cp.dropBucket(p)
	}
}

func falseChan(st WaitStatus) chan WaitStatus {
	ch := make(chan WaitStatus, 1)
	ch <- st
	return ch
}

func (p *pools) waitCh(ctx context.Context, kind uint8, l int) <-chan WaitStatus {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return falseChan(WaitContextDone)
	}

	k := waitKey{kind: kind, l: l}
	p.mu.Lock()
	if ctx.Err() != nil {
		p.mu.Unlock()
		return falseChan(WaitContextDone)
	}
	if p.cp.closed.Load() {
		p.mu.Unlock()
		return falseChan(WaitPoolClosed)
	}
	if st, done := p.evalLocked(k); done {
		if p.cp.closed.Load() {
			st = WaitPoolClosed
		}
		p.mu.Unlock()
		return falseChan(st)
	}
	w := &waiter{ch: make(chan WaitStatus, 1)}
	if ctx.Done() != nil {
		w.stop = make(chan struct{})
	}
	p.addWaitLocked(k, w)
	p.mu.Unlock()

	if w.stop != nil {
		go p.watchWait(ctx, k, w)
	}
	return w.ch
}

func (p *Pool) waitCond(ctx context.Context, kind uint8, addr net.Addr, l int) <-chan WaitStatus {
	if addr == nil {
		return falseChan(WaitErrOther)
	}
	if ctx != nil && ctx.Err() != nil {
		return falseChan(WaitContextDone)
	}
	if p.closed.Load() {
		return falseChan(WaitPoolClosed)
	}

	key := addrKey(addr.Network(), addr.String())

	// waitCh 必须在持有 Pool.mu 期间调用。先解锁再登记的话，
	// CloseIdleConnection 会把桶摘掉，等待挂到已脱离 map 的旧 *pools 上，永远等不到通知。
	// 无桶时 RLock 已挡住 withBucket 的建桶写锁，idle=0、num=0 与「键不存在」是同一事实。
	p.mu.RLock()
	ps := p.conns[key]
	closed := p.closed.Load()
	if ps != nil && !closed {
		ch := ps.waitCh(ctx, kind, l)
		p.mu.RUnlock()
		return ch
	}
	if !closed {
		if st, done := evalWait(kind, l, 0, 0, p.limits()); done {
			p.mu.RUnlock()
			if ctx != nil && ctx.Err() != nil {
				return falseChan(WaitContextDone)
			}
			if p.closed.Load() {
				return falseChan(WaitPoolClosed)
			}
			return falseChan(st)
		}
	}
	p.mu.RUnlock()
	if closed || p.closed.Load() {
		return falseChan(WaitPoolClosed)
	}
	if ctx != nil && ctx.Err() != nil {
		return falseChan(WaitContextDone)
	}

	p.ensureSweeper()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return falseChan(WaitPoolClosed)
	}
	if ctx != nil && ctx.Err() != nil {
		return falseChan(WaitContextDone)
	}
	if ps = p.conns[key]; ps != nil {
		ch := ps.waitCh(ctx, kind, l)
		p.dropIfEmpty(key, ps)
		return ch
	}
	if st, done := evalWait(kind, l, 0, 0, p.limits()); done {
		if p.closed.Load() {
			return falseChan(WaitPoolClosed)
		}
		return falseChan(st)
	}
	if p.conns == nil {
		p.conns = make(map[string]*pools)
	}
	ps = p.newBucket(key)
	p.conns[key] = ps
	ch := ps.waitCh(ctx, kind, l)
	p.dropIfEmpty(key, ps) // 没登记上就当场摘掉，不等 sweeper
	return ch
}

// WaitConnNumGeq 等到该地址连接数 >= l，向返回的 channel 发送一次状态。
// channel 不会被关闭，不要用 range。
//
// addr 必须与 Dial 使用同一套 network/address，例如 &Addr{Net: network, Name: address}。
// tcp4/tcp6 会归一成 tcp。不要传 conn.RemoteAddr()，解析后的 IP 是另一个桶。
//
// 计数含在途拨号。WaitSatisfied 只表示判定瞬间谓词为真，拨号失败会回滚，
// 收到后动作前必须重读 NumConn。ctx 为 nil 则等到满足、不可能或池关闭。
// MaxConn > 0 且 l > MaxConn 时立即 WaitErrOther；MaxConn <= 0 表示不限制。
// 因上限收到 WaitErrOther 后，调高配置不会恢复本次等待，需要重新调用。
func (p *Pool) WaitConnNumGeq(ctx context.Context, addr net.Addr, l int) <-chan WaitStatus {
	return p.waitCond(ctx, waitNumGeq, addr, l)
}

// WaitConnNumLeq 等到该地址连接数 <= l。l < 0 立即 WaitErrOther。
// 在途拨号已占桶计数，不会把「桶还没建出来」当成已经满足。其余约定同 WaitConnNumGeq。
func (p *Pool) WaitConnNumLeq(ctx context.Context, addr net.Addr, l int) <-chan WaitStatus {
	return p.waitCond(ctx, waitNumLeq, addr, l)
}

// WaitConnIdleGeq 等到该地址空闲连接数 >= l。
// IdleConn > 0 且 l > IdleConn，或 MaxConn > 0 且 l > MaxConn，立即 WaitErrOther。
// 其余约定同 WaitConnNumGeq。
func (p *Pool) WaitConnIdleGeq(ctx context.Context, addr net.Addr, l int) <-chan WaitStatus {
	return p.waitCond(ctx, waitIdleGeq, addr, l)
}

// WaitConnIdleLeq 等到该地址空闲连接数 <= l。l < 0 立即 WaitErrOther。
// 其余约定同 WaitConnNumGeq。
func (p *Pool) WaitConnIdleLeq(ctx context.Context, addr net.Addr, l int) <-chan WaitStatus {
	return p.waitCond(ctx, waitIdleLeq, addr, l)
}

func (p *Pool) getPoolConn(network, address string) (net.Conn, []byte, time.Duration, int, error) {
	ps := p.getPool(network, address)
	if ps == nil {
		return nil, nil, 0, 0, ErrConnNotAvailable
	}
	return ps.get()
}

func (p *Pool) getPoolConnCount(network, address string) int {
	if ps := p.getPool(network, address); ps != nil {
		if n := ps.connTotalCount.Load(); n > 0 {
			return int(n)
		}
	}
	return 0
}

func (p *Pool) getIdleConnCount(network, address string) int {
	ps := p.getPool(network, address)
	if ps == nil {
		return 0
	}
	ps.mu.Lock()
	n := ps.idleCountLocked()
	ps.mu.Unlock()
	return n
}

// getIdle 只在查找时碰 Pool.mu，不盖住后面的 Check / RawConnFull。
// 弹出的连接仍计入总数，桶不会因此被摘掉。
// (nil, nil, ..., nil) 表示没有可用连接（含地址尚不存在），不建桶。
func (p *Pool) getIdle(network, address string) (*pools, net.Conn, []byte, time.Duration, int, error) {
	if p.closed.Load() {
		return nil, nil, nil, 0, 0, ErrConnPoolClosed
	}
	ps := p.getPool(network, address)
	if ps == nil {
		return nil, nil, nil, 0, 0, nil
	}
	conn, b, timeout, bs, err := ps.get()
	if err != nil {
		if errors.Is(err, ErrConnNotAvailable) {
			return nil, nil, nil, 0, 0, nil
		}
		return nil, nil, nil, 0, 0, err
	}
	return ps, conn, b, timeout, bs, nil
}

// addConn 在拨号成功之后调用：取桶与 +1 在同一临界区里完成。
// 失败拨号不会走到这里，map 里不会多出空桶。
func (p *Pool) addConn(addr net.Addr) (*pools, error) {
	if addr == nil {
		return nil, errors.New("vconnpool: nil addr")
	}
	var ps *pools
	err := p.withBucket(addrKey(addr.Network(), addr.String()), func(b *pools) error {
		ps = b
		b.connTotalCountAdd(1)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ps, nil
}

func (p *Pool) Dial(network, address string) (Conn, error) {
	return p.DialContext(context.Background(), network, address)
}

func (p *Pool) DialContext(ctx context.Context, network, address string) (Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if p.closed.Load() {
		return nil, ErrConnPoolClosed
	}

	poolConn, exist := ctx.Value(PoolConnContextKey).(bool)
	wantPool := !exist || poolConn
	addr := &Addr{Net: network, Name: address}

	var (
		ps         *pools
		conn       net.Conn
		b          []byte
		err        error
		fromPool   bool
		putTimeout time.Duration
		putBS      int
	)

	if wantPool {
		ps, conn, b, putTimeout, putBS, err = p.getIdle(network, address)
		if err != nil {
			return nil, err
		}
		if conn != nil {
			if cerr := ctx.Err(); cerr != nil {
				conn.Close()
				p.releaseCount(ps, 1)
				return nil, cerr
			}
			fromPool = true
		}
	}

	if conn == nil && !poolConn {
		if p.closed.Load() {
			return nil, ErrConnPoolClosed
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		putTimeout, putBS = p.conf().IdleTimeout, 0
		if err = p.checkAndIncConnNum(p.conf().MaxConn); err != nil {
			return nil, err
		}
		// 先占桶再拨号，Wait 与 NumConn 才能看见在途连接。
		// 失败必须先 releaseCount 再 dropBucket，顺序不能反。
		ps, err = p.addConn(addr)
		if err != nil {
			p.releaseCount(nil, 1)
			return nil, err
		}
		if p.closed.Load() {
			p.releaseCount(ps, 1)
			p.dropBucket(ps)
			return nil, ErrConnPoolClosed
		}
		conn, err = p.dialNew(ctx, network, address)
		if err != nil {
			p.releaseCount(ps, 1)
			p.dropBucket(ps)
			return nil, err
		}
	}

	if conn == nil || ps == nil {
		if p.closed.Load() {
			return nil, ErrConnPoolClosed
		}
		return nil, ErrConnNotAvailable
	}

	cs := &connSingle{
		vc:         vconn.NewData(conn, b),
		cp:         p,
		ps:         ps,
		addr:       addr,
		isPool:     fromPool,
		laddr:      conn.LocalAddr(),
		raddr:      conn.RemoteAddr(),
		putTimeout: putTimeout,
		putBS:      putBS,
		raw:        conn,
	}
	cs.bufSize.Store(int64(len(b)))

	p.usedMu.Lock()
	if p.closed.Load() {
		p.usedMu.Unlock()
		cs.Discard()
		cs.Close()
		return nil, ErrConnPoolClosed
	}
	if p.usedConns == nil {
		p.usedConns = make(map[*connSingle]struct{}, 8)
	}
	p.usedConns[cs] = struct{}{}
	p.usedMu.Unlock()
	return cs, nil
}

// dialNew 只解析并拨号。
// 调用方必须已经 checkAndIncConnNum 并 addConn；失败由调用方 releaseCount，这里不再改计数。
func (p *Pool) dialNew(ctx context.Context, network, address string) (net.Conn, error) {
	cfg := p.conf()
	var (
		addr net.Addr
		err  error
	)
	if cfg.ResolveAddr != nil {
		addr, err = cfg.ResolveAddr(network, address)
	} else {
		addr, err = ResolveAddr(network, address)
	}
	if err != nil {
		return nil, err
	}
	dialer := cfg.Dialer
	if dialer == nil {
		dialer = defaultDialer
	}
	return dialer.DialContext(ctx, network, addr.String())
}

func (p *Pool) Get(addr net.Addr) (net.Conn, error) {
	conn, b, err := p.GetFull(addr)
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		return vconn.NewData(conn, b), nil
	}
	return conn, nil
}

func (p *Pool) GetFull1(addr net.Addr) (net.Conn, []byte, error) {
	if p.closed.Load() {
		return nil, nil, ErrConnPoolClosed
	}
	if addr == nil {
		return nil, nil, errors.New("vconnpool: address cannot be nil")
	}

	conn, b, _, _, err := p.getPoolConn(addr.Network(), addr.String())
	if err != nil {
		return nil, nil, err
	}
	p.releaseCount(p.getPoolAddr(addr), 1)
	return conn, b, nil
}

func (p *Pool) GetFull(addr net.Addr) (net.Conn, []byte, error) {
	if p.closed.Load() {
		return nil, nil, ErrConnPoolClosed
	}
	if addr == nil {
		return nil, nil, errors.New("vconnpool: address cannot be nil")
	}

	ps, conn, b, _, _, err := p.getIdle(addr.Network(), addr.String())
	if err != nil {
		return nil, nil, err
	}
	if conn == nil {
		return nil, nil, ErrConnNotAvailable
	}
	// 裸连接交给调用方，不再算池持有。桶若因此变空，由清扫器摘掉。
	p.releaseCount(ps, 1)
	return conn, b, nil
}

func (p *Pool) Add(conn net.Conn) error {
	if conn == nil {
		return errors.New("vconnpool: cannot add nil connection")
	}
	return p.Put(conn, conn.RemoteAddr())
}

func (p *Pool) Put(conn net.Conn, addr net.Addr) error {
	return p.putFull(conn, addr, p.conf().IdleTimeout, 0, false)
}

func (p *Pool) PutFull(conn net.Conn, addr net.Addr, timeout time.Duration, bs int) error {
	return p.putFull(conn, addr, timeout, bs, true)
}

func (p *Pool) putFull(conn net.Conn, addr net.Addr, timeout time.Duration, bs int, change bool) error {
	if conn == nil {
		return errors.New("vconnpool: nil conn")
	}
	if addr == nil {
		conn.Close()
		return errors.New("vconnpool: nil addr")
	}
	if p.closed.Load() {
		conn.Close()
		return ErrConnPoolClosed
	}

	if cs, ok := conn.(*connSingle); ok {
		if change {
			cs.mu.Lock()
			cs.putBS = bs
			cs.putTimeout = timeout
			cs.mu.Unlock()
		}
		return cs.Close()
	}

	if ps := p.getPoolAddr(addr); ps != nil {
		switch err := ps.precheck(conn); {
		case errors.Is(err, ErrConnExists):
			return nil
		case errors.Is(err, ErrConnPoolClosed):
			conn.Close()
			return ErrConnPoolClosed
		}
	}

	vc := vconn.New(conn)
	vc.SetBackgroundReadBuffer(bs)
	defer vc.Close()
	if err := p.checkAndIncConnNum(p.conf().MaxConn); err != nil {
		return err
	}
	if err := vc.CheckBlocking(); !reusablePut(vc, err) {
		p.connNum.Add(-1) // 回滚刚借的配额：连接没有入池
		return err
	}

	raw, pending, err := vc.RawConnFull()
	if err != nil {
		p.connNum.Add(-1)
		return err
	}

	if err = p.putIdle(addr, raw, pending, timeout, bs, 1); err != nil {
		p.connNum.Add(-1)
		if errors.Is(err, ErrConnExists) {
			return nil // 已在池中，fd 由池持有，不要关
		}
		raw.Close()
		return err
	}
	return nil
}

func (p *Pool) CloseIdleConnection(addr net.Addr) {
	if addr == nil {
		return
	}
	key := addrKey(addr.Network(), addr.String())
	if ps := p.getPool(addr.Network(), addr.String()); ps != nil {
		ps.clean()
	}
	p.mu.Lock()
	if ps := p.conns[key]; ps != nil {
		p.dropIfEmpty(key, ps)
	}
	p.mu.Unlock()
}

func (p *Pool) CloseIdleConnections() {
	p.mu.RLock()
	list := make([]*pools, 0, len(p.conns))
	for _, ps := range p.conns {
		list = append(list, ps)
	}
	p.mu.RUnlock()

	for _, ps := range list {
		ps.clean()
	}
	p.reapEmpty()
}

func (p *Pool) Close() error {
	if p.closed.Swap(true) {
		return nil
	}

	p.sweepMu.Lock()
	if p.sweepStop != nil {
		close(p.sweepStop)
	}
	p.sweepMu.Unlock()
	p.sweepWg.Wait()

	p.CloseIdleConnections()

	p.usedMu.Lock()
	list := make([]*connSingle, 0, len(p.usedConns))
	for cs := range p.usedConns {
		list = append(list, cs)
	}
	p.usedConns = nil
	p.usedMu.Unlock()

	for _, cs := range list {
		cs.Discard()
		cs.Close()
	}

	p.mu.Lock()
	p.conns = nil
	p.mu.Unlock()

	for {
		cur := p.connNum.Load()
		if cur <= 0 || p.connNum.CompareAndSwap(cur, 0) {
			break
		}
	}
	return nil
}

func (p *Pool) Num() int {
	if n := p.connNum.Load(); n > 0 {
		return int(n)
	}
	return 0 // 计数漂移时不给调用方负数
}

func (p *Pool) NumIdle(addr net.Addr) int {
	if p.closed.Load() || addr == nil {
		return 0
	}
	return p.getIdleConnCount(addr.Network(), addr.String())
}

// NumConn 返回该地址当前占用的连接数，含空闲、在用和在途拨号。
// 拨号失败会回滚。池已关闭或 addr 为 nil 时返回 0。
func (p *Pool) NumConn(addr net.Addr) int {
	if p.closed.Load() || addr == nil {
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
		return nil, fmt.Errorf("vconnpool: unsupported network %q", network)
	}
}

type Addr struct {
	Name string
	Net  string
}

func (a Addr) Network() string { return a.Net }
func (a Addr) String() string  { return a.Name }

type contextKey struct{ name string }

func (p *contextKey) String() string { return "connpool context value " + p.name }

var PoolConnContextKey = &contextKey{"pool-conn"}
