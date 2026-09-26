package dnschange

import "time"

// RRType 是 DNS 资源记录类型。
type RRType string

const (
	TypeA     RRType = "A"
	TypeAAAA  RRType = "AAAA"
	TypeCNAME RRType = "CNAME"
	TypeNS    RRType = "NS"
	TypeSOA   RRType = "SOA"
	TypeMX    RRType = "MX"
	TypeTXT   RRType = "TXT"
	TypeSRV   RRType = "SRV"
	TypeCAA   RRType = "CAA"
	TypePTR   RRType = "PTR"
)

// knownTypes 是被服务识别并允许出现在记录集中的类型。
var knownTypes = map[RRType]bool{
	TypeA: true, TypeAAAA: true, TypeCNAME: true, TypeNS: true,
	TypeSOA: true, TypeMX: true, TypeTXT: true, TypeSRV: true,
	TypeCAA: true, TypePTR: true,
}

// RecordSet 表示一个 DNS 资源记录集（同名同类型记录的集合）。
// Name 一律规范化为小写、以 "." 结尾的 FQDN。
type RecordSet struct {
	Name    string   `json:"name"`
	Type    RRType   `json:"type"`
	TTL     uint32   `json:"ttl"`
	Records []string `json:"records"`
}

// Key 返回记录集在区域视图中的唯一键。
func (rs RecordSet) Key() string { return rs.Name + "|" + string(rs.Type) }

// OpKind 是变更操作的种类。
type OpKind string

const (
	// OpAdd 新增一个此前不存在的记录集。
	OpAdd OpKind = "add"
	// OpReplace 整体替换一个已存在的记录集。
	OpReplace OpKind = "replace"
	// OpDelete 删除一个已存在的记录集（仅使用 Name 与 Type）。
	OpDelete OpKind = "delete"
)

// ChangeOp 是一次变更中的单个操作。
type ChangeOp struct {
	Kind      OpKind    `json:"kind"`
	RecordSet RecordSet `json:"record_set"`
}

// Policy 是区域的审批策略。每次提交变更时会把当前策略快照进变更，
// 后续审批资格与人数要求均以快照为准，不受策略后续修改影响。
type Policy struct {
	// RequiredApprovals 是发布前所需的不同审批人数量；0 表示无需审批。
	RequiredApprovals int `json:"required_approvals"`
	// EligibleApprovers 是有资格审批的人员列表；为空表示任何人可审批。
	EligibleApprovers []string `json:"eligible_approvers,omitempty"`
	// SeparationOfDuties 启用职责分离：提交者不得批准自己的变更。
	SeparationOfDuties bool `json:"separation_of_duties"`
	// MinTTL/MaxTTL 是区域允许的 TTL 边界；0 表示该方向不限制。
	MinTTL uint32 `json:"min_ttl,omitempty"`
	MaxTTL uint32 `json:"max_ttl,omitempty"`
}

// ChangeStatus 是变更的生命周期状态。
type ChangeStatus string

const (
	StatusPending   ChangeStatus = "pending"   // 已创建，等待审批
	StatusApproved  ChangeStatus = "approved"  // 审批要求已满足，可发布
	StatusPublished ChangeStatus = "published" // 已发布
	StatusWithdrawn ChangeStatus = "withdrawn" // 已撤销
)

// Approval 记录一次有效审批。
type Approval struct {
	Approver string    `json:"approver"`
	At       time.Time `json:"at"`
}

// Change 是一次区域变更申请，与一个修订号一一对应。
type Change struct {
	ID             string       `json:"id"`
	Zone           string       `json:"zone"`
	Revision       int64        `json:"revision"`
	BaseRevision   int64        `json:"base_revision"`
	Ops            []ChangeOp   `json:"ops"`
	Submitter      string       `json:"submitter"`
	Comment        string       `json:"comment,omitempty"`
	Policy         Policy       `json:"policy"` // 提交时的策略快照
	Approvals      []Approval   `json:"approvals,omitempty"`
	Status         ChangeStatus `json:"status"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	PayloadHash    string       `json:"payload_hash"`
	RollbackOf     int64        `json:"rollback_of,omitempty"` // 回滚目标修订号，非回滚为 0
	Seq            int64        `json:"seq"`                   // 区域级操作序号，保证确定顺序
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// Revision 是区域的一个修订，保存该修订号下区域的完整记录视图。
type Revision struct {
	Zone       string      `json:"zone"`
	Number     int64       `json:"number"`
	ChangeID   string      `json:"change_id"`
	Base       int64       `json:"base"`
	RecordSets []RecordSet `json:"record_sets"` // 完整区域视图，按键排序
	RollbackOf int64       `json:"rollback_of,omitempty"`
	Seq        int64       `json:"seq"`
	CreatedAt  time.Time   `json:"created_at"`
}

// OutboxEntry 是修订发布时写出的传播 outbox 条目。
// 每个实际发布的修订只写出一次。
type OutboxEntry struct {
	ID         string      `json:"id"`
	Zone       string      `json:"zone"`
	Revision   int64       `json:"revision"`
	RecordSets []RecordSet `json:"record_sets"`
	Seq        int64       `json:"seq"`
	CreatedAt  time.Time   `json:"created_at"`
}

// ZoneState 是区域的持久化聚合状态，包含全部历史。
type ZoneState struct {
	Name              string            `json:"name"`
	HeadRevision      int64             `json:"head_revision"`
	PublishedRevision int64             `json:"published_revision"`
	Policy            Policy            `json:"policy"`
	Seq               int64             `json:"seq"`
	Changes           []*Change         `json:"changes"`
	Revisions         []*Revision       `json:"revisions"`
	Outbox            []*OutboxEntry    `json:"outbox,omitempty"`
	Idem              map[string]string `json:"idem,omitempty"`     // 幂等键 -> 变更 ID
	Payloads          map[string]string `json:"payloads,omitempty"` // 幂等键 -> 负载哈希
}

// ZoneInfo 是区域的只读摘要。
type ZoneInfo struct {
	Name              string `json:"name"`
	HeadRevision      int64  `json:"head_revision"`
	PublishedRevision int64  `json:"published_revision"`
	Policy            Policy `json:"policy"`
	Seq               int64  `json:"seq"`
}

// SubmitInput 是提交变更的入参。
type SubmitInput struct {
	Zone           string
	BaseRevision   int64 // 提交者必须给出基准修订号
	Ops            []ChangeOp
	Submitter      string
	Comment        string
	IdempotencyKey string // 可选；相同键重复提交返回原变更
}
