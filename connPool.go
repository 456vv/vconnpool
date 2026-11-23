// Package vconnpool 提供高性能、可复用的网络连接池
package vconnpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
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
	RawConn() net.Conn
}

// ----------------------------------------------------------------------------
// connSingle：单连接包装
// ----------------------------------------------------------------------------

const (
	stAlive  int32 = iota // 可用
	stClosed              // 已关闭（幂等）
	stMoved               // 原始连接已移交
)

type connSingle struct {
	// mu 保护 vc 的快照读取（beginOp 每次操作读一次），以及移交/关闭路径的多字段清空。
	// 热路径只做「一次加锁 + 一次指针读取」，不含任何阻塞调用或 syscall；
	// 进一步去锁（vc 改 atomic.Pointer）属于结构性整改，不在本次热修范围。
	mu sync.Mutex

	vc     *vconn.Conn
	cp     *Pool
	ps     *pools
	addr   net.Addr
	laddr  net.Addr
	raddr  net.Addr
	isPool bool

	// 归还到池时使用的参数：构造时由 DialContext 回填（取自 idleConn
	// 登记的原始值），Close 时原样传回。二者构造后只读，因此无需额外同步。
	putTimeout time.Duration
	putBS      int

	bufSize   atomic.Int64 // 尚未被读走的预读字节数（永不为负，见 readBufSize）
	state     atomic.Int32 // stAlive / stClosed / stMoved
	discard   atomic.Bool  // 底层连接不可复用
	activeOps atomic.Int32 // 在途的 Read/Write/SetDeadline 数量
}

// beginOp 登记一次在途操作并取出当前 vconn。
//
// 正确性说明（与 Close 的协议，务必保持）：
//   - 本函数先 Add(1) 再二次判定 state；
//   - 若二次判定看到 stAlive，则 Close 的 CAS 必然发生在其后，
//     而 Close 的 activeOps.Load() 又在 CAS 之后，
//     因此 Close 一定能看到本次登记 → 置 discard → 不会把连接还回池子。
//     不存在「Read 正在读一条已被归还的连接」的窗口。
func (t *connSingle) beginOp() (*vconn.Conn, error) {
	if t.state.Load() != stAlive {
		return nil, net.ErrClosed
	}
	t.activeOps.Add(1)
	t.mu.Lock()
	vc := t.vc
	t.mu.Unlock()
	if vc == nil || t.state.Load() != stAlive {
		t.activeOps.Add(-1)
		return nil, net.ErrClosed
	}
	return vc, nil
}

// readBufSize 扣减「尚未被读走的预读字节数」，并夹到 0。
//
// 必须夹 0：Read 交付的字节既可能来自预读缓冲，也可能直接来自 socket。
// 若无条件 Add(-n)，第一次真实网络读之后计数就会转负并持续下沉，
// Buffered() 将永远返回 0（假阴性），调用方据此判断「预读是否读干净」会失真。
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
	vc, err := t.beginOp()
	if err != nil {
		return 0, err
	}
	defer t.activeOps.Add(-1)

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
	vc, err := t.beginOp()
	if err != nil {
		return 0, err
	}
	defer t.activeOps.Add(-1)

	n, err := vc.Write(b)
	if err != nil {
		t.errDiscard(err)
	}
	return n, err
}

// errDiscard 依据 vconn 的错误分类决定是否标记「不可复用」。
//
// 用 StatusOf 而不是裸的 net.Error.Timeout()：vconn 已经把「超时（控制流）」
// 与「对端结束/重置/本地关闭/配额耗尽（终结）」分好类，直接复用即可，
// 避免两边判定漂移。
//
// 注意白名单的边界：只有「连接未必坏」的类别才放行。
//   - StatusLatched：通知被手动/上下文取消锁存，连接本身未必坏（B11 修复）；
//   - vconn.ErrUnsupported 不在此列，且**不会**从 Read/Write 返回
//     （它只来自 SetKeepAlive/SetLinger/CloseWrite 等可选方法，不经过本函数），
//     因此不要为了「防止误丢」把它加进来——那只会削弱保守丢弃的默认策略。
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

func (t *connSingle) Close() error {
	if !t.state.CompareAndSwap(stAlive, stClosed) {
		return nil // 幂等；或已被 RawConn 移交（由移交方负责收尾）
	}
	// 关闭瞬间仍有在途操作 → 连接状态不可信，直接丢弃
	if t.activeOps.Load() > 0 {
		t.discard.Store(true)
	}

	t.mu.Lock()
	putTimeout, putBS := t.putTimeout, t.putBS
	vc, cp, ps, addr := t.vc, t.cp, t.ps, t.addr
	t.vc, t.cp, t.ps, t.addr = nil, nil, nil, nil
	t.mu.Unlock()

	if vc == nil || cp == nil {
		return nil // 已被其它路径收尾，避免空指针
	}

	if !t.discard.Load() {
		// 判定统一走**入池轴** reusablePut，与 PutFull 同一条规则。
		if err := vc.CheckReusable(); reusablePut(vc, err) {
			// RawConnFull：解除所有权 + 取走预读字节（零丢失）。
			//
			// 走到这里缓冲通常为空。pending 非空只可能是「探活之后、移交之前」
			// 新到的一批字节 —— 准入判定时缓冲为空，说明它是本条连接的干净流内容，
			// 交给 put 回灌不违反入池策略；直接丢反而会切掉对端刚发来的数据。
			raw, pending := vc.RawConnFull()
			err := cp.putPoolConn(raw, pending, addr, putTimeout, putBS)
			if err == nil {
				// 入池成功：从在用记录删除；配额不递减（连接仍在池内）
				cp.removeUsedConn(t)
				return nil
			}
			cp.releaseCount(ps, 1)
			cp.removeUsedConn(t)
			if errors.Is(err, ErrConnExists) {
				return nil
			}
			if raw != nil {
				if err := raw.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					return err
				}
			}
			return nil
		}
		// 其它失败（判脏：对端结束/重置/本地关闭/配额耗尽/缓冲残留等，
		// 或入池前置判定未通过）
	}

	cp.releaseCount(ps, 1)
	cp.removeUsedConn(t)
	if err := vc.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func (t *connSingle) LocalAddr() net.Addr  { return t.laddr }
func (t *connSingle) RemoteAddr() net.Addr { return t.raddr }

// doDeadline 统一处理三个 deadline 方法：参与 activeOps 记账，
// 避免在归还瞬间把 deadline 写到已经回到池中的连接上。
func (t *connSingle) doDeadline(fn func(*vconn.Conn) error) error {
	vc, err := t.beginOp()
	if err != nil {
		return err
	}
	defer t.activeOps.Add(-1)
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

// SetReadBuffer 设置操作系统接收缓冲区大小（仅 TCP/Unix 等支持该选项的连接有效）
//
// 与 vconn.Conn 对齐：底层不支持时返回 vconn.ErrUnsupported。
// 该错误不经过 errDiscard，因此不会把连接判成不可复用。
func (t *connSingle) SetReadBuffer(bytes int) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetReadBuffer(bytes) })
}

// SetWriteBuffer 设置操作系统发送缓冲区大小（仅 TCP/Unix 等支持该选项的连接有效）
func (t *connSingle) SetWriteBuffer(bytes int) error {
	return t.doDeadline(func(vc *vconn.Conn) error { return vc.SetWriteBuffer(bytes) })
}

// SetNoDelay 设置 TCP_NODELAY。
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

