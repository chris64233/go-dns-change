package dnschange

import "context"

// IdemSubmit 记录提交幂等键对应的首个请求（规范化后的载荷）。
type IdemSubmit struct {
	Revision  int64
	Base      int64
	Committer string
	Upserts   []RecordSet
	Deletes   []RecordSetRef
}

// IdemRollback 记录回滚幂等键对应的首个请求。
type IdemRollback struct {
	Revision  int64
	Source    int64
	Committer string
}

// IdemApprove 记录审批幂等键对应的首个请求。
type IdemApprove struct {
	Revision int64
	Approver string
}

// ZoneState 是一个区域的全部持久化状态。Store 的替代实现（如 SQL）
// 只需在事务中装载/保存这份结构；字段只能在 Store.Update 内修改。
type ZoneState struct {
	Name   string
	Config ZoneConfig
	Policy Policy

	// Tip 为已分配的最大修订号（含已撤销），绝不复用、绝不跳号。
	Tip int64
	// Head 为当前已发布修订号；0 表示尚无已发布修订（正常流程中不会出现）。
	Head int64
	// Pending 为在途（pending/approved 但未终结）修订号；0 表示没有在途修订。
	// 同一时间只允许一个在途修订，从机制上杜绝并发提交互相覆盖。
	Pending int64

	// Revisions 保留全部历史修订，键为修订号。
	Revisions map[int64]*Revision
	// Current 为 Head 修订的完整视图（按 key 排序）。
	Current []RecordSet

	SubmitKeys   map[string]IdemSubmit
	RollbackKeys map[string]IdemRollback
	ApprovalKeys map[string]IdemApprove
}

// Tx 是一个事务句柄。Update 内可读写，View 内只读（写操作会 panic）。
type Tx interface {
	// Zone 返回区域状态；不存在返回 nil。返回的指针在事务内有效。
	Zone(name string) *ZoneState
	// CreateZone 原子地创建区域；区域已存在时返回错误。
	CreateZone(z *ZoneState) error
	// Emit 写出一条传播事件并分配全局单调 ID。每个发布动作只调用一次。
	Emit(e *OutboxEvent)
	// Outbox 返回当前已写出的全部事件（按 ID 升序）。
	Outbox() []*OutboxEvent
}

// Store 是持久化抽象。Update 提供可串行化事务：fn 返回非 nil 错误时
// 整体回滚，不留任何部分状态。内存实现给出进程内串行语义，SQL 后端
// 可用单库事务实现同样的保证。
type Store interface {
	Update(ctx context.Context, fn func(tx Tx) error) error
	View(ctx context.Context, fn func(tx Tx) error) error
}
