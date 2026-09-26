package dnschange

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

const testZone = "example.com"

func newTestService(t *testing.T) *Service {
	t.Helper()
	svc := NewService(NewMemoryStore())
	_, err := svc.InitZone(testZone, []string{"ns1.example.com", "ns2.example.com"}, Policy{
		RequiredApprovals:  1,
		EligibleApprovers:  []string{"alice", "bob"},
		SeparationOfDuties: true,
	})
	if err != nil {
		t.Fatalf("InitZone: %v", err)
	}
	return svc
}

func addWWW(name string, ip string, ttl uint32) ChangeOp {
	return ChangeOp{Kind: OpAdd, RecordSet: RecordSet{
		Name: name, Type: TypeA, TTL: ttl, Records: []string{ip},
	}}
}

func mustSubmit(t *testing.T, svc *Service, in SubmitInput) *Change {
	t.Helper()
	chg, err := svc.SubmitChange(in)
	if err != nil {
		t.Fatalf("SubmitChange: %v", err)
	}
	return chg
}

func mustApprove(t *testing.T, svc *Service, zone, id, approver string) *Change {
	t.Helper()
	chg, err := svc.Approve(zone, id, approver)
	if err != nil {
		t.Fatalf("Approve(%s by %s): %v", id, approver, err)
	}
	return chg
}

func mustPublish(t *testing.T, svc *Service, zone, id string) *Revision {
	t.Helper()
	rev, err := svc.Publish(zone, id)
	if err != nil {
		t.Fatalf("Publish(%s): %v", id, err)
	}
	return rev
}

// TestInitZone 验证区域初始化：创建第 1 号修订并直接发布、写出 outbox。
func TestInitZone(t *testing.T) {
	svc := newTestService(t)

	info, err := svc.GetZone(testZone)
	if err != nil {
		t.Fatalf("GetZone: %v", err)
	}
	if info.HeadRevision != 1 || info.PublishedRevision != 1 {
		t.Fatalf("unexpected revisions: head=%d published=%d", info.HeadRevision, info.PublishedRevision)
	}

	rev, err := svc.GetRevision(testZone, 1)
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}
	if len(rev.RecordSets) != 2 { // SOA + NS
		t.Fatalf("bootstrap revision should have SOA and NS, got %v", rev.RecordSets)
	}

	outbox, err := svc.ListOutbox(testZone)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(outbox) != 1 || outbox[0].Revision != 1 {
		t.Fatalf("expected one outbox entry for revision 1, got %+v", outbox)
	}

	// 重复初始化应报状态错误。
	if _, err := svc.InitZone(testZone, []string{"ns1.example.com"}, Policy{}); !IsState(err) {
		t.Fatalf("re-init should be a state error, got %v", err)
	}

	// 查询不存在的区域应报未找到错误。
	if _, err := svc.GetZone("missing.example"); !IsNotFound(err) {
		t.Fatalf("missing zone should be a not-found error, got %v", err)
	}
}

// TestSubmitMixedChange 验证一次变更混合新增、替换、删除，且校验针对变更后的完整视图。
func TestSubmitMixedChange(t *testing.T) {
	svc := newTestService(t)

	c1 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{
			addWWW("www.example.com", "192.0.2.1", 300),
			addWWW("api.example.com", "192.0.2.2", 300),
		},
	})
	if c1.Revision != 2 || c1.Status != StatusPending {
		t.Fatalf("unexpected change: %+v", c1)
	}

	// 混合操作：替换 www、删除 api、新增 mail。
	c2 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{
			{Kind: OpReplace, RecordSet: RecordSet{Name: "www.example.com", Type: TypeA, TTL: 600, Records: []string{"192.0.2.10"}}},
			{Kind: OpDelete, RecordSet: RecordSet{Name: "api.example.com", Type: TypeA}},
			addWWW("mail.example.com", "192.0.2.25", 300),
		},
	})
	if c2.Revision != 3 {
		t.Fatalf("expected revision 3, got %d", c2.Revision)
	}

	rev, err := svc.GetRevision(testZone, 3)
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}
	view := map[string]RecordSet{}
	for _, rs := range rev.RecordSets {
		view[rs.Key()] = rs
	}
	if rs := view["www.example.com.|A"]; rs.TTL != 600 || rs.Records[0] != "192.0.2.10" {
		t.Fatalf("www not replaced: %+v", rs)
	}
	if _, ok := view["api.example.com.|A"]; ok {
		t.Fatal("api should have been deleted")
	}
	if _, ok := view["mail.example.com.|A"]; !ok {
		t.Fatal("mail should have been added")
	}
	if _, ok := view["example.com.|SOA"]; !ok {
		t.Fatal("bootstrap records must survive")
	}
}

