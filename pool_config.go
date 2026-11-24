package vconnpool

import (
	"net"
	"time"
)

// Config 是连接池的运行期不可变配置。
//
// 约定：一旦通过 New / SetConfig / UpdateConfig 发布，任何一方都不得再修改
// 它的字段。要改配置就构造一份新的再发布 —— 这样每个读者拿到的都是**自洽**
// 的一组值（不会出现「新 IdleTimeout + 旧 MaxConn」这种中间态）。
//
// 零值即默认配置，可直接使用。
type Config struct {
	// Dialer 为空时使用包内 defaultDialer。
	Dialer Dialer
	// ResolveAddr 为空时使用包内 ResolveAddr。
	ResolveAddr func(network, address string) (net.Addr, error)
	// IdleConn 单地址空闲连接上限。负值会被发布入口归一为 0。
	//
	// 注意：当前实现把 0 与负值都当作「不限」（precheck / put 以
	// `maxIdle > 0` 判定），发布入口因此不会对 0 触发 trimIdle。
	// 若需要「0 = 不允许任何空闲连接」的语义，需同时修改
	// connPool.go 的 precheck / put。
	IdleConn int
	// IdleTimeout 空闲超时，<= 0 表示空闲连接不过期。
	IdleTimeout time.Duration
	// MaxConn 池级连接总数上限（含拨号中）。负值会被发布入口归一为 0
	//（0 = 不限，与首次快照路径的 <= 0 语义一致）。
	MaxConn int
}

// New 用给定配置创建连接池。
//
// 等价于「零值 Pool + SetConfig」，但**不做收敛**：新池没有任何存量连接，
// reapplyTimeout / trimIdle / 唤醒扫描器都是纯开销。
//
// 返回的池可立即并发使用；后续热改走 SetConfig / UpdateConfig。
func New(cfg Config) *Pool {
	normalize(&cfg)
	p := &Pool{}
	p.cfg.Store(&cfg)
	return p
}

// conf 返回当前生效的配置快照，**永不为 nil**。
//
// 首次调用时发布一份全零（= 全默认）的快照：
//   - Pool 的零值即可用，不需要任何初始化；
//   - 一旦调用过 New / SetConfig / UpdateConfig，导出字段路径不复存在，
//     配置只有一个来源。
//
// 与 SetConfig / UpdateConfig 的互斥：发布入口发布后 cfg 非 nil，这里的
// CAS 必然失败并返回发布入口的那份；不存在「初始快照把热改配置覆盖回去」
// 的窗口。反过来，若本函数先发布空快照，发布入口会在它之上整体替换。
//
// 该函数无锁：一次 atomic.Load，命中后零分配。
func (p *Pool) conf() *Config {
	if c := p.cfg.Load(); c != nil {
		return c
	}
	// 必须是 CAS 而不是 Store：并发首次调用时只有一个成功，
	// 且绝不能覆盖另一条路径已经发布的真实配置。
	p.cfg.CompareAndSwap(nil, &Config{})
	return p.cfg.Load()
}

// Config 返回当前生效配置的副本（观测 / 测试 / 日志 / UpdateConfig 取基准用）。
func (p *Pool) Config() Config { return *p.conf() }

// normalize 归一化配置：负值归零，杜绝「负值被误当作不限以外的语义」。
func normalize(c *Config) {
	if c.IdleConn < 0 {
		c.IdleConn = 0
	}
	if c.MaxConn < 0 {
		c.MaxConn = 0
	}
}

// idleConnDecreased 报告「单地址空闲上限是否收紧」。
//
// 0 表示不限（见 precheck / put 的 `maxIdle > 0` 判定），因此：
//   - 0 → n（n > 0）：从不限改为有上限，是收紧；
//   - n → 0          ：从有上限改为不限，不是收紧；
//   - n → m          ：m < n 才是收紧。
//
// 只有收紧才需要 trimIdle：放宽时多出来的空闲连接会被 put 的
// ErrConnIdleMax 与新上限自然收敛，无需主动裁剪。
func idleConnDecreased(prev, next int) bool {
	if next <= 0 {
		return false
	}
	if prev <= 0 {
		return true
	}
	return next < prev
}

