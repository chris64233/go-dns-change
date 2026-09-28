package dnschange

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// setClock 注入服务时钟，便于测试最短观察时间。
func setClock(svc *Service, t0 time.Time) *time.Time {
	now := t0
	svc.now = func() time.Time { return now }
	return &now
}

// setupSplittableChange 构造一个可分流的已审批变更：
// 线上 www -> [192.0.2.1]（修订 2），目标修订把 www 扩成 [192.0.2.1, 192.0.2.10]（修订 3）。
func setupSplittableChange(t *testing.T, svc *Service) *Change {
	t.Helper()
	c1 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, c1.ID, "alice")
	mustPublish(t, svc, testZone, c1.ID)

	c2 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: 300,
			Records: []string{"192.0.2.1", "192.0.2.10"},
		}}},
	})
	mustApprove(t, svc, testZone, c2.ID, "alice")
	return c2
}

func testStages() []StageSpec {
	return []StageSpec{
		{TargetWeight: 20, MinObservation: time.Minute, HealthThreshold: 0.8, MinSampleCount: 2},
		{TargetWeight: 50, MinObservation: time.Minute, HealthThreshold: 0.8, MinSampleCount: 2},
		{TargetWeight: 100, MinObservation: time.Minute, HealthThreshold: 0.9, MinSampleCount: 2},
	}
}

func weightOf(rs RecordSet, rdata string) int {
	for _, w := range rs.Weights {
		if w.Record == rdata {
			return w.Weight
		}
	}
	return -1
}

func findRS(sets []RecordSet, key string) *RecordSet {
	for i := range sets {
		if sets[i].Key() == key {
			return &sets[i]
		}
	}
	return nil
}

// recordSamples 向指定阶段修订记录若干健康样本。
func recordSamples(t *testing.T, svc *Service, rev int64, stage int, prefix string, healthy ...bool) {
	t.Helper()
	for i, h := range healthy {
		_, err := svc.RecordHealthSample(HealthSample{
			SampleID: fmt.Sprintf("%s-%d-%d", prefix, rev, i),
			Zone:     testZone, Revision: rev, Stage: stage, Healthy: h,
		})
		if err != nil {
			t.Fatalf("RecordHealthSample: %v", err)
		}
	}
}