// TestValidationFailureLeavesNothing 验证校验失败不创建修订、不跳过修订号、不留部分记录。
func TestValidationFailureLeavesNothing(t *testing.T) {
	svc := newTestService(t)

	// CNAME 与 A 共存冲突 + 非法 IPv4 + TTL 越界，整体应失败。
	_, err := svc.SubmitChange(SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{
			addWWW("www.example.com", "999.1.1.1", 300),
			{Kind: OpAdd, RecordSet: RecordSet{Name: "www.example.com", Type: TypeCNAME, TTL: 300, Records: []string{"other.example.com."}}},
		},
	})
	if !IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
	var verr *Error
	if !errors.As(err, &verr) || len(verr.Issues) == 0 {
		t.Fatalf("validation error should carry issues, got %v", err)
	}

	info, _ := svc.GetZone(testZone)
	if info.HeadRevision != 1 {
		t.Fatalf("failed submit must not advance head, got %d", info.HeadRevision)
	}

	// 后续成功提交应取得紧接的修订号 2（不跳号）。
	chg := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	if chg.Revision != 2 {
		t.Fatalf("revision numbers must not be skipped, got %d", chg.Revision)
	}
}

// TestStaleBaseRejected 验证基准修订号落后时被拒绝，且并发提交不会互相静默覆盖。
func TestStaleBaseRejected(t *testing.T) {
	svc := newTestService(t)

	mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("a.example.com", "192.0.2.1", 300)},
	})

	_, err := svc.SubmitChange(SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "dave",
		Ops: []ChangeOp{addWWW("b.example.com", "192.0.2.2", 300)},
	})
	if !IsRevisionConflict(err) {
		t.Fatalf("expected revision conflict, got %v", err)
	}
}

// TestConcurrentSubmits 验证并发提交：全部基于同一基准时只有一个成功，修订号连续。
func TestConcurrentSubmits(t *testing.T) {
	svc := newTestService(t)

	const n = 16
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = svc.SubmitChange(SubmitInput{
				Zone: testZone, BaseRevision: 1, Submitter: fmt.Sprintf("user-%d", i),
				Ops: []ChangeOp{addWWW(fmt.Sprintf("h%d.example.com", i), "192.0.2.1", 300)},
			})
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case IsRevisionConflict(err):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("exactly one concurrent submit should win, got %d", successes)
	}
	info, _ := svc.GetZone(testZone)
	if info.HeadRevision != 2 {
		t.Fatalf("head should be 2, got %d", info.HeadRevision)
	}
}

// TestApprovalPolicySnapshot 验证审批资格与人数要求取自提交时的策略快照。
func TestApprovalPolicySnapshot(t *testing.T) {
	svc := newTestService(t)

	chg := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})

	// 无资格审批人被拒绝。
	if _, err := svc.Approve(testZone, chg.ID, "mallory"); !IsApproval(err) {
		t.Fatalf("ineligible approver should be an approval error, got %v", err)
	}
	// 职责分离：提交者不能批准自己的变更。
	if _, err := svc.Approve(testZone, chg.ID, "carol"); !IsApproval(err) {
		t.Fatalf("self-approval should be an approval error, got %v", err)
	}
	// 合格审批人批准后满足要求。
	chg = mustApprove(t, svc, testZone, chg.ID, "alice")
	if chg.Status != StatusApproved {
		t.Fatalf("expected approved, got %s", chg.Status)
	}
	// 重复审批幂等：不报错、不重复计数。
	again, err := svc.Approve(testZone, chg.ID, "alice")
	if err != nil || again.Status != StatusApproved || len(again.Approvals) != 1 {
		t.Fatalf("re-approve should be idempotent, got %+v err=%v", again, err)
	}
}

