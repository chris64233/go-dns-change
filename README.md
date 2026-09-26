# go-dns-change

DNS 区域记录集的原子变更、审批发布与回滚服务（Go 1.23）。

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

## 关键保证

1. **原子性**：变更在区域锁内完成「校验 → 建修订 → 持久化」，任一环节失败
   不留下部分记录，也不跳过修订号。
2. **乐观并发**：提交者必须给出基准修订号（`BaseRevision`）。基准落后于当前
   头部时返回修订冲突错误，并发提交不会互相静默覆盖。
3. **审批**：满足提交时快照的审批要求后才能发布；审批调用幂等（同一审批人
   重复审批是空操作）；启用职责分离时提交者不得批准自己的变更。
4. **确定顺序**：发布、撤销、回滚等所有变更操作按区域串行化，并赋予单调的
   区域级操作序号（`Seq`），并发下结果与某个串行顺序一致。
5. **回滚**：不改写历史，而是计算头部视图与目标修订的差异，作为一次普通变更
   提交（同样需要审批与发布），创建一个复用历史内容的新修订。
6. **幂等**：提交与回滚支持幂等键。相同键 + 相同负载返回原变更；
   相同键 + 不同负载返回幂等错误。

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

// 发布（重复发布幂等；每个修订只写一次 outbox）
rev, err := svc.Publish("example.com", chg.ID)

// 撤销（仅提交者；已发布不可撤销）
chg, err = svc.Withdraw("example.com", chg.ID, "carol")

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
