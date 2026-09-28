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
	// Weights 与 Records 平行的相对权重，仅分阶段发布的混合视图使用。
	// 为空表示传统等权记录集；非空时长度必须与 Records 相同，且至少一个 > 0。
	// 下游按相对权重（而非百分比）分流，因此无需归一化为 100。
	Weights []uint32 `json:"weights,omitempty"`
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
	PlanID         string       `json:"plan_id,omitempty"`     // 分阶段发布产生的合成变更
	StageIndex     int          `json:"stage_index,omitempty"`
	Seq            int64        `json:"seq"` // 区域级操作序号，保证确定顺序
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
	PlanID     string      `json:"plan_id,omitempty"` // 阶段推进/自动恢复产生的修订
	StageIndex int         `json:"stage_index,omitempty"`
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
	PlanID     string      `json:"plan_id,omitempty"` // 阶段发布产生的传播条目
	StageIndex int         `json:"stage_index,omitempty"`
	Seq        int64       `json:"seq"`
	CreatedAt  time.Time   `json:"created_at"`
}

// HealthSample 是绑定到 区域/计划/阶段 的健康样本。
// 相同 (PlanID, StageIndex, SampleKey) 重放幂等；阶段或修订不匹配的样本被拒绝。
type HealthSample struct {
	PlanID     string    `json:"plan_id"`
	StageIndex int       `json:"stage_index"`
	Revision   int64     `json:"revision"` // 该阶段实际发布的修订，参与判断前再次校验
	SampleKey  string    `json:"sample_key"`
	Healthy    bool      `json:"healthy"`
	At         time.Time `json:"at"`
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
	Plans             []*Plan           `json:"plans,omitempty"`
	Samples           []*HealthSample   `json:"samples,omitempty"`
	SampleSeen        map[string]bool   `json:"sample_seen,omitempty"` // 样本去重键集合
	PlanIdem          map[string]string `json:"plan_idem,omitempty"`   // 计划幂等键 -> 计划 ID
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

// --- 分阶段流量切换 ---

// PlanStatus 是分阶段发布计划的生命周期状态。
type PlanStatus string

const (
	PlanActive     PlanStatus = "active"     // 计划执行中，可上报样本、停止、推进
	PlanCompleted  PlanStatus = "completed"  // 最后一个阶段已发布，到达目标修订
	PlanAborted    PlanStatus = "aborted"    // 人工停止或健康不达标自动恢复，终态
	PlanSuperseded PlanStatus = "superseded" // 期间出现以已发布阶段为基准的新变更，终态
)

// Stage 是阶段计划中的一个不可变阶段。
// TargetWeight 是目标修订记录的相对权重（0..100），当前线上配置记录取 100-TargetWeight；
// 首阶段权重必须 > 0，权重严格递增，末阶段必须为 100（完全切换到目标修订）。
type Stage struct {
	Index           int           `json:"index"`            // 从 0 开始
	TargetWeight    uint32        `json:"target_weight"`    // 目标记录权重
	MinObservation  time.Duration `json:"min_observation"`  // 推进前的最短观察时间
	HealthThreshold float64       `json:"health_threshold"` // 成功率门槛 [0,1]
	MinSamples      int           `json:"min_samples"`      // 判定所需最小样本数
}

// StageState 是单个阶段的运行时状态。
type StageState struct {
	Index            int       `json:"index"`
	PublishedRev     int64     `json:"published_revision"` // 该阶段发布的合成修订号（推进后非 0）
	PublishedAt      time.Time `json:"published_at,omitempty"`
	OutboxID         string    `json:"outbox_id,omitempty"`
	ObservationStart time.Time `json:"observation_start,omitempty"` // 首个样本时间
	LastSampleAt     time.Time `json:"last_sample_at,omitempty"`
	TotalSamples     int64     `json:"total_samples"`
	HealthySamples   int64     `json:"healthy_samples"`
	AdvancedAt       time.Time `json:"advanced_at,omitempty"`
}

// Plan 是针对一个已审批修订的分阶段发布计划，创建后阶段定义不可修改。
type Plan struct {
	ID             string       `json:"id"`
	Zone           string       `json:"zone"`
	TargetChangeID string       `json:"target_change_id"`
	TargetRevision int64        `json:"target_revision"` // 目标修订（已审批）
	BaseRevision   int64        `json:"base_revision"`   // 创建计划时的线上修订
	Stages         []Stage      `json:"stages"`          // 不可变阶段定义
	CurrentStage   int          `json:"current_stage"`   // 当前阶段下标
	Status         PlanStatus   `json:"status"`
	TrafficKeys    []string     `json:"traffic_keys"` // 可分流记录集键（排序）
	StageStates    []StageState `json:"stage_states"`
	RecoveryOf     int64        `json:"recovery_of,omitempty"` // 自动恢复所恢复到的稳定修订号
	RecoveryChange string       `json:"recovery_change,omitempty"`
	StopActor      string       `json:"stop_actor,omitempty"`
	StopReason     string       `json:"stop_reason,omitempty"`
	Seq            int64        `json:"seq"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// StageView 描述某阶段实际发布的完整区域视图与健康依据，供查询展示。
type StageView struct {
	Plan        *Plan
	Stage       Stage
	State       StageState
	RecordSets  []RecordSet // 该阶段发布的完整区域视图
	HealthRatio float64     // healthy/total
	HealthMet   bool        // 门槛、最短观察时间、最小样本是否全部满足
	OutboxID    string
	OutboxSeq   int64
}

// PlanDetail 是计划的完整只读视图：各阶段实际内容、健康依据、传播状态、恢复链。
type PlanDetail struct {
	Plan             *Plan
	Stages           []StageView
	RecoveryChangeID string
	RecoveryRevision int64
}

// HealthSampleInput 是健康样本上报入参。Zone/PlanID/StageIndex 由服务端绑定，
// 调用方只需提供 SampleKey 与 Healthy；服务端校验阶段归属。
type HealthSampleInput struct {
	Zone       string
	PlanID     string
	Stage      int
	SampleKey  string // 幂等键：相同样本重放不重复计数
	Healthy    bool
	ObservedAt time.Time // 可选样本时间，默认服务端当前时间
}

// CreatePlanInput 是创建阶段计划的入参。
type CreatePlanInput struct {
	Zone           string
	ChangeID       string // 已审批且包含可分流记录的变更
	Stages         []StageInput
	IdempotencyKey string
}

// StageInput 是创建阶段的入参。
type StageInput struct {
	TargetWeight    uint32
	MinObservation  time.Duration
	HealthThreshold float64
	MinSamples      int
}
