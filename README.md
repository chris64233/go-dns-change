# go-dns-change

DNS 区域记录集的原子变更、审批发布、**分阶段流量切换**与回滚服务（Go 1.23）。

## 核心概念

- **区域（Zone）**：按单调递增的修订号（revision）演进。每个修订保存该版本下区域的
  **完整记录视图**，历史永不改写。
- **变更（Change）**：一次变更可混合 `add` / `replace` / `delete` 三种操作。
  校验针对**变更后的完整区域视图**进行，覆盖名称规范、TTL 边界与记录冲突
  （CNAME 排他、顶点 SOA/NS 约束、记录数据格式等）。
- **策略（Policy）**：区域的审批策略（所需审批人数、有资格审批人、职责分离、
  TTL 边界）。每次提交时把当前策略**快照**进变更，后续审批以快照为准。
- **修订（Revision）**：校验全部通过后才创建下一修订号；失败不消耗修订号。
- **Outbox**：每个实际发布的修订只写出一次传播 outbox 条目，供下游传播消费。
- **阶段计划（Plan）**：针对已审批且包含**可分流记录**（同名 A/AAAA 记录数据替换）
  的变更，创建若干连续阶段，把流量按权重从当前线上配置逐步切换到目标修订。

## 分阶段流量切换

### 阶段计划

对一个**已审批**、且差异可分流的变更调用 `CreatePlan`：

- 每个阶段声明目标权重 `TargetWeight`（0..100）、最短观察时间 `MinObservation`、
  健康门槛 `HealthThreshold`（成功率 [0,1]）与最小样本数 `MinSamples`。
- 阶段定义**创建后不可修改**。权重必须从当前线上配置（权重 0）严格递增，
  末阶段必须为 `100`，从而形成一条到目标修订的合法路径。
- 仅允许**可分流**差异：同一记录集（同名同类型）的 A/AAAA **记录数据**变化。
  新增/删除记录集、非地址类型变更、仅 TTL 变化都不可渐进发布（返回校验错误）。
- 同一区域同时只允许一个进行中（`active`）的计划。

### 推进与传播

`AdvancePlan` 在区域锁内串行推进，每个阶段发布一个**即发布的合成修订**：

1. 首次推进：发布当前阶段的**完整区域视图**（可分流记录集按相对权重混合
   旧/新地址，例如旧记录权重 `100-w`、新记录权重 `w`），并写出**唯一**的
   传播 outbox 条目。
2. 后续推进：先判定当前阶段——最短观察时间与最小样本数未满足时拒绝（不发布、
   不跳号）；健康达标则发布下一阶段；末阶段权重 `100` 的视图即目标修订，
   计划转为 `completed`，目标变更标记为已发布。
3. 失败的推进不留下部分记录集，也不跳过区域修订号。

### 健康样本

`RecordHealthSample` 上报样本，样本由服务端强制绑定到
**区域 / 计划 / 阶段 / 该阶段发布的修订**：

- 只接受当前进行中阶段的样本；旧阶段、未来阶段、其他修订或已终止计划的样本
  一律拒绝，不参与当前判断。
- 相同 `(计划, 阶段, SampleKey)` 的样本**重放幂等**，不重复计数。

### 自动恢复与人工停止

- 当前阶段观察充分但健康**不达标**时，推进不会继续放量，而是**自动创建一个
  恢复上一稳定权重的新修订并立即发布**——稳定权重取上一已发布阶段，首阶段则
  回到计划基准（原线上配置）。恢复不改写历史，恢复修订通过 `RollbackOf` 关联
  稳定修订，计划转为 `aborted`。
- `StopPlan` 人工停止同样触发上述恢复。
- 健康判定、人工停止与下一阶段推进在区域锁内互斥，**并发时只有一个结果**。

### 与并发区域变更的关系

- 计划执行期间出现的新区域变更，以**当前已发布阶段**为基准提交（乐观并发控制）。
- 一旦该新变更发布，旧计划的线上基准即失效：后续推进/停止/上报会被基线守卫
  拒绝，计划转为 `superseded`，**旧计划不会覆盖后来发布的修订**。
- 头部仍堆叠着“已提交未发布”的存活变更时，阶段操作会被阻止；撤回或发布后
  计划才可继续，保证每个阶段视图基于线性历史。
- 活跃计划的目标变更不能被直接 `Publish`（绕过放量）或 `Withdraw`。

### 查询

`GetPlan` 返回 `PlanDetail`，展示：

- 各阶段**实际发布的完整区域内容**（未发布阶段给出按权重计算的预览）；
- 各阶段的**健康依据**（样本总数、健康数、成功率、门槛是否满足）；
- 各阶段的**传播状态**（outbox ID、序号）；
- **恢复链**（自动/人工恢复到的稳定修订、恢复变更 ID）。

## 关键保证

1. **原子性**：变更在区域锁内完成「校验 → 建修订 → 持久化」，任一环节失败
   不留下部分记录，也不跳过修订号。阶段推进/恢复同样在锁内“全有或全无”。
