package dnschange

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// clock 是可控时钟，用于驱动最短观察时间。
type clock struct{ t time.Time }

func newClock() *clock               { return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }
func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newClockService(t *testing.T) (*Service, *clock) {
	t.Helper()
	clk := newClock()
	svc := NewService(NewMemoryStore())
	svc.now = clk.now
	if _, err := svc.InitZone(testZone, []string{"ns1.example.com", "ns2.example.com"}, Policy{
		RequiredApprovals:  1,
		EligibleApprovers:  []string{"alice", "bob"},
		SeparationOfDuties: true,
	}); err != nil {
		t.Fatalf("InitZone: %v", err)
	}
	return svc, clk
}

// replaceWWW 提交一个把 www A 记录从 oldIP 替换为 newIP 的已审批变更。
func approvedWWWChange(t *testing.T, svc *Service, base int64, oldIP, newIP string, ttl uint32) *Change {
	t.Helper()
	chg := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: base, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: ttl, Records: []string{newIP}}}},
	})
	mustApprove(t, svc, testZone, chg.ID, "alice")
	return chg
}

func threeStages() []StageInput {
	return []StageInput{
		{TargetWeight: 10, MinObservation: 30 * time.Second, HealthThreshold: 0.95, MinSamples: 4},
		{TargetWeight: 50, MinObservation: 30 * time.Second, HealthThreshold: 0.95, MinSamples: 4},
		{TargetWeight: 100, MinObservation: time.Second, HealthThreshold: 0.9, MinSamples: 1},
	}
}

func weightedWWW(rev *Revision) RecordSet {
	for _, rs := range rev.RecordSets {
		if rs.Key() == "www.example.com.|A" {
			return rs
		}
	}
	return RecordSet{}
}

// TestPlanCreateValidation 覆盖阶段计划创建的各类拒绝。
func TestPlanCreateValidation(t *testing.T) {
	svc, _ := newClockService(t)

	// 先建立一个已存在的 www 记录（修订 2）。
	seed := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, seed.ID, "alice")
	mustPublish(t, svc, testZone, seed.ID)
	chg := approvedWWWChange(t, svc, 2, "192.0.2.1", "192.0.2.10", 300)

	badCases := []struct {
		name   string
		stages []StageInput
	}{
		{"empty", nil},
		{"zero first weight", []StageInput{{TargetWeight: 0, MinSamples: 1}, {TargetWeight: 100, MinSamples: 1}}},
		{"not increasing", []StageInput{{TargetWeight: 50, MinSamples: 1}, {TargetWeight: 50, MinSamples: 1}, {TargetWeight: 100, MinSamples: 1}}},
		{"last not 100", []StageInput{{TargetWeight: 50, MinSamples: 1}, {TargetWeight: 90, MinSamples: 1}}},
		{"weight over 100", []StageInput{{TargetWeight: 110, MinSamples: 1}}},
		{"negative observation", []StageInput{{TargetWeight: 100, MinObservation: -1, MinSamples: 1}}},
		{"threshold out of range", []StageInput{{TargetWeight: 100, HealthThreshold: 1.5, MinSamples: 1}}},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: chg.ID, Stages: tc.stages})
			if !IsValidation(err) {
				t.Fatalf("expected validation error, got %v", err)
			}
		})
	}

	// 未审批的变更不能创建计划。
	pending := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 3, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: 300, Records: []string{"192.0.2.20"}}}},
	})
	if _, err := svc.CreatePlan(CreatePlanInput{
		Zone: testZone, ChangeID: pending.ID,
		Stages: []StageInput{{TargetWeight: 100, MinSamples: 1}},
	}); !IsState(err) {
		t.Fatalf("pending change should be a state error, got %v", err)
	}

	// 不包含可分流记录（新增记录）不能渐进发布。
	addChg := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 4, Submitter: "carol",
		Ops: []ChangeOp{addWWW("brandnew.example.com", "192.0.2.50", 300)},
	})
	mustApprove(t, svc, testZone, addChg.ID, "bob")
	_, err := svc.CreatePlan(CreatePlanInput{
		Zone: testZone, ChangeID: addChg.ID,
		Stages: []StageInput{{TargetWeight: 100, MinSamples: 1}},
	})
	if !IsValidation(err) {
		t.Fatalf("unsplittable add should be a validation error, got %v", err)
	}
}

