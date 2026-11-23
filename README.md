# vconnpool

高性能、可复用的 `net.Conn` 连接池，适用于 TCP / UDP / Unix / IP 等任意网络类型，同时支持「客户端主动拨号」与「服务端既有连接入池」两种使用模式。

- **零配置可用**：`Pool` 的零值即为一个可用连接池。
- **客户端复用**：`Dial` / `DialContext` 优先从池中取空闲连接，取不到才拨号，并且可以把「只用池、绝不拨号」作为策略强制生效。
- **服务端入池**：`Add` / `Put` / `PutFull` 可以把 `Accept()` 得到的连接、代理转发链路上的上游连接放回池中复用。
- **精确的空闲回收**：`IdleTimeout` 到期即回收，取连接前还会再校验一次，不依赖「全量轮询」的精度。
- **两级容量控制**：`MaxConn`（全局在册连接上限）、`IdleConn`（每个地址的空闲连接上限）。
- **条件等待 / 背压**：`WaitConnIdleLeq`、`WaitConnNumGeq` 等接口返回 channel，可与 `select` + `context` 组合，用来等待「空闲连接降到 N 条以下」「连接总数涨到 N 条」。
- **并发安全**：`Pool` 可被多个 goroutine 并发使用；`Close` 幂等。
- **不丢字节**：出借时若连接在空闲期已收到对端数据，会随连接一并交回（`GetFull` 的第二返回值），不会静默丢弃。