// TestProgressiveFullFlow 端到端验证三阶段灰度：阶段视图、唯一 outbox、
// 健康门槛、观察时间、修订号连续、计划完成。
func TestProgressiveFullFlow(t *testing.T) {
	svc := newTestService(t)
	clock := setClock(svc, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	target := setupSplittableChange(t, svc)

	plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if plan.TargetRevision != 3 || plan.BaseRevision != 2 || plan.Status != PlanActive ||
		plan.StableRevision != 2 || plan.CurrentStage != 0 {
		t.Fatalf("unexpected plan: %+v", plan)
	}

	// 首阶段发布：权重 20 给新值，80 留给旧值。
	r0, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil {
		t.Fatalf("AdvanceStage(0): %v", err)
	}
	if r0.Outcome != OutcomeStagePublished || r0.Revision != 4 || r0.Stage != 0 {
		t.Fatalf("unexpected first advance: %+v", r0)
	}
	rev4, _ := svc.GetRevision(testZone, 4)
	rs := findRS(rev4.RecordSets, "www.example.com.|A")
	if rs == nil || weightOf(*rs, "192.0.2.1") != 80 || weightOf(*rs, "192.0.2.10") != 20 {
		t.Fatalf("stage0 weights wrong: %+v", rs)
	}
	if got := targetStatus(t, svc, target.ID); got != StatusRolling {
		t.Fatalf("target change should be rolling, got %s", got)
	}

	outbox, _ := svc.ListOutbox(testZone)
	if len(outbox) != 3 || outbox[2].Revision != 4 || outbox[2].PlanID != plan.ID ||
		outbox[2].StageIndex != 0 || outbox[2].ID == "" {
		t.Fatalf("stage0 outbox wrong: %+v", outbox)
	}

	// 观察时间未到：不能推进，且不消耗修订号。
	if _, err := svc.AdvanceStage(testZone, plan.ID); !IsState(err) {
		t.Fatalf("advance within observation window should be a state error, got %v", err)
	}
	info, _ := svc.GetZone(testZone)
	if info.HeadRevision != 4 {
		t.Fatalf("failed advance must not consume a revision, head=%d", info.HeadRevision)
	}

	// 样本不足：仍不能推进。
	*clock = clock.Add(2 * time.Minute)
	recordSamples(t, svc, 4, 0, "s0", true)
	if _, err := svc.AdvanceStage(testZone, plan.ID); !IsState(err) {
		t.Fatalf("advance with too few samples should be a state error, got %v", err)
	}

	// 健康达标后推进到第二阶段（权重 50/50）。
	recordSamples(t, svc, 4, 0, "s0more", true)
	r1, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil {
		t.Fatalf("AdvanceStage(1): %v", err)
	}
	if r1.Outcome != OutcomeStagePublished || r1.Revision != 5 {
		t.Fatalf("unexpected second advance: %+v", r1)
	}
	rev5, _ := svc.GetRevision(testZone, 5)
	rs = findRS(rev5.RecordSets, "www.example.com.|A")
	if weightOf(*rs, "192.0.2.1") != 50 || weightOf(*rs, "192.0.2.10") != 50 {
		t.Fatalf("stage1 weights wrong: %+v", rs)
	}

	// 末阶段：健康达标后完成，线上视图等于目标修订（无权重）。
	*clock = clock.Add(2 * time.Minute)
	recordSamples(t, svc, 5, 1, "s1", true, true)
	r2, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil {
		t.Fatalf("AdvanceStage(2 publish): %v", err)
	}
	if r2.Outcome != OutcomeStagePublished || r2.Revision != 6 {
		t.Fatalf("unexpected third advance: %+v", r2)
	}
	rev6, _ := svc.GetRevision(testZone, 6)
	rs = findRS(rev6.RecordSets, "www.example.com.|A")
	if rs == nil || len(rs.Weights) != 0 || len(rs.Records) != 2 {
		t.Fatalf("final stage view should equal target (no weights): %+v", rs)
	}

	*clock = clock.Add(2 * time.Minute)
	recordSamples(t, svc, 6, 2, "s2", true, true)
	done, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil {
		t.Fatalf("AdvanceStage(complete): %v", err)
	}
	if done.Outcome != OutcomeCompleted {
		t.Fatalf("expected completed, got %+v", done)
	}
	final, _ := svc.GetPlan(testZone, plan.ID)
	if final.Plan.Status != PlanCompleted || final.Plan.CurrentStage != 3 {
		t.Fatalf("plan not completed: %+v", final.Plan)
	}
	if got := targetStatus(t, svc, target.ID); got != StatusPublished {
		t.Fatalf("target change should be published after completion, got %s", got)
	}
	info, _ = svc.GetZone(testZone)
	if info.HeadRevision != 6 || info.PublishedRevision != 6 {
		t.Fatalf("revisions should be contiguous 4,5,6, got head=%d published=%d",
			info.HeadRevision, info.PublishedRevision)
	}
	outbox, _ = svc.ListOutbox(testZone)
	if len(outbox) != 5 { // bootstrap + 普通发布 + 3 个阶段
		t.Fatalf("each advance must write exactly one outbox entry, got %d", len(outbox))
	}
}

// TestProgressivePlanValidation 覆盖计划创建的各类非法输入。
func TestProgressivePlanValidation(t *testing.T) {
	svc := newTestService(t)
	target := setupSplittableChange(t, svc)

	bad := [][]StageSpec{
		{},
		{{TargetWeight: 50, MinObservation: 0, HealthThreshold: 0.8, MinSampleCount: 1}}, // 末阶段必须 100
		{{TargetWeight: 100, MinObservation: 0, HealthThreshold: 0.8, MinSampleCount: 1},
			{TargetWeight: 100, MinObservation: 0, HealthThreshold: 0.8, MinSampleCount: 1}}, // 必须严格递增
		{{TargetWeight: 80, MinObservation: 0, HealthThreshold: 0.8, MinSampleCount: 1},
			{TargetWeight: 20, MinObservation: 0, HealthThreshold: 0.8, MinSampleCount: 1},
			{TargetWeight: 100, MinObservation: 0, HealthThreshold: 0.8, MinSampleCount: 1}}, // 倒退
		{{TargetWeight: 20, MinObservation: 0, HealthThreshold: 1.5, MinSampleCount: 1},
			{TargetWeight: 100, MinObservation: 0, HealthThreshold: 1.5, MinSampleCount: 1}}, // 门槛越界
		{{TargetWeight: 20, MinObservation: 0, HealthThreshold: 0.8, MinSampleCount: 0},
			{TargetWeight: 100, MinObservation: 0, HealthThreshold: 0.8, MinSampleCount: 1}}, // 样本数为 0
	}
	for i, stages := range bad {
		if _, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: stages}); !IsValidation(err) {
			t.Fatalf("case %d: expected validation error, got %v", i, err)
		}
	}

	// 未审批 / 不存在的变更不能建计划。
	c := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 3, Submitter: "dave",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: 300,
			Records: []string{"192.0.2.1", "192.0.2.11", "192.0.2.12"}}}},
	})
	if _, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: c.ID, Stages: testStages()}); !IsState(err) {
		t.Fatalf("pending change should give a state error, got %v", err)
	}
	if _, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: "nope", Stages: testStages()}); !IsNotFound(err) {
		t.Fatalf("missing change should give a not-found error, got %v", err)
	}

	// 无可分流记录（仅 TTL 变化）不能建计划。需要独立环境：上面被撤回的变更仍占用修订号。
	svcTTL := newTestService(t)
	cTTL := mustSubmit(t, svcTTL, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{
			addWWW("www.example.com", "192.0.2.1", 300),
		},
	})
	mustApprove(t, svcTTL, testZone, cTTL.ID, "alice")
	mustPublish(t, svcTTL, testZone, cTTL.ID)
	cTTL2 := mustSubmit(t, svcTTL, SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: 600, Records: []string{"192.0.2.1"}}}},
	})
	mustApprove(t, svcTTL, testZone, cTTL2.ID, "alice")
	if _, err := svcTTL.CreatePlan(PlanInput{Zone: testZone, ChangeID: cTTL2.ID, Stages: testStages()}); !IsValidation(err) {
		t.Fatalf("change without splittable sets should give a validation error, got %v", err)
	}

	// 计划创建后不可修改：没有更新入口，且不能为同一区域创建第二个活跃计划。
	// 先撤回上面占用修订 4 的待审批变更，使目标变更 #3 重新成为可规划对象。
	if _, err := svc.Withdraw(testZone, c.ID, "dave"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()}); !IsState(err) {
		t.Fatalf("second active plan should give a state error, got %v", err)
	}
	// 计划创建后外部无法改 Stages：直接改切片不影响服务内状态。
	plan.Stages[0].Spec.TargetWeight = 99
	detail, err := svc.GetPlan(testZone, plan.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if detail.Plan.Stages[0].Spec.TargetWeight != 20 {
		t.Fatal("plan must be immutable after creation")
	}
}