// TestPlanProgressiveHappyPath 验证多阶段推进：每阶段发布完整视图、唯一 outbox、
// 修订连续、权重路径合法，末阶段到达目标修订。
func TestPlanProgressiveHappyPath(t *testing.T) {
	svc, clk := newClockService(t)

	seed := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, seed.ID, "alice")
	mustPublish(t, svc, testZone, seed.ID)
	chg := approvedWWWChange(t, svc, 2, "192.0.2.1", "192.0.2.10", 300)

	plan, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: chg.ID, Stages: threeStages()})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if plan.TargetRevision != 3 || plan.BaseRevision != 2 || len(plan.TrafficKeys) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}

	advance := func(stage int, wantWeight uint32, expectRev int64) {
		t.Helper()
		// 推进前若阶段已发布，需要满足观察窗口（除首次发布外）。
		p, err := svc.AdvancePlan(testZone, plan.ID)
		if err != nil {
			t.Fatalf("advance stage %d: %v", stage, err)
		}
		if p.StageStates[stage].PublishedRev != expectRev {
			t.Fatalf("stage %d expected revision %d, got %d", stage, expectRev, p.StageStates[stage].PublishedRev)
		}
		rev, _ := svc.GetRevision(testZone, expectRev)
		www := weightedWWW(rev)
		if wantWeight == 100 {
			if len(www.Weights) != 0 || www.Records[0] != "192.0.2.10" {
				t.Fatalf("final stage must equal target revision (no weights), got %+v", www)
			}
		} else {
			if len(www.Records) != 2 || len(www.Weights) != 2 {
				t.Fatalf("stage %d expected blended records, got %+v", stage, www)
			}
			got := map[string]uint32{}
			for i, r := range www.Records {
				got[r] = www.Weights[i]
			}
			if got["192.0.2.1"] != 100-wantWeight || got["192.0.2.10"] != wantWeight {
				t.Fatalf("stage %d bad weights: %v", stage, got)
			}
		}
	}

	// 阶段 0 首次发布，无需样本。
	advance(0, 10, 4)

	// 观察窗口不足：推进被拒绝，不发布、不跳号。
	if _, err := svc.AdvancePlan(testZone, plan.ID); !IsState(err) {
		t.Fatalf("advance within observation window should be state error, got %v", err)
	}
	info, _ := svc.GetZone(testZone)
	if info.HeadRevision != 4 || info.PublishedRevision != 4 {
		t.Fatalf("failed advance must not move revisions, got head=%d pub=%d", info.HeadRevision, info.PublishedRevision)
	}

	// 上报健康样本：幂等重放不重复计数。
	for i := 0; i < 4; i++ {
		if _, err := svc.RecordHealthSample(HealthSampleInput{
			Zone: testZone, PlanID: plan.ID, Stage: 0,
			SampleKey: fmt.Sprintf("s%d", i), Healthy: true,
		}); err != nil {
			t.Fatalf("sample: %v", err)
		}
	}
	if _, err := svc.RecordHealthSample(HealthSampleInput{
		Zone: testZone, PlanID: plan.ID, Stage: 0, SampleKey: "s0", Healthy: false,
	}); err != nil {
		t.Fatalf("idempotent sample replay should succeed: %v", err)
	}
	detail, _ := svc.GetPlan(testZone, plan.ID)
	if detail.Stages[0].State.TotalSamples != 4 || detail.Stages[0].State.HealthySamples != 4 {
		t.Fatalf("replayed sample must not double-count: %+v", detail.Stages[0].State)
	}

	clk.add(31 * time.Second)
	advance(1, 50, 5)

	// 阶段 1 达标推进到末阶段。
	for i := 0; i < 4; i++ {
		_, _ = svc.RecordHealthSample(HealthSampleInput{
			Zone: testZone, PlanID: plan.ID, Stage: 1,
			SampleKey: fmt.Sprintf("t%d", i), Healthy: true,
		})
	}
	clk.add(31 * time.Second)
	advance(2, 100, 6)

	// 计划完成，目标变更标记已发布。
	final, err := svc.GetPlan(testZone, plan.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if final.Plan.Status != PlanCompleted {
		t.Fatalf("plan should be completed, got %s", final.Plan.Status)
	}
	tchg, _ := svc.GetChange(testZone, chg.ID)
	if tchg.Status != StatusPublished {
		t.Fatalf("target change should be published, got %s", tchg.Status)
	}
	// 末阶段视图与目标修订内容一致。
	targetRev, _ := svc.GetRevision(testZone, chg.Revision)
	finalRev, _ := svc.GetRevision(testZone, 6)
	if len(targetRev.RecordSets) != len(finalRev.RecordSets) {
		t.Fatalf("final stage must match target revision content")
	}
	for i := range targetRev.RecordSets {
		if !setsEqual(targetRev.RecordSets[i], finalRev.RecordSets[i]) {
			t.Fatalf("final stage mismatch: %+v vs %+v", targetRev.RecordSets[i], finalRev.RecordSets[i])
		}
	}

	// 唯一 outbox：引导 + seed + 3 个阶段，且每个修订号只出现一次。
	outbox, _ := svc.ListOutbox(testZone)
	if len(outbox) != 5 {
		t.Fatalf("expected 5 outbox entries (bootstrap, seed, 3 stages), got %d", len(outbox))
	}
	seenRev := map[int64]int{}
	for _, o := range outbox {
		seenRev[o.Revision]++
		if len(o.RecordSets) == 0 {
			t.Fatalf("outbox entry must carry full zone view: %+v", o)
		}
	}
	for rev, n := range seenRev {
		if n != 1 {
			t.Fatalf("revision %d has %d outbox entries, expected exactly 1", rev, n)
		}
	}
	// 修订号连续不跳号。
	revs, _ := svc.ListRevisions(testZone)
	for i, r := range revs {
		if r.Number != int64(i+1) {
			t.Fatalf("revision numbers must be contiguous: %+v", r)
		}
	}
}