// Buffered 返回尚未被读走的预读字节数（调用方据此判断是否已读干净）
//
// 计数侧由 readBufSize 保证非负，这里的 >0 判定只是防御性兜底。
func (t *connSingle) Buffered() int {
	if n := t.bufSize.Load(); n > 0 {
		return int(n)
	}
	return 0
}

// RawConn 剥离包装并归还底层连接。
func (t *connSingle) RawConn() net.Conn {
	conn, _, err := t.moveOut()
	if err != nil {
		panic(err)
	}
	return conn
}

// RawConnFull 归还底层连接并交出预读字节（零丢失）。
// 与 vconn.Conn.RawConnFull 语义一致，连接池/代理转发请优先使用。
func (t *connSingle) RawConnFull() (net.Conn, []byte) {
	conn, data, err := t.moveOut()
	if err != nil {
		panic(err)
	}
	return conn, data
}

// moveOutStalled 迁移前的在途操作等待总轮数。
// 前 32 轮只做 Gosched：短操作（刚写完 / 刚返回）通常几十轮内就退出，
// 让出 P 的成本远低于睡眠。
// 之后 32 轮改为 50µs Sleep：Gosched 对阻塞在 syscall 里的 Read 毫无推进，
// 继续空转只是白烧 CPU。合计上限 32×Gosched + 32×50µs ≈ 1.6ms
// （睡眠粒度受平台影响，实际可能略高）。
// 上限之后不再等：RawConnFull 会用「过去时间 deadline」打断仍阻塞的读
// （其文档明确允许所有权转移打断并发读），绝不无限等待拖住调用方。
const moveOutStalled = 64

func (t *connSingle) moveOut() (net.Conn, []byte, error) {
	if !t.state.CompareAndSwap(stAlive, stMoved) {
		if t.state.Load() == stMoved {
			return nil, nil, ErrConnRAWRead
		}
		return nil, nil, net.ErrClosed
	}
	for i := 0; i < moveOutStalled && t.activeOps.Load() > 0; i++ {
		if i < moveOutStalled/2 {
			runtime.Gosched()
		} else {
			time.Sleep(50 * time.Microsecond)
		}
	}
	// state 已是 stMoved，Close 不会再走到读 discard 的分支，此后无任何决策
	// 依赖该标记；置位只是让「移交瞬间仍有在途操作」这一事实与 discard 语义一致
	//（Discard() 也置它，两者此后都只作状态记录）。
	if t.activeOps.Load() > 0 {
		t.discard.Store(true)
	}

	t.mu.Lock()
	vc, cp, ps := t.vc, t.cp, t.ps
	t.vc, t.cp, t.ps, t.addr = nil, nil, nil, nil
	t.mu.Unlock()
	if vc == nil {
		return nil, nil, net.ErrClosed
	}

	// 必须在 RawConnFull 之前采样：打断可能在它返回前就把 activeOps 降到 0，
	// 之后再采样就分不清「本来就没人用」和「刚被打断」。
	interrupted := t.activeOps.Load() > 0
	raw, data := vc.RawConnFull()
	if interrupted && raw != nil {
		// 过去时间 deadline 已武装，被阻塞的读应在微秒级返回；20×50µs 只是兜底。
		// 仍未退出时必须保留该 deadline：此刻清掉等于取消打断，卡在 syscall 里的
		// 读再也不会返回，new owner 的首读也会永久阻塞在同一个 fd 上。
		// 无在途操作时不清，避免抹掉调用方自己设置的超时。
		for i := 0; i < 20 && t.activeOps.Load() > 0; i++ {
			time.Sleep(50 * time.Microsecond)
		}
		if t.activeOps.Load() == 0 {
			raw.SetDeadline(time.Time{})
		}
	}
	if cp != nil {
		cp.releaseCount(ps, 1)
		cp.removeUsedConn(t)
	}
	return raw, data, nil
}

// ----------------------------------------------------------------------------
// idleConn / pools：按地址的空闲队列
// ----------------------------------------------------------------------------

// idleConn 一条空闲连接。
type idleConn struct {
	conn     net.Conn
	vc       *vconn.Conn
	putAt    time.Time
	expireAt time.Time // 零值 = 永不过期

	// 入池时登记的归还参数，取出时原样交回 connSingle，
	// 保证「PutFull 自定义 timeout/bs → 取出 → 再归还」不会被抹成默认值。
	timeout time.Duration
	bs      int
}

func (ic *idleConn) expired(now time.Time) bool {
	return !ic.expireAt.IsZero() && !now.Before(ic.expireAt)
}

type pools struct {
	cp *Pool

	mu      sync.Mutex
	idle    []*idleConn // 环形队列：head 之前是已弹出的空位
	head    int
	present map[net.Conn]struct{} // O(1) 查重

	connIdleGeq map[int][]chan bool
	connIdleLeq map[int][]chan bool
	connNumGeq  map[int][]chan bool
	connNumLeq  map[int][]chan bool

	connTotalCount atomic.Int64 // 该地址当前持有的总连接数（空闲 + 在用）

	// cleanSeq 是 clean() 的代际，仅由 p.mu 保护。
	// removeIdle 摘除时记下当时的值，restoreIdle 放回前必须相等：
	// clean 之后扫描器不得把「清理前就在队列里」的连接放回队列。
	// 不能用 present==nil 代替：put 会把 present 重建，
	// 于是 CloseIdleConnections 返回后，被扫描器摘走的那条空闲连接会被复活。
	cleanSeq uint64

	// minExpire 是该桶内所有空闲连接最早的 expireAt（UnixNano，0 = 无）。
	// 它只决定扫描器何时醒来；精确过期判定仍是 ic.expired(now)。
	minExpire atomic.Int64
}

func (p *pools) idleCountLocked() int { return len(p.idle) - p.head }

// pushLocked 入队；head 过半时做一次紧凑化，避免底层数组无限右移。
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

// noteExpireLocked 只在「新连接更早到期」时下推 minExpire。
// 绝大多数 put 的 expireAt 都晚于当前最小值，因此通常只做一次 Load 就返回。
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

// recomputeMinLocked 全量重算 minExpire（调用方必须持有 p.mu）。
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

// refreshMinLocked 在摘除一条连接后维护 minExpire。
//
// 只有「被摘除的到期时间正是当前最小值」时才需要 O(n) 重扫；
// 其余情况 minExpire 仍指向队列里一条真实存在的连接，直接返回（摊还 O(1)）。
// 这条判定保证 minExpire 不会长期指向已不存在的连接（否则扫描器会忙等）。
func (p *pools) refreshMinLocked(gone time.Time) {
	if gone.IsZero() || p.minExpire.Load() != gone.UnixNano() {
		return
	}
	p.recomputeMinLocked()
}

// popLocked 弹出队首（FIFO，保证 IdleTimeout 的「最老优先淘汰」语义）
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

// removeLocked 从队列中摘除指定元素（扫描器使用，O(n) 但频率低）
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

// wakeLocked 向一组等待者非阻塞投递结果
func wakeLocked(chs []chan bool, ok bool) {
	for _, ch := range chs {
		select {
		case ch <- ok:
		default:
		}
	}
}

// notifyAllLocked 按「谓词重新求值」唤醒等待者。
func (p *pools) notifyAllLocked() {
	idle := p.idleCountLocked()
	num := int(p.connTotalCount.Load())

	for l, chs := range p.connIdleGeq {
		if idle >= l {
			wakeLocked(chs, true)
			delete(p.connIdleGeq, l)
		}
	}
	for l, chs := range p.connIdleLeq {
		if idle <= l {
			wakeLocked(chs, true)
			delete(p.connIdleLeq, l)
		}
	}
	for l, chs := range p.connNumGeq {
		if num >= l {
			wakeLocked(chs, true)
			delete(p.connNumGeq, l)
		}
	}
	for l, chs := range p.connNumLeq {
		if num <= l {
			wakeLocked(chs, true)
			delete(p.connNumLeq, l)
		}
	}
}

