package dnschange

import (
	"context"
	"reflect"
	"sort"
	"time"
)

// Service 是 DNS 区域变更服务。所有操作都在单个可串行化事务内完成，
// 因而并发的提交/审批/发布/撤销/回滚具有确定顺序。
type Service struct {
	store Store
	now   func() time.Time
}

// NewService 创建服务。
func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// InitZone 初始化区域：规范化并校验初始记录后直接产生 1 号已发布修订，
// 并写出第一条传播事件。
func (s *Service) InitZone(ctx context.Context, req InitZoneRequest) (*Revision, *OutboxEvent, error) {
	name, err := normalizeZone(req.Name)
	if err != nil {
		return nil, nil, err
	}
	policy, err := validatePolicy(req.Policy)
	if err != nil {
		return nil, nil, err
	}
	if err := validateConfig(req.Config); err != nil {
		return nil, nil, err
	}

	records, err := normalizeAndValidateView(req.Records, name, req.Config)
	if err != nil {
		return nil, nil, err
	}

	now := s.now()
	rev := &Revision{
		Zone:        name,
		Number:      1,
		Kind:        RevInit,
		Status:      StatusPublished,
		Records:     records,
		Upserts:     cloneRecordSets(records),
		Policy:      policy,
		Comment:     req.Comment,
		CreatedAt:   now,
		PublishedAt: &now,
	}
	var event *OutboxEvent
	err = s.store.Update(ctx, func(tx Tx) error {
		if tx.Zone(name) != nil {
			return revisionError("zone %q already exists", name)
		}
		z := &ZoneState{
			Name:         name,
			Config:       req.Config,
			Policy:       policy,
			Tip:          1,
			Head:         1,
			Revisions:    map[int64]*Revision{1: rev},
			Current:      cloneRecordSets(records),
			SubmitKeys:   map[string]IdemSubmit{},
			RollbackKeys: map[string]IdemRollback{},
			ApprovalKeys: map[string]IdemApprove{},
		}
		if err := tx.CreateZone(z); err != nil {
			return err
		}
		event = &OutboxEvent{Zone: name, Revision: 1, Kind: RevInit, Records: cloneRecordSets(records), CreatedAt: now}
		tx.Emit(event)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return rev, event, nil
}

// SubmitChange 提交一次混合新增/替换/删除变更。
// 全部校验在分配新修订号之前完成：校验失败不留修订、不跳号。
// 基准号必须等于当前已发布修订号，且区域不能存在另一个在途修订。
func (s *Service) SubmitChange(ctx context.Context, req SubmitChangeRequest) (*SubmitResult, error) {
	name, err := normalizeZone(req.Zone)
	if err != nil {
		return nil, err
	}
	upserts := make([]RecordSet, 0, len(req.Upserts))
	for _, rs := range req.Upserts {
		n, err := normalizeRecordSet(rs, name)
		if err != nil {
			return nil, err
		}
		upserts = append(upserts, n)
	}
	deletes := make([]RecordSetRef, 0, len(req.Deletes))
	for _, d := range req.Deletes {
		n, err := normalizeRef(d, name)
		if err != nil {
			return nil, err
		}
		deletes = append(deletes, n)
	}
	if req.Committer == "" {
		return nil, validationError("committer is required")
	}
	if len(upserts) == 0 && len(deletes) == 0 {
		return nil, validationError("change contains neither upserts nor deletes")
	}

	var result *SubmitResult
	err = s.store.Update(ctx, func(tx Tx) error {
		z := tx.Zone(name)
		if z == nil {
			return revisionError("zone %q does not exist", name)
		}

		if req.IdempotencyKey != "" {
			if rec, ok := z.SubmitKeys[req.IdempotencyKey]; ok {
				if rec.Base != req.BaseRevision || rec.Committer != req.Committer ||
					!reflect.DeepEqual(rec.Upserts, upserts) || !reflect.DeepEqual(rec.Deletes, deletes) {
					return idempotencyError("idempotency key %q was already used with a different payload", req.IdempotencyKey)
				}
				result = &SubmitResult{Revision: cloneRevision(z.Revisions[rec.Revision]), Replayed: true}
				return nil
			}
		}

		// 基准号必须指向当前已发布修订。
		if req.BaseRevision != z.Head {
			return revisionError("base revision %d does not match current head %d", req.BaseRevision, z.Head)
		}

		// 在基准完整视图上应用变更，并针对变更后的完整视图做冲突与 TTL 校验。
		// 校验在占用在途槽位与分配修订号之前完成：失败不留修订、不跳号。
		view, err := applyChange(z.Current, upserts, deletes)
		if err != nil {
			return err
		}
		if err := validateFullView(view, z.Config); err != nil {
			return err
		}
		records := sortedRecords(view)

		// 载荷合法后再检查在途槽位：并发提交只有一个能进入分配，
		// 其余收到 revision 冲突而不会互相静默覆盖。
		if z.Pending != 0 {
			return revisionError("revision %d is still in flight; concurrent changes cannot be queued", z.Pending)
		}

		// 校验全部通过后才分配修订号。
		number := z.Tip + 1
		now := s.now()
		rev := &Revision{
			Zone:      name,
			Number:    number,
			Kind:      RevChange,
			Status:    StatusPending,
			Base:      z.Head,
			Records:   records,
			Upserts:   cloneRecordSets(upserts),
			Deletes:   cloneRefs(deletes),
			Committer: req.Committer,
			Comment:   req.Comment,
			Policy:    clonePolicy(z.Policy),
			CreatedAt: now,
		}
		if z.Policy.RequiredApprovals == 0 {
			rev.Status = StatusApproved
		}
		z.Revisions[number] = rev
		z.Tip = number
		z.Pending = number

		if req.IdempotencyKey != "" {
			z.SubmitKeys[req.IdempotencyKey] = IdemSubmit{
				Revision: number, Base: req.BaseRevision, Committer: req.Committer,
				Upserts: cloneRecordSets(upserts), Deletes: cloneRefs(deletes),
			}
		}
		result = &SubmitResult{Revision: cloneRevision(rev)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Approve 对在途修订追加一个审批。同一审批人重复审批天然幂等；
// 带幂等键的重放在键载荷一致时返回原审批。审批资格按提交时的策略快照判定。
func (s *Service) Approve(ctx context.Context, req ApproveRequest) (*ApproveResult, error) {
	name, err := normalizeZone(req.Zone)
	if err != nil {
		return nil, err
	}
	if req.Approver == "" {
		return nil, approvalError("approver is required")
	}

	var result *ApproveResult
	err = s.store.Update(ctx, func(tx Tx) error {
		z := tx.Zone(name)
		if z == nil {
			return revisionError("zone %q does not exist", name)
		}
		rev := z.Revisions[req.Revision]
		if rev == nil {
			return revisionError("revision %d does not exist in zone %q", req.Revision, name)
		}

		if req.IdempotencyKey != "" {
			if rec, ok := z.ApprovalKeys[req.IdempotencyKey]; ok {
				if rec.Revision != req.Revision || rec.Approver != req.Approver {
					return idempotencyError("idempotency key %q was already used with a different payload", req.IdempotencyKey)
				}
				a := approvalOf(rev, req.Approver)
				result = &ApproveResult{Revision: cloneRevision(rev), Approval: a, Replayed: true}
				return nil
			}
		}

		if rev.Status == StatusPublished {
			return stateError("revision %d is already published", req.Revision)
		}
		if rev.Status == StatusCanceled {
			return stateError("revision %d was canceled", req.Revision)
		}

		// 同一审批人的重复审批直接幂等返回（无论配额是否已满）。
		if a := approvalOf(rev, req.Approver); a != nil {
			result = &ApproveResult{Revision: cloneRevision(rev), Approval: approvalOf(rev, req.Approver), Replayed: true}
			if req.IdempotencyKey != "" {
				z.ApprovalKeys[req.IdempotencyKey] = IdemApprove{Revision: req.Revision, Approver: req.Approver}
			}
			return nil
		}

		if rev.Status == StatusApproved {
			return approvalError("revision %d already has all required approvals", req.Revision)
		}

		// 资格检查全部基于提交时快照。
		p := rev.Policy
		if len(p.Approvers) > 0 {
			allowed := false
			for _, a := range p.Approvers {
				if a == req.Approver {
					allowed = true
					break
				}
			}
			if !allowed {
				return approvalError("%q is not an authorized approver", req.Approver)
			}
		}
		if p.SeparationOfDuties && req.Approver == rev.Committer {
			return approvalError("separation of duties: committer %q cannot approve own change", req.Approver)
		}

		a := &Approval{Approver: req.Approver, Comment: req.Comment, CreatedAt: s.now()}
		rev.Approvals = append(rev.Approvals, *a)
		if len(rev.Approvals) >= p.RequiredApprovals {
			rev.Status = StatusApproved
		}
		if req.IdempotencyKey != "" {
			z.ApprovalKeys[req.IdempotencyKey] = IdemApprove{Revision: req.Revision, Approver: req.Approver}
		}
		result = &ApproveResult{Revision: cloneRevision(rev), Approval: a}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Publish 发布修订：状态 approved→published，推进 Head/Current 且恰好写出一条 outbox。
// 对已发布修订的重复调用幂等，返回既有事件而不会再写 outbox。
func (s *Service) Publish(ctx context.Context, req PublishRequest) (*PublishResult, error) {
	name, err := normalizeZone(req.Zone)
	if err != nil {
		return nil, err
	}
	var result *PublishResult
	err = s.store.Update(ctx, func(tx Tx) error {
		z := tx.Zone(name)
		if z == nil {
			return revisionError("zone %q does not exist", name)
		}
		rev := z.Revisions[req.Revision]
		if rev == nil {
			return revisionError("revision %d does not exist in zone %q", req.Revision, name)
		}

		if rev.Status == StatusPublished {
			if existing := outboxEvent(tx, name, rev.Number); existing != nil {
				result = &PublishResult{Revision: cloneRevision(rev), OutboxEvent: existing}
				return nil
			}
			return stateError("revision %d is published but has no outbox event", req.Revision)
		}
		if rev.Status == StatusCanceled {
			return stateError("revision %d was canceled and cannot be published", req.Revision)
		}
		if rev.Status == StatusPending {
			return approvalError("revision %d has %d of %d required approvals",
				req.Revision, len(rev.Approvals), rev.Policy.RequiredApprovals)
		}

		now := s.now()
		rev.Status = StatusPublished
		rev.PublishedAt = &now
		z.Head = rev.Number
		z.Current = cloneRecordSets(rev.Records)
		if z.Pending == rev.Number {
			z.Pending = 0
		}

		event := &OutboxEvent{
			Zone: name, Revision: rev.Number, Kind: rev.Kind,
			Records: cloneRecordSets(rev.Records), CreatedAt: now,
		}
		tx.Emit(event)
		result = &PublishResult{Revision: cloneRevision(rev), OutboxEvent: event}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Cancel 由提交者撤销一个尚未发布的在途修订，释放在途槽位。
func (s *Service) Cancel(ctx context.Context, req CancelRequest) (*Revision, error) {
	name, err := normalizeZone(req.Zone)
	if err != nil {
		return nil, err
	}
	var out *Revision
	err = s.store.Update(ctx, func(tx Tx) error {
		z := tx.Zone(name)
		if z == nil {
			return revisionError("zone %q does not exist", name)
		}
		rev := z.Revisions[req.Revision]
		if rev == nil {
			return revisionError("revision %d does not exist in zone %q", req.Revision, name)
		}
		if rev.Status == StatusPublished {
			return stateError("revision %d is already published and cannot be canceled", req.Revision)
		}
		if rev.Status == StatusCanceled {
			return stateError("revision %d is already canceled", req.Revision)
		}
		if req.Requester != rev.Committer {
			return approvalError("only committer %q may cancel revision %d", rev.Committer, req.Revision)
		}
		now := s.now()
		rev.Status = StatusCanceled
		rev.CanceledAt = &now
		if z.Pending == rev.Number {
			z.Pending = 0
		}
		out = cloneRevision(rev)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Rollback 创建一个复用历史已发布修订完整内容的新在途修订。
// 历史不被改写：回滚本身占用一个新修订号，照常走审批与发布。
func (s *Service) Rollback(ctx context.Context, req RollbackRequest) (*SubmitResult, error) {
	name, err := normalizeZone(req.Zone)
	if err != nil {
		return nil, err
	}
	if req.Committer == "" {
		return nil, validationError("committer is required")
	}

	var result *SubmitResult
	err = s.store.Update(ctx, func(tx Tx) error {
		z := tx.Zone(name)
		if z == nil {
			return revisionError("zone %q does not exist", name)
		}
		src := z.Revisions[req.SourceRevision]
		if src == nil {
			return revisionError("source revision %d does not exist in zone %q", req.SourceRevision, name)
		}
		if src.Status != StatusPublished {
			return stateError("source revision %d was never published", req.SourceRevision)
		}

		if req.IdempotencyKey != "" {
			if rec, ok := z.RollbackKeys[req.IdempotencyKey]; ok {
				if rec.Source != req.SourceRevision || rec.Committer != req.Committer {
					return idempotencyError("idempotency key %q was already used with a different payload", req.IdempotencyKey)
				}
				result = &SubmitResult{Revision: cloneRevision(z.Revisions[rec.Revision]), Replayed: true}
				return nil
			}
		}

		if z.Pending != 0 {
			return revisionError("revision %d is still in flight; concurrent changes cannot be queued", z.Pending)
		}
		if src.Number == z.Head {
			return stateError("source revision %d is already the current head", src.Number)
		}

		number := z.Tip + 1
		now := s.now()
		rev := &Revision{
			Zone:           name,
			Number:         number,
			Kind:           RevRollback,
			Status:         StatusPending,
			Base:           z.Head,
			SourceRevision: src.Number,
			Records:        cloneRecordSets(src.Records),
			Committer:      req.Committer,
			Comment:        req.Comment,
			Policy:         clonePolicy(z.Policy),
			CreatedAt:      now,
		}
		if z.Policy.RequiredApprovals == 0 {
			rev.Status = StatusApproved
		}
		z.Revisions[number] = rev
		z.Tip = number
		z.Pending = number

		if req.IdempotencyKey != "" {
			z.RollbackKeys[req.IdempotencyKey] = IdemRollback{
				Revision: number, Source: req.SourceRevision, Committer: req.Committer,
			}
		}
		result = &SubmitResult{Revision: cloneRevision(rev)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// UpdatePolicy 更新区域当前审批策略。已在途的修订继续使用各自提交时的
// 策略快照；新策略只对之后提交/回滚产生的修订生效。
func (s *Service) UpdatePolicy(ctx context.Context, zone string, policy Policy) (Policy, error) {
	name, err := normalizeZone(zone)
	if err != nil {
		return Policy{}, err
	}
	policy, err = validatePolicy(policy)
	if err != nil {
		return Policy{}, err
	}
	err = s.store.Update(ctx, func(tx Tx) error {
		z := tx.Zone(name)
		if z == nil {
			return revisionError("zone %q does not exist", name)
		}
		z.Policy = clonePolicy(policy)
		return nil
	})
	if err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// GetZone 返回区域当前视图。
func (s *Service) GetZone(ctx context.Context, zone string) (*ZoneView, error) {
	name, err := normalizeZone(zone)
	if err != nil {
		return nil, err
	}
	var view *ZoneView
	err = s.store.View(ctx, func(tx Tx) error {
		z := tx.Zone(name)
		if z == nil {
			return revisionError("zone %q does not exist", name)
		}
		view = &ZoneView{
			Name:    z.Name,
			Config:  z.Config,
			Policy:  clonePolicy(z.Policy),
			Tip:     z.Tip,
			Head:    z.Head,
			Records: cloneRecordSets(z.Current),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

// GetRevision 查询单个修订（含完整视图、审批记录与状态）。
func (s *Service) GetRevision(ctx context.Context, zone string, number int64) (*Revision, error) {
	name, err := normalizeZone(zone)
	if err != nil {
		return nil, err
	}
	var rev *Revision
	err = s.store.View(ctx, func(tx Tx) error {
		z := tx.Zone(name)
		if z == nil {
			return revisionError("zone %q does not exist", name)
		}
		r := z.Revisions[number]
		if r == nil {
			return revisionError("revision %d does not exist in zone %q", number, name)
		}
		rev = cloneRevision(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rev, nil
}

// ListRevisions 按修订号升序返回区域的全部修订。
func (s *Service) ListRevisions(ctx context.Context, zone string) ([]*Revision, error) {
	name, err := normalizeZone(zone)
	if err != nil {
		return nil, err
	}
	var out []*Revision
	err = s.store.View(ctx, func(tx Tx) error {
		z := tx.Zone(name)
		if z == nil {
			return revisionError("zone %q does not exist", name)
		}
		numbers := make([]int64, 0, len(z.Revisions))
		for n := range z.Revisions {
			numbers = append(numbers, n)
		}
		sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
		out = make([]*Revision, 0, len(numbers))
		for _, n := range numbers {
			out = append(out, cloneRevision(z.Revisions[n]))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListOutbox 返回传播事件。zone 为空时返回所有区域的事件，按事件 ID 升序。
func (s *Service) ListOutbox(ctx context.Context, zone string) ([]*OutboxEvent, error) {
	name := ""
	if zone != "" {
		n, err := normalizeZone(zone)
		if err != nil {
			return nil, err
		}
		name = n
	}
	var out []*OutboxEvent
	err := s.store.View(ctx, func(tx Tx) error {
		for _, e := range tx.Outbox() {
			if name == "" || e.Zone == name {
				cp := *e
				cp.Records = cloneRecordSets(e.Records)
				out = append(out, &cp)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- helpers ---

func validatePolicy(p Policy) (Policy, error) {
	if p.RequiredApprovals < 0 {
		return Policy{}, validationError("required_approvals cannot be negative")
	}
	seen := map[string]bool{}
	approvers := make([]string, 0, len(p.Approvers))
	for _, a := range p.Approvers {
		if a == "" {
			return Policy{}, validationError("approver list contains an empty name")
		}
		if seen[a] {
			continue
		}
		seen[a] = true
		approvers = append(approvers, a)
	}
	sort.Strings(approvers)
	if p.RequiredApprovals > 0 && len(approvers) > 0 && p.RequiredApprovals > len(approvers) {
		return Policy{}, validationError("required_approvals %d exceeds number of authorized approvers %d",
			p.RequiredApprovals, len(approvers))
	}
	p.Approvers = approvers
	return p, nil
}

func validateConfig(c ZoneConfig) error {
	if c.MinTTL > 0 && c.MaxTTL > 0 && c.MinTTL > c.MaxTTL {
		return validationError("min_ttl %d is greater than max_ttl %d", c.MinTTL, c.MaxTTL)
	}
	return nil
}

// normalizeAndValidateView 规范化一批记录集并对整视图做 TTL/冲突校验。
func normalizeAndValidateView(raw []RecordSet, zone string, cfg ZoneConfig) ([]RecordSet, error) {
	view := make(map[string]RecordSet, len(raw))
	for _, rs := range raw {
		n, err := normalizeRecordSet(rs, zone)
		if err != nil {
			return nil, err
		}
		k := recordKey(n.Name, n.Type)
		if _, dup := view[k]; dup {
			return nil, validationError("duplicate record set %s %s", n.Name, n.Type)
		}
		view[k] = n
	}
	if err := validateFullView(view, cfg); err != nil {
		return nil, err
	}
	return sortedRecords(view), nil
}

func clonePolicy(p Policy) Policy {
	p.Approvers = append([]string(nil), p.Approvers...)
	return p
}

func cloneRefs(in []RecordSetRef) []RecordSetRef {
	out := make([]RecordSetRef, len(in))
	copy(out, in)
	return out
}

func cloneRevision(r *Revision) *Revision {
	c := *r
	c.Records = cloneRecordSets(r.Records)
	c.Upserts = cloneRecordSets(r.Upserts)
	if r.Deletes != nil {
		c.Deletes = cloneRefs(r.Deletes)
	}
	if r.Approvals != nil {
		c.Approvals = append([]Approval(nil), r.Approvals...)
	}
	c.Policy = clonePolicy(r.Policy)
	return &c
}

func approvalOf(rev *Revision, approver string) *Approval {
	for i := range rev.Approvals {
		if rev.Approvals[i].Approver == approver {
			a := rev.Approvals[i]
			return &a
		}
	}
	return nil
}

func outboxEvent(tx Tx, zone string, revision int64) *OutboxEvent {
	for _, e := range tx.Outbox() {
		if e.Zone == zone && e.Revision == revision {
			cp := *e
			cp.Records = cloneRecordSets(e.Records)
			return &cp
		}
	}
	return nil
}
