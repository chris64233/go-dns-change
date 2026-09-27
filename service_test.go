package dnschange_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	dns "github.com/chris64233/go-dns-change"
)

func aRecord(name string, ttl uint32, values ...string) dns.RecordSet {
	return dns.RecordSet{Name: name, Type: "A", TTL: ttl, Values: values}
}

func newService() *dns.Service {
	return dns.NewService(dns.NewMemoryStore())
}

func mustInit(t *testing.T, svc *dns.Service, name string, recs []dns.RecordSet, policy dns.Policy, cfg dns.ZoneConfig) *dns.Revision {
	t.Helper()
	rev, _, err := svc.InitZone(context.Background(), dns.InitZoneRequest{
		Name: name, Config: cfg, Policy: policy, Records: recs,
	})
	if err != nil {
		t.Fatalf("InitZone: %v", err)
	}
	return rev
}

func requireCode(t *testing.T, err error, code dns.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", code)
	}
	if got := dns.CodeOf(err); got != code {
		t.Fatalf("expected error code %s, got %s (%v)", code, got, err)
	}
}

// --- 初始化与校验 ---

func TestInitZoneAndNormalization(t *testing.T) {
	svc := newService()
	rev := mustInit(t, svc, "Example.COM",
		[]dns.RecordSet{
			aRecord("www", 300, "10.0.0.1"),
			{Name: "API.Example.com.", Type: "txt", TTL: 600, Values: []string{"v=spf1 -all"}},
		},
		dns.Policy{}, dns.ZoneConfig{MinTTL: 60, MaxTTL: 3600})

	if rev.Number != 1 || rev.Status != dns.StatusPublished || rev.Kind != dns.RevInit {
		t.Fatalf("unexpected init revision: %+v", rev)
	}
	view, err := svc.GetZone(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if view.Head != 1 || view.Tip != 1 {
		t.Fatalf("head/tip = %d/%d, want 1/1", view.Head, view.Tip)
	}
	// 名称被规范化为小写 FQDN；记录按 name/type 排序。
	want := []dns.RecordSet{
		{Name: "api.example.com.", Type: "TXT", TTL: 600, Values: []string{"v=spf1 -all"}},
		aRecord("www.example.com.", 300, "10.0.0.1"),
	}
	if fmt.Sprint(view.Records) != fmt.Sprint(want) {
		t.Fatalf("records = %+v, want %+v", view.Records, want)
	}
}

func TestInitValidationFailures(t *testing.T) {
	cases := []struct {
		name string
		recs []dns.RecordSet
		cfg  dns.ZoneConfig
	}{
		{"bad label", []dns.RecordSet{aRecord("bad name", 300, "10.0.0.1")}, dns.ZoneConfig{}},
		{"outside zone", []dns.RecordSet{aRecord("www.other.com.", 300, "10.0.0.1")}, dns.ZoneConfig{}},
		{"bad address", []dns.RecordSet{aRecord("www", 300, "not-an-ip")}, dns.ZoneConfig{}},
		{"duplicate value", []dns.RecordSet{aRecord("www", 300, "10.0.0.1", "10.0.0.1")}, dns.ZoneConfig{}},
		{"cname multi value", []dns.RecordSet{{Name: "www", Type: "CNAME", TTL: 300, Values: []string{"a.example.com.", "b.example.com."}}}, dns.ZoneConfig{}},
		{"cname conflict", []dns.RecordSet{
			{Name: "www", Type: "CNAME", TTL: 300, Values: []string{"host.example.com."}},
			aRecord("www", 300, "10.0.0.1"),
		}, dns.ZoneConfig{}},
		{"cname self loop", []dns.RecordSet{{Name: "www", Type: "CNAME", TTL: 300, Values: []string{"www.example.com."}}}, dns.ZoneConfig{}},
		{"ttl below min", []dns.RecordSet{aRecord("www", 10, "10.0.0.1")}, dns.ZoneConfig{MinTTL: 60}},
		{"ttl above max", []dns.RecordSet{aRecord("www", 9999, "10.0.0.1")}, dns.ZoneConfig{MaxTTL: 3600}},
		{"empty values", []dns.RecordSet{aRecord("www", 300)}, dns.ZoneConfig{}},
		{"unsupported type", []dns.RecordSet{{Name: "www", Type: "PTR", TTL: 300, Values: []string{"x"}}}, dns.ZoneConfig{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newService()
			_, _, err := svc.InitZone(context.Background(), dns.InitZoneRequest{
				Name: "example.com", Records: tc.recs, Config: tc.cfg,
			})
			requireCode(t, err, dns.ErrValidation)
		})
	}
}

func TestInitZoneIdempotentExistence(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", nil, dns.Policy{}, dns.ZoneConfig{})
	_, _, err := svc.InitZone(context.Background(), dns.InitZoneRequest{Name: "example.com"})
	requireCode(t, err, dns.ErrRevision)
}

func TestPolicyValidation(t *testing.T) {
	svc := newService()
	_, _, err := svc.InitZone(context.Background(), dns.InitZoneRequest{
		Name:   "example.com",
		Policy: dns.Policy{RequiredApprovals: 3, Approvers: []string{"alice", "bob"}},
	})
	requireCode(t, err, dns.ErrValidation)
}

// --- 提交：混合变更、全视图校验、原子性 ---

func submitOK(t *testing.T, svc *dns.Service, req dns.SubmitChangeRequest) *dns.SubmitResult {
	t.Helper()
	res, err := svc.SubmitChange(context.Background(), req)
	if err != nil {
		t.Fatalf("SubmitChange: %v", err)
	}
	return res
}

func TestSubmitMixedChangeAndFullViewValidation(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com",
		[]dns.RecordSet{
			aRecord("www", 300, "10.0.0.1"),
			aRecord("api", 300, "10.0.0.2"),
		},
		dns.Policy{}, dns.ZoneConfig{MinTTL: 60, MaxTTL: 3600})

	// 合法的混合变更：替换 www、新增 mail、删除 api。
	res := submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{
			aRecord("www", 600, "10.0.1.1", "10.0.1.2"),
			aRecord("mail", 300, "10.0.0.9"),
		},
		Deletes: []dns.RecordSetRef{{Name: "api", Type: "A"}},
	})
	if res.Revision.Number != 2 || res.Revision.Base != 1 || res.Revision.Status != dns.StatusApproved {
		t.Fatalf("unexpected revision: %+v", res.Revision)
	}
	names := map[string]bool{}
	for _, rs := range res.Revision.Records {
		if names[rs.Name+"|"+rs.Type] {
			t.Fatal("duplicate record set in resulting view")
		}
		names[rs.Name+"|"+rs.Type] = true
	}
	if names["api.example.com.|A"] {
		t.Fatal("api should have been deleted")
	}
	if !names["mail.example.com.|A"] || !names["www.example.com.|A"] {
		t.Fatalf("resulting view missing records: %v", names)
	}
}