// TestHealthSampleBindingAndIdempotency 验证样本绑定与幂等：
// 旧阶段/其他修订的样本不参与判定，相同样本重放幂等。
func TestHealthSampleBindingAndIdempotency(t *testing.T) {
	svc := newTestService(t)
	clock := setClock(svc, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	target := setupSplittableChange(t, svc)
	plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	r0, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil {
		t.Fatalf("AdvanceStage: %v", err)
	}
	*clock = clock.Add(2 * time.Minute)

	// 重放同一幂等样本：只算一条。
	s1 := HealthSample{SampleID: "dup", Zone: testZone, Revision: r0.Revision, Stage: 0, Healthy: true}
	if _, err := svc.RecordHealthSample(s1); err != nil {
		t.Fatalf("RecordHealthSample: %v", err)
	}
	if _, err := svc.RecordHealthSample(s1); err != nil {
		t.Fatalf("replaying the same sample should be idempotent, got %v", err)
	}
	// 同一样本 ID 携带不同内容 → 幂等错误。
	s1.Healthy = false
	if _, err := svc.RecordHealthSample(s1); !IsIdempotency(err) {
		t.Fatalf("same sample id with different content should be an idempotency error, got %v", err)
	}

	// 绑定到未来修订 / 错误阶段的样本：未来修订直接被拒绝；
	// 其他阶段/修订的样本可以留存但不能参与判定。
	if _, err := svc.RecordHealthSample(HealthSample{
		SampleID: "ghost", Zone: testZone, Revision: 99, Stage: 0, Healthy: true,
	}); !IsNotFound(err) {
		t.Fatalf("sample for unknown revision should be a not-found error, got %v", err)
	}
	recordSamples(t, svc, 2, 0, "oldrev", false, false, false) // 旧修订
	recordSamples(t, svc, r0.Revision, 7, "oldstage", false, false)

	detail, err := svc.GetPlan(testZone, plan.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if detail.Health.Total != 1 || detail.Health.HealthyCount != 1 {
		t.Fatalf("only the bound current-stage sample should count, got %+v", detail.Health)
	}

	// 补一条健康样本后即可推进。
	recordSamples(t, svc, r0.Revision, 0, "cur", true)
	r1, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil || r1.Outcome != OutcomeStagePublished {
		t.Fatalf("plan should advance with two healthy current-stage samples, got %+v err=%v", r1, err)
	}
}

// TestAutoRecoveryCreatesNewRevision 验证健康不达标时原子创建恢复修订，不改写历史。
func TestProgressiveAutoRecovery(t *testing.T) {
	svc := newTestService(t)
	clock := setClock(svc, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	target := setupSplittableChange(t, svc)
	plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	r0, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil {
		t.Fatalf("AdvanceStage: %v", err)
	}
	*clock = clock.Add(2 * time.Minute)
	// 3 条样本只有 1 条健康（0.33 < 0.8）。
	recordSamples(t, svc, r0.Revision, 0, "bad", true, false, false)

	res, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil {
		t.Fatalf("AdvanceStage recovery: %v", err)
	}
	if res.Outcome != OutcomeRecovered || res.Recovery == nil {
		t.Fatalf("expected recovery, got %+v", res)
	}
	info, _ := svc.GetZone(testZone)
	if info.HeadRevision != 5 || info.PublishedRevision != 5 {
		t.Fatalf("recovery must allocate the next contiguous revision, got %+v", info)
	}
	// 恢复修订内容等于上一稳定点（基线修订 2）。
	recRev, _ := svc.GetRevision(testZone, 5)
	baseRev, _ := svc.GetRevision(testZone, 2)
	if len(recRev.RecordSets) != len(baseRev.RecordSets) {
		t.Fatalf("recovery view should match stable revision: %v vs %v", recRev.RecordSets, baseRev.RecordSets)
	}
	for i := range baseRev.RecordSets {
		if !setsEqual(baseRev.RecordSets[i], recRev.RecordSets[i]) {
			t.Fatalf("recovery content mismatch: %+v vs %+v", baseRev.RecordSets[i], recRev.RecordSets[i])
		}
	}
	if recRev.RollbackOf != 2 || recRev.PlanID != plan.ID {
		t.Fatalf("recovery revision metadata wrong: %+v", recRev)
	}
	recChg, _ := svc.GetChange(testZone, res.Recovery.RecoveryChangeID)
	if recChg.Status != StatusPublished || recChg.Submitter != autoRecoverySubmitter {
		t.Fatalf("recovery change should be auto-published: %+v", recChg)
	}
	if got := targetStatus(t, svc, target.ID); got != StatusRecovered {
		t.Fatalf("target change should be recovered, got %s", got)
	}
	detail, _ := svc.GetPlan(testZone, plan.ID)
	if detail.Plan.Status != PlanRecovered || len(detail.RecoveryChain) != 1 ||
		detail.RecoveryChain[0].RecoveryRevision != 5 ||
		detail.Stages[0].Stage.RevertedToRevision != 5 {
		t.Fatalf("recovery state wrong: %+v chain=%+v", detail.Plan, detail.RecoveryChain)
	}
	// outbox 唯一：引导 + rev2 + 阶段 rev4 + 恢复 rev5。
	outbox, _ := svc.ListOutbox(testZone)
	if len(outbox) != 4 || outbox[3].Revision != 5 {
		t.Fatalf("recovery must write exactly one outbox entry, got %+v", outbox)
	}
	// 历史未被改写：阶段修订 4 仍保留加权视图。
	rev4, _ := svc.GetRevision(testZone, 4)
	if rs := findRS(rev4.RecordSets, "www.example.com.|A"); rs == nil || len(rs.Weights) == 0 {
		t.Fatalf("stage revision history must survive recovery: %+v", rs)
	}
	// 恢复后不能再推进。
	if _, err := svc.AdvanceStage(testZone, plan.ID); !IsState(err) {
		t.Fatalf("recovered plan should not advance, got %v", err)
	}
}

// TestRecoveryToPreviousStableStage 验证第二阶段失败时恢复到第一阶段（带权重）的稳定视图。
func TestRecoveryToPreviousStableStage(t *testing.T) {
	svc := newTestService(t)
	clock := setClock(svc, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	target := setupSplittableChange(t, svc)
	plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	r0, _ := svc.AdvanceStage(testZone, plan.ID)
	*clock = clock.Add(2 * time.Minute)
	recordSamples(t, svc, r0.Revision, 0, "s0", true, true)
	r1, _ := svc.AdvanceStage(testZone, plan.ID) // 发布阶段 1（rev5）
	*clock = clock.Add(2 * time.Minute)
	recordSamples(t, svc, r1.Revision, 1, "s1", false, false)
	res, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil || res.Outcome != OutcomeRecovered {
		t.Fatalf("expected recovery at stage 1, got %+v err=%v", res, err)
	}
	// 恢复修订（rev6）内容必须精确等于阶段 0 稳定视图（rev4，80/20 权重）。
	recRev, _ := svc.GetRevision(testZone, res.Recovery.RecoveryRevision)
	stableRev, _ := svc.GetRevision(testZone, 4)
	if len(recRev.RecordSets) != len(stableRev.RecordSets) {
		t.Fatalf("recovery should match stage0 view: %v vs %v", recRev.RecordSets, stableRev.RecordSets)
	}
	for i := range stableRev.RecordSets {
		if !setsEqual(stableRev.RecordSets[i], recRev.RecordSets[i]) {
			t.Fatalf("recovery to weighted stable view mismatch: %+v vs %+v",
				stableRev.RecordSets[i], recRev.RecordSets[i])
		}
	}
	if res.Recovery.StableRevision != 4 {
		t.Fatalf("stable revision should be 4, got %+v", res.Recovery)
	}
}

// TestStopRollout 验证人工停止：并发时与推进只有一个结果，停止后流量停在当前阶段。
func TestStopRollout(t *testing.T) {
	svc := newTestService(t)
	target := setupSplittableChange(t, svc)
	plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if _, err := svc.AdvanceStage(testZone, plan.ID); err != nil {
		t.Fatalf("AdvanceStage: %v", err)
	}
	stopped, err := svc.StopRollout(testZone, plan.ID, "alice")
	if err != nil || stopped.Status != PlanStopped {
		t.Fatalf("StopRollout: %+v %v", stopped, err)
	}
	if got := targetStatus(t, svc, target.ID); got != StatusStopped {
		t.Fatalf("target change should be stopped, got %s", got)
	}
	// 重复停止幂等；停止后不能推进；完成后的计划不能停止。
	if _, err := svc.StopRollout(testZone, plan.ID, "alice"); err != nil {
		t.Fatalf("re-stop should be idempotent, got %v", err)
	}
	if _, err := svc.AdvanceStage(testZone, plan.ID); !IsState(err) {
		t.Fatalf("stopped plan should not advance, got %v", err)
	}
	// 流量停在阶段视图（rev4 仍是线上）。
	info, _ := svc.GetZone(testZone)
	if info.PublishedRevision != 4 {
		t.Fatalf("traffic should stay at stage0 revision 4, got %d", info.PublishedRevision)
	}
	// 不能绕过计划直接发布/撤销目标变更。
	if _, err := svc.Publish(testZone, target.ID); !IsState(err) {
		t.Fatalf("direct publish of rolling/stopped change should fail, got %v", err)
	}
}

// TestConcurrentAdvanceExclusive 验证并发推进只有一个结果：恰好一次阶段发布。
func TestConcurrentAdvanceExclusive(t *testing.T) {
	for run := 0; run < 20; run++ {
		svc := newTestService(t)
		clock := setClock(svc, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		target := setupSplittableChange(t, svc)
		plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
		if err != nil {
			t.Fatalf("CreatePlan: %v", err)
		}

		const n = 12
		var wg sync.WaitGroup
		results := make([]*AdvanceResult, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = svc.AdvanceStage(testZone, plan.ID)
			}(i)
		}
		wg.Wait()

		published := 0
		for i := 0; i < n; i++ {
			if errs[i] == nil {
				if results[i].Outcome != OutcomeStagePublished {
					t.Fatalf("run %d: unexpected outcome %+v", run, results[i])
				}
				published++
			} else if !IsState(errs[i]) {
				t.Fatalf("run %d: unexpected error %v", run, errs[i])
			}
		}
		if published != 1 {
			t.Fatalf("run %d: exactly one concurrent advance should publish, got %d", run, published)
		}
		outbox, _ := svc.ListOutbox(testZone)
		var stageEntries int
		for _, e := range outbox {
			if e.PlanID == plan.ID {
				stageEntries++
			}
		}
		if stageEntries != 1 {
			t.Fatalf("run %d: exactly one stage outbox entry expected, got %d", run, stageEntries)
		}
		_ = clock
	}
}

// TestConcurrentStopAndAdvance 验证健康判定与人工停止并发时串行化为确定结果：
// 要么停止先生效（推进失败，停在阶段 0），要么推进先生效、随后停止在新阶段 1。
func TestConcurrentStopAndAdvance(t *testing.T) {
	for run := 0; run < 30; run++ {
		svc := newTestService(t)
		clock := setClock(svc, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		target := setupSplittableChange(t, svc)
		plan, _ := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
		r0, _ := svc.AdvanceStage(testZone, plan.ID)
		*clock = clock.Add(2 * time.Minute)
		recordSamples(t, svc, r0.Revision, 0, "s", true, true)

		var wg sync.WaitGroup
		var advanceErr, stopErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, advanceErr = svc.AdvanceStage(testZone, plan.ID) }()
		go func() { defer wg.Done(); _, stopErr = svc.StopRollout(testZone, plan.ID, "alice") }()
		wg.Wait()

		// 停止要么先于推进（推进报状态错误），要么晚于推进（停在新阶段）：二者都合法，
		// 但停止本身永远成功，且终态必须与某个串行顺序一致。
		if stopErr != nil {
			t.Fatalf("run %d: stop should never fail against an active plan: %v", run, stopErr)
		}
		detail, _ := svc.GetPlan(testZone, plan.ID)
		info, _ := svc.GetZone(testZone)
		switch {
		case advanceErr != nil:
			if !IsState(advanceErr) {
				t.Fatalf("run %d: loser advance should give a state error, got %v", run, advanceErr)
			}
			// 停止先生效：停在阶段 0 修订 4。
			if detail.Plan.Status != PlanStopped || info.PublishedRevision != 4 {
				t.Fatalf("run %d: stop-first state wrong: status=%s published=%d",
					run, detail.Plan.Status, info.PublishedRevision)
			}
		default:
			// 推进先生效（发布修订 5），停止随后停在阶段 1：不能停在半成品上。
			if detail.Plan.Status != PlanStopped || info.PublishedRevision != 5 {
				t.Fatalf("run %d: advance-then-stop state wrong: status=%s published=%d",
					run, detail.Plan.Status, info.PublishedRevision)
			}
			if detail.Plan.CurrentStage != 1 || !detail.Plan.Stages[1].Published {
				t.Fatalf("run %d: stop after advance must land on a fully published stage: %+v",
					run, detail.Plan)
			}
		}
	}
}

// TestPlanSupersededByNewChange 验证计划执行期间新变更以已发布阶段为基准，
// 一旦发布旧计划即失效且不能覆盖后来的修订。
func TestPlanSupersededByNewChange(t *testing.T) {
	svc := newTestService(t)
	target := setupSplittableChange(t, svc)
	plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	// 首阶段发布前：头部是尚未上线的目标修订，此时禁止叠加新变更。
	if _, err := svc.SubmitChange(SubmitInput{
		Zone: testZone, BaseRevision: 3, Submitter: "dave",
		Ops: []ChangeOp{addWWW("mail.example.com", "192.0.2.99", 300)},
	}); !IsState(err) {
		t.Fatalf("submitting before first stage should be a state error, got %v", err)
	}

	r0, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil {
		t.Fatalf("AdvanceStage: %v", err)
	}
	// 阶段发布后可以以当前已发布阶段为基准提交新变更。
	fix := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: r0.Revision, Submitter: "dave",
		Ops: []ChangeOp{addWWW("mail.example.com", "192.0.2.99", 300)},
	})
	mustApprove(t, svc, testZone, fix.ID, "alice")
	mustPublish(t, svc, testZone, fix.ID)

	detail, _ := svc.GetPlan(testZone, plan.ID)
	if !detail.Plan.Superseded || detail.Plan.Status != PlanStopped {
		t.Fatalf("old plan should be superseded after a newer publish: %+v", detail.Plan)
	}
	if _, err := svc.AdvanceStage(testZone, plan.ID); !IsState(err) {
		t.Fatalf("superseded plan should not advance, got %v", err)
	}
	info, _ := svc.GetZone(testZone)
	if info.PublishedRevision != fix.Revision {
		t.Fatalf("later revision must stay live: published=%d want %d", info.PublishedRevision, fix.Revision)
	}
}

// TestNewChangeWithdrawnLetsPlanContinue 验证新变更被撤销后计划可以继续推进。
func TestNewChangeWithdrawnLetsPlanContinue(t *testing.T) {
	svc := newTestService(t)
	clock := setClock(svc, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	target := setupSplittableChange(t, svc)
	plan, _ := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
	r0, _ := svc.AdvanceStage(testZone, plan.ID)
	fix := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: r0.Revision, Submitter: "dave",
		Ops: []ChangeOp{addWWW("mail.example.com", "192.0.2.99", 300)},
	})
	// 新变更未落定时计划被挡住。
	if _, err := svc.AdvanceStage(testZone, plan.ID); !IsState(err) {
		t.Fatalf("advance with a pending newer change should fail, got %v", err)
	}
	if _, err := svc.Withdraw(testZone, fix.ID, "dave"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	*clock = clock.Add(2 * time.Minute)
	recordSamples(t, svc, r0.Revision, 0, "s0", true, true)
	r1, err := svc.AdvanceStage(testZone, plan.ID)
	if err != nil || r1.Outcome != OutcomeStagePublished {
		t.Fatalf("plan should resume after the newer change is withdrawn, got %+v err=%v", r1, err)
	}
}

// TestPlanDetailShowsViewsHealthAndPropagation 验证查询展示各阶段实际区域内容、
// 健康依据与传播状态。
func TestPlanDetailShowsViewsHealthAndPropagation(t *testing.T) {
	svc := newTestService(t)
	clock := setClock(svc, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	target := setupSplittableChange(t, svc)
	plan, _ := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})

	detail, err := svc.GetPlan(testZone, plan.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if len(detail.Stages) != 3 || detail.Stages[0].StableWeight != 0 ||
		detail.Stages[1].StableWeight != 20 || detail.Stages[2].StableWeight != 50 ||
		detail.Stages[0].RecordSets != nil {
		t.Fatalf("plan detail before any advance wrong: %+v", detail.Stages)
	}

	r0, _ := svc.AdvanceStage(testZone, plan.ID)
	*clock = clock.Add(30 * time.Second)
	recordSamples(t, svc, r0.Revision, 0, "s0", true, false)
	detail, _ = svc.GetPlan(testZone, plan.ID)
	rs := findRS(detail.Stages[0].RecordSets, "www.example.com.|A")
	if rs == nil || weightOf(*rs, "192.0.2.10") != 20 {
		t.Fatalf("stage0 actual content not exposed: %+v", rs)
	}
	if detail.Stages[0].Stage.OutboxID == "" || !detail.Stages[0].Stage.Published {
		t.Fatalf("stage propagation state missing: %+v", detail.Stages[0].Stage)
	}
	if detail.Health.Total != 2 || detail.Health.HealthyCount != 1 ||
		detail.Health.Ratio != 0.5 || detail.Health.Revision != r0.Revision {
		t.Fatalf("health basis wrong: %+v", detail.Health)
	}

	plans, _ := svc.ListPlans(testZone)
	if len(plans) != 1 || plans[0].ID != plan.ID {
		t.Fatalf("ListPlans: %+v", plans)
	}
}

// TestProgressivePersistsAcrossRestart 验证计划、阶段、样本随 FileStore 跨实例持久化。
func TestProgressivePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	svc := NewService(store)
	if _, err := svc.InitZone(testZone, []string{"ns1.example.com"}, Policy{RequiredApprovals: 1}); err != nil {
		t.Fatalf("InitZone: %v", err)
	}
	target := setupSplittableChange(t, svc)
	plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: []StageSpec{
		{TargetWeight: 50, MinObservation: 0, HealthThreshold: 1, MinSampleCount: 1},
		{TargetWeight: 100, MinObservation: 0, HealthThreshold: 1, MinSampleCount: 1},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	r0, _ := svc.AdvanceStage(testZone, plan.ID)
	if _, err := svc.RecordHealthSample(HealthSample{
		SampleID: "persisted", Zone: testZone, Revision: r0.Revision, Stage: 0, Healthy: true,
	}); err != nil {
		t.Fatalf("RecordHealthSample: %v", err)
	}

	store2, _ := NewFileStore(dir)
	svc2 := NewService(store2)
	detail, err := svc2.GetPlan(testZone, plan.ID)
	if err != nil {
		t.Fatalf("GetPlan after restart: %v", err)
	}
	if detail.Plan.Status != PlanActive || detail.Stages[0].Stage.Revision != r0.Revision ||
		detail.Health.Total != 1 {
		t.Fatalf("plan/sample state not persisted: %+v health=%+v", detail.Plan, detail.Health)
	}
	// 恢复后可以继续推进直至完成。
	r1, err := svc2.AdvanceStage(testZone, plan.ID)
	if err != nil || r1.Outcome != OutcomeStagePublished {
		t.Fatalf("resume after restart should publish stage1, got %+v err=%v", r1, err)
	}
	if _, err := svc2.RecordHealthSample(HealthSample{
		SampleID: "persisted2", Zone: testZone, Revision: r1.Revision, Stage: 1, Healthy: true,
	}); err != nil {
		t.Fatalf("RecordHealthSample: %v", err)
	}
	done, err := svc2.AdvanceStage(testZone, plan.ID)
	if err != nil || done.Outcome != OutcomeCompleted {
		t.Fatalf("plan should complete after restart, got %+v err=%v", done, err)
	}
}

func targetStatus(t *testing.T, svc *Service, id string) ChangeStatus {
	t.Helper()
	c, err := svc.GetChange(testZone, id)
	if err != nil {
		t.Fatalf("GetChange: %v", err)
	}
	return c.Status
}

// TestWeightDistributionMultiRecord 验证多条新增记录时权重尽量均分且总和精确，
// 并验证计划起点视图等于当前线上配置（权重 0）。
func TestWeightDistributionMultiRecord(t *testing.T) {
	svc := newTestService(t)
	c1 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{addWWW("www.example.com", "192.0.2.1", 300)},
	})
	mustApprove(t, svc, testZone, c1.ID, "alice")
	mustPublish(t, svc, testZone, c1.ID)
	// 新增两条记录：权重 33 在两条新值间均分（17+16），旧值得 67。
	c2 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: 300,
			Records: []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"},
		}}},
	})
	mustApprove(t, svc, testZone, c2.ID, "alice")
	plan, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: c2.ID, Stages: []StageSpec{
		{TargetWeight: 33, MinObservation: 0, HealthThreshold: 1, MinSampleCount: 1},
		{TargetWeight: 100, MinObservation: 0, HealthThreshold: 1, MinSampleCount: 1},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	r0, _ := svc.AdvanceStage(testZone, plan.ID)
	rev, _ := svc.GetRevision(testZone, r0.Revision)
	rs := findRS(rev.RecordSets, "www.example.com.|A")
	weights := map[string]int{}
	sum := 0
	for _, w := range rs.Weights {
		weights[w.Record] = w.Weight
		sum += w.Weight
	}
	if sum != 100 || weights["192.0.2.1"] != 67 ||
		weights["192.0.2.2"]+weights["192.0.2.3"] != 33 {
		t.Fatalf("weight split wrong: %+v sum=%d", weights, sum)
	}
	diff := weights["192.0.2.2"] - weights["192.0.2.3"]
	if diff < -1 || diff > 1 {
		t.Fatalf("new weights should be evenly distributed: %+v", weights)
	}
}

