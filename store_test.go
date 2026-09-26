package dnschange

import (
	"testing"
)

// TestFileStorePersistence 验证文件存储：状态与历史跨服务实例持久化。
func TestFileStorePersistence(t *testing.T) {
	dir := t.TempDir()

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	svc := NewService(store)
	if _, err := svc.InitZone(testZone, []string{"ns1.example.com"}, Policy{}); err != nil {
		t.Fatalf("InitZone: %v", err)
	}
	chg := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustPublish(t, svc, testZone, chg.ID) // 策略无需审批，提交即批准

	// 用同一目录重建服务，状态与历史应完整恢复。
	store2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	svc2 := NewService(store2)

	info, err := svc2.GetZone(testZone)
	if err != nil {
		t.Fatalf("GetZone after reload: %v", err)
	}
	if info.HeadRevision != 2 || info.PublishedRevision != 2 {
		t.Fatalf("state not persisted: %+v", info)
	}
	revs, err := svc2.ListRevisions(testZone)
	if err != nil || len(revs) != 2 {
		t.Fatalf("history not persisted: %v %v", revs, err)
	}
	outbox, err := svc2.ListOutbox(testZone)
	if err != nil || len(outbox) != 2 {
		t.Fatalf("outbox not persisted: %v %v", outbox, err)
	}

	zones, err := store2.ListZones()
	if err != nil || len(zones) != 1 || zones[0] != "example.com." {
		t.Fatalf("ListZones: %v %v", zones, err)
	}

	// 恢复后的服务可以继续演进：新提交取得修订号 3。
	c3, err := svc2.SubmitChange(SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{addWWW("api.example.com", "192.0.2.2", 300)},
	})
	if err != nil || c3.Revision != 3 {
		t.Fatalf("submit after reload: %v %v", c3, err)
	}
}

// TestMemoryStoreIsolation 验证内存存储返回深拷贝，外部修改不污染内部状态。
func TestMemoryStoreIsolation(t *testing.T) {
	svc := newTestService(t)
	rev, err := svc.GetRevision(testZone, 1)
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}
	rev.RecordSets[0].Records[0] = "tampered"
	rev2, err := svc.GetRevision(testZone, 1)
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}
	if rev2.RecordSets[0].Records[0] == "tampered" {
		t.Fatal("store must return isolated copies")
	}
}