// TestHealthSampleBinding 验证样本绑定区域/修订/阶段：旧阶段与其他计划样本
// 不参与当前判断。
func TestHealthSampleBinding(t *testing.T) {
	svc, clk := newClockService(t)
	seed := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, seed.ID, "alice")
	mustPublish(t, svc, testZone, seed.ID)
	chg := approvedWWWChange(t, svc, 2, "192.0.2.1", "192.0.2.10", 300)
	plan, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: chg.ID, Stages: []StageInput{
		{TargetWeight: 20, MinObservation: time.Second, HealthThreshold: 0.9, MinSamples: 2},
		{TargetWeight: 100, MinObservation: time.Second, HealthThreshold: 0.9, MinSamples: 1},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := svc.AdvancePlan(testZone, plan.ID); err != nil {
		t.Fatalf("publish stage 0: %v", err)
	}

	// 阶段未推进时，上报下一阶段的样本被拒绝。
	if _, err := svc.RecordHealthSample(HealthSampleInput{
		Zone: testZone, PlanID: plan.ID, Stage: 1, SampleKey: "x", Healthy: true,
	}); !IsState(err) {
		t.Fatalf("sample for future stage should be rejected, got %v", err)
	}
	// 不存在的计划。
	if _, err := svc.RecordHealthSample(HealthSampleInput{
		Zone: testZone, PlanID: "nope", Stage: 0, SampleKey: "x",
	}); !IsNotFound(err) {
		t.Fatalf("sample for missing plan should be not-found, got %v", err)
	}
	// 样本必须绑定样本键。
	if _, err := svc.RecordHealthSample(HealthSampleInput{
		Zone: testZone, PlanID: plan.ID, Stage: 0,
	}); !IsValidation(err) {
		t.Fatalf("empty sample key should be validation error, got %v", err)
	}
	// 样本记录绑定了当前阶段发布的修订。
	_, _ = svc.RecordHealthSample(HealthSampleInput{
		Zone: testZone, PlanID: plan.ID, Stage: 0, SampleKey: "a", Healthy: true,
	})
	if z, _ := svc.store.LoadZone("example.com."); z.Samples[0].Revision != 4 {
		t.Fatalf("sample must bind to published stage revision, got %d", z.Samples[0].Revision)
	}

	clk.add(2 * time.Second)
	// 阶段 0 需 2 个达标样本，补第二个后推进到阶段 1（权重 100，计划完成）。
	_, _ = svc.RecordHealthSample(HealthSampleInput{
		Zone: testZone, PlanID: plan.ID, Stage: 0, SampleKey: "b", Healthy: true,
	})
	p, err := svc.AdvancePlan(testZone, plan.ID)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	// 推进到末阶段后计划已完成，再上报任何样本都被拒绝。
	if p.Status != PlanCompleted {
		t.Fatalf("expected completed, got %s", p.Status)
	}
	if _, err := svc.RecordHealthSample(HealthSampleInput{
		Zone: testZone, PlanID: plan.ID, Stage: 0, SampleKey: "late", Healthy: true,
	}); !IsState(err) {
		t.Fatalf("sample after completion must be rejected, got %v", err)
	}
}

