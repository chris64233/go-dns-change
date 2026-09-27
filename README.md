# go-dns-change

DNS 区域记录集的**原子变更、审批发布与回滚**服务。提供区域初始化、变更提交、
审批、发布、撤销、回滚与修订查询能力，所有状态持久化在可替换的 `Store` 中，
并为每次实际发布写出恰好一条传播 outbox 事件。

开发环境：Go 1.23.0。

## 核心模型与保证

### 1. 单调修订号 + 变更后完整视图校验

- 每个区域按 `1, 2, 3, ...` 单调演进；`Tip` 是已分配的最大修订号，
  **永不复用、绝不跳号**（撤销的修订号也保留在历史里）。
- 一次变更可以混合 **upsert（新增或整体替换）** 与 **delete**：
  - upsert/delete 规范化后在批次内不得重复或互相撞键；
  - delete 目标必须在基准视图中存在。
- 校验针对**应用变更后的完整区域视图**，而非仅针对变更片段：
  - 名称规范化为小写 FQDN，标签语法、长度、通配符位置校验；
  - 记录名必须落在区域内；记录值按类型校验（A/AAAA 地址、CNAME/NS 目标、
    MX `优先级 域名`、TXT 长度），值去重，CNAME 必须单值；
  - TTL 必须落在区域配置的 `[min_ttl, max_ttl]` 区间；
  - CNAME 与同名任何其他类型冲突、CNAME 自指。

### 2. 基准修订号 + 原子提交

- 提交必须给出 `base_revision`，且等于当前已发布的 `head`，否则返回
  `revision_conflict`。
- 同一区域**同一时刻只允许一个在途修订**（pending/approved 未终结）。
  并发提交中恰好一个成功，其余得到 `revision_conflict`——不会互相静默覆盖。
- 全部校验通过**之后**才分配新修订号；事务内任何错误整体回滚：
  失败操作不留下部分记录，也不消耗修订号。

### 3. 策略快照 + 幂等审批

- 审批要求（人数、审批人名单、是否启用职责分离）在**提交时整体快照**到修订上。
  事后修改区域当前策略不影响任何在途修订。
- 发布要求满足快照中的 `required_approvals` 个**不同审批人**。
- 审批人必须在快照名单内（名单为空表示任何人可审批）；
  启用 `separation_of_duties` 时，提交者不得审批自己的变更。
- 审批天然幂等：同一审批人重复审批返回原审批，不重复计数；
  也支持 `idempotency_key`，键被不同载荷复用返回 `idempotency_error`。

### 4. 发布 / 撤销 / 回滚的确定性与 outbox

- 修订状态机：`pending → approved → published`，或在终结前 `→ canceled`。
  发布、撤销并发发生时，事务串行化保证**恰好一方成功**，顺序确定。
- 发布只在 `approved → published` 跃迁时写出**一条** outbox 事件并推进
  head/当前视图；重复发布幂等返回原事件，不会重复写 outbox。
- 撤销仅提交者本人可做；撤销释放在途槽位，后续提交拿到连续的下一个修订号。
- **回滚不改写历史**：它复用某个历史已发布修订的完整记录视图，创建一个
  `kind=rollback`、`source_revision=<旧修订>`、`base=<当前 head>` 的新在途修订，
  照常走审批与发布；旧修订保持 `published` 不变。

### 5. 错误分类

所有领域错误都携带错误码（`errors.go`，可用 `dns.CodeOf(err)` 提取）：

| 错误码 | 含义 |
| --- | --- |
| `validation_error` | 名称/TTL/记录值/冲突等校验失败，不创建修订 |
| `revision_conflict` | 区域/修订不存在、基准号不匹配、存在在途修订 |
| `approval_error` | 审批人资格、职责分离、审批数不足、无权撤销 |
| `state_error` | 非法状态迁移（发布已撤销/已发布修订等） |
| `idempotency_error` | 幂等键被不同载荷复用 |

## 代码结构

```
types.go         领域类型：记录集、策略、修订、outbox 事件、请求/响应
errors.go        五类错误码与 Error
validation.go    名称/记录集规范化、全视图校验、变更应用
store.go         Store/Tx 持久化抽象（可串行化事务语义）
memory_store.go  进程内 Store 实现（深拷贝隔离，失败整体丢弃）
service.go       业务服务：初始化/提交/审批/发布/撤销/回滚/查询/策略更新
httpapi/         REST 接口
cmd/server/      可运行的演示服务器（内存存储）
```