func TestSubmitValidationFailureLeavesNoRevision(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com",
		[]dns.RecordSet{aRecord("www", 300, "10.0.0.1")},
		dns.Policy{}, dns.ZoneConfig{MinTTL: 60})

	// 新增记录触发变更后视图的 TTL 冲突；原有记录没问题，仍要整体拒绝。
	_, err := svc.SubmitChange(context.Background(), dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("bad", 10, "10.0.0.2")},
	})
	requireCode(t, err, dns.ErrValidation)

	// 引入 CNAME 与既有同名记录冲突，也要拒绝。
	_, err = svc.SubmitChange(context.Background(), dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{{Name: "www", Type: "CNAME", TTL: 300, Values: []string{"host.example.com."}}},
	})
	requireCode(t, err, dns.ErrValidation)

	// 删除不存在的记录集。
	_, err = svc.SubmitChange(context.Background(), dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Deletes: []dns.RecordSetRef{{Name: "ghost", Type: "A"}},
	})
	requireCode(t, err, dns.ErrValidation)

	// 同批 upsert 与 delete 撞键。
	_, err = svc.SubmitChange(context.Background(), dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("www", 300, "10.0.0.2")},
		Deletes: []dns.RecordSetRef{{Name: "www", Type: "A"}},
	})
	requireCode(t, err, dns.ErrValidation)

	view, err := svc.GetZone(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if view.Tip != 1 || view.Head != 1 {
		t.Fatalf("tip/head = %d/%d, failed validation must not allocate a revision", view.Tip, view.Head)
	}
}