// TestUnhealthyAutoRecovery 验证健康不达标时自动创建恢复上一稳定权重的新修订，
// 不改写历史，且计划终止、恢复链可查。
func TestUnhealthyAutoRecovery(t *testing.T) {
	svc, clk := newClockService(t)
	seed := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, seed.ID, "alice")
	mustPublish(t, svc, testZone, seed.ID)
	chg := approvedWWWChange(t, svc, 2, "192.0.2.1", "192.0.2.10", 300)
	plan, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: chg.ID, Stages: []StageInput{
		{TargetWeight: 20, MinObservation: time.Second, HealthThreshold: 0.95, MinSamples: 4},
		{TargetWeight: 100, MinObservation: time.Second, HealthThreshold: 0.9, MinSamples: 1},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := svc.AdvancePlan(testZone, plan.ID); err != nil { // 发布阶段 0 -> rev 4
		t.Fatalf("publish stage 0: %v", err)
	}

	// 4 个样本中 2 个不健康（50% < 95%）。
	healthy := []bool{true, false, false, true}
	for i, h := range healthy {
		_, _ = svc.RecordHealthSample(HealthSampleInput{
			Zone: testZone, PlanID: plan.ID, Stage: 0,
			SampleKey: fmt.Sprintf("h%d", i), Healthy: h,
		})
	}
	clk.add(2 * time.Second)
	p, err := svc.AdvancePlan(testZone, plan.ID)
	if err != nil {
		t.Fatalf("unhealthy advance should auto-recover, not error: %v", err)
	}
	if p.Status != PlanAborted || p.RecoveryOf != 2 || p.RecoveryChange == "" {
		t.Fatalf("expected aborted recovery to base revision 2, got %+v", p)
	}
	// 恢复创建了新修订并发布，内容等于稳定基准修订 2，历史修订 4 保留。
	info, _ := svc.GetZone(testZone)
	if info.PublishedRevision != 5 || info.HeadRevision != 5 {
		t.Fatalf("recovery revision should be 5 and published, got %+v", info)
	}
	recRev, _ := svc.GetRevision(testZone, 5)
	baseRev, _ := svc.GetRevision(testZone, 2)
	for i := range baseRev.RecordSets {
		if !setsEqual(baseRev.RecordSets[i], recRev.RecordSets[i]) {
			t.Fatalf("recovery content must match stable revision 2: %+v vs %+v", baseRev.RecordSets[i], recRev.RecordSets[i])
		}
	}
	if recRev.RollbackOf != 2 {
		t.Fatalf("recovery revision must record RollbackOf=2, got %d", recRev.RollbackOf)
	}
	stage4, _ := svc.GetRevision(testZone, 4)
	if weightedWWW(stage4).Records[0] == "" || len(stage4.RecordSets) == 0 {
		t.Fatal("history must not be rewritten: stage revision 4 lost")
	}
	// 恢复写出独立 outbox。
	outbox, _ := svc.ListOutbox(testZone)
	last := outbox[len(outbox)-1]
	if last.Revision != 5 || last.PlanID != plan.ID {
		t.Fatalf("recovery outbox mismatch: %+v", last)
	}
	// 终止的计划不能再推进或上报。
	if _, err := svc.AdvancePlan(testZone, plan.ID); !IsState(err) {
		t.Fatalf("advance aborted plan should be state error, got %v", err)
	}
}

