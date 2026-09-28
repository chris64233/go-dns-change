# go-dns-change

DNS 区域记录集的原子变更、审批发布、**分阶段流量切换（灰度发布）**与回滚服务（Go 1.23）。

## 核心概念

- **区域（Zone）**：按单调递增的修订号（revision）演进。每个修订保存该版本下区域的
  **完整记录视图**，历史永不改写。
- **变更（Change）**：一次变更可混合 `add` / `replace` / `delete` 三种操作。
  校验针对**变更后的完整区域视图**进行，覆盖名称规范、TTL 边界与记录冲突
  （CNAME 排他、顶点 SOA/NS 约束、记录数据格式等）。
- **策略（Policy）**：区域的审批策略（所需审批人数、有资格审批人、职责分离、
  TTL 边界）。每次提交时把当前策略**快照**进变更，后续审批以快照为准。
- **修订（Revision）**：校验全部通过后才创建下一修订号；失败不消耗修订号。
  分阶段发布中的每个阶段视图、以及自动恢复视图也各占一个修订号。
- **Outbox**：每个实际发布的修订（含每次阶段推进）只写出一次传播 outbox 条目，
  供下游传播消费。
- **分阶段计划（Rollout Plan）**：对包含**可分流记录**的已审批变更，可声明多个
  连续阶段，把流量按权重从当前线上配置逐步切到目标修订。

## 关键保证

1. **原子性**：变更在区域锁内完成「校验 → 建修订 → 持久化」，任一环节失败
   不留下部分记录，也不跳过修订号。阶段推进与自动恢复同样在单个锁内原子完成：
   健康判定、阶段发布、outbox 书写要么全部生效，要么全部不发生。
2. **乐观并发**：提交者必须给出基准修订号（`BaseRevision`）。基准落后于当前
   头部时返回修订冲突错误，并发提交不会互相静默覆盖。
3. **审批**：满足提交时快照的审批要求后才能发布；审批调用幂等（同一审批人
   重复审批是空操作）；启用职责分离时提交者不得批准自己的变更。
4. **确定顺序**：发布、撤销、回滚、阶段推进、人工停止等所有变更操作按区域
   串行化，并赋予单调的区域级操作序号（`Seq`），并发下结果与某个串行顺序一致。
5. **回滚**：不改写历史，而是计算头部视图与目标修订的差异，作为一次普通变更
   提交（同样需要审批与发布），创建一个复用历史内容的新修订。
6. **幂等**：提交、回滚支持幂等键；健康样本按样本 ID 幂等。相同键 + 相同负载
   返回原结果；相同键 + 不同负载返回幂等错误。

## 分阶段流量切换

### 阶段计划

- 只有**已审批**且包含可分流记录的变更才能创建计划。可分流记录指：同一记录集
  在当前线上视图与目标修订中都存在，且目标修订为其**新增了记录值**
  （典型蓝绿场景：`A` 记录由 `10.0.0.1` 变为 `[10.0.0.1, 10.0.0.2]`）。
  CNAME/SOA 等单值类型、纯删除或 TTL-only 变更不可分流，应直接 `Publish`。
- 每个阶段声明：目标权重 `TargetWeight`（中间阶段 1..99，严格递增，
  **最后一个阶段必须为 100**）、最短观察时间 `MinObservation`、健康门槛
  `HealthThreshold` 与最少样本数 `MinSampleCount`。
- 计划创建时会预生成并校验**每个权重对应的完整区域视图**，因此所有权重必然构成
  从当前线上配置（权重 0）到目标修订（权重 100）的合法路径。
- **计划一经创建不可修改**（服务不提供任何更新入口）；同一区域同时只能有一个
  活跃计划。

### 阶段视图与权重模型

- 中间阶段的可分流记录集取新旧记录值并集：当前线上记录值分摊 `100-W`，
  目标修订新增的记录值分摊 `W`（多条时尽量均分，余数给靠前记录，总和恒为 100）。
  权重通过记录集上的稀疏 `Weights` 字段表达。
- 无法分流的变更（整集删除、新增记录集等）只在末阶段（权重 100）落地，
  中间视图保持线上内容，因此每个阶段视图都能通过完整的区域校验。

### 推进、健康与恢复

- `AdvanceStage` 每次调用只做一件事，且结果唯一：
  1. 当前阶段未发布 → 发布该阶段的**完整区域视图**（新修订号）并写出**唯一 outbox**；
  2. 已发布且达到最短观察时间、样本数与健康门槛 → 稳定点前移，发布下一阶段；
  3. 已发布但健康比例低于门槛 → **在同一原子操作内**创建并发布一个恢复到
     **上一稳定权重**的新修订（基线视图或上一健康阶段的加权视图），计划终止，
     历史阶段视图原样保留（不改写历史）；
  4. 末阶段（权重 100）健康通过 → 计划完成，目标变更转为 `published`，
     线上即目标修订内容。