func TestSubmitBaseRevisionConflict(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", []dns.RecordSet{aRecord("www", 300, "10.0.0.1")}, dns.Policy{}, dns.ZoneConfig{})

	// 第一个变更占用在途槽位（需审批，保持 pending）。
	policy := dns.Policy{RequiredApprovals: 1, Approvers: []string{"alice"}}
	if _, err := svc.UpdatePolicy(context.Background(), "example.com", policy); err != nil {
		t.Fatal(err)
	}
	submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("a", 300, "10.0.0.2")},
	})

	// 基准号错误。
	_, err := svc.SubmitChange(context.Background(), dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 99, Committer: "dave",
		Upserts: []dns.RecordSet{aRecord("b", 300, "10.0.0.3")},
	})
	requireCode(t, err, dns.ErrRevision)

	// 基准号正确但存在在途修订：不允许排队，防止静默覆盖。
	_, err = svc.SubmitChange(context.Background(), dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "dave",
		Upserts: []dns.RecordSet{aRecord("b", 300, "10.0.0.3")},
	})
	requireCode(t, err, dns.ErrRevision)
}

// --- 审批 ---

func TestApprovalFlowQuorumAndSnapshot(t *testing.T) {
	svc := newService()
	// 初始策略：1 个审批人、名单含 carol、不启用职责分离。
	mustInit(t, svc, "example.com", []dns.RecordSet{aRecord("www", 300, "10.0.0.1")},
		dns.Policy{RequiredApprovals: 1, Approvers: []string{"alice", "carol"}}, dns.ZoneConfig{})

	res := submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("a", 300, "10.0.0.2")},
	})
	if res.Revision.Status != dns.StatusPending {
		t.Fatalf("new revision should be pending, got %s", res.Revision.Status)
	}

	// 提交后收紧策略：需要 2 人、启用职责分离。在途修订仍按提交时快照执行。
	_, err := svc.UpdatePolicy(context.Background(), "example.com",
		dns.Policy{RequiredApprovals: 2, Approvers: []string{"alice", "bob"}, SeparationOfDuties: true})
	if err != nil {
		t.Fatal(err)
	}

	// 快照里 carol 有资格（当前策略已经没有 carol），自审批在快照策略下允许。
	ar, err := svc.Approve(context.Background(), dns.ApproveRequest{
		Zone: "example.com", Revision: 2, Approver: "carol",
	})
	if err != nil {
		t.Fatalf("snapshot policy should allow carol: %v", err)
	}
	if ar.Revision.Status != dns.StatusApproved {
		t.Fatalf("snapshot required 1 approval, got status %s", ar.Revision.Status)
	}

	// 审批数已满后，新的审批人被拒绝。
	_, err = svc.Approve(context.Background(), dns.ApproveRequest{
		Zone: "example.com", Revision: 2, Approver: "alice",
	})
	requireCode(t, err, dns.ErrApproval)

	// 未满足审批前不能发布。
	pub, err := svc.Publish(context.Background(), dns.PublishRequest{Zone: "example.com", Revision: 2})
	if err != nil {
		t.Fatalf("approved revision should publish: %v", err)
	}
	if pub.OutboxEvent == nil || pub.OutboxEvent.Revision != 2 {
		t.Fatal("publish must return the outbox event")
	}

	// 新提交的修订使用新策略快照：需要 2 人且职责分离。
	res2 := submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 2, Committer: "bob",
		Upserts: []dns.RecordSet{aRecord("b", 300, "10.0.0.3")},
	})
	_, err = svc.Approve(context.Background(), dns.ApproveRequest{
		Zone: "example.com", Revision: res2.Revision.Number, Approver: "bob",
	})
	requireCode(t, err, dns.ErrApproval) // SoD：自己不能批
	_, err = svc.Approve(context.Background(), dns.ApproveRequest{
		Zone: "example.com", Revision: res2.Revision.Number, Approver: "mallory",
	})
	requireCode(t, err, dns.ErrApproval) // 不在名单
	a1, err := svc.Approve(context.Background(), dns.ApproveRequest{
		Zone: "example.com", Revision: 3, Approver: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a1.Revision.Status != dns.StatusPending {
		t.Fatalf("1 of 2 approvals should stay pending, got %s", a1.Revision.Status)
	}
	a2, err := svc.Approve(context.Background(), dns.ApproveRequest{
		Zone: "example.com", Revision: 3, Approver: "carol",
	})
	// carol 不在新策略名单里。
	if err == nil {
		_ = a2
		t.Fatal("carol is not authorized under the new snapshot")
	}
	requireCode(t, err, dns.ErrApproval)
	if _, err := svc.Approve(context.Background(), dns.ApproveRequest{
		Zone: "example.com", Revision: 3, Approver: "bob",
	}); err == nil {
		t.Fatal("SoD must prevent bob from approving own change")
	}
}

func TestApprovalIdempotency(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", nil,
		dns.Policy{RequiredApprovals: 2, Approvers: []string{"alice", "bob"}}, dns.ZoneConfig{})
	submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("a", 300, "10.0.0.2")},
	})

	req := dns.ApproveRequest{Zone: "example.com", Revision: 2, Approver: "alice", IdempotencyKey: "apk-1"}
	r1, err := svc.Approve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Replayed {
		t.Fatal("first approval should not be a replay")
	}
	r2, err := svc.Approve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Replayed {
		t.Fatal("repeated approval with same key must be a replay")
	}
	// 同一审批人不带键重复调用同样幂等，不产生第二条审批。
	r3, err := svc.Approve(context.Background(), dns.ApproveRequest{Zone: "example.com", Revision: 2, Approver: "alice"})
	if err != nil || !r3.Replayed || len(r3.Revision.Approvals) != 1 {
		t.Fatalf("duplicate approver must be idempotent: %+v err=%v", r3, err)
	}
	// 键被不同载荷复用：幂等错误。
	_, err = svc.Approve(context.Background(), dns.ApproveRequest{
		Zone: "example.com", Revision: 2, Approver: "bob", IdempotencyKey: "apk-1",
	})
	requireCode(t, err, dns.ErrIdempotency)
}