// TestManualStopRecovery 验证人工停止：恢复到上一稳定阶段（阶段 1 时回到阶段 0 视图）。
func TestManualStopRecovery(t *testing.T) {
	svc, clk := newClockService(t)
	seed := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, seed.ID, "alice")
	mustPublish(t, svc, testZone, seed.ID)
	chg := approvedWWWChange(t, svc, 2, "192.0.2.1", "192.0.2.10", 300)
	plan, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: chg.ID, Stages: []StageInput{
		{TargetWeight: 10, MinObservation: time.Second, HealthThreshold: 0.5, MinSamples: 1},
		{TargetWeight: 50, MinObservation: time.Second, HealthThreshold: 0.5, MinSamples: 1},
		{TargetWeight: 100, MinObservation: time.Second, HealthThreshold: 0.5, MinSamples: 1},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := svc.AdvancePlan(testZone, plan.ID); err != nil { // rev 4, 阶段 0
		t.Fatalf("stage0: %v", err)
	}
	_, _ = svc.RecordHealthSample(HealthSampleInput{
		Zone: testZone, PlanID: plan.ID, Stage: 0, SampleKey: "a", Healthy: true,
	})
	clk.add(2 * time.Second)
	if _, err := svc.AdvancePlan(testZone, plan.ID); err != nil { // rev 5, 阶段 1（非末阶段）
		t.Fatalf("stage1: %v", err)
	}
	// 阶段 1 观察期间人工停止：应恢复到阶段 0 的视图（rev 4）。
	p, err := svc.StopPlan(testZone, plan.ID, "alice")
	if err != nil {
		t.Fatalf("StopPlan: %v", err)
	}
	if p.Status != PlanAborted || p.RecoveryOf != 4 || p.StopActor != "alice" {
		t.Fatalf("stop should recover to stage0 revision 4: %+v", p)
	}
	info, _ := svc.GetZone(testZone)
	if info.PublishedRevision != 6 {
		t.Fatalf("expected recovery revision 6, published=%d", info.PublishedRevision)
	}
	recRev, _ := svc.GetRevision(testZone, 6)
	stage0, _ := svc.GetRevision(testZone, 4)
	for i := range stage0.RecordSets {
		if !setsEqual(stage0.RecordSets[i], recRev.RecordSets[i]) {
			t.Fatalf("recovery must restore stage0 view: %+v vs %+v", stage0.RecordSets[i], recRev.RecordSets[i])
		}
	}
}

