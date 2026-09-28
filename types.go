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

// WeightedRecord 给记录集中的单条记录赋流量权重。
type WeightedRecord struct {
	Record string `json:"record"`
	Weight int    `json:"weight"`
}

// RecordSet 表示一个 DNS 资源记录集（同名同类型记录的集合）。
// Name 一律规范化为小写、以 "." 结尾的 FQDN。
type RecordSet struct {
	Name    string   `json:"name"`
	Type    RRType   `json:"type"`
	TTL     uint32   `json:"ttl"`
	Records []string `json:"records"`
	// Weights 仅出现在分阶段发布的中间阶段视图里，稀疏地列出参与分流的记录值，
	// 权重合计 100；未列出的记录值（新旧版本共有的记录）始终全量保留。
	// 普通变更与最终阶段不携带权重。
	Weights []WeightedRecord `json:"weights,omitempty"`
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
	StatusPublished ChangeStatus = "published" // 已发布（普通发布）或分阶段计划已完成
	StatusWithdrawn ChangeStatus = "withdrawn" // 已撤销
	// 以下三个状态仅用于分阶段发布计划对应的变更。
	StatusRolling   ChangeStatus = "rolling"   // 分阶段发布进行中
	StatusStopped   ChangeStatus = "stopped"   // 人工停止，流量停在当前已发布阶段
	StatusRecovered ChangeStatus = "recovered" // 健康不达标，已创建恢复修订
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
	RollbackOf     int64        `json:"rollback_of,omitempty"`  // 回滚目标修订号，非回滚为 0
	PlanID         string       `json:"plan_id,omitempty"`      // 所属分阶段计划（仅计划相关变更）
	RecoveredBy    string       `json:"recovered_by,omitempty"` // 触发恢复后，新创建的恢复变更 ID
	Seq            int64        `json:"seq"`                    // 区域级操作序号，保证确定顺序
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// Revision 是区域的一个修订，保存该修订号下区域的完整记录视图。
// 分阶段发布中每个阶段视图也占用一个修订号（完整视图，历史永不改写）。
type Revision struct {
	Zone       string      `json:"zone"`
	Number     int64       `json:"number"`
	ChangeID   string      `json:"change_id"`
	Base       int64       `json:"base"`
	RecordSets []RecordSet `json:"record_sets"` // 完整区域视图，按键排序
	RollbackOf int64       `json:"rollback_of,omitempty"`
	// PlanID/StageIndex 非空表示该修订是某计划某阶段发布的加权视图。
	PlanID     string    `json:"plan_id,omitempty"`
	StageIndex int       `json:"stage_index,omitempty"`
	Seq        int64     `json:"seq"`
	CreatedAt  time.Time `json:"created_at"`
}

// OutboxEntry 是发布（含阶段推进）时写出的传播 outbox 条目。
// 每个实际发布的修订只写出一次，因此一次推进恰好产生一个唯一 outbox 条目。
type OutboxEntry struct {
	ID         string      `json:"id"`
	Zone       string      `json:"zone"`
	Revision   int64       `json:"revision"`
	RecordSets []RecordSet `json:"record_sets"`
	PlanID     string      `json:"plan_id,omitempty"`
	StageIndex int         `json:"stage_index,omitempty"`
	Seq        int64       `json:"seq"`
	CreatedAt  time.Time   `json:"created_at"`
}

// ZoneState 是区域的持久化聚合根，包含全部历史。
type ZoneState struct {
	Name              string            `json:"name"`
	HeadRevision      int64             `json:"head_revision"`
	PublishedRevision int64             `json:"published_revision"`
	Policy            Policy            `json:"policy"`
	Seq               int64             `json:"seq"`
	Changes           []*Change         `json:"changes"`
	Revisions         []*Revision       `json:"revisions"`
	Outbox            []*OutboxEntry    `json:"outbox,omitempty"`
	Plans             []*RolloutPlan    `json:"plans,omitempty"`
	HealthSamples     []*HealthSample   `json:"health_samples,omitempty"`
	Idem              map[string]string `json:"idem,omitempty"`     // 幂等键 -> 变更 ID
	Payloads          map[string]string `json:"payloads,omitempty"` // 幂等键 -> 负载哈希
}

// --- 分阶段流量切换 ---

// StageSpec 是创建计划时对单个阶段的声明；阶段计划创建后不可修改。
// TargetWeight 是该阶段「目标修订」记录对可分流记录分摊的流量百分比，
// 剩余 (100-TargetWeight) 保留给当前线上版本；最后一个阶段必须为 100。
type StageSpec struct {
	TargetWeight    int           `json:"target_weight"`    // 目标修订记录的权重 0..100，严格递增，末阶段 100
	MinObservation  time.Duration `json:"min_observation"`  // 最短观察时间
	HealthThreshold float64       `json:"health_threshold"` // 允许推进的最低健康比例 (0..1]
	MinSampleCount  int           `json:"min_sample_count"` // 判定前至少需要的样本数
	Comment         string        `json:"comment,omitempty"`
}

// PlanStatus 是分阶段计划的生命周期状态。
type PlanStatus string

const (
	PlanActive    PlanStatus = "active"    // 还有阶段待观察/推进
	PlanCompleted PlanStatus = "completed" // 全部阶段已发布，线上即目标修订内容
	PlanStopped   PlanStatus = "stopped"   // 人工停止
	PlanRecovered PlanStatus = "recovered" // 健康不达标，已回退到上一稳定权重
)

// RolloutPlan 是一次分阶段流量切换计划，创建后不可变（除执行状态字段外）。
type RolloutPlan struct {
	ID             string         `json:"id"`
	Zone           string         `json:"zone"`
	ChangeID       string         `json:"change_id"`       // 目标变更
	TargetRevision int64          `json:"target_revision"` // 目标修订（变更对应的原始修订号）
	BaseRevision   int64          `json:"base_revision"`   // 创建时的线上修订（权重 0 起点）
	Stages         []StageRuntime `json:"stages"`
	Status         PlanStatus     `json:"status"`
	// CurrentStage 是当前待观察/推进的阶段下标；completed 时为 len(Stages)。
	CurrentStage int `json:"current_stage"`
	// StableRevision 是最近一个被视为稳定的线上修订：计划开始时为 BaseRevision，
	// 每次健康完成一个阶段后前移到该阶段修订，恢复时回退到此修订的权重。
	StableRevision int64 `json:"stable_revision"`
	// RecoveryChangeID 是最近一次健康失败自动创建的恢复变更 ID（可形成恢复链）。
	RecoveryChangeID string `json:"recovery_change_id,omitempty"`
	// Superseded 在计划执行期间区域出现更新的发布（含其他计划终态/普通发布）时置位，
	// 此时旧计划的任何阶段都不能再推进，避免覆盖后来发布的修订。
	Superseded bool      `json:"superseded"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// StageRuntime 是阶段的不可变声明加上其执行状态（实际区域内容与 outbox 在修订/outbox 中按 PlanID+StageIndex 关联）。
type StageRuntime struct {
	Spec        StageSpec `json:"spec"`
	Published   bool      `json:"published"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	ObservedAt  time.Time `json:"observed_at,omitempty"` // 健康判定通过时刻
	Revision    int64     `json:"revision,omitempty"`    // 该阶段发布的修订号
	OutboxID    string    `json:"outbox_id,omitempty"`
	// Reverted/RevertedToRevision 在该阶段被自动恢复时记录恢复去向。
	Reverted           bool  `json:"reverted,omitempty"`
	RevertedToRevision int64 `json:"reverted_to_revision,omitempty"`
}

// HealthSample 是一条健康样本，必须绑定区域、修订与阶段。
// 相同样本（按 SampleID）重放幂等；只有当前计划当前阶段、且修订匹配的样本才参与判定。
type HealthSample struct {
	SampleID string    `json:"sample_id"`
	Zone     string    `json:"zone"`
	Revision int64     `json:"revision"`
	Stage    int       `json:"stage"`
	Healthy  bool      `json:"healthy"`
	At       time.Time `json:"at"`
}

// PlanInput 是创建分阶段计划的入参。
type PlanInput struct {
	Zone     string
	ChangeID string
	Stages   []StageSpec
}

// StageView 是查询返回的单阶段实际视图信息。
type StageView struct {
	PlanID       string
	Index        int
	Spec         StageSpec
	Status       PlanStatus
	Stage        StageRuntime
	RecordSets   []RecordSet // 该阶段发布的实际完整区域内容；未发布为 nil
	StableWeight int         // 当前稳定视图（上一阶段）的目标权重
}

// PlanDetail 是计划查询结果，展示各阶段实际区域内容、健康依据、传播状态与恢复链。
type PlanDetail struct {
	Plan             *RolloutPlan
	BaseRecordSets   []RecordSet
	TargetRecordSets []RecordSet
	Stages           []StageView
	// HealthStats 是当前阶段的健康统计（样本数、健康数、比例、最早/最晚样本时间）。
	Health        HealthStats
	RecoveryChain []RecoveryLink // 从本次计划出发的恢复链（按时间）
}

// HealthStats 是当前阶段健康判定依据。
type HealthStats struct {
	Stage        int
	Revision     int64
	Total        int
	HealthyCount int
	Ratio        float64
	EarliestAt   time.Time
	LatestAt     time.Time
}

// RecoveryLink 表示恢复链上的一环：计划 -> 恢复变更 -> 恢复发布的修订。
type RecoveryLink struct {
	PlanID           string
	FromStage        int
	FromRevision     int64
	StableRevision   int64 // 恢复目标（上一稳定权重）
	RecoveryChangeID string
	RecoveryRevision int64 // 实际恢复发布的修订号；未发布为 0
	PlanStatus       PlanStatus
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