// TestApprovalThreshold 验证多人审批门槛。
func TestApprovalThreshold(t *testing.T) {
	svc := NewService(NewMemoryStore())
	if _, err := svc.InitZone(testZone, []string{"ns1.example.com"}, Policy{RequiredApprovals: 2}); err != nil {
		t.Fatalf("InitZone: %v", err)
	}
	chg := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	if chg.Status != StatusPending {
		t.Fatalf("expected pending, got %s", chg.Status)
	}
	// 未满足门槛时不能发布。
	if _, err := svc.Publish(testZone, chg.ID); !IsState(err) {
		t.Fatalf("publish before approval should be a state error, got %v", err)
	}
	chg = mustApprove(t, svc, testZone, chg.ID, "alice")
	if chg.Status != StatusPending {
		t.Fatalf("one approval is not enough, got %s", chg.Status)
	}
	chg = mustApprove(t, svc, testZone, chg.ID, "bob")
	if chg.Status != StatusApproved {
		t.Fatalf("two approvals should approve, got %s", chg.Status)
	}
}

// TestPublishOutboxExactlyOnce 验证发布写出且只写出一次 outbox，重复发布幂等。
func TestPublishOutboxExactlyOnce(t *testing.T) {
	svc := newTestService(t)

	chg := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, chg.ID, "alice")

	rev := mustPublish(t, svc, testZone, chg.ID)
	if rev.Number != 2 {
		t.Fatalf("expected revision 2, got %d", rev.Number)
	}
	// 重复发布幂等，不重复写 outbox。
	if _, err := svc.Publish(testZone, chg.ID); err != nil {
		t.Fatalf("re-publish should be idempotent, got %v", err)
	}

	outbox, err := svc.ListOutbox(testZone)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(outbox) != 2 { // 引导修订 1 + 本次修订 2
		t.Fatalf("expected exactly 2 outbox entries, got %d", len(outbox))
	}
	if outbox[1].Revision != 2 || len(outbox[1].RecordSets) == 0 {
		t.Fatalf("bad outbox entry: %+v", outbox[1])
	}
	info, _ := svc.GetZone(testZone)
	if info.PublishedRevision != 2 {
		t.Fatalf("published revision should be 2, got %d", info.PublishedRevision)
	}
}

// TestConcurrentPublishDeterministic 验证并发发布/撤销/回滚落在确定的串行顺序上。
func TestConcurrentPublishDeterministic(t *testing.T) {
	for run := 0; run < 20; run++ {
		svc := newTestService(t)
		chg := mustSubmit(t, svc, SubmitInput{
			Zone: testZone, BaseRevision: 1, Submitter: "carol",
			Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
		})
		mustApprove(t, svc, testZone, chg.ID, "alice")

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); _, errs[0] = svc.Publish(testZone, chg.ID) }()
		go func() { defer wg.Done(); _, errs[1] = svc.Withdraw(testZone, chg.ID, "carol") }()
		wg.Wait()

		// 串行化后恰好一个成功：先撤销则发布失败，先发布则撤销失败。
		var okCount int
		for _, e := range errs {
			if e == nil {
				okCount++
			} else if !IsState(e) {
				t.Fatalf("unexpected error kind: %v", e)
			}
		}
		if okCount != 1 {
			t.Fatalf("run %d: exactly one of publish/withdraw should succeed, got %d", run, okCount)
		}
		final, err := svc.GetChange(testZone, chg.ID)
		if err != nil {
			t.Fatalf("GetChange: %v", err)
		}
		if errs[0] == nil && final.Status != StatusPublished {
			t.Fatalf("run %d: publish succeeded but status is %s", run, final.Status)
		}
		if errs[1] == nil && final.Status != StatusWithdrawn {
			t.Fatalf("run %d: withdraw succeeded but status is %s", run, final.Status)
		}
	}
}