// TestRecoveryChainMultipleHops 验证恢复链可以跨多个计划串联。
func TestRecoveryChainMultipleHops(t *testing.T) {
	svc := newTestService(t)
	clock := setClock(svc, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	target := setupSplittableChange(t, svc) // rev3 目标；基线 rev2
	plan1, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: []StageSpec{
		{TargetWeight: 50, MinObservation: time.Minute, HealthThreshold: 0.9, MinSampleCount: 1},
		{TargetWeight: 100, MinObservation: time.Minute, HealthThreshold: 0.9, MinSampleCount: 1},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	r0, _ := svc.AdvanceStage(testZone, plan1.ID)
	*clock = clock.Add(2 * time.Minute)
	recordSamples(t, svc, r0.Revision, 0, "bad", false)
	rec1, err := svc.AdvanceStage(testZone, plan1.ID)
	if err != nil || rec1.Outcome != OutcomeRecovered {
		t.Fatalf("expected first recovery, got %+v err=%v", rec1, err)
	}
	// rev5 = 恢复到 rev2 的新修订。基于它提交一个新的可分流目标并建第二个计划。
	c3 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 5, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeA, TTL: 300,
			Records: []string{"192.0.2.1", "192.0.2.20"},
		}}},
	})
	mustApprove(t, svc, testZone, c3.ID, "alice")
	plan2, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: c3.ID, Stages: []StageSpec{
		{TargetWeight: 50, MinObservation: time.Minute, HealthThreshold: 0.9, MinSampleCount: 1},
		{TargetWeight: 100, MinObservation: time.Minute, HealthThreshold: 0.9, MinSampleCount: 1},
	}})
	if err != nil {
		t.Fatalf("CreatePlan(2): %v", err)
	}
	if plan2.BaseRevision != rec1.Recovery.RecoveryRevision {
		t.Fatalf("new plan must be based on the recovery revision, got %d", plan2.BaseRevision)
	}
	r2, _ := svc.AdvanceStage(testZone, plan2.ID)
	*clock = clock.Add(2 * time.Minute)
	recordSamples(t, svc, r2.Revision, 0, "bad2", false)
	rec2, err := svc.AdvanceStage(testZone, plan2.ID)
	if err != nil || rec2.Outcome != OutcomeRecovered {
		t.Fatalf("expected second recovery, got %+v err=%v", rec2, err)
	}
	detail, _ := svc.GetPlan(testZone, plan2.ID)
	if len(detail.RecoveryChain) != 1 || detail.RecoveryChain[0].RecoveryRevision != rec2.Recovery.RecoveryRevision {
		t.Fatalf("plan2 own recovery link wrong: %+v", detail.RecoveryChain)
	}
	// 计划 1 的恢复链沿「恢复修订 -> 以其为基线的下一计划」继续延伸，共两跳。
	d1, _ := svc.GetPlan(testZone, plan1.ID)
	if len(d1.RecoveryChain) != 2 ||
		d1.RecoveryChain[0].RecoveryRevision != 5 || d1.RecoveryChain[0].FromRevision != 4 ||
		d1.RecoveryChain[1].RecoveryRevision != 8 || d1.RecoveryChain[1].FromRevision != 7 {
		t.Fatalf("plan1 recovery chain should span both plans, got %+v", d1.RecoveryChain)
	}
	info, _ := svc.GetZone(testZone)
	// rev: 1 引导, 2 普通, 3 目标, 4 阶段, 5 恢复, 6 新目标, 7 阶段, 8 恢复
	if info.HeadRevision != 8 || info.PublishedRevision != 8 {
		t.Fatalf("recovery chain must keep contiguous revision history, got %+v", info)
	}
	outbox, _ := svc.ListOutbox(testZone)
	var recoveryEntries int
	for _, e := range outbox {
		if e.PlanID != "" && e.Revision == 5 || e.PlanID != "" && e.Revision == 8 {
			recoveryEntries++
		}
	}
	if recoveryEntries != 2 {
		t.Fatalf("each recovery must write one outbox entry, got %d of %+v", recoveryEntries, outbox)
	}
}

