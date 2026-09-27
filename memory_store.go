package dnschange

import (
	"context"
	"sync"
)

// MemoryStore 是进程内的 Store 实现，用一把互斥锁给出可串行化事务语义。
// 事务以深拷贝隔离：Update 中 fn 返回错误时所有修改随拷贝丢弃，
// 因此失败操作不可能留下部分记录。
type MemoryStore struct {
	mu     sync.Mutex
	zones  map[string]*ZoneState
	outbox []*OutboxEvent
	outSeq int64
}

// NewMemoryStore 创建一个空的内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{zones: make(map[string]*ZoneState)}
}

// Update 在串行临界区内执行读写事务。
func (s *MemoryStore) Update(ctx context.Context, fn func(tx Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot := s.deepCopy()
	tx := &memoryTx{store: s, state: snapshot, writable: true}
	if err := fn(tx); err != nil {
		// 直接丢弃快照，原状态分毫未动。
		return err
	}
	s.zones = snapshot.zones
	s.outbox = snapshot.outbox
	s.outSeq = snapshot.outSeq
	return nil
}

// View 在串行临界区内执行只读事务。
func (s *MemoryStore) View(ctx context.Context, fn func(tx Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	tx := &memoryTx{store: s, state: s.deepCopy(), writable: false}
	return fn(tx)
}

// snapshot 是持锁期间整个存储的一份深拷贝。
type storeSnapshot struct {
	zones  map[string]*ZoneState
	outbox []*OutboxEvent
	outSeq int64
}

func (s *MemoryStore) deepCopy() *storeSnapshot {
	snap := &storeSnapshot{
		zones:  make(map[string]*ZoneState, len(s.zones)),
		outbox: make([]*OutboxEvent, len(s.outbox)),
		outSeq: s.outSeq,
	}
	for name, z := range s.zones {
		snap.zones[name] = cloneZoneState(z)
	}
	copy(snap.outbox, s.outbox)
	return snap
}

func cloneRecordSets(in []RecordSet) []RecordSet {
	if in == nil {
		return nil
	}
	out := make([]RecordSet, len(in))
	for i, rs := range in {
		out[i] = rs
		out[i].Values = append([]string(nil), rs.Values...)
	}
	return out
}

func cloneApprovalKeys(in map[string]IdemApprove) map[string]IdemApprove {
	if in == nil {
		return map[string]IdemApprove{}
	}
	out := make(map[string]IdemApprove, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneZoneState(z *ZoneState) *ZoneState {
	c := &ZoneState{
		Name:         z.Name,
		Config:       z.Config,
		Policy:       z.Policy,
		Tip:          z.Tip,
		Head:         z.Head,
		Pending:      z.Pending,
		Revisions:    make(map[int64]*Revision, len(z.Revisions)),
		Current:      cloneRecordSets(z.Current),
		SubmitKeys:   make(map[string]IdemSubmit, len(z.SubmitKeys)),
		RollbackKeys: make(map[string]IdemRollback, len(z.RollbackKeys)),
		ApprovalKeys: cloneApprovalKeys(z.ApprovalKeys),
	}
	c.Policy.Approvers = append([]string(nil), z.Policy.Approvers...)
	for n, rev := range z.Revisions {
		r := *rev
		r.Records = cloneRecordSets(rev.Records)
		r.Upserts = cloneRecordSets(rev.Upserts)
		r.Deletes = cloneRefs(rev.Deletes)
		if rev.Approvals != nil {
			r.Approvals = append([]Approval(nil), rev.Approvals...)
		}
		r.Policy.Approvers = append([]string(nil), rev.Policy.Approvers...)
		c.Revisions[n] = &r
	}
	for k, v := range z.SubmitKeys {
		v.Upserts = cloneRecordSets(v.Upserts)
		v.Deletes = cloneRefs(v.Deletes)
		c.SubmitKeys[k] = v
	}
	for k, v := range z.RollbackKeys {
		c.RollbackKeys[k] = v
	}
	return c
}

type memoryTx struct {
	store    *MemoryStore
	state    *storeSnapshot
	writable bool
}

func (t *memoryTx) Zone(name string) *ZoneState { return t.state.zones[name] }

func (t *memoryTx) CreateZone(z *ZoneState) error {
	if _, ok := t.state.zones[z.Name]; ok {
		return revisionError("zone %q already exists", z.Name)
	}
	t.state.zones[z.Name] = z
	return nil
}

func (t *memoryTx) Emit(e *OutboxEvent) {
	t.state.outSeq++
	e.ID = t.state.outSeq
	t.state.outbox = append(t.state.outbox, e)
}

func (t *memoryTx) Outbox() []*OutboxEvent {
	out := make([]*OutboxEvent, len(t.state.outbox))
	copy(out, t.state.outbox)
	return out
}