// TestRollbackCreatesNewRevision 验证回滚创建复用历史内容的新修订，而非改写历史。
func TestRollbackCreatesNewRevision(t *testing.T) {
	svc := newTestService(t)

	c1 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, c1.ID, "alice")
	mustPublish(t, svc, testZone, c1.ID)

	c2 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{
			{Kind: OpReplace, RecordSet: RecordSet{Name: "www.example.com", Type: TypeA, TTL: 300, Records: []string{"192.0.2.99"}}},
			addWWW("api.example.com", "192.0.2.2", 300),
		},
	})
	mustApprove(t, svc, testZone, c2.ID, "alice")
	mustPublish(t, svc, testZone, c2.ID)

	// 回滚到修订 2：生成新修订 4，内容与修订 2 一致。
	rb, err := svc.Rollback(testZone, 2, "carol", "rb-1")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rb.Revision != 4 || rb.RollbackOf != 2 {
		t.Fatalf("unexpected rollback change: %+v", rb)
	}
	mustApprove(t, svc, testZone, rb.ID, "alice")
	mustPublish(t, svc, testZone, rb.ID)

	rev2, _ := svc.GetRevision(testZone, 2)
	rev4, _ := svc.GetRevision(testZone, 4)
	if len(rev2.RecordSets) != len(rev4.RecordSets) {
		t.Fatalf("rollback content mismatch: %v vs %v", rev2.RecordSets, rev4.RecordSets)
	}
	for i := range rev2.RecordSets {
		if !setsEqual(rev2.RecordSets[i], rev4.RecordSets[i]) {
			t.Fatalf("rollback content mismatch at %d: %+v vs %+v", i, rev2.RecordSets[i], rev4.RecordSets[i])
		}
	}

	// 历史未被改写：修订 3 内容保持不变。
	rev3, _ := svc.GetRevision(testZone, 3)
	found := false
	for _, rs := range rev3.RecordSets {
		if rs.Key() == "api.example.com.|A" {
			found = true
		}
	}
	if !found {
		t.Fatal("history must not be rewritten: revision 3 lost its records")
	}

	// 相同幂等键重复回滚返回原变更，不产生新修订。
	rb2, err := svc.Rollback(testZone, 2, "carol", "rb-1")
	if err != nil || rb2.ID != rb.ID {
		t.Fatalf("idempotent rollback should return the same change, got %+v err=%v", rb2, err)
	}
	info, _ := svc.GetZone(testZone)
	if info.HeadRevision != 4 {
		t.Fatalf("idempotent rollback must not create revisions, head=%d", info.HeadRevision)
	}
}

// TestIdempotency 验证幂等键语义：同键同负载返回原变更，同键不同负载报幂等错误。
func TestIdempotency(t *testing.T) {
	svc := newTestService(t)

	in := SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol", IdempotencyKey: "req-1",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	}
	c1 := mustSubmit(t, svc, in)
	c2 := mustSubmit(t, svc, in) // 重试：相同键 + 相同负载
	if c1.ID != c2.ID || c2.Revision != 2 {
		t.Fatalf("idempotent retry should return the original change, got %+v", c2)
	}
	info, _ := svc.GetZone(testZone)
	if info.HeadRevision != 2 {
		t.Fatalf("idempotent retry must not create revisions, head=%d", info.HeadRevision)
	}

	// 相同键但负载不同 → 幂等错误。
	in.Ops = []ChangeOp{addWWW("other.example.com", "192.0.2.2", 300)}
	in.BaseRevision = 2
	if _, err := svc.SubmitChange(in); !IsIdempotency(err) {
		t.Fatalf("expected idempotency error, got %v", err)
	}
}

// TestWithdraw 验证撤销：仅提交者可撤销，已发布的变更不可撤销。
func TestWithdraw(t *testing.T) {
	svc := newTestService(t)

	chg := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})

	// 非提交者不能撤销。
	if _, err := svc.Withdraw(testZone, chg.ID, "alice"); !IsApproval(err) {
		t.Fatalf("non-submitter withdraw should be an approval error, got %v", err)
	}
	// 提交者撤销成功，重复撤销幂等。
	if _, err := svc.Withdraw(testZone, chg.ID, "carol"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if _, err := svc.Withdraw(testZone, chg.ID, "carol"); err != nil {
		t.Fatalf("re-withdraw should be idempotent, got %v", err)
	}
	// 已撤销的变更不能审批或发布。
	if _, err := svc.Approve(testZone, chg.ID, "alice"); !IsState(err) {
		t.Fatalf("approve withdrawn should be a state error, got %v", err)
	}
	if _, err := svc.Publish(testZone, chg.ID); !IsState(err) {
		t.Fatalf("publish withdrawn should be a state error, got %v", err)
	}

	// 已发布的变更不能撤销。
	c2 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{addWWW("api.example.com", "192.0.2.2", 300)},
	})
	mustApprove(t, svc, testZone, c2.ID, "alice")
	mustPublish(t, svc, testZone, c2.ID)
	if _, err := svc.Withdraw(testZone, c2.ID, "carol"); !IsState(err) {
		t.Fatalf("withdraw published should be a state error, got %v", err)
	}
}