依赖：[github.com/456vv/vconn/v2](https://github.com/456vv/vconn)

---

## 目录

- [安装](#安装)
- [核心概念](#核心概念)
- [使用步骤](#使用步骤)
- [快速开始](#快速开始)
- [常见场景示例](#常见场景示例)
- [API 一览](#api-一览)
- [错误说明](#错误说明)
- [注意事项与最佳实践](#注意事项与最佳实践)
- [设计要点](#设计要点)

---

## 安装

```bash
go get github.com/456vv/vconnpool/v3
```

```go
import "github.com/456vv/vconnpool/v3"
```

需要 Go 1.19 及以上版本（使用了 `sync/atomic` 的泛型化类型）。

---

## 核心概念

### Pool

`vconnpool.Pool` 是连接池本体。内部按「网络名 + 地址」分桶管理连接：

```go
pool := &vconnpool.Pool{
    IdleConn:    16,               // 每个地址最多保留 16 条空闲连接
    IdleTimeout: 30 * time.Second, // 空闲连接 30s 未复用即回收
    MaxConn:     200,              // 全局最多 200 条连接（含正在拨号的）
}
defer pool.Close()
```

`tcp` / `tcp4` / `tcp6` 会被归一为同一个桶，因此 `DialContext(ctx, "tcp4", ...)` 与 `conn.RemoteAddr()` 归还不会落到两个不同的桶里。

> `Dialer`、`ResolveAddr`、`IdleConn`、`IdleTimeout`、`MaxConn` 都是**配置项**，必须在任何 `Dial` 之前设置完毕；运行期修改它们会产生数据竞争。

### Conn

`vconnpool.Conn` 是连接池交付给调用方的连接接口：

```go
type Conn interface {
    net.Conn
    Discard()        // 标记为不可复用
    IsReuseConn() bool
    RawConn() net.Conn
}
```

它是 `net.Conn` 的超集，用法与普通连接一致（`Read` / `Write` / `Close` / `SetDeadline` 等），但有两个关键差异：

| 动作 | 语义 |
| --- | --- |
| `conn.Close()` | **把连接归还给池**（健康时），而不是立刻关闭文件描述符 |
| `conn.Discard()` + `conn.Close()` | 明确放弃这条连接：直接销毁，不再回到池中 |
| `conn.IsReuseConn()` | 该连接是否来自池（`true` = 复用，`false` = 本次新建） |
| `conn.RawConn()` | 交出底层 `net.Conn` 并转移所有权（见注意事项） |

> `Get` / `GetFull` 返回的是**原始连接**（`net.Conn`），不是 `Conn` 包装：它们的所有权已经转移给调用方，`Close` 就是真正关闭，且不受 `Pool.Close` 管理。

### 两种连接来源

| 场景 | 入口 | 说明 |
| --- | --- | --- |
| 客户端主动拨号 | `Dial` / `DialContext` | 池中有空闲连接就复用，否则新建 |
| 已有连接（服务端 `Accept`、代理上游、自定义 `Dialer`） | `Add` / `Put` / `PutFull` | 把连接所有权交给池 |
| 直接取出（不经过 `Dial`） | `Get` / `GetFull` | 只出借，绝不拨号 |

---

## 使用步骤

1. **创建并配置池**：`&vconnpool.Pool{...}`，配置项一次设定，之后只读。
2. **获取连接**
   - 需要「自动复用 + 自动新建」→ `DialContext`；
   - 已经有连接（服务端 / 上游）→ 入池用 `Add` / `Put` / `PutFull`；
   - 只想从池里拿，不新建 → `Get` / `GetFull`。
3. **使用连接**：正常读写；建议每次请求都设置 `SetDeadline`，避免单次请求永久阻塞。
4. **归还或丢弃**
   - 正常用完 → `conn.Close()`（回到池中，供下次复用）；
   - 协议出错 / 状态不确定 → `conn.Discard()` 后再 `Close()`，直接销毁。
5. **关闭池**：进程退出或服务下线时 `pool.Close()`，它会停止后台扫描、关闭空闲连接、关闭仍在册的在用连接，并把计数归零。

---

## 快速开始

```go
package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/456vv/vconnpool/v3"
)

func main() {
	pool := &vconnpool.Pool{
		IdleConn:    16,
		IdleTimeout: 30 * time.Second,
		MaxConn:     200,
	}
	defer pool.Close() // 幂等；会关闭空闲与在用连接

	// 单次请求的超时用 context 控制
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := pool.DialContext(ctx, "tcp", "127.0.0.1:9000")
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	// 注意：这里的 Close 是「归还给池」，不是关闭 fd
	defer conn.Close()

	// 建议显式设置读写超时，避免请求悬挂
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		log.Printf("set deadline: %v", err)
	}

	if _, err := conn.Write([]byte("PING\n")); err != nil {
		conn.Discard() // 写失败：连接状态不可信，标记为不可复用
		log.Printf("write: %v", err)
		return
	}

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Discard()
		log.Printf("read: %v", err)
		return
	}
	log.Printf("recv: %q", buf[:n])
	// 走到函数结尾，defer conn.Close() 把连接还回池中
}

var _ = errors.Is // 示例占位
```

---

## 常见场景示例

### 1. 控制取池策略：只用池 / 强制新建

`DialContext` 会读取 context 中的 `vconnpool.PoolConnContextKey`，取值语义：

| 取值 | 行为 |
| --- | --- |
| 未设置（或非 `bool` 类型） | 先取池，取不到再拨号 |
| `true` | 只使用池中的连接：池空返回 `ErrConnNotAvailable`，**绝不拨号** |
| `false` | 跳过池，强制新建 |

```go
// 只用池中的空闲连接：池空直接失败，适合「不允许新建连接」的限流场景
ctx := context.WithValue(context.Background(), vconnpool.PoolConnContextKey, true)
conn, err := pool.DialContext(ctx, "tcp", "127.0.0.1:9000")
if errors.Is(err, vconnpool.ErrConnNotAvailable) {
	// 池里此刻没有可用连接
}

// 强制新建：适合需要独占连接（如长连接会话、TLS 会话绑定）的场景
ctx = context.WithValue(context.Background(), vconnpool.PoolConnContextKey, false)
conn, err = pool.DialContext(ctx, "tcp", "127.0.0.1:9000")
```

### 2. 服务端 / 代理场景：连接入池

```go
var addr = &vconnpool.Addr{Net: "tcp", Name: "127.0.0.1:9000"}

// 把 Accept 到的连接交给池（所有权随之转移）
func addConn(pool *vconnpool.Pool, c net.Conn) {
	// 返回 error 时连接已被池关闭，调用方不要再 Close
	if err := pool.Add(c); err != nil {
		log.Printf("add conn: %v", err)
	}
}

// 用自定义空闲存活时长与后台预读字节数归还
func putWithOptions(pool *vconnpool.Pool, c net.Conn) {
	if err := pool.PutFull(c, addr, 10*time.Second, 4096); err != nil {
		log.Printf("put: %v", err) // 同样：出错时连接已被关闭
	}
}

// 从池中取连接，并拿到「空闲期间对端新发来的数据」
func takeWithPending(pool *vconnpool.Pool) (net.Conn, []byte) {
	conn, pending, err := pool.GetFull(addr)
	if err != nil {
		return nil, nil
	}
	return conn, pending // 调用方完全拥有 conn，用完必须自己 Close
}
```

### 3. 背压 / 等待连接条件

```go
// 等到该地址的空闲连接降到 0 条（即全部借出），再决定是否继续下发任务
ch := pool.WaitConnIdleLeq(addr, 0)

select {
case ok := <-ch:
	if !ok {
		// 条件「永不可能成立」：池已关闭，或 addr 为 nil
		return
	}
	// 条件已成立
case <-ctx.Done():
	return
}
```

可用接口：

| 接口 | 条件 |
| --- | --- |
| `WaitConnIdleGeq(addr, l)` | 该地址空闲连接数 ≥ `l` |
| `WaitConnIdleLeq(addr, l)` | 该地址空闲连接数 ≤ `l` |
| `WaitConnNumGeq(addr, l)` | 该地址连接总数（空闲 + 在用）≥ `l` |
| `WaitConnNumLeq(addr, l)` | 该地址连接总数（空闲 + 在用）≤ `l` |

channel 带 1 个缓冲，非阻塞投递；条件当前已成立时立刻收到 `true`。

### 4. 优雅关闭

```go
// 只清空空闲连接，不影响在用连接（例如滚动发布时先释放冗余资源）
pool.CloseIdleConnections()

// 只清空某个地址的空闲连接
pool.CloseIdleConnection(addr)

// 整体关闭：停扫描器 → 关空闲 → 关在用 → 计数归零；幂等
pool.Close()

// 关闭后再 Dial 会返回 ErrConnPoolClosed
_, err := pool.DialContext(context.Background(), "tcp", "127.0.0.1:9000")
if errors.Is(err, vconnpool.ErrConnPoolClosed) {
	// 池已关闭
}
```

---

## API 一览

### 类型

| 类型 | 说明 |
| --- | --- |
| `Pool` | 连接池。零值可用 |
| `Conn` | 池交付的连接接口：`net.Conn` + `Discard()` + `IsReuseConn()` + `RawConn()` |
| `Dialer` | 拨号器接口：`Dial(network, addr)` / `DialContext(ctx, network, addr)` |
| `Addr` | 轻量 `net.Addr` 实现：`&vconnpool.Addr{Net: "tcp", Name: "host:port"}`，无需解析即可作为桶键 |
| `PoolConnContextKey` | context 键，控制 `DialContext` 的取池策略 |

### `Pool` 字段（配置项）

| 字段 | 说明 |
| --- | --- |
| `Dialer Dialer` | 拨号器；`nil` 时使用内置的 `net.Dialer{Timeout: 30s, KeepAlive: 30s}` |
| `ResolveAddr func(network, address string) (net.Addr, error)` | 自定义地址解析；`nil` 时使用包级 `ResolveAddr`。可用于服务发现或测试重定向 |
| `IdleConn int` | 每个地址的空闲连接上限；`<= 0` 表示不限 |
| `IdleTimeout time.Duration` | 默认空闲存活时长（`Put` 与新拨号连接归还时使用）；`<= 0` 表示不因空闲超时回收 |
| `MaxConn int` | 池内连接总数上限（含正在拨号的连接）；`<= 0` 表示不限 |

### `Pool` 方法

| 方法 | 说明 |
| --- | --- |
| `Dial(network, address) (Conn, error)` | 等价于 `DialContext(context.Background(), ...)` |
| `DialContext(ctx, network, address) (Conn, error)` | 优先复用池中连接，取不到则拨号；受 `PoolConnContextKey` 影响 |
| `Get(addr net.Addr) (net.Conn, error)` | 从池中取连接；空闲期数据会回灌进返回的连接，后续 `Read` 仍能读到 |
| `GetFull(addr net.Addr) (net.Conn, []byte, error)` | 从池中取连接，并把空闲期收到的数据作为第二返回值交出 |
| `Add(conn net.Conn) error` | 以 `conn.RemoteAddr()` 为地址入池（适合客户端发起的连接） |
| `Put(conn net.Conn, addr net.Addr) error` | 按指定地址入池，使用 `IdleTimeout` 作为空闲存活时长 |
| `PutFull(conn, addr, timeout, bs) error` | 指定地址、空闲存活时长与空闲期后台预读字节数 |
| `CloseIdleConnection(addr net.Addr)` | 关闭指定地址的空闲连接 |
| `CloseIdleConnections()` | 关闭所有地址的空闲连接 |
| `Close() error` | 关闭池：停扫描器、关空闲、关在用、计数归零；幂等 |
| `Num() int` | 池内连接总数（含空闲、在用、正在拨号的） |
| `NumIdle(addr net.Addr) int` | 指定地址的空闲连接数 |
| `NumConn(addr net.Addr) int` | 指定地址的连接数（空闲 + 在用，不含拨号中的） |
| `WaitConnIdleGeq / Leq`、`WaitConnNumGeq / Leq` | 条件等待，返回 `<-chan bool` |

### 包级函数

| 函数 | 说明 |
| --- | --- |
| `ResolveAddr(network, address) (net.Addr, error)` | 内置地址解析，支持 `tcp/tcp4/tcp6`、`udp/udp4/udp6`、`unix/unixgram/unixpacket`、`ip/ip4/ip6` |

---

## 错误说明

| 错误 | 含义 | 调用方该做什么 |
| --- | --- | --- |
| `ErrConnPoolClosed` | 池已关闭 | 停止使用该池，不要重试 |
| `ErrConnNotAvailable` | 池中没有可用连接 | 可自行决定新建或稍后重试；`PoolConnContextKey=true` 时表示「池空即失败」 |
| `ErrConnPoolMax` | 达到 `MaxConn` 上限，拨号被拒 | 稍后重试或提高上限 |
| `ErrConnIdleMax` | 达到 `IdleConn` 上限，归还被拒 | 连接已被池关闭，无需再处理 |
| `ErrConnExists` | 该连接已在空闲池中（重复归还） | 视作成功；`Put*` 系列会返回 `nil`，连接仍由池持有 |
| `ErrConnRAWRead` | 底层连接已被移交，不能重复获取 | 每条连接只允许 `RawConn()` 一次 |

`ErrConnRAWRead` 与 `ErrConnPoolClosed` 之外的错误都通过 `errors.Is` 判定。

---

## 注意事项与最佳实践

### 归还语义（最重要）

- **`Put` / `Add` / `PutFull` 返回错误时，连接已经被池关闭**，调用方不要再 `Close`。唯一例外是 `ErrConnExists`：连接仍在池中，且方法返回 `nil`。
- **归还前请把响应读干净**。如果连接上还残留上一个使用者没读完的字节（只读了半个响应、协议已失步），入池会被拒绝，连接随之被关闭。这是刻意的保守策略：宁可少复用一条连接，也不让下一个使用者读到的第一批字节是上一个人的残留。
  - 与上述规则不冲突的是「空闲期预读」：连接在池内空闲时对端新到的数据**不会丢**，`GetFull` 会把它作为第二返回值交出，`Get` 会自动回灌进返回的连接。
- 协议出错、状态可疑时，请显式 `Discard()`，让连接不再回到池中。

### 连接所有权

- `DialContext` / `Dial` 返回的 `Conn` 受池管理：`Close()` = 归还。**想真正关闭，请先 `Discard()` 再 `Close()`**。
- `Get` / `GetFull` 返回的连接**所有权已转移给调用方**：不在池的管辖范围内，`Pool.Close()` 不会关闭它，调用方必须自己 `Close()`。
- `RawConn()` 会交出底层 `net.Conn` 并转移所有权，此后该 `Conn` 包装不可再用（再次获取会 panic / 返回 `ErrConnRAWRead`）。仅在确实需要裸连接时使用，并自行负责关闭。

### 配置与容量

- 所有配置项必须在首次使用前设定；运行期修改会产生数据竞争。
- `IdleConn` 是**每个地址**的空闲上限；`MaxConn` 是**全局**上限，并且把「正在拨号中」的连接也计入。
- `Num()` / `NumIdle()` / `NumConn()` 适合做监控与粗略判断，不要用于精确同步；计数在关闭瞬间会归零，且内部对负漂移做了夹取，不会返回负数。

### 性能与资源

- `PutFull` 的 `bs`（空闲期后台预读字节数）**非 0 时，每条空闲连接会常驻一个后台监控协程**（约 200ms 一轮）。空闲连接很多时请评估 CPU 占用；不需要「空闲期预读」就传 `0`。
- `IdleTimeout` 只影响空闲连接；`<= 0` 时不会因空闲超时回收，但仍会做健康探活（用于发现对端已断开的死连接）。
- 后台扫描是**全局共享**的，不随空闲连接数增长而新增 goroutine 或定时器。
- 从池中取出的连接会**自动清零 deadline**，因此上一个使用者设置的超时不会污染下一个使用者；即便如此，每次请求仍建议自行设置合理的 deadline。

### 其它

- `Close()` 幂等，返回后计数归零、再 `Dial` 返回 `ErrConnPoolClosed`。
- 等待接口收到 `false` 表示「条件永不可能成立」（池已关闭或地址为 `nil`），收到 `true` 表示条件已成立；请配合 `context` / `select` 超时使用，避免无限等待。
- 池返回的连接还额外提供 `SetNoDelay`、`SetKeepAlive`、`SetLinger`、`CloseWrite`、`SetReadBuffer` 等可选方法，可通过接口断言按需调用（底层不支持时返回 `vconn.ErrUnsupported`，不会因此把连接判成不可复用）。

---

## 设计要点

- **两级判定轴**：「入池」与「出借」使用不同的健康标准——出借时容忍空闲期预读数据（零丢失），入池时拒绝一切缓冲残留（避免协议失步）。
- **精确的空闲回收**：出借前再校验一次过期，后台扫描器按「最早到期时刻」精确唤醒，而不是固定周期兜底，`IdleTimeout` 不会退化成「一个扫描周期」的精度。
- **O(1) 后台协程**：单个共享扫描协程服务所有地址桶，取代「每条空闲连接一个 goroutine + 一个 Timer」。
- **保守的关闭协议**：关闭瞬间若有在途读写，连接状态不可信，直接丢弃而不是归还，避免把「读了一半」的连接交给下一位使用者。

---

## 许可证

以仓库根目录的 `LICENSE` 文件为准。