func TestApproveTerminalRevision(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", nil, dns.Policy{}, dns.ZoneConfig{})
	// 对已发布修订审批：状态错误。
	_, err := svc.Approve(context.Background(), dns.ApproveRequest{Zone: "example.com", Revision: 1, Approver: "alice"})
	requireCode(t, err, dns.ErrState)
	// 不存在的修订。
	_, err = svc.Approve(context.Background(), dns.ApproveRequest{Zone: "example.com", Revision: 42, Approver: "alice"})
	requireCode(t, err, dns.ErrRevision)
}

// --- 发布、撤销、回滚 ---

func TestPublishOutboxExactlyOnce(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", []dns.RecordSet{aRecord("www", 300, "10.0.0.1")}, dns.Policy{}, dns.ZoneConfig{})
	submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("a", 300, "10.0.0.2")},
	})

	p1, err := svc.Publish(context.Background(), dns.PublishRequest{Zone: "example.com", Revision: 2})
	if err != nil {
		t.Fatal(err)
	}
	// 重复发布：幂等返回同一事件，不再写 outbox。
	p2, err := svc.Publish(context.Background(), dns.PublishRequest{Zone: "example.com", Revision: 2})
	if err != nil {
		t.Fatal(err)
	}
	if p1.OutboxEvent.ID != p2.OutboxEvent.ID {
		t.Fatalf("outbox event changed across replays: %d vs %d", p1.OutboxEvent.ID, p2.OutboxEvent.ID)
	}
	events, err := svc.ListOutbox(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Revision == 2 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("revision 2 emitted %d outbox events, want 1", count)
	}
}