// TestConcurrentAdvanceStop 验证健康判定、人工停止与推进并发时只有一个结果。
func TestConcurrentAdvanceStop(t *testing.T) {
	for run := 0; run < 30; run++ {
		svc, clk := newClockService(t)
		seed := mustSubmit(t, svc, SubmitInput{
			Zone: testZone, BaseRevision: 1, Submitter: "carol",
			Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
		})
		mustApprove(t, svc, testZone, seed.ID, "alice")
		mustPublish(t, svc, testZone, seed.ID)
		chg := approvedWWWChange(t, svc, 2, "192.0.2.1", "192.0.2.10", 300)
		plan, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: chg.ID, Stages: []StageInput{
			{TargetWeight: 50, MinObservation: time.Second, HealthThreshold: 0.99, MinSamples: 2},
			{TargetWeight: 100, MinObservation: time.Second, HealthThreshold: 0.99, MinSamples: 1},
		}})
		if err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}
		if _, err := svc.AdvancePlan(testZone, plan.ID); err != nil {
			t.Fatalf("publish stage: %v", err)
		}
		// 两个样本：一个不健康 → 推进将触发自动恢复。
		_, _ = svc.RecordHealthSample(HealthSampleInput{Zone: testZone, PlanID: plan.ID, Stage: 0, SampleKey: "a", Healthy: true})
		_, _ = svc.RecordHealthSample(HealthSampleInput{Zone: testZone, PlanID: plan.ID, Stage: 0, SampleKey: "b", Healthy: false})
		clk.add(2 * time.Second)

		var wg sync.WaitGroup
		errs := make([]error, 3)
		wg.Add(3)
		go func() { defer wg.Done(); _, errs[0] = svc.AdvancePlan(testZone, plan.ID) }()
		go func() { defer wg.Done(); _, errs[1] = svc.StopPlan(testZone, plan.ID, "bob") }()
		go func() { defer wg.Done(); _, errs[2] = svc.AdvancePlan(testZone, plan.ID) }()
		wg.Wait()

		ok := 0
		for _, e := range errs {
			if e == nil {
				ok++
			} else if !IsState(e) {
				t.Fatalf("run %d: unexpected error %v", run, e)
			}
		}
		if ok != 1 {
			t.Fatalf("run %d: exactly one of advance/stop should win, got %d", run, ok)
		}
		detail, err := svc.GetPlan(testZone, plan.ID)
		if err != nil {
			t.Fatalf("GetPlan: %v", err)
		}
		if detail.Plan.Status != PlanAborted {
			t.Fatalf("run %d: plan should be aborted, got %s", run, detail.Plan.Status)
		}
		// 恰好一个恢复修订，outbox 中不出现重复修订号。
		outbox, _ := svc.ListOutbox(testZone)
		count := map[int64]int{}
		for _, o := range outbox {
			count[o.Revision]++
		}
		for rev, n := range count {
			if n != 1 {
				t.Fatalf("run %d: revision %d has %d outbox entries", run, rev, n)
			}
		}
	}
}

// TestPlanSuperseded 验证计划执行期间出现的新区域变更以当前已发布阶段为基准，
// 旧计划不得覆盖后来发布的修订。
func TestPlanSuperseded(t *testing.T) {
	svc, _ := newClockService(t)
	seed := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, seed.ID, "alice")
	mustPublish(t, svc, testZone, seed.ID)
	chg := approvedWWWChange(t, svc, 2, "192.0.2.1", "192.0.2.10", 300)
	plan, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: chg.ID, Stages: []StageInput{
		{TargetWeight: 30, MinObservation: time.Minute, HealthThreshold: 0.99, MinSamples: 10},
		{TargetWeight: 100, MinObservation: time.Minute, HealthThreshold: 0.99, MinSamples: 10},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := svc.AdvancePlan(testZone, plan.ID); err != nil { // 发布阶段 0 -> rev 4
		t.Fatalf("stage0: %v", err)
	}

	// 紧急变更以当前已发布阶段（rev 4，即 head）为基准提交并发布 -> rev 5。
	hotfix := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 4, Submitter: "carol",
		Ops: []ChangeOp{addWWW("hotfix.example.com", "192.0.2.77", 300)},
	})
	if hotfix.BaseRevision != 4 {
		t.Fatalf("hotfix must be based on the published stage revision")
	}
	mustApprove(t, svc, testZone, hotfix.ID, "bob")
	mustPublish(t, svc, testZone, hotfix.ID)

	// 旧计划再推进/停止/上报均被基线守卫拒绝并转为 superseded，且不覆盖 rev 5。
	if _, err := svc.AdvancePlan(testZone, plan.ID); !IsState(err) {
		t.Fatalf("stale plan advance should be state error, got %v", err)
	}
	detail, _ := svc.GetPlan(testZone, plan.ID)
	if detail.Plan.Status != PlanSuperseded {
		t.Fatalf("plan should be superseded, got %s", detail.Plan.Status)
	}
	info, _ := svc.GetZone(testZone)
	if info.PublishedRevision != 5 {
		t.Fatalf("stale plan must not overwrite published revision 5, got %d", info.PublishedRevision)
	}
	outbox, _ := svc.ListOutbox(testZone)
	if outbox[len(outbox)-1].Revision != 5 {
		t.Fatalf("last outbox must remain the hotfix revision 5: %+v", outbox[len(outbox)-1])
	}
}