// wakeAllLocked 清理/关闭时唤醒等待者。
//
// abort=true（池已关闭）：一律投递 false 并清空注册表 —— 条件永不可能再成立。
// abort=false（只清空空闲连接，池仍可用）：只投递并摘除**已经成立**的等待者。
// 未成立的必须保留注册：clean 之后 idle 归零，connIdleGeq(l>=1) 的等待者只是
// 「现在不成立」，下一次 put 就会成立；这里若投 false，调用方会把它当成
// 「永不可能成立」而放弃等待，之后真正成立时再也等不到。
//
// 唯一确实不可能成立的是 Leq 系列里的负阈值（idle / num 恒 ≥ 0），
// 对它投 false 解除等待，避免注册项一直挂到池关闭才被清理。
func (p *pools) wakeAllLocked(abort bool) {
	idle := p.idleCountLocked()
	num := int(p.connTotalCount.Load())

	for l, chs := range p.connIdleGeq {
		if abort || idle >= l {
			wakeLocked(chs, !abort && idle >= l)
			delete(p.connIdleGeq, l)
		}
	}
	for l, chs := range p.connIdleLeq {
		if abort || l < 0 || idle <= l {
			wakeLocked(chs, !abort && idle <= l)
			delete(p.connIdleLeq, l)
		}
	}
	for l, chs := range p.connNumGeq {
		if abort || num >= l {
			wakeLocked(chs, !abort && num >= l)
			delete(p.connNumGeq, l)
		}
	}
	for l, chs := range p.connNumLeq {
		if abort || l < 0 || num <= l {
			wakeLocked(chs, !abort && num <= l)
			delete(p.connNumLeq, l)
		}
	}
	if abort {
		p.connIdleGeq, p.connIdleLeq, p.connNumGeq, p.connNumLeq = nil, nil, nil, nil
	}
}

// connTotalCountAdd 原子更新该地址的总连接数并唤醒等待者（不持锁调用）
func (p *pools) connTotalCountAdd(n int64) {
	p.connTotalCount.Add(n)
	p.mu.Lock()
	p.notifyAllLocked()
	p.mu.Unlock()
}

// unverifiedUsable 对 StatusUnverified 做「零成本交叉校验」：
// 非阻塞探测给不出答案时，只要 vconn 已落定「对端断开」或「本地已关闭」
// （监控协程、早前的读写探测都可能落定），就必须拒绝 —— 否则 net.Pipe / TLS /
// 自定义 Conn 这些无 fd 连接的空闲死连接会被反复出借，调用方要等第一次
// Write 失败才发现。只做「拒绝」，绝不因 Unverified 主动判死：无法判定 ≠ 已断开。
func unverifiedUsable(vc *vconn.Conn) bool {
	if vc.IsPeerClosed() {
		return false
	}
	if vc.Snapshot() == vconn.StatusLocalClosed {
		return false
	}
	return true
}

// reusableIdle 判定一条空闲连接能否交给下一个使用者（出借轴）。
//
// 只用于「池 → 调用方」方向：get / sweep。本方向上的 StatusDataPending
// 不是「脏」——连接在池内空闲期间（bs > 0 的后台预读）累积的数据，
// 属于这条连接的流内容，会经 RawConnFull 随连接一并交出，
// 再由 Get / GetFull 交付给调用方，零丢失。
//
// StatusUnverified 仍附加 unverifiedUsable 交叉校验。
//
// 注意：入池方向（调用方 → 池）必须用 reusablePut。两个方向对
// StatusDataPending 的结论相反：出借时它是空闲期预读，入池时它是
// 上一个使用者留在缓冲里的未读字节。
func reusableIdle(vc *vconn.Conn, err error) bool {
	switch st := vconn.StatusOf(err); st.Class() {
	case vconn.StatusAlive, vconn.StatusDataPending:
		return true
	case vconn.StatusUnverified:
		return unverifiedUsable(vc)
	}
	return false
}

// reusablePut 判定一条「待入池」的连接能否进入空闲队列（入池轴）。
//
// 入池路径（PutFull、connSingle.Close 的归还分支）只能用本函数，
// 这是全池唯一的入池准入标准。与 reusableIdle 的差别只有 StatusDataPending：
//
// 入池时刻缓冲里的未读字节来自上一个使用者（响应只读了一半就归还、失步的帧、
// 自己开了后台预读却没读完等）。池无法区分它与合法流内容，因此一律拒绝：
//   - 放行的后果不是「多几条连接」，而是下一位使用者经 Get 读到的第一批字节
//     就是上一个人的残留（Get 把 pending 静默垫在 Read 流前面）→ 协议失步，
//     且错误要到很晚才暴露；
//   - 拒绝的后果只是少复用一条本就不该复用的连接。
//
// 这不影响零丢失承诺：连接在池内**空闲期**的预读数据仍由 Get/GetFull 交付，
// 那条路径走出借轴 reusableIdle。
//
// StatusUnverified 与 reusableIdle 相同：附加 unverifiedUsable 交叉校验。
// 本函数只看错误分类，与探活用的是 Check 还是 CheckReusable 无关。
func reusablePut(vc *vconn.Conn, err error) bool {
	switch st := vconn.StatusOf(err); st.Class() {
	case vconn.StatusAlive:
		return true
	case vconn.StatusUnverified:
		return unverifiedUsable(vc)
	}
	return false
}

// precheck 做一次「不碰 socket」的准入判定（内部自行加锁，调用方不得持锁）。
//
// 它的存在只是为了让「注定失败的归还」尽早被拒掉，省一次加锁与一次 vconn
// 包装分配；真正的最终判定仍由 put 的锁内复核完成。
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
	if p.cp.IdleConn > 0 && p.idleCountLocked() >= p.cp.IdleConn {
		return ErrConnIdleMax
	}
	return nil
}

