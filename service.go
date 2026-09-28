package dnschange

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Service 提供 DNS 区域的原子变更、审批、发布、回滚与修订查询。
// 所有变更操作按区域串行化，并赋予单调递增的区域级操作序号，
// 因此并发的发布、撤销与回滚总是落在一个确定的全序上。
type Service struct {
	store Store

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	now func() time.Time // 可注入，便于测试
}

// NewService 基于给定存储创建服务。
func NewService(store Store) *Service {
	return &Service{
		store: store,
		locks: map[string]*sync.Mutex{},
		now:   func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) lockFor(zone string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[zone]
	if !ok {
		l = &sync.Mutex{}
		s.locks[zone] = l
	}
	return l
}

// mutate 在区域锁内加载状态、执行 fn，且仅当 fn 成功时才持久化，
// 因此失败的操作不会留下任何部分记录。
func (s *Service) mutate(zone string, fn func(z *ZoneState) error) error {
	name, verr := normalizeName(zone)
	if verr != nil {
		return verr
	}
	lock := s.lockFor(name)
	lock.Lock()
	defer lock.Unlock()
	z, err := s.store.LoadZone(name)
	if err != nil {
		return err
	}
	if err := fn(z); err != nil {
		return err
	}
	return s.store.SaveZone(z)
}

// read 在区域锁内加载状态并执行只读操作。
func (s *Service) read(zone string, fn func(z *ZoneState) error) error {
	name, verr := normalizeName(zone)
	if verr != nil {
		return verr
	}
	lock := s.lockFor(name)
	lock.Lock()
	defer lock.Unlock()
	z, err := s.store.LoadZone(name)
	if err != nil {
		return err
	}
	return fn(z)
}

// InitZone 初始化一个区域：以 SOA 与 NS 记录集创建第 1 号修订并直接发布
// （引导修订无需审批），同时写出对应的传播 outbox 条目。
func (s *Service) InitZone(zone string, nameServers []string, policy Policy) (*ZoneInfo, error) {
	name, verr := normalizeName(zone)
	if verr != nil {
		return nil, verr
	}
	if len(nameServers) == 0 {
		return nil, errf(KindValidation, "zone %s requires at least one name server", name)
	}
	if policy.RequiredApprovals < 0 {
		return nil, errf(KindValidation, "required approvals must be >= 0")
	}
	if policy.MinTTL > 0 && policy.MaxTTL > 0 && policy.MinTTL > policy.MaxTTL {
		return nil, errf(KindValidation, "min ttl %d exceeds max ttl %d", policy.MinTTL, policy.MaxTTL)
	}

	lock := s.lockFor(name)
	lock.Lock()
	defer lock.Unlock()

	if _, err := s.store.LoadZone(name); err == nil {
		return nil, errf(KindState, "zone %s already exists", name)
	} else if !IsNotFound(err) {
		return nil, err
	}

	now := s.now()
	nsRecords := make([]string, len(nameServers))
	for i, ns := range nameServers {
		fqdn, verr := normalizeName(ns)
		if verr != nil {
			return nil, verr
		}
		nsRecords[i] = fqdn
	}
	view := map[string]RecordSet{
		name + "|SOA": {
			Name: name, Type: TypeSOA, TTL: 3600,
			Records: []string{fmt.Sprintf("%s hostmaster.%s 1 7200 3600 1209600 300", nsRecords[0], name)},
		},
		name + "|NS": {Name: name, Type: TypeNS, TTL: 3600, Records: nsRecords},
	}
	if verr := validateZoneView(name, view, policy); verr != nil {
		return nil, verr
	}

	chg := &Change{
		ID:           changeID(name, 1),
		Zone:         name,
		Revision:     1,
		BaseRevision: 0,
		Submitter:    "system",
		Comment:      "zone bootstrap",
		Policy:       policy,
		Status:       StatusPublished,
		Seq:          1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	rev := &Revision{
		Zone: name, Number: 1, ChangeID: chg.ID, Base: 0,
		RecordSets: sortedSets(view), Seq: 1, CreatedAt: now,
	}
	z := &ZoneState{
		Name:              name,
		HeadRevision:      1,
		PublishedRevision: 1,
		Policy:            policy,
		Seq:               1,
		Changes:           []*Change{chg},
		Revisions:         []*Revision{rev},
		Outbox: []*OutboxEntry{{
			ID: outboxID(name, 1), Zone: name, Revision: 1,
			RecordSets: rev.RecordSets, Seq: 1, CreatedAt: now,
		}},
		Idem:     map[string]string{},
		Payloads: map[string]string{},
	}
	if err := s.store.SaveZone(z); err != nil {
		return nil, err
	}
	return zoneInfo(z), nil
}

// SubmitChange 提交一次变更。只有全部校验（针对变更后的完整区域视图）
// 成功才会创建下一修订；校验失败或基准修订号落后时不产生任何记录，
// 也不会消耗修订号。携带相同幂等键与相同负载的重复提交返回原变更；
// 相同幂等键但负载不同则返回幂等错误。
func (s *Service) SubmitChange(in SubmitInput) (*Change, error) {
	zone, verr := normalizeName(in.Zone)
	if verr != nil {
		return nil, verr
	}
	ops, verr := normalizeOps(in.Ops)
	if verr != nil {
		return nil, verr
	}
	if in.Submitter == "" {
		return nil, errf(KindValidation, "submitter is required")
	}
	in.Zone, in.Ops = zone, ops

	var out *Change
	err := s.mutate(zone, func(z *ZoneState) error {
		chg, err := s.submitLocked(z, in, 0)
		if err != nil {
			return err
		}
		out = chg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// submitLocked 在已持有区域锁、已加载状态的前提下执行提交逻辑。
// rollbackOf 非 0 表示该变更是回滚到指定修订。
func (s *Service) submitLocked(z *ZoneState, in SubmitInput, rollbackOf int64) (*Change, error) {
	hash, err := hashOps(in.Ops)
	if err != nil {
		return nil, err
	}

	// 幂等检查：同键同负载直接返回原变更，同键不同负载报错。
	if in.IdempotencyKey != "" {
		if id, ok := z.Idem[in.IdempotencyKey]; ok {
			if z.Payloads[in.IdempotencyKey] != hash {
				return nil, errf(KindIdempotency,
					"idempotency key %q was already used with a different payload", in.IdempotencyKey)
			}
			return findChange(z, id), nil
		}
	}

	// 乐观并发控制：基准修订号必须等于当前头部，否则拒绝而非静默覆盖。
	if in.BaseRevision != z.HeadRevision {
		return nil, errf(KindRevision,
			"base revision %d is stale: zone %s head is at revision %d",
			in.BaseRevision, z.Name, z.HeadRevision)
	}
	// 分阶段计划刚创建、首个阶段尚未发布时，头部是尚未上线的目标修订；
	// 此时禁止在其之上叠加新变更（需要先停止计划）。阶段一旦发布，
	// 头部即当前已发布阶段，提交自然以该阶段为基准。
	if p := activePlan(z); p != nil && z.HeadRevision != z.PublishedRevision {
		return nil, errf(KindState,
			"rollout plan %s is active before its first stage; stop the plan before submitting new changes", p.ID)
	}

	view := viewAt(z, z.HeadRevision)
	newView, verr := applyOps(view, in.Ops)
	if verr != nil {
		return nil, verr
	}
	// 权重只是分阶段中间视图的内部产物；普通变更与回滚产生的视图一律不带权重。
	for k, rs := range newView {
		rs.Weights = nil
		newView[k] = rs
	}
	if verr := validateZoneView(z.Name, newView, z.Policy); verr != nil {
		return nil, verr
	}

	// 全部校验通过后才分配下一修订号，保证修订号不被失败操作消耗。
	now := s.now()
	z.Seq++
	rev := z.HeadRevision + 1
	status := StatusPending
	if z.Policy.RequiredApprovals == 0 {
		status = StatusApproved
	}
	chg := &Change{
		ID:             changeID(z.Name, rev),
		Zone:           z.Name,
		Revision:       rev,
		BaseRevision:   in.BaseRevision,
		Ops:            in.Ops,
		Submitter:      in.Submitter,
		Comment:        in.Comment,
		Policy:         z.Policy, // 策略快照
		Status:         status,
		IdempotencyKey: in.IdempotencyKey,
		PayloadHash:    hash,
		RollbackOf:     rollbackOf,
		Seq:            z.Seq,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	z.HeadRevision = rev
	z.Changes = append(z.Changes, chg)
	z.Revisions = append(z.Revisions, &Revision{
		Zone: z.Name, Number: rev, ChangeID: chg.ID, Base: in.BaseRevision,
		RecordSets: sortedSets(newView), RollbackOf: rollbackOf,
		Seq: z.Seq, CreatedAt: now,
	})
	if in.IdempotencyKey != "" {
		if z.Idem == nil {
			z.Idem = map[string]string{}
			z.Payloads = map[string]string{}
		}
		z.Idem[in.IdempotencyKey] = chg.ID
		z.Payloads[in.IdempotencyKey] = hash
	}
	return chg, nil
}

// Approve 审批一次变更。审批人资格与人数要求取自变更提交时的策略快照。
// 同一审批人重复审批是幂等的（返回当前状态，不产生错误）；
// 启用职责分离时，提交者审批自己的变更会被拒绝。
func (s *Service) Approve(zone, id, approver string) (*Change, error) {
	if approver == "" {
		return nil, errf(KindValidation, "approver is required")
	}
	var out *Change
	err := s.mutate(zone, func(z *ZoneState) error {
		chg := findChange(z, id)
		if chg == nil {
			return errf(KindNotFound, "change %q not found in zone %s", id, z.Name)
		}
		switch chg.Status {
		case StatusWithdrawn:
			return errf(KindState, "change %s is withdrawn and cannot be approved", id)
		case StatusPublished:
			return errf(KindState, "change %s is already published", id)
		case StatusRolling:
			return errf(KindState, "change %s is being rolled out in stages and cannot be approved again", id)
		case StatusStopped, StatusRecovered:
			return errf(KindState, "change %s is %s and cannot be approved", id, chg.Status)
		}
		// 同一审批人重复审批：幂等成功。
		for _, a := range chg.Approvals {
			if a.Approver == approver {
				out = chg
				return nil
			}
		}
		// 审批要求已满足：后续审批为幂等空操作。
		if chg.Status == StatusApproved {
			out = chg
			return nil
		}
		p := chg.Policy // 使用提交时的策略快照
		if p.SeparationOfDuties && approver == chg.Submitter {
			return errf(KindApproval,
				"separation of duties: submitter %q cannot approve their own change", approver)
		}
		if len(p.EligibleApprovers) > 0 && !contains(p.EligibleApprovers, approver) {
			return errf(KindApproval, "approver %q is not eligible for change %s", approver, id)
		}
		z.Seq++
		chg.Approvals = append(chg.Approvals, Approval{Approver: approver, At: s.now()})
		if len(chg.Approvals) >= p.RequiredApprovals {
			chg.Status = StatusApproved
		}
		chg.Seq = z.Seq
		chg.UpdatedAt = s.now()
		out = chg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Withdraw 撤销一次尚未发布的变更，仅提交者本人可执行。
// 重复撤销是幂等的；撤销已发布的变更返回状态错误。
func (s *Service) Withdraw(zone, id, actor string) (*Change, error) {
	var out *Change
	err := s.mutate(zone, func(z *ZoneState) error {
		chg := findChange(z, id)
		if chg == nil {
			return errf(KindNotFound, "change %q not found in zone %s", id, z.Name)
		}
		if chg.Submitter != actor {
			return errf(KindApproval, "only submitter %q can withdraw change %s", chg.Submitter, id)
		}
		switch chg.Status {
		case StatusWithdrawn:
			out = chg // 幂等
			return nil
		case StatusPublished:
			return errf(KindState, "change %s is already published and cannot be withdrawn", id)
		case StatusRolling:
			return errf(KindState, "change %s is mid rollout; use StopRollout to halt it instead of Withdraw", id)
		case StatusRecovered:
			return errf(KindState, "change %s was recovered by a new revision and cannot be withdrawn", id)
		}
		z.Seq++
		chg.Status = StatusWithdrawn
		chg.Seq = z.Seq
		chg.UpdatedAt = s.now()
		// 撤销正在分阶段发布的目标变更时，同步终止其计划，流量停在当前已发布阶段。
		for _, p := range z.Plans {
			if p.ChangeID == chg.ID && p.Status == PlanActive {
				p.Status = PlanStopped
				p.UpdatedAt = s.now()
			}
		}
		out = chg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Publish 发布一次已满足审批要求的变更。
// 发布必须按修订号前进（不允许发布落后于当前已发布修订的变更），
// 重复发布同一变更是幂等的，且每个实际发布的修订只写出一次 outbox。
func (s *Service) Publish(zone, id string) (*Revision, error) {
	var out *Revision
	err := s.mutate(zone, func(z *ZoneState) error {
		chg := findChange(z, id)
		if chg == nil {
			return errf(KindNotFound, "change %q not found in zone %s", id, z.Name)
		}
		rev := findRevision(z, chg.Revision)
		if chg.Status == StatusPublished {
			out = rev // 幂等：不重复写 outbox
			return nil
		}
		if chg.Status == StatusRolling {
			return errf(KindState,
				"change %s is being rolled out in stages; use AdvanceStage/StopRollout instead of Publish", id)
		}
		if chg.Status != StatusApproved {
			return errf(KindState, "change %s is %s and cannot be published", id, chg.Status)
		}
		if p := planOfChange(z, chg.ID); p != nil && p.Status == PlanActive {
			return errf(KindState,
				"change %s has an active rollout plan %s; withdraw the plan target or stop the plan instead of direct publish",
				id, p.ID)
		}
		if chg.Revision <= z.PublishedRevision {
			return errf(KindState,
				"change %s (revision %d) cannot be published after revision %d",
				id, chg.Revision, z.PublishedRevision)
		}
		z.Seq++
		chg.Status = StatusPublished
		chg.Seq = z.Seq
		chg.UpdatedAt = s.now()
		z.PublishedRevision = chg.Revision
		z.Outbox = append(z.Outbox, &OutboxEntry{
			ID: outboxID(z.Name, rev.Number), Zone: z.Name, Revision: rev.Number,
			RecordSets: rev.RecordSets, Seq: z.Seq, CreatedAt: s.now(),
		})
		// 分阶段计划执行期间若有另一个变更实际发布，它一定以当前已发布阶段为基准
		// （头部即已发布阶段时才能提交）。旧计划随即失效，不能再推进或覆盖该修订。
		if p := activePlan(z); p != nil && chg.PlanID == "" {
			if target := findChange(z, p.ChangeID); target != nil {
				target.Status = StatusStopped
				target.UpdatedAt = s.now()
			}
			p.Status = PlanStopped
			p.Superseded = true
			p.UpdatedAt = s.now()
		}
		out = rev
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Rollback 回滚区域到指定历史修订的内容。
// 回滚不改写历史，而是计算头部视图与目标修订视图的差异，
// 作为一次普通变更提交（同样需要满足审批要求后才能发布），
// 从而创建一个复用历史内容的新修订。相同幂等键的重复回滚返回原变更。
func (s *Service) Rollback(zone string, toRevision int64, submitter, idempotencyKey string) (*Change, error) {
	if submitter == "" {
		return nil, errf(KindValidation, "submitter is required")
	}
	var out *Change
	err := s.mutate(zone, func(z *ZoneState) error {
		// 幂等键命中时直接返回原变更，无论当前头部内容如何。
		if idempotencyKey != "" {
			if id, ok := z.Idem[idempotencyKey]; ok {
				out = findChange(z, id)
				return nil
			}
		}
		target := findRevision(z, toRevision)
		if target == nil {
			return errf(KindNotFound, "revision %d not found in zone %s", toRevision, z.Name)
		}
		head := viewAt(z, z.HeadRevision)
		targetView := make(map[string]RecordSet, len(target.RecordSets))
		for _, rs := range target.RecordSets {
			targetView[rs.Key()] = rs
		}
		ops := diffOps(head, targetView)
		if len(ops) == 0 {
			return errf(KindState, "zone %s head already matches revision %d", z.Name, toRevision)
		}
		chg, err := s.submitLocked(z, SubmitInput{
			Zone:           z.Name,
			BaseRevision:   z.HeadRevision,
			Ops:            ops,
			Submitter:      submitter,
			Comment:        fmt.Sprintf("rollback to revision %d", toRevision),
			IdempotencyKey: idempotencyKey,
		}, toRevision)
		if err != nil {
			return err
		}
		out = chg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetChange 查询一次变更。
func (s *Service) GetChange(zone, id string) (*Change, error) {
	var out *Change
	err := s.read(zone, func(z *ZoneState) error {
		chg := findChange(z, id)
		if chg == nil {
			return errf(KindNotFound, "change %q not found in zone %s", id, z.Name)
		}
		out = chg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetRevision 查询指定修订号的完整区域视图。
func (s *Service) GetRevision(zone string, number int64) (*Revision, error) {
	var out *Revision
	err := s.read(zone, func(z *ZoneState) error {
		rev := findRevision(z, number)
		if rev == nil {
			return errf(KindNotFound, "revision %d not found in zone %s", number, z.Name)
		}
		out = rev
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListRevisions 按修订号升序列出区域的全部修订。
func (s *Service) ListRevisions(zone string) ([]*Revision, error) {
	var out []*Revision
	err := s.read(zone, func(z *ZoneState) error {
		out = append(out, z.Revisions...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetZone 返回区域摘要。
func (s *Service) GetZone(zone string) (*ZoneInfo, error) {
	var out *ZoneInfo
	err := s.read(zone, func(z *ZoneState) error {
		out = zoneInfo(z)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListOutbox 按写出顺序列出区域的传播 outbox 条目。
func (s *Service) ListOutbox(zone string) ([]*OutboxEntry, error) {
	var out []*OutboxEntry
	err := s.read(zone, func(z *ZoneState) error {
		out = append(out, z.Outbox...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- 内部辅助 ---

func changeID(zone string, rev int64) string { return fmt.Sprintf("%s#%d", zone, rev) }

func outboxID(zone string, rev int64) string { return fmt.Sprintf("%s/outbox/%d", zone, rev) }

func zoneInfo(z *ZoneState) *ZoneInfo {
	return &ZoneInfo{
		Name:              z.Name,
		HeadRevision:      z.HeadRevision,
		PublishedRevision: z.PublishedRevision,
		Policy:            z.Policy,
		Seq:               z.Seq,
	}
}

func findChange(z *ZoneState, id string) *Change {
	for _, c := range z.Changes {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func findRevision(z *ZoneState, number int64) *Revision {
	for _, r := range z.Revisions {
		if r.Number == number {
			return r
		}
	}
	return nil
}

// viewAt 返回指定修订号下的区域视图。
func viewAt(z *ZoneState, number int64) map[string]RecordSet {
	rev := findRevision(z, number)
	view := map[string]RecordSet{}
	if rev == nil {
		return view
	}
	for _, rs := range rev.RecordSets {
		view[rs.Key()] = rs
	}
	return view
}

func hashOps(ops []ChangeOp) (string, error) {
	data, err := json.Marshal(ops)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