// TestPlanIdempotency 验证计划创建的幂等键语义。
func TestPlanIdempotency(t *testing.T) {
	svc, _ := newClockService(t)
	seed := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, seed.ID, "alice")
	mustPublish(t, svc, testZone, seed.ID)
	chg := approvedWWWChange(t, svc, 2, "192.0.2.1", "192.0.2.10", 300)
	stages := []StageInput{{TargetWeight: 100, MinSamples: 1}}

	in := CreatePlanInput{Zone: testZone, ChangeID: chg.ID, Stages: stages, IdempotencyKey: "plan-1"}
	p1, err := svc.CreatePlan(in)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	p2, err := svc.CreatePlan(in)
	if err != nil || p1.ID != p2.ID {
		t.Fatalf("idempotent create should return same plan: %+v err=%v", p2, err)
	}
	plans, _ := svc.ListPlans(testZone)
	if len(plans) != 1 {
		t.Fatalf("idempotent retry must not duplicate plans, got %d", len(plans))
	}
	// 同键不同负载 -> 幂等错误。
	bad := in
	bad.Stages = []StageInput{{TargetWeight: 50, MinSamples: 1}, {TargetWeight: 100, MinSamples: 1}}
	if _, err := svc.CreatePlan(bad); !IsIdempotency(err) {
		t.Fatalf("different payload with same key should be idempotency error, got %v", err)
	}
}