func TestPublishCanceledRevision(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", nil, dns.Policy{RequiredApprovals: 1, Approvers: []string{"alice"}}, dns.ZoneConfig{})
	submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("a", 300, "10.0.0.2")},
	})
	if _, err := svc.Cancel(context.Background(), dns.CancelRequest{
		Zone: "example.com", Revision: 2, Requester: "carol",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Publish(context.Background(), dns.PublishRequest{Zone: "example.com", Revision: 2})
	requireCode(t, err, dns.ErrState)
}

func TestCancelPermissionsAndSlotRelease(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", nil, dns.Policy{RequiredApprovals: 1, Approvers: []string{"alice"}}, dns.ZoneConfig{})
	submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("a", 300, "10.0.0.2")},
	})

	// 非提交者不能撤销。
	_, err := svc.Cancel(context.Background(), dns.CancelRequest{
		Zone: "example.com", Revision: 2, Requester: "mallory",
	})
	requireCode(t, err, dns.ErrApproval)

	// 提交者撤销成功。修订号 2 保留为 canceled，不被复用。
	canceled, err := svc.Cancel(context.Background(), dns.CancelRequest{
		Zone: "example.com", Revision: 2, Requester: "carol",
	})
	if err != nil || canceled.Status != dns.StatusCanceled {
		t.Fatalf("cancel failed: %+v err=%v", canceled, err)
	}
	// 重复撤销：状态错误。
	_, err = svc.Cancel(context.Background(), dns.CancelRequest{Zone: "example.com", Revision: 2, Requester: "carol"})
	requireCode(t, err, dns.ErrState)

	// 槽位释放后新提交拿到 3（连续，不跳号），基准仍是 1。
	res := submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "dave",
		Upserts: []dns.RecordSet{aRecord("b", 300, "10.0.0.3")},
	})
	if res.Revision.Number != 3 {
		t.Fatalf("expected revision 3, got %d", res.Revision.Number)
	}
	revs, err := svc.ListRevisions(context.Background(), "example.com")
	if err != nil || len(revs) != 3 {
		t.Fatalf("revisions = %d, err=%v", len(revs), err)
	}
	if revs[1].Status != dns.StatusCanceled {
		t.Fatal("canceled revision must be retained in history")
	}
}