// put 把连接放回空闲队列（调用方保证 conn 的所有权已经转移给本池）。
//
// 入池准入必须由调用方先用 reusablePut 判定（PutFull 与 connSingle.Close
// 均如此）。本函数只做「池已关闭 / 重复 / 空闲上限」的簿记与包装重建，
// 不再重复探活：既拿不到 vconn 的 socket 状态，也不允许在锁内做 syscall。
func (p *pools) put(conn net.Conn, pending []byte, timeout time.Duration, bs int) error {
	if conn == nil {
		return errors.New("vconnpool: nil conn")
	}

	// 快速拒绝（省一次加锁 + 一次包装分配）。
	if err := p.precheck(conn); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 上面释放过锁，期间可能有并发 put / clean / Close，必须重新判定。
	// 本段只做 map 操作与内存分配，不执行任何 syscall。
	if p.cp.closed.Load() {
		return ErrConnPoolClosed
	}
	if p.present == nil {
		p.present = make(map[net.Conn]struct{}, 8)
	}
	if _, ok := p.present[conn]; ok {
		return ErrConnExists
	}
	if p.cp.IdleConn > 0 && p.idleCountLocked() >= p.cp.IdleConn {
		return ErrConnIdleMax
	}

	vc := vconn.NewData(conn, pending)
	if bs != 0 {
		// bs != 0 表示调用方希望空闲期间预读对端数据（GetFull 会把它交给下一个使用者）。
		// vconn 的后台预读由监控协程驱动，CloseNotify 是启动监控的唯一入口；
		// 订阅本身无需消费——RawConnFull/Close 都会 fire 并关闭该 channel。
		//
		// 代价提示：每条 bs != 0 的空闲连接都会常驻一个 vconn 监控协程
		// （默认 200ms 一轮 poll）。空闲连接很多时请评估 CPU 占用，
		// 不需要「空闲期预读」时传 bs = 0。
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
	p.notifyAllLocked()
	return nil
}

// get 取出一条可用连接，并把它入池时登记的归还参数一并交回。
//
// 所有可能阻塞的调用（Check / RawConnFull / Close / SetDeadline）都在
// pools.mu 之外执行。
func (p *pools) get() (net.Conn, []byte, time.Duration, int, error) {
	for {
		// 池已关闭：立刻停止出借，且不得消费空闲队列
		// （否则 Close 的「返回后计数归零」语义会被破坏，计数还可能被扣成负数）。
		if p.cp.closed.Load() {
			return nil, nil, 0, 0, ErrConnPoolClosed
		}

		p.mu.Lock()
		// 与 popLocked 合并进同一临界区：否则 Close 的 CloseIdleConnections
		// 可能在两次判定之间清空队列并返回，而本协程仍把连接交给调用方，
		// 违反「Close 返回后不再出借」的承诺。
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

		// 空闲超时必须在出借前校验：扫描器只是「兜底回收」，不能当作唯一防线。
		// sweepInterval 上限 30s，只靠它会让 IdleTimeout 的严格性退化成一个
		// 扫描周期——长超时场景最多超期 30s，短超时场景最多超期一倍。
		// 过期连接在这里就地关闭并归还配额，然后继续尝试下一条。
		if ic.expired(time.Now()) {
			p.discardOwned(ic)
			continue
		}

		// 出借判定用观测轴（reusableIdle）：空闲期累积的数据随连接一并交出。
		err := ic.vc.Check()
		if reusableIdle(ic.vc, err) {
			conn, b := ic.vc.RawConnFull()
			// 出借前清零 deadline：归还路径不再碰 socket，
			// 由这里保证「上一个使用者的超时不会污染下一个使用者」。
			// 放在 RawConnFull 之后（fg 已释放、无并发监控）最安全。
			if derr := conn.SetDeadline(time.Time{}); derr != nil {
				conn.Close()
				p.cp.releaseCount(p, 1)
				continue
			}
			return conn, b, ic.timeout, ic.bs, nil
		}
		// 不健康：关闭并归还配额，继续尝试下一条
		p.discardOwned(ic)
	}
}

// removeIdle 尝试从空闲队列摘除（并发安全）；返回摘除时的代际，以及是否真的由本调用摘除。
//
// 本函数**故意不唤醒等待者**：摘除只是扫描器的临时独占，健康连接会被
// restoreIdle 原样放回。若此处唤醒，WaitConnIdleLeq 会看到瞬时低值而
// 收到假阳性（例：idle=1 时 WaitConnIdleLeq(0) 立即成功，但连接随后又回来了）。
// 真正的唤醒由 restoreIdle（放回）或 discardOwned→releaseCount（永久摘除）负责。
func (p *pools) removeIdle(ic *idleConn) (seq uint64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.removeLocked(ic) {
		return p.cleanSeq, false
	}
	delete(p.present, ic.conn)
	return p.cleanSeq, true
}

// discardOwned 关闭一条**已经确定由本调用方独占**的空闲连接并归还配额。
//
// 调用前必须已完成摘除且该连接不再出现在队列中，具体为以下三种之一：
//   - popLocked 返回了它；
//   - removeIdle 返回 true；
//   - sweepExpired 的压缩摘除已把它从 p.idle / p.present 中移除。
//
// 否则会重复记账（releaseCount 被调用两次）。
func (p *pools) discardOwned(ic *idleConn) {
	ic.vc.Close()
	p.cp.releaseCount(p, 1)
}

// snapshotIdle 在锁内快照空闲队列，供扫描器在锁外逐个做可能阻塞的 Check()。
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

// sweep 清理该桶的过期/失效空闲连接。
//
// health=true 时额外做健康探活（判定与 get 一致，使用观测轴 reusableIdle）；
// health=false 时只做过期回收 —— 供 deadline 精确唤醒使用，
// 避免「每条连接到期都触发一轮全量 Check()」。
func (p *pools) sweep(now time.Time, health bool) {
	p.sweepExpired(now)
	if !health {
		return
	}
	for _, ic := range p.snapshotIdle() {
		// 先摘除取得独占权，再探活。
		//
		// snapshotIdle 只复制指针：get 可能在快照之后立刻 popLocked 并把这条连接
		// 交给调用方（随后在锁外 Check / RawConnFull）。若本协程仍按陈旧快照直接
		// ic.vc.Check()，两者会并发操作同一个 *vconn.Conn —— 轻则 -race 报警，
		// 重则探活读走调用方尚未读到的首字节（丢包 / 协议失步）。
		// removeIdle 在锁内完成「摘除 + 从 present 删除」，返回 true 即表示
		// 本协程已独占该连接，此后不会再被 get 取走。
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
				// 同一个 net.Conn 已被池重新持有：fd 归池，只归还多出来的配额
				p.cp.releaseCount(p, 1)
			}
		}
	}
}