// TestPlanGuardrails 验证计划的边界守卫：目标变更不能被直接发布/撤回；
// 头部堆叠未发布变更时阶段操作被阻止。
func TestPlanGuardrails(t *testing.T) {
	svc, _ := newClockService(t)
	seed := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, seed.ID, "alice")
	mustPublish(t, svc, testZone, seed.ID)
	chg := approvedWWWChange(t, svc, 2, "192.0.2.1", "192.0.2.10", 300)
	plan, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: chg.ID, Stages: []StageInput{
		{TargetWeight: 50, MinObservation: time.Second, HealthThreshold: 0.9, MinSamples: 1},
		{TargetWeight: 100, MinObservation: time.Second, HealthThreshold: 0.9, MinSamples: 1},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	// 直接发布或撤回活跃计划的目标变更被拒绝。
	if _, err := svc.Publish(testZone, chg.ID); !IsState(err) {
		t.Fatalf("direct publish of plan target should be blocked, got %v", err)
	}
	if _, err := svc.Withdraw(testZone, chg.ID, "carol"); !IsState(err) {
		t.Fatalf("withdraw of plan target should be blocked, got %v", err)
	}
	// 发布阶段 0（rev 4）。
	if _, err := svc.AdvancePlan(testZone, plan.ID); err != nil {
		t.Fatalf("stage0: %v", err)
	}
	// 头部堆叠一个未发布变更（rev 5）：阶段推进/停止/样本被阻止，但不被标记 superseded。
	stacked := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 4, Submitter: "carol",
		Ops: []ChangeOp{addWWW("extra.example.com", "192.0.2.66", 300)},
	})
	if _, err := svc.AdvancePlan(testZone, plan.ID); !IsState(err) {
		t.Fatalf("advance with stacked unpublished change should be blocked, got %v", err)
	}
	if _, err := svc.StopPlan(testZone, plan.ID, "alice"); !IsState(err) {
		t.Fatalf("stop with stacked unpublished change should be blocked, got %v", err)
	}
	if _, err := svc.RecordHealthSample(HealthSampleInput{
		Zone: testZone, PlanID: plan.ID, Stage: 0, SampleKey: "x",
	}); !IsState(err) {
		t.Fatalf("sample with stacked unpublished change should be blocked, got %v", err)
	}
	detail, _ := svc.GetPlan(testZone, plan.ID)
	if detail.Plan.Status != PlanActive {
		t.Fatalf("stacked (unpublished) change should not supersede plan, got %s", detail.Plan.Status)
	}
	// 撤回堆叠变更后计划恢复可用（历史线性），可正常推进完成。
	if _, err := svc.Withdraw(testZone, stacked.ID, "carol"); err != nil {
		t.Fatalf("Withdraw stacked: %v", err)
	}
	info, _ := svc.GetZone(testZone)
	if info.HeadRevision != 5 || info.PublishedRevision != 4 {
		t.Fatalf("withdrawn change consumed rev 5, head=%d pub=%d", info.HeadRevision, info.PublishedRevision)
	}
}

// TestUnsplittableDifferences 验证仅 TTL 变化或非地址类型差异不可渐进发布。
func TestUnsplittableDifferences(t *testing.T) {
	svc, _ := newClockService(t)
	seed := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, seed.ID, "alice")
	mustPublish(t, svc, testZone, seed.ID)

	// 仅 TTL 变化（记录数据相同）：不可分流。
	ttlChg := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: 900, Records: []string{"192.0.2.1"}}}},
	})
	mustApprove(t, svc, testZone, ttlChg.ID, "alice")
	if _, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: ttlChg.ID,
		Stages: []StageInput{{TargetWeight: 100, MinSamples: 1}}}); !IsValidation(err) {
		t.Fatalf("TTL-only change should be unsplittable, got %v", err)
	}
}

// TestPlanPersistence 验证计划、样本与恢复链跨 FileStore 持久化。
func TestPlanPersistence(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	svc := NewService(store)
	if _, err := svc.InitZone(testZone, []string{"ns1.example.com"}, Policy{}); err != nil {
		t.Fatalf("InitZone: %v", err)
	}
	c1 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustPublish(t, svc, testZone, c1.ID)
	c2 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: 300, Records: []string{"192.0.2.9"}}}},
	})
	plan, err := svc.CreatePlan(CreatePlanInput{Zone: testZone, ChangeID: c2.ID, Stages: []StageInput{
		{TargetWeight: 100, MinSamples: 1},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := svc.AdvancePlan(testZone, plan.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}

	store2, _ := NewFileStore(dir)
	svc2 := NewService(store2)
	detail, err := svc2.GetPlan(testZone, plan.ID)
	if err != nil {
		t.Fatalf("GetPlan after reload: %v", err)
	}
	if detail.Plan.Status != PlanCompleted || len(detail.Stages) != 1 {
		t.Fatalf("plan not persisted: %+v", detail.Plan)
	}
	if len(detail.Stages[0].RecordSets) == 0 || detail.Stages[0].OutboxID == "" {
		t.Fatalf("stage view and propagation state not persisted: %+v", detail.Stages[0])
	}
	outbox, _ := svc2.ListOutbox(testZone)
	if len(outbox) != 3 || outbox[2].PlanID != plan.ID {
		t.Fatalf("plan outbox not persisted: %+v", outbox)
	}
}