func TestRollbackCreatesNewRevisionReusingHistory(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", []dns.RecordSet{aRecord("www", 300, "10.0.0.1")}, dns.Policy{}, dns.ZoneConfig{})

	// rev2：替换 www。
	submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("www", 300, "10.0.2.1")},
	})
	if _, err := svc.Publish(context.Background(), dns.PublishRequest{Zone: "example.com", Revision: 2}); err != nil {
		t.Fatal(err)
	}
	// rev3：删除 www。
	submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 2, Committer: "carol",
		Deletes: []dns.RecordSetRef{{Name: "www", Type: "A"}},
	})
	if _, err := svc.Publish(context.Background(), dns.PublishRequest{Zone: "example.com", Revision: 3}); err != nil {
		t.Fatal(err)
	}

	// 回滚到 rev1 的内容。
	rb, err := svc.Rollback(context.Background(), dns.RollbackRequest{
		Zone: "example.com", SourceRevision: 1, Committer: "dave",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rb.Revision.Number != 4 || rb.Revision.Kind != dns.RevRollback || rb.Revision.SourceRevision != 1 ||
		rb.Revision.Base != 3 || rb.Revision.Status != dns.StatusApproved {
		t.Fatalf("unexpected rollback revision: %+v", rb.Revision)
	}
	if fmt.Sprint(rb.Revision.Records) != fmt.Sprint([]dns.RecordSet{aRecord("www.example.com.", 300, "10.0.0.1")}) {
		t.Fatalf("rollback records = %+v", rb.Revision.Records)
	}
	if _, err := svc.Publish(context.Background(), dns.PublishRequest{Zone: "example.com", Revision: 4}); err != nil {
		t.Fatal(err)
	}

	view, _ := svc.GetZone(context.Background(), "example.com")
	if view.Head != 4 || len(view.Records) != 1 || view.Records[0].Values[0] != "10.0.0.1" {
		t.Fatalf("zone after rollback: head=%d records=%+v", view.Head, view.Records)
	}
	// 旧历史未被改写。
	r1, _ := svc.GetRevision(context.Background(), "example.com", 1)
	if r1.Status != dns.StatusPublished || r1.Kind != dns.RevInit {
		t.Fatalf("history revision 1 altered: %+v", r1)
	}
	// 每个实际发布的修订恰好一条 outbox（rev2/3/4 各一条 + init）。
	events, _ := svc.ListOutbox(context.Background(), "example.com")
	if len(events) != 4 {
		t.Fatalf("outbox len = %d, want 4", len(events))
	}
}

func TestRollbackGuards(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", []dns.RecordSet{aRecord("www", 300, "10.0.0.1")},
		dns.Policy{RequiredApprovals: 1, Approvers: []string{"alice"}}, dns.ZoneConfig{})
	submitOK(t, svc, dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts: []dns.RecordSet{aRecord("a", 300, "10.0.0.2")},
	})
	// 回滚到一个从未发布的在途修订：状态错误。
	_, err := svc.Rollback(context.Background(), dns.RollbackRequest{
		Zone: "example.com", SourceRevision: 2, Committer: "carol",
	})
	requireCode(t, err, dns.ErrState)

	// 回滚到不存在的修订。
	_, err = svc.Rollback(context.Background(), dns.RollbackRequest{
		Zone: "example.com", SourceRevision: 42, Committer: "carol",
	})
	requireCode(t, err, dns.ErrRevision)

	// 存在在途修订时不能回滚（避免并发排队）。
	_, err = svc.Rollback(context.Background(), dns.RollbackRequest{
		Zone: "example.com", SourceRevision: 1, Committer: "carol",
	})
	requireCode(t, err, dns.ErrRevision)

	// 撤掉 rev2 后，回滚到当前 head 没有意义。
	if _, err := svc.Cancel(context.Background(), dns.CancelRequest{
		Zone: "example.com", Revision: 2, Requester: "carol",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Rollback(context.Background(), dns.RollbackRequest{
		Zone: "example.com", SourceRevision: 1, Committer: "carol",
	})
	requireCode(t, err, dns.ErrState)
}

func TestSubmitAndRollbackIdempotencyKeys(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", nil, dns.Policy{}, dns.ZoneConfig{})

	req := dns.SubmitChangeRequest{
		Zone: "example.com", BaseRevision: 1, Committer: "carol",
		Upserts:        []dns.RecordSet{aRecord("a", 300, "10.0.0.2")},
		IdempotencyKey: "sub-1",
	}
	r1, err := svc.SubmitChange(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.SubmitChange(context.Background(), req)
	if err != nil || !r2.Replayed || r2.Revision.Number != r1.Revision.Number {
		t.Fatalf("submit replay failed: %+v err=%v", r2, err)
	}
	// 相同键、不同载荷：幂等错误，不产生新修订。
	req2 := req
	req2.IdempotencyKey = "sub-1"
	req2.Upserts = []dns.RecordSet{aRecord("b", 300, "10.0.0.3")}
	_, err = svc.SubmitChange(context.Background(), req2)
	requireCode(t, err, dns.ErrIdempotency)

	if _, err := svc.Publish(context.Background(), dns.PublishRequest{Zone: "example.com", Revision: 2}); err != nil {
		t.Fatal(err)
	}
	rbReq := dns.RollbackRequest{Zone: "example.com", SourceRevision: 1, Committer: "carol", IdempotencyKey: "rb-1"}
	rb1, err := svc.Rollback(context.Background(), rbReq)
	if err != nil {
		t.Fatal(err)
	}
	rb2, err := svc.Rollback(context.Background(), rbReq)
	if err != nil || !rb2.Replayed || rb2.Revision.Number != rb1.Revision.Number {
		t.Fatalf("rollback replay failed: %+v err=%v", rb2, err)
	}
}

// --- 并发：不互相覆盖、不跳号、确定顺序 ---

func TestConcurrentSubmitExactlyOneWins(t *testing.T) {
	svc := newService()
	mustInit(t, svc, "example.com", nil, dns.Policy{}, dns.ZoneConfig{})

	const n = 32
	var wg sync.WaitGroup
	winners := make(chan int64, n)
	var errs []error
	var mu sync.Mutex
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := svc.SubmitChange(context.Background(), dns.SubmitChangeRequest{
				Zone: "example.com", BaseRevision: 1, Committer: fmt.Sprintf("user-%d", i),
				Upserts: []dns.RecordSet{aRecord(fmt.Sprintf("host-%d", i), 300, fmt.Sprintf("10.0.0.%d", i+1))},
			})
			if err == nil {
				winners <- res.Revision.Number
			} else {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(winners)

	if len(winners) != 1 {
		t.Fatalf("winners = %d, want exactly 1; errors=%d", len(winners), len(errs))
	}
	for _, err := range errs {
		if dns.CodeOf(err) != dns.ErrRevision {
			t.Fatalf("losers must get revision_conflict, got %v", err)
		}
	}
	view, _ := svc.GetZone(context.Background(), "example.com")
	if view.Tip != 2 {
		t.Fatalf("tip = %d, want 2 (no gaps, no silent overwrites)", view.Tip)
	}
}

func TestConcurrentPublishCancelDeterministic(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		svc := newService()
		mustInit(t, svc, "example.com", nil, dns.Policy{}, dns.ZoneConfig{})
		submitOK(t, svc, dns.SubmitChangeRequest{
			Zone: "example.com", BaseRevision: 1, Committer: "carol",
			Upserts: []dns.RecordSet{aRecord("a", 300, "10.0.0.2")},
		})

		start := make(chan struct{})
		var wg sync.WaitGroup
		var publishOK, cancelOK bool
		var mu sync.Mutex
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.Publish(context.Background(), dns.PublishRequest{Zone: "example.com", Revision: 2}); err == nil {
				mu.Lock()
				publishOK = true
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.Cancel(context.Background(), dns.CancelRequest{
				Zone: "example.com", Revision: 2, Requester: "carol",
			}); err == nil {
				mu.Lock()
				cancelOK = true
				mu.Unlock()
			}
		}()
		close(start)
		wg.Wait()

		if publishOK == cancelOK {
			t.Fatalf("iter %d: exactly one of publish/cancel must succeed (publish=%v cancel=%v)", iter, publishOK, cancelOK)
		}
		rev, _ := svc.GetRevision(context.Background(), "example.com", 2)
		events, _ := svc.ListOutbox(context.Background(), "example.com")
		switch {
		case publishOK && rev.Status != dns.StatusPublished:
			t.Fatalf("iter %d: publish won but status=%s", iter, rev.Status)
		case cancelOK && rev.Status != dns.StatusCanceled:
			t.Fatalf("iter %d: cancel won but status=%s", iter, rev.Status)
		}
		outboxForRev2 := 0
		for _, e := range events {
			if e.Revision == 2 {
				outboxForRev2++
			}
		}
		if publishOK && outboxForRev2 != 1 {
			t.Fatalf("iter %d: published revision has %d outbox events", iter, outboxForRev2)
		}
		if cancelOK && outboxForRev2 != 0 {
			t.Fatalf("iter %d: canceled revision emitted outbox", iter)
		}
	}
}

// --- 查询 ---

func TestRevisionQueryErrors(t *testing.T) {
	svc := newService()
	_, err := svc.GetZone(context.Background(), "example.com")
	requireCode(t, err, dns.ErrRevision)
	mustInit(t, svc, "example.com", nil, dns.Policy{}, dns.ZoneConfig{})
	if _, err := svc.GetRevision(context.Background(), "example.com", 7); err == nil {
		t.Fatal("expected error for missing revision")
	} else if dns.CodeOf(err) != dns.ErrRevision {
		t.Fatalf("missing revision should be a revision error, got %v", err)
	}
}