// publish 发布一份新快照，并按**新旧差异**执行最小收敛。c 必须已归一化。
//
// 差异收敛的意义：早先的实现无条件 reapplyTimeout，会在「只改 Dialer」
// 这类与空闲策略无关的热改里把存量空闲连接的 timeout 全部重置为池级
// IdleTimeout，覆盖掉 PutFull 曾为它们单独指定的超时。现在只有真正变化的
// 维度才触发动作：
//   - IdleTimeout 变了（含 有超时 → 不过期）→ reapplyTimeout + 唤醒扫描器；
//   - IdleConn 收紧 → trimIdle。
//
// 发布与 closed 的并发：closed 置位后没有任何决策路径会再读 cfg，本次 Store
// 属于无害的冗余写；收敛循环遍历的是快照出的桶列表，桶的 clean 后状态由
// 各方法自身的 closed 判定兜底。
func (p *Pool) publish(c *Config) {
	prev := p.cfg.Swap(c)

	// prev == nil 表示此前从未发布过（零值 Pool 直接 SetConfig）。
	// 此时以零值配置为基准比较：零值即全默认，语义正确。
	base := Config{}
	if prev != nil {
		base = *prev
	}
	needReapply := base.IdleTimeout != c.IdleTimeout
	needTrim := idleConnDecreased(base.IdleConn, c.IdleConn)
	if !needReapply && !needTrim {
		return
	}

	// 读锁内只做桶快照，锁外逐桶执行可能阻塞 / 碰 socket 的操作，
	// 与 CloseIdleConnections / sweepAll 的模式一致。
	p.mu.RLock()
	list := make([]*pools, 0, len(p.conns))
	for _, ps := range p.conns {
		list = append(list, ps)
	}
	p.mu.RUnlock()

	for _, ps := range list {
		// IdleTimeout <= 0 也要重算：reapplyTimeout(0) 会清空存量空闲连接的
		// expireAt，否则「从有超时改为不过期」不会对已有连接生效。
		if needReapply {
			ps.reapplyTimeout(c.IdleTimeout)
		}
		// 放在 reapplyTimeout 之后：若本次同时缩短了超时并调小了上限，
		// 先重算能让 trimIdle 摘掉的那批与新超时一致。
		// needTrim 为真时 c.IdleConn 必然 > 0（见 idleConnDecreased）。
		if needTrim {
			ps.trimIdle(c.IdleConn)
		}
	}

	// 无条件唤醒扫描器：必须区别于条件投递的 wakeSweeperAt ——
	// 热改可能让「最早到期时间」变小或让基准节拍变化，
	// 只有无条件唤醒才能让 sweepLoop 立刻重算睡眠时长。
	p.wakeSweeper()
}

// SetConfig 用一份完整配置整体替换当前生效配置，并执行收敛。
//
// 与 UpdateConfig 的差别：本方法不基于现有快照，**未填写的字段会被零值覆盖**
// （Dialer / ResolveAddr 回落包级默认，IdleConn / MaxConn 归零为「不限」，
// IdleTimeout 归零为「不过期」）。适合「从配置文件整体加载」的场景；
// 只想改一两个字段请用 UpdateConfig。
//
// 池已关闭时返回 ErrConnPoolClosed，且不会发布。
// 与 UpdateConfig 并发时后发布者获胜（lost update），需要严格串行请由调用方加锁。
func (p *Pool) SetConfig(c Config) error {
	if p.closed.Load() {
		return ErrConnPoolClosed
	}
	normalize(&c)
	p.publish(&c)
	return nil
}

// UpdateConfig 运行期热改配置的增量入口：基于当前生效快照的副本执行 mutate，
// 再整体发布。适合「只改一两个字段」的场景，不会意外清掉未触及的字段。
//
// 用法：
//
//	err := pool.UpdateConfig(func(c *vconnpool.PoolConfig) {
//	    c.IdleTimeout = 30 * time.Second
//	})
//
// 约束：
//   - mutate 在调用方 goroutine 的副本上执行，不持任何锁；
//   - mutate 内**不得**调用本池的任何方法（Dial / Get / Put / Close /
//     Num / Config / 再入 UpdateConfig 等），否则可能自锁或读到中间态；
//   - mutate 内只需赋值，不必做负值归一（发布前统一归一）。
//
// 语义与 SetConfig 一致：一次原子发布 + 收敛，多字段在同一份快照中生效，
// 并发读者不会看到「mutate 改了一半」的中间组合。
//
// 池已关闭时返回 ErrConnPoolClosed（mutate 不会被执行）。
// 与 SetConfig 的并发：后发布者获胜（两次并发增量更新只有一份留存，
// 即 lost update）。需要严格串行的热改请由调用方自行加外层锁。
func (p *Pool) UpdateConfig(mutate func(c *Config)) error {
	if p.closed.Load() {
		return ErrConnPoolClosed
	}
	if mutate == nil {
		return nil // 无操作：不发新快照，避免无意义的收敛扰动
	}
	c := *p.conf() // 当前快照的副本，mutate 随便改
	mutate(&c)
	normalize(&c)
	p.publish(&c)
	return nil
}

// wakeSweeper 无条件唤醒扫描器。
//
// 扫描器尚未启动时 sweepWake 为 nil，此时无需唤醒 ——
// 它启动时自然会读到新配置。
func (p *Pool) wakeSweeper() {
	if pw := p.sweepWake.Load(); pw != nil {
		select {
		case *pw <- struct{}{}:
		default:
		}
	}
}