2. **乐观并发**：提交者必须给出基准修订号（`BaseRevision`）。基准落后于当前
   头部时返回修订冲突错误，并发提交不会互相静默覆盖。
3. **审批**：满足提交时快照的审批要求后才能发布；审批调用幂等（同一审批人
   重复审批是空操作）；启用职责分离时提交者不得批准自己的变更。
4. **确定顺序**：发布、撤销、回滚、阶段推进、停止、样本等所有变更操作按区域
   串行化，并赋予单调的区域级操作序号（`Seq`），并发下结果与某个串行顺序一致。
5. **回滚/恢复**：不改写历史，而是计算视图差异并创建复用历史内容的新修订；
   阶段不健康时自动恢复到上一稳定权重。
6. **幂等**：提交、回滚与计划创建支持幂等键；健康样本按样本键去重重放。
   相同键 + 相同负载返回原对象；相同键 + 不同负载返回幂等错误。

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

// 提交一个把 www A 记录替换为新地址的变更（可分流），并审批
chg, err := svc.SubmitChange(dnschange.SubmitInput{
    Zone: "example.com", BaseRevision: info.HeadRevision, Submitter: "carol",
    Ops: []dnschange.ChangeOp{{Kind: dnschange.OpReplace, RecordSet: dnschange.RecordSet{
        Name: "www.example.com", Type: dnschange.TypeA, TTL: 300,
        Records: []string{"192.0.2.10"}}}},
})
chg, err = svc.Approve("example.com", chg.ID, "alice")

// 创建阶段计划：10% -> 50% -> 100%，定义不可修改
plan, err := svc.CreatePlan(dnschange.CreatePlanInput{
    Zone:     "example.com",
    ChangeID: chg.ID,
    Stages: []dnschange.StageInput{
        {TargetWeight: 10, MinObservation: 30 * time.Second, HealthThreshold: 0.95, MinSamples: 10},
        {TargetWeight: 50, MinObservation: 30 * time.Second, HealthThreshold: 0.95, MinSamples: 10},
        {TargetWeight: 100, MinObservation: time.Second, HealthThreshold: 0.9, MinSamples: 1},
    },
    IdempotencyKey: "plan-www-42",
})

// 首次推进发布阶段 0（10% 新地址 + 90% 旧地址的完整视图 + 唯一 outbox）
plan, err = svc.AdvancePlan("example.com", plan.ID)

// 观察期内上报健康样本（绑定区域/计划/阶段/修订，重放幂等）
_, err = svc.RecordHealthSample(dnschange.HealthSampleInput{
    Zone: "example.com", PlanID: plan.ID, Stage: 0,
    SampleKey: "probe-0001", Healthy: true,
})

// 观察充分且健康达标后继续推进到阶段 1、阶段 2（100% 即目标修订，计划完成）
plan, err = svc.AdvancePlan("example.com", plan.ID)

// 不健康时推进会自动创建恢复到上一稳定权重的新修订；也可人工停止
plan, err = svc.StopPlan("example.com", plan.ID, "alice")

// 查询各阶段实际内容、健康依据、传播状态与恢复链
detail, err := svc.GetPlan("example.com", plan.ID)
plans, err := svc.ListPlans("example.com")

// 普通发布（未挂在活跃计划上的已审批变更）
rev, err := svc.Publish("example.com", chg.ID)

// 回滚到历史修订（创建新修订，不改写历史）
rb, err := svc.Rollback("example.com", 2, "carol", "rb-1")

// 查询
rev, err = svc.GetRevision("example.com", 3)
revs, err := svc.ListRevisions("example.com")
entries, err := svc.ListOutbox("example.com")
```

## 错误分类

所有领域错误都是 `*dnschange.Error`，按 `Kind` 区分，可用谓词判断：

| 类别 | 谓词 | 含义 |
| --- | --- | --- |
| `validation` | `IsValidation` | 名称 / TTL / 记录冲突等校验失败（含逐项 `Issues`） |
| `revision_conflict` | `IsRevisionConflict` | 基准修订号落后 |
| `approval` | `IsApproval` | 审批人无资格、职责分离冲突、非提交者撤销 |
| `state` | `IsState` | 非法状态迁移（发布未批准、撤销已发布等） |
| `idempotency` | `IsIdempotency` | 同一幂等键携带不同负载 |
| `not_found` | `IsNotFound` | 区域 / 变更 / 修订不存在 |

## 持久化

`Store` 接口抽象持久化，提供两种实现：

- `MemoryStore`：进程内存储（读写返回深拷贝），适合测试与嵌入。
- `FileStore`：每区域一个 JSON 文件，临时文件 + rename 原子写入，
  状态与全部历史（变更、修订、outbox、幂等键）跨进程持久化。

## 运行测试

    go test ./...          # 全部测试
    go test -race ./...    # 含并发（提交/发布/撤销/回滚）竞态检测