// TestPlanNotSplittableCNAME 验证 CNAME 替换不可分流，只能直接发布。
func TestPlanNotSplittableCNAME(t *testing.T) {
	svc := NewService(NewMemoryStore())
	if _, err := svc.InitZone(testZone, []string{"ns1.example.com"}, Policy{}); err != nil {
		t.Fatalf("InitZone: %v", err)
	}
	c1 := mustSubmit(t, svc, SubmitInput{
		Zone: testZone, BaseRevision: 1, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpAdd, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeCNAME, TTL: 300,
			Records: []string{"old.example.com."}}}},
	})
	mustPublish(t, svc, testZone, c1.ID)
	chg, err := svc.SubmitChange(SubmitInput{
		Zone: testZone, BaseRevision: 2, Submitter: "carol",
		Ops: []ChangeOp{{Kind: OpReplace, RecordSet: RecordSet{
			Name: "www.example.com", Type: TypeCNAME, TTL: 300,
			Records: []string{"new.example.com."}}}},
	})
	if err != nil {
		t.Fatalf("SubmitChange: %v", err)
	}
	if _, err := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: chg.ID, Stages: []StageSpec{
		{TargetWeight: 50, MinObservation: 0, HealthThreshold: 1, MinSampleCount: 1},
		{TargetWeight: 100, MinObservation: 0, HealthThreshold: 1, MinSampleCount: 1},
	}}); !IsValidation(err) {
		t.Fatalf("CNAME replacement must not be splittable, got %v", err)
	}
}

// TestRollingChangeCannotWithdrawOrPublish 验证灰度中的变更不能被直接发布或撤销。
func TestRollingChangeCannotWithdrawOrPublish(t *testing.T) {
	svc := newTestService(t)
	target := setupSplittableChange(t, svc)
	plan, _ := svc.CreatePlan(PlanInput{Zone: testZone, ChangeID: target.ID, Stages: testStages()})
	if _, err := svc.AdvanceStage(testZone, plan.ID); err != nil {
		t.Fatalf("AdvanceStage: %v", err)
	}
	if _, err := svc.Publish(testZone, target.ID); !IsState(err) {
		t.Fatalf("Publish during rollout should fail, got %v", err)
	}
	if _, err := svc.Withdraw(testZone, target.ID, "carol"); !IsState(err) {
		t.Fatalf("Withdraw during rollout should fail, got %v", err)
	}
	if _, err := svc.Approve(testZone, target.ID, "alice"); !IsState(err) {
		t.Fatalf("Approve during rollout should fail, got %v", err)
	}
}