接入 SQL/其他持久化后端时，只需实现 `Store` 接口：`Update` 用单库可串行化
（或等价的行锁/SNAPSHOT）事务执行 `fn`，`fn` 返回错误时回滚；`Emit` 与状态
推进在同一事务内提交，即天然获得与内存实现相同的原子性与 outbox 保证。

## Service API（Go）

```go
store := dns.NewMemoryStore()
svc := dns.NewService(store)

// 初始化：直接产生 1 号已发布修订 + 第一条 outbox
rev, event, err := svc.InitZone(ctx, dns.InitZoneRequest{
    Name:   "example.com",
    Config: dns.ZoneConfig{MinTTL: 60, MaxTTL: 3600},
    Policy: dns.Policy{RequiredApprovals: 2, Approvers: []string{"alice", "bob"},
                       SeparationOfDuties: true},
    Records: []dns.RecordSet{{Name: "www", Type: "A", TTL: 300, Values: []string{"10.0.0.1"}}},
})

// 提交混合变更（必须带基准号）
res, _ := svc.SubmitChange(ctx, dns.SubmitChangeRequest{
    Zone: "example.com", BaseRevision: rev.Number, Committer: "carol",
    Upserts: []dns.RecordSet{{Name: "api", Type: "A", TTL: 300, Values: []string{"10.0.0.5"}}},
    Deletes: []dns.RecordSetRef{{Name: "old", Type: "A"}},
    IdempotencyKey: "change-123", // 可选
})

// 审批（满足快照人数；提交者自审批在 SoD 下被拒）
svc.Approve(ctx, dns.ApproveRequest{Zone: "example.com", Revision: 2, Approver: "alice"})
svc.Approve(ctx, dns.ApproveRequest{Zone: "example.com", Revision: 2, Approver: "bob"})

// 发布：推进 head，恰好写一条 outbox
pub, _ := svc.Publish(ctx, dns.PublishRequest{Zone: "example.com", Revision: 2})

// 撤销 / 回滚 / 查询
svc.Cancel(ctx, dns.CancelRequest{Zone: "example.com", Revision: 2, Requester: "carol"})
svc.Rollback(ctx, dns.RollbackRequest{Zone: "example.com", SourceRevision: 1, Committer: "carol"})
svc.GetZone(ctx, "example.com")
svc.GetRevision(ctx, "example.com", 2)
svc.ListRevisions(ctx, "example.com")
svc.ListOutbox(ctx, "example.com")
```

## HTTP API

启动演示服务：

```bash
go run ./cmd/server -addr :8080
```

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/zones` | 初始化区域（1 号修订直接发布） |
| GET | `/zones/{zone}` | 区域当前视图（head/tip/记录/策略） |
| PUT | `/zones/{zone}/policy` | 更新当前策略（不影响在途修订快照） |
| POST | `/zones/{zone}/changes` | 提交混合变更 |
| POST | `/zones/{zone}/revisions/{n}/approve` | 审批 |
| POST | `/zones/{zone}/revisions/{n}/publish` | 发布 |
| POST | `/zones/{zone}/revisions/{n}/cancel` | 撤销（提交者） |
| POST | `/zones/{zone}/rollbacks` | 以历史修订内容创建回滚修订 |
| GET | `/zones/{zone}/revisions` | 修订历史（按号升序） |
| GET | `/zones/{zone}/revisions/{n}` | 单个修订 |

错误码到 HTTP 状态：`validation_error → 422`，`revision_conflict → 409`
（不存在为 `404`），`approval_error → 403`，`state_error/idempotency_error → 409`。
幂等重放返回 `200` 且响应体 `replayed=true`，首次创建返回 `201`。

## 测试

```bash
go test ./... -race -count=1
```

测试覆盖：

- 名称规范化与各类校验失败（含变更后视图的 TTL/CNAME 冲突）；
- 校验失败不留修订、不跳号；基准号冲突与在途修订互斥；
- 审批配额、资格、职责分离、策略快照、审批幂等与键冲突；
- 发布 outbox 恰好一次、撤销权限与槽位释放；
- 回滚复用历史内容、旧历史不改写、回滚后 outbox 计数；
- 32 路并发提交恰好 1 个成功；50 轮发布/撤销竞争恰好一方成功；
- HTTP 全生命周期端到端与错误码映射。