// restoreIdle 把「已被 removeIdle 摘除且探活通过」的空闲连接放回队列。
//
// 与 put 的区别：不重建 vconn 包装（保留预读缓冲与监控协程），也不改动
// putAt / expireAt —— 过期判定用绝对值，放回队尾只影响「最老优先淘汰」的顺序，
// 不影响正确性。
//
// seq 必须是 removeIdle 返回的代际：期间发生过 clean 就不能再放回，
// 因为那条连接在 CloseIdleConnections 语义下已经应该消失。
//
// 返回 (ok, closeConn)：
//   - ok == true   ：已成功放回队列；
//   - ok == false  ：无法放回。closeConn == true 表示连接已无主，调用方必须关闭
//     并归还配额；closeConn == false 表示同一个 net.Conn 已被池重新持有，
//     fd 归池，调用方只能归还配额、绝不能关闭。
//
// 调用方必须已通过 removeIdle 取得独占权。
func (p *pools) restoreIdle(ic *idleConn, seq uint64) (ok bool, closeConn bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 池已关闭、队列已被 clean() 清空、或代际已变：不能再塞回队列。
	// present==nil 仍保留，作为「未来有路径清空 present 却忘了递增 cleanSeq」的兜底。
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

// sweepExpired 摘除并关闭该桶内全部已过期的空闲连接。
//
// 锁内原地压缩：一遍扫过队列，只对真正摘除的连接分配切片，
// 避免「先快照整个队列（O(n) 分配）再逐条 removeIdle（O(n²)）」。
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

// clean 关闭该地址的全部空闲连接。
func (p *pools) clean() {
	p.mu.Lock()
	idle := p.idle[p.head:]
	p.idle, p.head = nil, 0
	p.present = nil
	p.minExpire.Store(0)
	// 无条件递增，即使本次队列为空。
	// 队列为空时若不递增，就存在这条路径：扫描器已 removeIdle 摘走唯一一条
	// → clean 无连接可清（n==0）→ restoreIdle 时代际仍相等，把它放回队列。
	// 那条连接在 CloseIdleConnections 返回时属于「空闲」却存活了下来。
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

// ---- 等待器注册（调用方未持锁）----

func (p *pools) geqConnIdleWait(l int) <-chan bool {
	ch := make(chan bool, 1)
	p.mu.Lock()
	if p.cp.closed.Load() {
		p.mu.Unlock()
		ch <- false
		return ch
	}
	if p.idleCountLocked() >= l {
		p.mu.Unlock()
		ch <- true
		return ch
	}
	if p.connIdleGeq == nil {
		p.connIdleGeq = make(map[int][]chan bool)
	}
	p.connIdleGeq[l] = append(p.connIdleGeq[l], ch)
	p.mu.Unlock()
	return ch
}

func (p *pools) leqConnIdleWait(l int) <-chan bool {
	ch := make(chan bool, 1)
	p.mu.Lock()
	if p.cp.closed.Load() {
		p.mu.Unlock()
		ch <- false
		return ch
	}
	if p.idleCountLocked() <= l {
		p.mu.Unlock()
		ch <- true
		return ch
	}
	if p.connIdleLeq == nil {
		p.connIdleLeq = make(map[int][]chan bool)
	}
	p.connIdleLeq[l] = append(p.connIdleLeq[l], ch)
	p.mu.Unlock()
	return ch
}

func (p *pools) geqConnNumWait(l int) <-chan bool {
	ch := make(chan bool, 1)
	p.mu.Lock()
	if p.cp.closed.Load() {
		p.mu.Unlock()
		ch <- false
		return ch
	}
	if int(p.connTotalCount.Load()) >= l {
		p.mu.Unlock()
		ch <- true
		return ch
	}
	if p.connNumGeq == nil {
		p.connNumGeq = make(map[int][]chan bool)
	}
	p.connNumGeq[l] = append(p.connNumGeq[l], ch)
	p.mu.Unlock()
	return ch
}

func (p *pools) leqConnNumWait(l int) <-chan bool {
	ch := make(chan bool, 1)
	p.mu.Lock()
	if p.cp.closed.Load() {
		p.mu.Unlock()
		ch <- false
		return ch
	}
	if int(p.connTotalCount.Load()) <= l {
		p.mu.Unlock()
		ch <- true
		return ch
	}
	if p.connNumLeq == nil {
		p.connNumLeq = make(map[int][]chan bool)
	}
	p.connNumLeq[l] = append(p.connNumLeq[l], ch)
	p.mu.Unlock()
	return ch
}

// ----------------------------------------------------------------------------
// Pool：连接池主体
// ----------------------------------------------------------------------------

var defaultDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

// Pool 连接池。
//
// 并发约定：Dialer / ResolveAddr / IdleConn / IdleTimeout / MaxConn 是**配置项**，
// 必须在任何 Dial 之前设置完毕；运行期修改它们会产生数据竞争。
type Pool struct {
	Dialer      Dialer
	ResolveAddr func(network, address string) (net.Addr, error)
	IdleConn    int
	IdleTimeout time.Duration
	MaxConn     int

	connNum atomic.Int64
	conns   map[string]*pools
	mu      sync.RWMutex

	usedMu    sync.Mutex
	usedConns map[*connSingle]struct{}

	closed atomic.Bool

	sweepOnce sync.Once
	// 保护 sweepStop 的创建与关闭，并把 sweepWg.Add 与 Wait 串行化。
	// 它是叶子锁：持有时不获取任何其它锁。
	sweepMu   sync.Mutex
	sweepStop chan struct{}

	// sweepWake 是「可能有更早的到期时间」的唤醒通道（缓冲 1，非阻塞投递）。
	// 用 atomic.Pointer 而非受 sweepMu 保护，是为了让归还路径（高频）
	// 不抢全局锁；也不依赖「调用点必在 ensureSweeper 之后」这一隐式约定。
	sweepWake atomic.Pointer[chan struct{}]

	// sweepNext 是扫描协程「当前已排定的下次醒来时刻」（UnixNano）：
	//   > 0 → 正在睡到该时刻；
	//   == 0 → 正在重算（或尚未启动），此时任何新到期时间都必须唤醒它。
	// 它只服务「要不要唤醒」的启发式判定，不参与正确性：真正的睡眠时长
	// 每轮都由 nextExpiry() 重算，漏唤醒最多让某个到期晚一轮基准节拍。
	sweepNext atomic.Int64

	sweepWg sync.WaitGroup
}

// 编译期接口断言：connSingle 必须完整实现 Conn / net.Conn，Pool 必须是 io.Closer。
var (
	_ Conn      = (*connSingle)(nil)
	_ net.Conn  = (*connSingle)(nil)
	_ io.Closer = (*Pool)(nil)
)

// addrKey 生成地址桶的键。
//
// 必须做网络名归一：Pool.Add / Put(conn, conn.RemoteAddr()) 走的是
// net.Addr.Network()，而 *net.TCPAddr / *net.UDPAddr / *net.IPAddr 的
// Network() 恒为 "tcp" / "udp" / "ip"（不带 4/6 后缀）。
// 若 DialContext("tcp4", ...) 直接按 "tcp4" 建键，同一目标会被拆成两个桶：
//   - Dial 取到的桶与 Put(conn.RemoteAddr()) 归还的桶不同 → 复用率下降；
//   - NumConn / NumIdle 的统计按桶读取，会出现「明明归还了却统计为 0」。
//
// 归一后 tcp/tcp4/tcp6 共用一个桶，与 net.Addr.Network() 的取值完全对齐。
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

// loadPool 获取或创建指定地址的 pools 实例。
//
// 池已关闭时返回 **nil**（而不是新建桶）：否则 Pool.Close 把 p.conns 置空之后，
// 一条并发路径（DialContext 取桶、拨号成功后累加地址计数）就能把桶重新建出来，
// 造成「已关闭的池被复活」——桶常驻内存、计数写回死池，
// Close 承诺的「返回后计数归零」也会被击穿。
//
// 所有调用方都必须能安全处理 nil 返回值（见 DialContext / putPoolConn）。
func (p *Pool) loadPool(addr net.Addr) *pools {
	if addr == nil {
		return nil
	}
	key := addrKey(addr.Network(), addr.String())

	p.mu.RLock()
	if p.closed.Load() {
		p.mu.RUnlock()
		return nil
	}
	if ps, ok := p.conns[key]; ok {
		p.mu.RUnlock()
		return ps
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	// 写锁内二次判定：读锁到写锁之间 Close 可能已经跑完
	//（closed 置位 + conns 置空），此时绝不能再建桶。
	if p.closed.Load() {
		return nil
	}
	if p.conns == nil {
		p.conns = make(map[string]*pools)
	}
	if ps, ok := p.conns[key]; ok {
		return ps
	}
	ps := &pools{cp: p, present: make(map[net.Conn]struct{}, 8)}
	p.conns[key] = ps
	return ps
}

// getPoolAddr 按 net.Addr 做只读查找（不建池、nil 安全）。
func (p *Pool) getPoolAddr(addr net.Addr) *pools {
	if addr == nil {
		return nil
	}
	return p.getPool(addr.Network(), addr.String())
}

// getPool 只读查找（不创建），用于统计类接口与配额记账。
func (p *Pool) getPool(network, address string) *pools {
	key := addrKey(network, address)
	p.mu.RLock()
	ps := p.conns[key]
	p.mu.RUnlock()
	return ps
}

// ---- 共享扫描器：O(1) 个 goroutine 取代「每空闲连接 1 goroutine + 1 Timer」----
const (
	// sweepMinSleep 是单轮睡眠下限，防止「到期时刻已到但 expired() 仍为 false」
	// （定时器可能早于 expireAt 数纳秒触发）导致定时器自旋。
	//
	// 取 5ms 而不是 1ms：get() 出借前会独立校验 expired()，扫描器只是兜底回收，
	// 回收晚 5ms 不影响 IdleTimeout 语义。1ms 会在到期密集时让扫描协程约 1000Hz
	// 醒来，每轮 RLock 扫全部地址桶；5ms 把同一批到期的连接并成一轮处理。
	sweepMinSleep = 5 * time.Millisecond
)

func (p *Pool) ensureSweeper() {
	p.sweepOnce.Do(func() {
		p.sweepMu.Lock()
		defer p.sweepMu.Unlock()
		// 池已关闭：不再启动扫描器（Close 也不会再等它）。
		if p.closed.Load() {
			return
		}
		wake := make(chan struct{}, 1)
		p.sweepStop = make(chan struct{})
		p.sweepWake.Store(&wake)
		p.sweepWg.Add(1)
		go p.sweepLoop(wake)
	})
}

// wakeSweeperAt 通知扫描器「有一条连接将在 deadline 到期」，让它重算睡眠时长。
//
// 条件投递：只有 deadline 早于扫描器当前排定的醒来时刻才投递。否则每次 Put
// 都唤醒一次，写密集场景下扫描协程会以 Put 速率空转（每轮一次 RLock + 逐桶读 atomic）。
//
// 丢失唤醒的防护：sweepLoop 每轮重算前把 sweepNext 置 0。于是对任意一次 Put：
//   - 入队发生在扫描器读 minExpire 之前 → 扫描器算出的 d 已覆盖该到期；
//   - 入队发生在之后 → Put 要么看到 sweepNext==0 而投递，要么看到旧值而投递，
//     要么发现缓冲已满 —— 而缓冲满意味着必有一条待处理唤醒，扫描器消费后重算，
//     此时该连接已在队列里。
//
// 因此不存在「入队后永不重算」的窗口。
//
// 调用方必须已释放 pools.mu（本函数不取任何锁）。
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

// nextExpiry 返回所有桶中最早的 idleConn.expireAt。
//
// 桶数 = 不同地址数，通常只有几十个，逐桶读一个 atomic 可忽略；
// 若地址数上千，可换成全局最小堆，但不必为此改变扫描循环的形态。
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

// sweepInterval 返回扫描器的「基准节拍」，承担两个职责：
//
//  1. 健康探活（ic.vc.Check）的最长间隔 —— IdleTimeout == 0 时仍然需要它：
//     net.Pipe / TLS 这类无 fd 连接的死连接只能靠这里发现；
//  2. 单轮睡眠的上限（无任何 deadline 可参照时的兜底长睡）。
//
// 它不再决定过期回收的精度：过期回收由 nextExpiry 的 deadline 驱动，
// 扫描器会睡到「最早到期时间」与「基准节拍」二者中更早的那个。
func (p *Pool) sweepInterval() time.Duration {
	iv := 5 * time.Second
	if p.IdleTimeout > 0 {
		iv = p.IdleTimeout / 2
	}
	if iv < 100*time.Millisecond {
		iv = 100 * time.Millisecond
	}
	if iv > 30*time.Second {
		iv = 30 * time.Second
	}
	return iv
}

// sweepLoop 是唯一的扫描协程（O(1) 个 goroutine，无 per-conn Timer）。
//
// 睡眠时长 = min(距下次健康探活的时间, 最早一条空闲连接的 expireAt)，
// 下限 sweepMinSleep（5ms：把同时到期的连接并成一批处理）。
// 醒来后按需分流：到点的是到期 → 只做到期回收（不碰 socket）；
// 到点的是基准节拍 → 做全量（到期回收 + 健康探活）。
// 这个分流是必需的：到期时刻通常远密于基准节拍，若每次到期都跑 Check()，
// 探活频率会被放大到与 put/get 同阶。
//
// p.IdleTimeout 等是配置项（Dial 前设置完毕），运行期修改属数据竞争。
func (p *Pool) sweepLoop(wake <-chan struct{}) {
	defer p.sweepWg.Done()
	if p.closed.Load() {
		return
	}

	// 占位 timer：循环首轮就 Reset。避免「NewTimer(interval) 后立刻 Reset」
	// 在 Go 1.23 之前因未 drain 而提前触发。
	tm := time.NewTimer(time.Hour)
	tm.Stop()
	defer tm.Stop()

	lastHealth := time.Now()

	for {
		// 进入重算窗口：置 0 后，并发的 wakeSweeperAt 一定投递唤醒。
		// 这是消除「Put 已入队、扫描器却按旧 minExpire 睡死」的关键。
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
			// 有更早的到期时间入池：重算（Stop+drain 保持 Go 1.23 之前的可移植性）
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

// sweepAll 对全部地址桶执行一轮扫描。
// health=false 时只做到期回收，不碰 socket（供 deadline 精确唤醒使用）。
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

// ---- 计数与配额 ----

func (p *Pool) checkAndIncConnNum() error {
	if p.MaxConn <= 0 {
		p.connNum.Add(1)
		return nil
	}
	for {
		cur := p.connNum.Load()
		if int(cur) >= p.MaxConn {
			return ErrConnPoolMax
		}
		if p.connNum.CompareAndSwap(cur, cur+1) {
			return nil
		}
	}
}

// releaseCount 成对归还「池级配额」与「该地址总连接数」，是归还路径的唯一记账入口。
func (p *Pool) releaseCount(ps *pools, n int64) {
	if n == 0 {
		return
	}
	p.connNum.Add(-n)
	if ps != nil {
		ps.connTotalCountAdd(-n)
	}
}

// removeUsedConn 从在用集合中移除一个包装连接
func (p *Pool) removeUsedConn(t *connSingle) {
	p.usedMu.Lock()
	delete(p.usedConns, t)
	p.usedMu.Unlock()
}

// ---- 等待接口 ----

// falseChan 返回一个已投递 false 的等待 channel（条件永不可能成立的场景）。
func falseChan() chan bool {
	ch := make(chan bool, 1)
	ch <- false
	return ch
}

// waitBucket 返回 addr 对应的桶；桶尚不存在时创建一个空桶用于登记等待者。
//
// 「桶不存在」在计数语义上等价于「该地址 0 条连接」（Num/NumIdle/NumConn
// 对未知地址同样返回 0），因此不能像只读统计路径那样直接判 false：
//   - 阈值 <= 0：条件此刻已成立，由 geqXxxWait/leqXxxWait 立即投递 true；
//   - 阈值  > 0：只是「现在不成立」，后续 Dial/Put 建桶后完全可能成立，
//     必须登记下来等 notifyAllLocked 唤醒；返回 false 等于告诉调用方
//     「永不可能成立」，是错误信号。
//
// loadPool 已处理 closed / addr == nil（返回 nil），调用方按「永不可能成立」
// 投递 false，与桶内 wait 函数在池关闭时的行为保持一致。
func (p *Pool) waitBucket(addr net.Addr) *pools {
	if ps := p.getPoolAddr(addr); ps != nil {
		return ps
	}
	return p.loadPool(addr)
}

func (p *Pool) WaitConnNumGeq(addr net.Addr, l int) <-chan bool {
	if ps := p.waitBucket(addr); ps != nil {
		return ps.geqConnNumWait(l)
	}
	return falseChan()
}

func (p *Pool) WaitConnNumLeq(addr net.Addr, l int) <-chan bool {
	if ps := p.waitBucket(addr); ps != nil {
		return ps.leqConnNumWait(l)
	}
	return falseChan()
}

func (p *Pool) WaitConnIdleGeq(addr net.Addr, l int) <-chan bool {
	if ps := p.waitBucket(addr); ps != nil {
		return ps.geqConnIdleWait(l)
	}
	return falseChan()
}

func (p *Pool) WaitConnIdleLeq(addr net.Addr, l int) <-chan bool {
	if ps := p.waitBucket(addr); ps != nil {
		return ps.leqConnIdleWait(l)
	}
	return falseChan()
}

// ---- 内部取放 ----

// getPoolConn 额外返回该连接入池时登记的归还参数
// （空闲超时 / 后台预读字节数），供 connSingle 归还时原样回传。
func (p *Pool) getPoolConn(network, address string) (net.Conn, []byte, time.Duration, int, error) {
	ps := p.getPool(network, address)
	if ps == nil {
		return nil, nil, 0, 0, ErrConnNotAvailable
	}
	return ps.get()
}

// getPoolConnCount 返回该地址当前持有的总连接数（永不为负）。
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

// putPoolConn 归还连接到池。
//
// 关闭责任集中到调用方，避免原实现里 putPoolConn 与调用方各关一次。
func (p *Pool) putPoolConn(conn net.Conn, pending []byte, addr net.Addr, timeout time.Duration, bs int) error {
	if conn == nil || addr == nil {
		return errors.New("vconnpool: nil conn or addr")
	}
	if p.closed.Load() {
		return ErrConnPoolClosed
	}
	p.ensureSweeper()

	ps := p.loadPool(addr)
	if ps == nil {
		// loadPool 返回 nil 只有两种可能：addr 为 nil（上面已挡住），
		// 或池在 closed 判定之后被关闭。后者必须如实上报，
		// 让调用方走「入池失败 → 关闭连接」的收尾路径，而不是让连接无主。
		if p.closed.Load() {
			return ErrConnPoolClosed
		}
		return errors.New("vconnpool: nil addr")
	}
	if err := ps.put(conn, pending, timeout, bs); err != nil {
		return err
	}
	// 新连接可能带更早的 expireAt（尤其 PutFull 的自定义 timeout 短于
	// p.IdleTimeout）：按需叫醒扫描器。timeout == 0 的连接没有 expireAt，
	// 唤醒无意义。此时 pools.mu 已释放，投递非阻塞，不改变归还路径的锁序。
	if timeout > 0 {
		p.wakeSweeperAt(time.Now().Add(timeout))
	}
	return nil
}

// ---- 拨号 ----

func (p *Pool) Dial(network, address string) (Conn, error) {
	return p.DialContext(context.Background(), network, address)
}

// DialContext 从池中取连接，取不到则新建。
//
// 优先级语义（PoolConnContextKey）：
//   - 未设置   → 先取池，取不到再拨号；
//   - 设置 true → 只使用池中的连接：池空返回 ErrConnNotAvailable，**绝不拨号**；
//   - 设置 false → 跳过池，强制新建。
func (p *Pool) DialContext(ctx context.Context, network, address string) (Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if p.closed.Load() {
		return nil, ErrConnPoolClosed
	}

	var (
		conn       net.Conn
		b          []byte
		err        error
		fromPool   bool
		addr       = &Addr{Net: network, Name: address}
		putTimeout time.Duration
		putBS      int
	)

	// 三态判定：非 bool 值按「未设置」处理（类型断言 ok=false）。
	poolConn, exist := ctx.Value(PoolConnContextKey).(bool)
	if !exist || poolConn { // 等价于原来的 exist == poolConn，展开只为可读
		conn, b, putTimeout, putBS, err = p.getPoolConn(network, address)
		if err == nil {
			if cerr := ctx.Err(); cerr != nil {
				if conn != nil {
					conn.Close()
				}
				// 丢弃一条已经脱离池子的原始连接并归还配额。
				p.releaseCount(p.getPoolAddr(addr), 1)
				return nil, cerr
			}
			fromPool = true
		} else if errors.Is(err, ErrConnPoolClosed) {
			// 取连接的过程中池被关闭：如实上报，不要退化成拨号。
			return nil, ErrConnPoolClosed
		}
	}

	// 创建连接（只有 poolConn == false 才允许拨号）：
	//   - 未设置键 / 设置为 false 且池里没拿到连接 → 拨号；
	//   - 设置为 true 时 poolConn == true，池空即失败，不拨号。
	if conn == nil && !poolConn {
		// 新建的连接按池的默认策略归还
		putTimeout, putBS = p.IdleTimeout, 0
		conn, err = p.dialNew(ctx, network, address)
	}
	if err != nil {
		return nil, err
	}

	// 取地址桶。拨号 / 取池期间池可能已被关闭：此时 loadPool 返回 nil，
	// 既不能把连接交给调用方（它既不在池里、又不受 Pool.Close 管理），
	// 也不能把计数写进已被清空的桶。
	ps := p.loadPool(addr)
	if ps == nil {
		if conn != nil {
			conn.Close()
		}
		// 只有「从池里取出」的连接才计入过该地址的 connTotalCount。
		// 新拨号的连接只在下方 !fromPool 分支补登 +1，这里若无条件按地址回滚，
		// 会把桶计数扣成负数（Close 尚未把 p.conns 置 nil 的窗口内尤其如此）。
		// 桶已随 Close 清掉时 getPoolAddr 返回 nil，releaseCount 只回滚全局计数。
		var rps *pools
		if fromPool {
			rps = p.getPoolAddr(addr)
		}
		p.releaseCount(rps, 1)
		return nil, ErrConnPoolClosed
	}
	if !fromPool {
		// 新拨号的连接：全局配额已由 dialNew 借好，这里补登地址总连接数；
		// 从池里取出的连接早已计入该桶，重复累加会让 NumConn 虚高。
		ps.connTotalCountAdd(1)
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

func (p *Pool) dialNew(ctx context.Context, network, address string) (net.Conn, error) {
	if err := p.checkAndIncConnNum(); err != nil {
		return nil, err
	}

	var (
		addr net.Addr
		err  error
	)
	if p.ResolveAddr != nil {
		addr, err = p.ResolveAddr(network, address)
	} else {
		addr, err = ResolveAddr(network, address)
	}
	if err != nil {
		p.connNum.Add(-1)
		return nil, err
	}

	dialer := p.Dialer
	if dialer == nil {
		dialer = defaultDialer
	}
	conn, err := dialer.DialContext(ctx, network, addr.String())
	if err != nil {
		p.connNum.Add(-1)
		return nil, err
	}
	return conn, nil
}

// ---- 公开取放接口 ----

// Get 从连接池获取指定地址的连接。
//
// 与 GetFull 的差别：本方法会把空闲期间收到的数据**回灌**进返回的连接，
// 调用方后续 Read 依然能读到它们（不会丢数据）。
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

// GetFull 从连接池获取指定地址的连接，并返回空闲期间收到的数据。
//
// 注意：本方法把连接所有权转移给调用方（全局计数随之递减），
// 出借的连接**不受 Pool.Close 管理**，调用方必须自行 Close。
func (p *Pool) GetFull(addr net.Addr) (net.Conn, []byte, error) {
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
	// 只读查找：绝不能在只读路径上创建桶（否则已关闭的池会被"复活"）。
	p.releaseCount(p.getPoolAddr(addr), 1)
	return conn, b, nil
}

// Add 适合存放客户端发起的连接
//
// 所有权语义与 PutFull 一致：返回错误时连接已被关闭，调用方不应再 Close。
func (p *Pool) Add(conn net.Conn) error {
	if conn == nil {
		return errors.New("vconnpool: cannot add nil connection")
	}
	return p.Put(conn, conn.RemoteAddr())
}

// Put 适合存放客户端发起的连接，支持指定的地址
//
// 所有权语义与 PutFull 一致：返回错误时连接已被关闭，调用方不应再 Close。
func (p *Pool) Put(conn net.Conn, addr net.Addr) error {
	return p.PutFull(conn, addr, p.IdleTimeout, 0)
}

// PutFull 适合存放客户端/服务端的连接，支持指定地址、超时和缓存大小
func (p *Pool) PutFull(conn net.Conn, addr net.Addr, timeout time.Duration, bs int) error {
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
	// 包装连接走自身的回收逻辑
	if cs, ok := conn.(*connSingle); ok {
		return cs.Close()
	}

	// 重复归还 / 池已关闭的快速拒绝，必须放在 SetBackgroundReadBuffer / CheckBlocking 之前。
	// ErrConnExists：fd 已由池持有，不能关闭。
	// ErrConnPoolClosed：此处连接还没包成 vconn，没有预读字节，直接关原始 conn，
	// 与下方 p.closed 分支的收尾方式一致。
	// ErrConnIdleMax 不在此短路：锁外读到的空闲数可能已经回落，最终判定交给 put。
	if ps := p.getPoolAddr(addr); ps != nil {
		switch err := ps.precheck(conn); {
		case errors.Is(err, ErrConnExists):
			return nil
		case errors.Is(err, ErrConnPoolClosed):
			conn.Close()
			return ErrConnPoolClosed
		}
	}

	// 入池前的健康检查
	vc, isVC := conn.(*vconn.Conn)
	if !isVC {
		vc = vconn.New(conn)
	}
	// 统一以 bs 为准（两种来源一视同仁）：见方法注释的「预读配置约定」。
	// 注意 bs == 0 时 peekUserSet 会被置位，使无 fd 连接（net.Pipe 等）的
	// CheckBlocking 退化为 StatusUnverified —— 这与未包装连接的既有行为一致。
	vc.SetBackgroundReadBuffer(bs)

	// 顺序敏感：先借配额，再做可能碰 socket 的 CheckBlocking。
	// 反过来（先 CheckBlocking）时，无 fd 连接（net.Pipe 等）的探活会读走
	// 1 字节进 vc 的预读缓冲；此时若配额不足直接 return，vc 被丢弃，
	// 那 1 字节就随它一起丢了 —— 而连接本身仍可被调用方继续使用。
	//
	// 用 vc.Close() 而不是 conn.Close()：CheckBlocking 可能已把字节读进 vc 的
	// 预读缓冲，必须连同包装一起销毁，避免留下一条"少了几字节"的连接。
	if err := p.checkAndIncConnNum(); err != nil {
		vc.Close()
		return err
	}
	// 入池判定改用复用轴 reusablePut
	if err := vc.CheckBlocking(); !reusablePut(vc, err) {
		p.connNum.Add(-1) // 回滚刚借的配额：连接没有入池
		vc.Close()
		return err
	}

	// vconn 包装：用 RawConnFull 保全预读字节（RawConn 会丢弃）
	conn, pending := vc.RawConnFull()
	err := p.putPoolConn(conn, pending, addr, timeout, bs)
	if err == nil {
		// 用只读查找补登地址计数：这里绝不能建桶（池可能刚被关闭）。
		if ps := p.getPoolAddr(addr); ps != nil {
			ps.connTotalCountAdd(1)
		}
		return nil
	}

	// 入池失败：connTotalCount 从未增加，只需归还全局配额。
	p.releaseCount(nil, 1)
	if errors.Is(err, ErrConnExists) {
		return nil // 已在池中，不要关闭（fd 由池持有）
	}
	// 注意：走到这里时所有权已经随 RawConnFull 转移到 conn，vc 已不可用，
	// 调用方也拿不回去。ErrConnIdleMax / ErrConnPoolClosed 等路径必须由这里
	// 兜底关闭，否则就是纯 fd 泄漏。
	if conn != nil {
		conn.Close()
	}
	return err
}

// ---- 关闭 ----

// CloseIdleConnection 关闭指定地址的空闲连接
func (p *Pool) CloseIdleConnection(addr net.Addr) {
	if addr == nil {
		return
	}
	if ps := p.getPool(addr.Network(), addr.String()); ps != nil {
		ps.clean()
	}
}

// CloseIdleConnections 关闭所有地址的空闲连接
//
// 先在读锁内快照，释放后再执行（clean 内含 syscall，持锁会阻塞建池）。
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
}

// Close 关闭连接池：停止扫描器、关闭全部空闲连接、关闭全部在用连接。
//
// 幂等。返回后池内计数归零，再次 Dial 返回 ErrConnPoolClosed。
func (p *Pool) Close() error {
	if p.closed.Swap(true) {
		return nil
	}

	// 1. 停扫描器（先停，避免它在清理过程中再次触碰池）
	//    sweepMu 与 ensureSweeper 互斥：既保证不会漏掉尚未创建的 sweepStop，
	//    也保证 sweepWg.Add 不再发生于 Wait 之后。
	p.sweepMu.Lock()
	if p.sweepStop != nil {
		close(p.sweepStop)
	}
	p.sweepMu.Unlock()
	p.sweepWg.Wait()

	// 2. 关空闲
	p.CloseIdleConnections()

	// 3. 关在用（通过 connSingle 走完整收尾，避免「vconn 未感知 fd 被关」的悬空状态）
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

	// 4. 归零收尾
	p.mu.Lock()
	p.conns = nil
	p.mu.Unlock()

	// 用 CAS 归零而不是 Store(0)：并发的在途归还仍可能在 Add(-1)，
	// Store(0) 与之交错会留下负数，而 NumConn 会把这个负值直接暴露给调用方。
	for {
		cur := p.connNum.Load()
		if cur <= 0 || p.connNum.CompareAndSwap(cur, 0) {
			break
		}
	}
	return nil
}

// Num 当前池内连接总数（含空闲与在用，也包含正在拨号中的连接）
func (p *Pool) Num() int {
	if n := p.connNum.Load(); n > 0 {
		return int(n)
	}
	return 0 // 计数漂移时不给调用方负数
}

// NumIdle 当前空闲连接数量
func (p *Pool) NumIdle(addr net.Addr) int {
	if p.closed.Load() || addr == nil {
		return 0
	}
	return p.getIdleConnCount(addr.Network(), addr.String())
}

// NumConn 当前地址连接数量（空闲 + 在用，不含拨号中的连接）
func (p *Pool) NumConn(addr net.Addr) int {
	if p.closed.Load() || addr == nil {
		return 0
	}
	return p.getPoolConnCount(addr.Network(), addr.String())
}

// ----------------------------------------------------------------------------
// 地址解析
// ----------------------------------------------------------------------------

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

// contextKey 用于在 context 中存储连接池相关的值
type contextKey struct{ name string }

func (p *contextKey) String() string { return "connpool context value " + p.name }

// PoolConnContextKey 上下文键，用于控制 DialContext 的取连接策略。
//
// 取值语义（与实现一致）：
//
//	未设置       → 先取池，取不到再拨号
//	设置为 true   → 只使用池中的连接：池空返回 ErrConnNotAvailable，绝不拨号
//	设置为 false  → 跳过池，强制新建
//	非 bool 值    → 按「未设置」处理
var PoolConnContextKey = &contextKey{"pool-conn"}