- 观察时间不足或样本不足时返回 `state` 错误，不产生任何记录，也不消耗修订号。
- **健康样本绑定区域、修订与阶段**。只有当前计划当前阶段、且修订号匹配的样本
  才参与判定；旧阶段、其他修订或其他计划的样本即使重放也不参与。
  相同 `SampleID` 重放幂等，同 ID 不同内容返回幂等错误。
- `StopRollout` 人工停止：流量停在当前**已完整发布**的阶段，计划不再推进。
  人工停止与自动推进并发时由区域锁串行化，只会留下与某个串行顺序一致的结果，
  不会出现半成品阶段或重复 outbox。

### 与并发区域变更的关系

- 阶段一旦发布，头部即当前已发布阶段；此时提交新变更自然**以当前已发布阶段为
  基准**。新变更发布后，旧计划被标记为 `superseded` 并终止，**不能再推进或
  覆盖后来发布的修订**；新变更被撤销则旧计划可继续。
- 首个阶段发布之前（目标修订尚未上线）不允许在其之上叠加新变更，需要先停止计划。

### 查询

`GetPlan` 返回完整的执行视图：每个阶段的**实际区域内容**（该阶段修订的完整
记录集与权重）、发布/观察状态与 outbox 等**传播状态**、当前阶段的**健康依据**
（样本数、健康数、比例、时间范围），以及从本计划出发的**恢复链**
（`RecoveryChain`：失败阶段 → 恢复变更 → 恢复修订 → 以该修订为基线的下一计划）。

```go
// 1. 变更审批通过后创建三阶段计划（20% -> 50% -> 100%）
plan, err := svc.CreatePlan(dnschange.PlanInput{
    Zone:     "example.com",
    ChangeID: chg.ID,
    Stages: []dnschange.StageSpec{
        {TargetWeight: 20, MinObservation: time.Minute, HealthThreshold: 0.9, MinSampleCount: 5},
        {TargetWeight: 50, MinObservation: 5 * time.Minute, HealthThreshold: 0.9, MinSampleCount: 10},
        {TargetWeight: 100, MinObservation: 10 * time.Minute, HealthThreshold: 0.99, MinSampleCount: 20},
    },
})

// 2. 推进：发布阶段 -> 观察 -> 健康通过后发布下一阶段
res, err := svc.AdvanceStage("example.com", plan.ID) // OutcomeStagePublished

// 3. 上报健康样本（绑定修订与阶段；重放幂等）
svc.RecordHealthSample(dnschange.HealthSample{
    SampleID: "probe-20260928-0001", Zone: "example.com",
    Revision: res.Revision, Stage: res.Stage, Healthy: true,
})
res, err = svc.AdvanceStage("example.com", plan.ID)
// 观察不足/样本不足返回 state 错误；健康不达标返回 OutcomeRecovered（已原子恢复）；
// 末阶段健康通过最后一次推进返回 OutcomeCompleted。

// 人工停止（流量停在当前已发布阶段，重复停止幂等）
svc.StopRollout("example.com", plan.ID, "alice")

// 查询：各阶段实际内容、健康依据、传播状态、恢复链
detail, err := svc.GetPlan("example.com", plan.ID)
plans, err := svc.ListPlans("example.com")
```

## API 概览

```go
store := dnschange.NewFileStore("./data") // 或 NewMemoryStore()
svc := dnschange.NewService(store)

// 初始化区域（创建并发布引导修订 1：SOA + NS）
info, err := svc.InitZone("example.com", []string{"ns1.example.com"}, dnschange.Policy{
    RequiredApprovals:  1,
    EligibleApprovers:  []string{"alice", "bob"},
    SeparationOfDuties: true,
})

// 提交变更（混合操作，需给出基准修订号）
chg, err := svc.SubmitChange(dnschange.SubmitInput{
    Zone:         "example.com",
    BaseRevision: info.HeadRevision,
    Submitter:    "carol",
    IdempotencyKey: "req-42",
    Ops: []dnschange.ChangeOp{
        {Kind: dnschange.OpAdd, RecordSet: dnschange.RecordSet{
            Name: "www.example.com", Type: dnschange.TypeA, TTL: 300,
            Records: []string{"192.0.2.1"}}},
        {Kind: dnschange.OpDelete, RecordSet: dnschange.RecordSet{
            Name: "old.example.com", Type: dnschange.TypeA}},
    },
})

// 审批（幂等；资格取自提交时的策略快照）
chg, err = svc.Approve("example.com", chg.ID, "alice")

// 直接发布（重复发布幂等；每个修订只写一次 outbox）
// 注意：已创建分阶段计划的变更不能再直接发布，需使用 AdvanceStage/StopRollout。
rev, err := svc.Publish("example.com", chg.ID)

// 撤销（仅提交者；已发布或灰度进行中不可撤销，灰度中请用 StopRollout）
chg, err = svc.Withdraw("example.com", chg.ID, "carol")

// 回滚到历史修订（创建新修订，不改写历史）
rb, err := svc.Rollback("example.com", 2, "carol", "rb-1")

// 查询
rev, err = svc.GetRevision("example.com", 3)
revs, err = svc.ListRevisions("example.com")
entries, err = svc.ListOutbox("example.com")
```
