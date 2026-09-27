package dnschange

import "time"

// RevisionKind 描述修订是如何产生的。
type RevisionKind string

const (
	// RevInit 是区域初始化时写入的首个修订。
	RevInit RevisionKind = "init"
	// RevChange 是一次普通的新增/替换/删除混合变更。
	RevChange RevisionKind = "change"
	// RevRollback 是复用某个历史已发布修订内容的回滚修订。
	RevRollback RevisionKind = "rollback"
)

// RevisionStatus 是修订的生命周期状态。
type RevisionStatus string

const (
	// StatusPending 已提交，尚在等待审批。
	StatusPending RevisionStatus = "pending"
	// StatusApproved 审批要求已满足，可以发布。
	StatusApproved RevisionStatus = "approved"
	// StatusPublished 已发布，成为区域当前版本。
	StatusPublished RevisionStatus = "published"
	// StatusCanceled 被提交者撤销，永不发布。
	StatusCanceled RevisionStatus = "canceled"
)

// RecordSet 是一个名称+类型下的完整记录集。一次 Upsert 即新增或整体替换。
type RecordSet struct {
	// Name 会在提交时规范化为小写 FQDN（以点结尾）。
	Name string `json:"name"`
	// Type 为大写的 DNS 记录类型，如 A/AAAA/CNAME/TXT/MX/NS。
	Type string `json:"type"`
	// TTL 秒。
	TTL uint32 `json:"ttl"`
	// Values 为记录值；不同类型有各自的合法性要求。
	Values []string `json:"values"`
}

// RecordSetRef 用名称与类型定位一个记录集，用于删除。
type RecordSetRef struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Policy 是审批策略。提交时整体快照到修订上，之后策略变更不影响在途修订。
type Policy struct {
	// RequiredApprovals 为发布所需的不同审批人数量；0 表示无需审批。
	RequiredApprovals int `json:"required_approvals"`
	// Approvers 为有资格审批的名单；为空表示任何人都可审批（仍受职责分离约束）。
	Approvers []string `json:"approvers,omitempty"`
	// SeparationOfDuties 为 true 时提交者不得审批自己的变更。
	SeparationOfDuties bool `json:"separation_of_duties,omitempty"`
}

// ZoneConfig 是区域级配置。
type ZoneConfig struct {
	// MinTTL/MaxTTL 限定全区域（变更后完整视图中）每条记录的 TTL。
	MinTTL uint32 `json:"min_ttl,omitempty"`
	MaxTTL uint32 `json:"max_ttl,omitempty"`
}

// Approval 记录一次审批动作。
type Approval struct {
	Approver  string    `json:"approver"`
	Comment   string    `json:"comment,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Revision 是一个不可变的区域修订（状态字段除外，状态迁移受状态机约束）。
type Revision struct {
	Zone   string         `json:"zone"`
	Number int64          `json:"number"`
	Kind   RevisionKind   `json:"kind"`
	Status RevisionStatus `json:"status"`

	// Base 是提交者声明的基准修订号；init 修订为 0。
	Base int64 `json:"base"`
	// SourceRevision 仅回滚修订使用，指向被复用内容的历史修订。
	SourceRevision int64 `json:"source_revision,omitempty"`

	// Records 是该修订对应的区域完整视图，始终按 key 排序。
	Records []RecordSet `json:"records"`

	// 变更内容本身（审计用）；回滚修订这两个列表为空。
	Upserts []RecordSet    `json:"upserts,omitempty"`
	Deletes []RecordSetRef `json:"deletes,omitempty"`

	Committer string `json:"committer"`
	Comment   string `json:"comment,omitempty"`

	// Policy 是提交时刻的策略快照。
	Policy Policy `json:"policy"`

	Approvals []Approval `json:"approvals,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	CanceledAt  *time.Time `json:"canceled_at,omitempty"`
}

// OutboxEvent 是发布时写出的一条传播事件。每个实际发布的修订只写一次。
type OutboxEvent struct {
	ID       int64        `json:"id"`
	Zone     string       `json:"zone"`
	Revision int64        `json:"revision"`
	Kind     RevisionKind `json:"kind"`
	// Records 为该发布修订的完整视图。
	Records   []RecordSet `json:"records"`
	CreatedAt time.Time   `json:"created_at"`
}

// InitZoneRequest 初始化一个区域，直接产生 1 号已发布修订。
type InitZoneRequest struct {
	Name    string      `json:"name"`
	Config  ZoneConfig  `json:"config"`
	Policy  Policy      `json:"policy"`
	Records []RecordSet `json:"records,omitempty"`
	Comment string      `json:"comment,omitempty"`
}

// SubmitChangeRequest 提交一次混合变更。
type SubmitChangeRequest struct {
	Zone         string         `json:"zone"`
	BaseRevision int64          `json:"base_revision"`
	Committer    string         `json:"committer"`
	Comment      string         `json:"comment,omitempty"`
	Upserts      []RecordSet    `json:"upserts,omitempty"`
	Deletes      []RecordSetRef `json:"deletes,omitempty"`
	// IdempotencyKey 可选。相同键+相同载荷重放返回原修订；载荷冲突报幂等错误。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ApproveRequest 审批一个在途修订。
type ApproveRequest struct {
	Zone           string `json:"zone"`
	Revision       int64  `json:"revision"`
	Approver       string `json:"approver"`
	Comment        string `json:"comment,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// PublishRequest 发布一个已满足审批要求的修订。
type PublishRequest struct {
	Zone     string `json:"zone"`
	Revision int64  `json:"revision"`
}

// CancelRequest 撤销一个在途修订（仅提交者本人）。
type CancelRequest struct {
	Zone      string `json:"zone"`
	Revision  int64  `json:"revision"`
	Requester string `json:"requester"`
	Comment   string `json:"comment,omitempty"`
}

// RollbackRequest 以某个历史已发布修订的内容创建一个新的在途修订。
type RollbackRequest struct {
	Zone           string `json:"zone"`
	SourceRevision int64  `json:"source_revision"`
	Committer      string `json:"committer"`
	Comment        string `json:"comment,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// SubmitResult 是提交与回滚的结果。
type SubmitResult struct {
	Revision *Revision
	// Replayed 为 true 表示这是幂等重放，没有创建新修订。
	Replayed bool
}

// ApproveResult 是审批结果。
type ApproveResult struct {
	Revision *Revision
	Approval *Approval
	Replayed bool
}

// PublishResult 是发布结果。
type PublishResult struct {
	Revision    *Revision
	OutboxEvent *OutboxEvent
}

// ZoneView 是区域查询视图。
type ZoneView struct {
	Name    string      `json:"name"`
	Config  ZoneConfig  `json:"config"`
	Policy  Policy      `json:"policy"`
	Tip     int64       `json:"tip"`
	Head    int64       `json:"head"`
	Records []RecordSet `json:"records"`
}