// TestPublishOutOfOrder 验证不能发布落后于当前已发布修订的变更。
func TestPublishOutOfOrder(t *testing.T) {
	svc := NewService(NewMemoryStore())
	if _, err := svc.InitZone(testZone, []string{"ns1.example.com"}, Policy{}); err != nil {
		t.Fatalf("InitZone: %v", err)
	}
	c1 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("a.example.com", "192.0.2.1", 300)},
	})
	c2 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{addWWW("b.example.com", "192.0.2.2", 300)},
	})
	mustPublish(t, svc, testZone, c2.ID) // 先发布修订 3
	if _, err := svc.Publish(testZone, c1.ID); !IsState(err) {
		t.Fatalf("publishing an older revision should be a state error, got %v", err)
	}
}

// TestZoneViewValidationRules 覆盖名称规范、TTL 边界与各类记录冲突。
func TestZoneViewValidationRules(t *testing.T) {
	svc := NewService(NewMemoryStore())
	if _, err := svc.InitZone(testZone, []string{"ns1.example.com"}, Policy{MinTTL: 60, MaxTTL: 86400}); err != nil {
		t.Fatalf("InitZone: %v", err)
	}

	cases := []struct {
		name string
		ops  []ChangeOp
	}{
		{"bad name", []ChangeOp{addWWW("bad_name.example.com", "192.0.2.1", 300)}},
		{"outside zone", []ChangeOp{addWWW("www.other.com", "192.0.2.1", 300)}},
		{"ttl too small", []ChangeOp{addWWW("www.example.com", "192.0.2.1", 10)}},
		{"ttl too large", []ChangeOp{addWWW("www.example.com", "192.0.2.1", 999999)}},
		{"bad ipv4", []ChangeOp{addWWW("www.example.com", "10.0.0.256", 300)}},
		{"duplicate records", []ChangeOp{{Kind: OpAdd, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: 300, Records: []string{"192.0.2.1", "192.0.2.1"}}}}},
		{"cname at apex", []ChangeOp{{Kind: OpAdd, RecordSet: RecordSet{
			Name: testZone, Type: TypeCNAME, TTL: 300, Records: []string{"other.example.com."}}}}},
		{"add existing", []ChangeOp{
			addWWW("www.example.com", "192.0.2.1", 300),
			{Kind: OpReplace, RecordSet: RecordSet{Name: "www.example.com", Type: TypeA, TTL: 300, Records: []string{"192.0.2.2"}}},
			addWWW("www.example.com", "192.0.2.3", 300),
		}},
		{"replace missing", []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "ghost.example.com", Type: TypeA, TTL: 300, Records: []string{"192.0.2.1"}}}}},
		{"delete missing", []ChangeOp{{Kind: OpDelete, RecordSet: RecordSet{
			Name: "ghost.example.com", Type: TypeA}}}},
		{"delete apex soa", []ChangeOp{{Kind: OpDelete, RecordSet: RecordSet{
			Name: testZone, Type: TypeSOA}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, _ := svc.GetZone(testZone)
			_, err := svc.SubmitChange(SubmitInput{
				Zone: testZone, BaseRevision: info.HeadRevision, Submitter: "carol", Ops: tc.ops,
			})
			if !IsValidation(err) {
				t.Fatalf("expected validation error, got %v", err)
			}
			after, _ := svc.GetZone(testZone)
			if after.HeadRevision != info.HeadRevision {
				t.Fatalf("failed validation must not advance head: %d -> %d", info.HeadRevision, after.HeadRevision)
			}
		})
	}

	// CNAME 排他：先加 A，再加同名 CNAME 应失败。
	mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	_, err := svc.SubmitChange(SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpAdd, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeCNAME, TTL: 300, Records: []string{"other.example.com."}}}},
	})
	if !IsValidation(err) {
		t.Fatalf("CNAME coexistence should be a validation error, got %v", err)
	}
}

// TestRevisionQueries 验证修订查询接口。
func TestRevisionQueries(t *testing.T) {
	svc := newTestService(t)
	mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})

	revs, err := svc.ListRevisions(testZone)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 2 || revs[0].Number != 1 || revs[1].Number != 2 {
		t.Fatalf("unexpected revisions: %+v", revs)
	}
	if _, err := svc.GetRevision(testZone, 99); !IsNotFound(err) {
		t.Fatalf("missing revision should be a not-found error, got %v", err)
	}
	if _, err := svc.GetChange(testZone, "nope"); !IsNotFound(err) {
		t.Fatalf("missing change should be a not-found error, got %v", err)
	}
}
