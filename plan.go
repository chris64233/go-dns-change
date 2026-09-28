package dnschange

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// planPayload 用于计划幂等键的负载哈希。
type planPayload struct {
	ChangeID string       `json:"change_id"`
	Stages   []StageInput `json:"stages"`
}

// CreatePlan 为一个已审批且包含可分流记录（A/AAAA 记录数据变化）的变更
// 创建分阶段发布计划。阶段定义创建后不可修改；权重必须从当前线上配置（0）
// 严格递增到 100（目标修订），形成合法路径。同一区域同一时间只允许一个
// 进行中的计划。携带相同幂等键与相同负载的重复创建返回原计划。
func (s *Service) CreatePlan(in CreatePlanInput) (*Plan, error) {
	zone, verr := normalizeName(in.Zone)
	if verr != nil {
		return nil, verr
	}
	if verr := validateStages(in.Stages); verr != nil {
		return nil, verr
	}

	var out *Plan
	err := s.mutate(zone, func(z *ZoneState) error {
		hash, err := hashPlanPayload(in)
		if err != nil {
			return err
		}
		if in.IdempotencyKey != "" {
			if id, ok := z.PlanIdem[in.IdempotencyKey]; ok {
				if z.Payloads["plan#"+in.IdempotencyKey] != hash {
					return errf(KindIdempotency,
						"idempotency key %q was already used with a different plan payload", in.IdempotencyKey)
				}
				out = findPlan(z, id)
				return nil
			}
		}

		chg := findChange(z, in.ChangeID)
		if chg == nil {
			return errf(KindNotFound, "change %q not found in zone %s", in.ChangeID, z.Name)
		}
		switch chg.Status {
		case StatusPublished:
			return errf(KindState, "change %s is already published", in.ChangeID)
		case StatusWithdrawn:
			return errf(KindState, "change %s is withdrawn", in.ChangeID)
		case StatusPending:
			return errf(KindState, "change %s is not approved yet", in.ChangeID)
		}
		if chg.Revision != z.HeadRevision {
			return errf(KindState,
				"change %s (revision %d) is not the head revision %d; submit a fresh change first",
				in.ChangeID, chg.Revision, z.HeadRevision)
		}
		for _, p := range z.Plans {
			if p.Status == PlanActive {
				return errf(KindState, "zone %s already has an active plan %s", z.Name, p.ID)
			}
		}

		baseView := viewAt(z, z.PublishedRevision)
		targetView := viewAt(z, chg.Revision)
		trafficKeys, verr := splittableKeys(baseView, targetView)
		if verr != nil {
			return verr
		}

		now := s.now()
		z.Seq++
		plan := &Plan{
			ID:             fmt.Sprintf("%s/plan/%d", z.Name, len(z.Plans)+1),
			Zone:           z.Name,
			TargetChangeID: chg.ID,
			TargetRevision: chg.Revision,
			BaseRevision:   z.PublishedRevision,
			Stages:         toStages(in.Stages),
			CurrentStage:   0,
			Status:         PlanActive,
			TrafficKeys:    trafficKeys,
			StageStates:    make([]StageState, len(in.Stages)),
			Seq:            z.Seq,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		for i := range plan.StageStates {
			plan.StageStates[i].Index = i
		}
		z.Plans = append(z.Plans, plan)
		if in.IdempotencyKey != "" {
			if z.PlanIdem == nil {
				z.PlanIdem = map[string]string{}
			}
			if z.Payloads == nil {
				z.Payloads = map[string]string{}
			}
			z.PlanIdem[in.IdempotencyKey] = plan.ID
			z.Payloads["plan#"+in.IdempotencyKey] = hash
		}
		out = plan
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AdvancePlan 推进计划。首次调用发布当前阶段对应的完整区域视图（写出唯一
// outbox 条目）；后续调用先判定当前阶段健康门槛：
//   - 观察时间或样本数不足：返回状态错误，不产生任何记录；
//   - 健康达标：发布下一阶段视图；
//   - 健康不达标：自动创建恢复上一稳定权重的新修订并发布（不改写历史），
//     计划转为 aborted，返回该计划与 nil 错误。
//
// 全部判定与发布在区域锁内完成，健康判定、人工停止与推进并发时只有一个结果；
// 失败的推进不留下部分记录集，也不消耗修订号。
func (s *Service) AdvancePlan(zone, planID string) (*Plan, error) {
	var out *Plan
	err := s.mutate(zone, func(z *ZoneState) error {
		plan := findPlan(z, planID)
		if plan == nil {
			return errf(KindNotFound, "plan %q not found in zone %s", planID, z.Name)
		}
		if plan.Status != PlanActive {
			return errf(KindState, "plan %s is %s and cannot be advanced", planID, plan.Status)
		}
		if verr := s.checkPlanBaseline(z, plan); verr != nil {
			return verr
		}
		out = plan // 计划存在且仍可操作；任何成功路径都返回该计划

		st := &plan.StageStates[plan.CurrentStage]
		stage := plan.Stages[plan.CurrentStage]

		// 当前阶段尚未发布：直接发布其完整区域视图。
		if st.PublishedRev == 0 {
			return s.publishStageLocked(z, plan)
		}

		// 当前阶段已发布：判定健康门槛。
		ready, healthy := evaluateStage(stage, st, s.now())
		if !ready {
			return errf(KindState,
				"plan %s stage %d: observation window or minimum samples not satisfied "+
					"(samples %d/%d, elapsed %s/%s)",
				planID, plan.CurrentStage, st.TotalSamples, stage.MinSamples,
				s.now().Sub(st.PublishedAt), stage.MinObservation)
		}
		if !healthy {
			// 健康不达标：自动恢复到上一稳定权重，计划终止。
			if verr := s.recoverPlanLocked(z, plan, "system",
				fmt.Sprintf("stage %d health below threshold %.2f", plan.CurrentStage, stage.HealthThreshold)); verr != nil {
				return verr
			}
			return nil
		}

		// 达标：推进到下一阶段并发布其视图。
		st.AdvancedAt = s.now()
		plan.CurrentStage++
		return s.publishStageLocked(z, plan)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StopPlan 人工停止一个进行中的计划：自动创建恢复上一稳定权重的新修订并发布，
// 计划转为 aborted。与推进、健康判定在区域锁内串行，并发时只有一个结果。
func (s *Service) StopPlan(zone, planID, actor string) (*Plan, error) {
	if actor == "" {
		return nil, errf(KindValidation, "actor is required")
	}
	var out *Plan
	err := s.mutate(zone, func(z *ZoneState) error {
		plan := findPlan(z, planID)
		if plan == nil {
			return errf(KindNotFound, "plan %q not found in zone %s", planID, z.Name)
		}
		if plan.Status != PlanActive {
			return errf(KindState, "plan %s is %s and cannot be stopped", planID, plan.Status)
		}
		if verr := s.checkPlanBaseline(z, plan); verr != nil {
			return verr
		}
		if verr := s.recoverPlanLocked(z, plan, actor, "manual stop"); verr != nil {
			return verr
		}
		out = plan
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordHealthSample 上报一条健康样本。样本由服务端绑定到 区域/计划/阶段/修订：
// 只有当前进行中阶段的样本被接受，旧阶段或其他修订的样本被拒绝，因此不会参与
// 当前判断。相同 (计划, 阶段, SampleKey) 的样本重放幂等，不重复计数。
func (s *Service) RecordHealthSample(in HealthSampleInput) (*Plan, error) {
	if in.SampleKey == "" {
		return nil, errf(KindValidation, "sample key is required")
	}
	var out *Plan
	err := s.mutate(in.Zone, func(z *ZoneState) error {
		plan := findPlan(z, in.PlanID)
		if plan == nil {
			return errf(KindNotFound, "plan %q not found in zone %s", in.PlanID, z.Name)
		}
		if plan.Status != PlanActive {
			return errf(KindState, "plan %s is %s and no longer accepts samples", in.PlanID, plan.Status)
		}
		if in.Stage != plan.CurrentStage {
			return errf(KindState,
				"plan %s is at stage %d: samples for stage %d cannot participate",
				in.PlanID, plan.CurrentStage, in.Stage)
		}
		st := &plan.StageStates[in.Stage]
		if st.PublishedRev == 0 {
			return errf(KindState, "plan %s stage %d is not published yet", in.PlanID, in.Stage)
		}
		// 基线守卫：阶段期间出现以已发布阶段为基准的新变更并发布后，旧计划样本
		// 不能参与当前判断，计划转为 superseded。
		if verr := s.checkPlanBaseline(z, plan); verr != nil {
			return verr
		}

		dedup := fmt.Sprintf("%s/%d/%s", plan.ID, in.Stage, in.SampleKey)
		if z.SampleSeen == nil {
			z.SampleSeen = map[string]bool{}
		}
		if z.SampleSeen[dedup] {
			out = plan // 重放幂等：不重复计数
			return nil
		}
		at := in.ObservedAt
		if at.IsZero() {
			at = s.now()
		}
		z.Seq++
		z.SampleSeen[dedup] = true
		z.Samples = append(z.Samples, &HealthSample{
			PlanID: plan.ID, StageIndex: in.Stage, Revision: st.PublishedRev,
			SampleKey: in.SampleKey, Healthy: in.Healthy, At: at,
		})
		st.TotalSamples++
		if in.Healthy {
			st.HealthySamples++
		}
		st.LastSampleAt = at
		plan.Seq = z.Seq
		plan.UpdatedAt = s.now()
		out = plan
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetPlan 查询计划的完整视图：各阶段实际区域内容、健康依据、传播状态与恢复链。
func (s *Service) GetPlan(zone, planID string) (*PlanDetail, error) {
	var out *PlanDetail
	err := s.read(zone, func(z *ZoneState) error {
		plan := findPlan(z, planID)
		if plan == nil {
			return errf(KindNotFound, "plan %q not found in zone %s", planID, z.Name)
		}
		out = s.planDetail(z, plan)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPlans 按创建顺序列出区域的全部计划。
func (s *Service) ListPlans(zone string) ([]*Plan, error) {
	var out []*Plan
	err := s.read(zone, func(z *ZoneState) error {
		out = append(out, z.Plans...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- 内部实现 ---

// validateStages 校验阶段路径：至少一个阶段；权重从 0（当前线上配置）严格递增，
// 末阶段必须为 100（完整切换到目标修订）；门槛与时间非负且在界内。
func validateStages(stages []StageInput) *Error {
	if len(stages) == 0 {
		return errf(KindValidation, "plan requires at least one stage")
	}
	var prev uint32
	for i, st := range stages {
		if st.TargetWeight == 0 || st.TargetWeight > 100 {
			return errf(KindValidation, "stage %d: target weight %d must be in 1..100", i, st.TargetWeight)
		}
		if st.TargetWeight <= prev {
			return errf(KindValidation,
				"stage %d: target weight %d must exceed previous stage weight %d", i, st.TargetWeight, prev)
		}
		prev = st.TargetWeight
		if st.MinObservation < 0 {
			return errf(KindValidation, "stage %d: min observation must be >= 0", i)
		}
		if st.HealthThreshold < 0 || st.HealthThreshold > 1 {
			return errf(KindValidation, "stage %d: health threshold %v must be in [0,1]", i, st.HealthThreshold)
		}
		if st.MinSamples < 0 {
			return errf(KindValidation, "stage %d: min samples must be >= 0", i)
		}
	}
	if prev != 100 {
		return errf(KindValidation,
			"last stage must have target weight 100 to reach the target revision, got %d", prev)
	}
	return nil
}

// splittableKeys 计算基准视图到目标视图之间的可分流记录集键（同名同类型、
// A/AAAA、记录数据发生变化）。存在任何不可分流的差异（新增、删除、非地址类型
// 变更、仅 TTL 变化）时返回校验错误；没有可分流记录时同样报错。
func splittableKeys(base, target map[string]RecordSet) ([]string, *Error) {
	var traffic, unsplittable []string
	for key, ts := range target {
		bs, ok := base[key]
		if !ok {
			unsplittable = append(unsplittable, key+" (added)")
			continue
		}
		if setsEqual(bs, ts) {
			continue
		}
		if (ts.Type == TypeA || ts.Type == TypeAAAA) && !sameRecords(bs.Records, ts.Records) {
			traffic = append(traffic, key)
		} else {
			unsplittable = append(unsplittable, key)
		}
	}
	for key := range base {
		if _, ok := target[key]; !ok {
			unsplittable = append(unsplittable, key+" (deleted)")
		}
	}
	if len(unsplittable) > 0 {
		sort.Strings(unsplittable)
		return nil, &Error{Kind: KindValidation,
			Message: "change contains differences that cannot be progressively shifted",
			Issues:  unsplittable}
	}
	if len(traffic) == 0 {
		return nil, errf(KindValidation, "change contains no splittable A/AAAA record changes")
	}
	sort.Strings(traffic)
	return traffic, nil
}

// blendStageView 计算某阶段权重下的完整区域视图：可分流记录集按相对权重
// 混合基准与目标记录，其余记录集与目标视图一致。权重 100 时视图与目标修订
// 完全一致（不带权重）。
func blendStageView(base, target map[string]RecordSet, trafficKeys []string, weight uint32) map[string]RecordSet {
	out := make(map[string]RecordSet, len(target))
	blend := make(map[string]bool, len(trafficKeys))
	for _, k := range trafficKeys {
		blend[k] = true
	}
	for key, ts := range target {
		bs, isTraffic := base[key]
		if !isTraffic || !blend[key] || weight == 100 {
			out[key] = ts
			continue
		}
		out[key] = blendRecordSet(bs, ts, weight)
	}
	return out
}

// blendRecordSet 按目标权重混合基准与目标记录集：基准记录取相对权重 100-w，
// 目标记录取 w，两侧共有的记录权重相加。TTL 取两者较小值，保证过渡期间
// 缓存尽快过期。记录按键排序，保证确定性。
func blendRecordSet(base, target RecordSet, w uint32) RecordSet {
	type rec struct {
		data   string
		weight uint32
	}
	merged := map[string]*rec{}
	var order []string
	add := func(data string, weight uint32) {
		if r, ok := merged[data]; ok {
			r.weight += weight
			return
		}
		merged[data] = &rec{data: data, weight: weight}
		order = append(order, data)
	}
	for _, r := range base.Records {
		add(r, 100-w)
	}
	for _, r := range target.Records {
		add(r, w)
	}
	sort.Strings(order)
	rs := RecordSet{Name: target.Name, Type: target.Type, TTL: minTTL(base.TTL, target.TTL)}
	for _, data := range order {
		rs.Records = append(rs.Records, data)
		rs.Weights = append(rs.Weights, merged[data].weight)
	}
	return rs
}

// evaluateStage 判定当前阶段是否可推进：ready 表示最短观察时间与最小样本数
// 已满足；healthy 表示健康样本比例达到门槛。
func evaluateStage(stage Stage, st *StageState, now time.Time) (ready, healthy bool) {
	ready = st.TotalSamples >= int64(stage.MinSamples) && now.Sub(st.PublishedAt) >= stage.MinObservation
	ratio := 1.0
	if st.TotalSamples > 0 {
		ratio = float64(st.HealthySamples) / float64(st.TotalSamples)
	}
	return ready, ratio >= stage.HealthThreshold
}

// publishStageLocked 发布当前阶段的完整区域视图：创建合成变更、修订与唯一
// outbox 条目。末阶段（权重 100）发布时计划完成，目标变更标记为已发布。
// 调用前所有校验已通过，因此这里分配修订号不会跳号。
func (s *Service) publishStageLocked(z *ZoneState, plan *Plan) error {
	idx := plan.CurrentStage
	stage := plan.Stages[idx]
	baseView := viewAt(z, plan.BaseRevision)
	targetView := viewAt(z, plan.TargetRevision)
	traffic := make([]string, len(plan.TrafficKeys))
	copy(traffic, plan.TrafficKeys)
	view := blendStageView(baseView, targetView, traffic, stage.TargetWeight)
	if verr := validateZoneView(z.Name, view, z.Policy); verr != nil {
		return verr
	}

	now := s.now()
	z.Seq++
	rev := z.HeadRevision + 1
	headView := viewAt(z, z.HeadRevision)
	ops := diffOps(headView, view)
	hash, err := hashOps(ops)
	if err != nil {
		return err
	}
	chg := &Change{
		ID:           changeID(z.Name, rev),
		Zone:         z.Name,
		Revision:     rev,
		BaseRevision: z.HeadRevision,
		Ops:          ops,
		Submitter:    "system",
		Comment:      fmt.Sprintf("plan %s stage %d (target weight %d)", plan.ID, idx, stage.TargetWeight),
		Policy:       z.Policy,
		Status:       StatusPublished,
		PayloadHash:  hash,
		PlanID:       plan.ID,
		StageIndex:   idx,
		Seq:          z.Seq,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	z.HeadRevision = rev
	z.PublishedRevision = rev
	z.Changes = append(z.Changes, chg)
	z.Revisions = append(z.Revisions, &Revision{
		Zone: z.Name, Number: rev, ChangeID: chg.ID, Base: chg.BaseRevision,
		RecordSets: sortedSets(view), PlanID: plan.ID, StageIndex: idx,
		Seq: z.Seq, CreatedAt: now,
	})
	outbox := &OutboxEntry{
		ID: outboxID(z.Name, rev), Zone: z.Name, Revision: rev,
		RecordSets: sortedSets(view), PlanID: plan.ID, StageIndex: idx,
		Seq: z.Seq, CreatedAt: now,
	}
	z.Outbox = append(z.Outbox, outbox)

	st := &plan.StageStates[idx]
	st.PublishedRev = rev
	st.PublishedAt = now
	st.ObservationStart = now
	st.OutboxID = outbox.ID
	plan.Seq = z.Seq
	plan.UpdatedAt = now

	if stage.TargetWeight == 100 {
		plan.Status = PlanCompleted
		if target := findChange(z, plan.TargetChangeID); target != nil && target.Status == StatusApproved {
			target.Status = StatusPublished
			target.Seq = z.Seq
			target.UpdatedAt = now
		}
	}
	return nil
}

// recoverPlanLocked 创建恢复上一稳定权重的新修订并立即发布（不改写历史），
// 计划转为 aborted。稳定权重：当前阶段之前最近一个已发布阶段的视图；
// 当前是第一个阶段时恢复到计划基准（原线上配置）。返回非 nil 错误时不修改任何状态。
func (s *Service) recoverPlanLocked(z *ZoneState, plan *Plan, actor, reason string) *Error {
	var stableRev int64
	if plan.CurrentStage == 0 {
		stableRev = plan.BaseRevision
	} else {
		stableRev = plan.StageStates[plan.CurrentStage-1].PublishedRev
	}
	stableView := viewAt(z, stableRev)
	headView := viewAt(z, z.HeadRevision)
	ops := diffOps(headView, stableView)

	// 先完成所有可能失败的准备，再改动状态，保证失败时不留半成品、不消耗序号。
	var hash string
	if len(ops) > 0 {
		h, err := hashOps(ops)
		if err != nil {
			return errf(KindValidation, "plan %s recovery failed to hash ops: %v", plan.ID, err)
		}
		hash = h
	}

	now := s.now()
	z.Seq++
	plan.Status = PlanAborted
	plan.RecoveryOf = stableRev
	plan.StopActor = actor
	plan.StopReason = reason
	plan.Seq = z.Seq
	plan.UpdatedAt = now

	if len(ops) == 0 {
		// 头部已与稳定视图一致（极端情况）：无需新修订，仅终止计划。
		return nil
	}
	rev := z.HeadRevision + 1
	chg := &Change{
		ID:           changeID(z.Name, rev),
		Zone:         z.Name,
		Revision:     rev,
		BaseRevision: z.HeadRevision,
		Ops:          ops,
		Submitter:    "system",
		Comment:      fmt.Sprintf("plan %s recovery to revision %d: %s", plan.ID, stableRev, reason),
		Policy:       z.Policy,
		Status:       StatusPublished,
		PayloadHash:  hash,
		RollbackOf:   stableRev,
		PlanID:       plan.ID,
		Seq:          z.Seq,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	z.HeadRevision = rev
	z.PublishedRevision = rev
	z.Changes = append(z.Changes, chg)
	z.Revisions = append(z.Revisions, &Revision{
		Zone: z.Name, Number: rev, ChangeID: chg.ID, Base: chg.BaseRevision,
		RecordSets: sortedSets(stableView), RollbackOf: stableRev, PlanID: plan.ID,
		Seq: z.Seq, CreatedAt: now,
	})
	z.Outbox = append(z.Outbox, &OutboxEntry{
		ID: outboxID(z.Name, rev), Zone: z.Name, Revision: rev,
		RecordSets: sortedSets(stableView), PlanID: plan.ID,
		Seq: z.Seq, CreatedAt: now,
	})
	plan.RecoveryChange = chg.ID
	return nil
}

// checkPlanBaseline 校验计划仍是线上基准：当前已发布修订必须等于计划当前
// 阶段发布的修订（首个阶段未发布时为计划基准修订）。期间出现以已发布阶段
// 为基准的新变更并发布后，旧计划不得覆盖，转为 superseded（该状态即使本次
// 操作返回错误也会持久化，保证查询可见）。
func (s *Service) checkPlanBaseline(z *ZoneState, plan *Plan) *Error {
	expected := plan.BaseRevision
	if st := plan.StageStates[plan.CurrentStage]; st.PublishedRev != 0 {
		expected = st.PublishedRev
	}
	if z.PublishedRevision != expected {
		plan.Status = PlanSuperseded
		plan.UpdatedAt = s.now()
		_ = s.store.SaveZone(z) // 失败时本次调用仍返回状态错误，下次调用会重新标记
		return errf(KindState,
			"plan %s is superseded: zone %s published revision %d, plan expected %d",
			plan.ID, z.Name, z.PublishedRevision, expected)
	}
	// 阶段推进依赖线性历史：若存在任何仍存活（pending/approved）且修订号高于
	// 当前已发布修订的变更，它的内容会错误地并入下一阶段视图，因此阻止计划操作。
	// 已撤回的堆叠修订不阻止（阶段 diff 会自然剔除其内容）。
	liveUnpublished := false
	for _, c := range z.Changes {
		if c.Revision > z.PublishedRevision &&
			(c.Status == StatusPending || c.Status == StatusApproved) {
			liveUnpublished = true
			break
		}
	}
	if liveUnpublished {
		firstStageUnpublished := plan.StageStates[plan.CurrentStage].PublishedRev == 0
		if !(firstStageUnpublished && z.HeadRevision == plan.TargetRevision) {
			return errf(KindState,
				"zone %s has unpublished live changes stacked above revision %d while plan %s is active; "+
					"publish or withdraw them before operating the plan",
				z.Name, z.PublishedRevision, plan.ID)
		}
	}
	return nil
}

// planDetail 组装计划的完整只读视图。
func (s *Service) planDetail(z *ZoneState, plan *Plan) *PlanDetail {
	baseView := viewAt(z, plan.BaseRevision)
	targetView := viewAt(z, plan.TargetRevision)
	detail := &PlanDetail{
		Plan:             plan,
		RecoveryChangeID: plan.RecoveryChange,
		RecoveryRevision: plan.RecoveryOf,
	}
	now := s.now()
	for i, stage := range plan.Stages {
		st := plan.StageStates[i]
		sv := StageView{Plan: plan, Stage: stage, State: st, OutboxID: st.OutboxID}
		if st.PublishedRev != 0 {
			if rev := findRevision(z, st.PublishedRev); rev != nil {
				sv.RecordSets = rev.RecordSets
			}
			if ob := findOutbox(z, st.OutboxID); ob != nil {
				sv.OutboxSeq = ob.Seq
			}
			if st.TotalSamples > 0 {
				sv.HealthRatio = float64(st.HealthySamples) / float64(st.TotalSamples)
			}
			ready, healthy := evaluateStage(stage, &st, now)
			sv.HealthMet = ready && healthy
		} else {
			// 未发布阶段展示按权重计算的预览视图。
			sv.RecordSets = sortedSets(blendStageView(baseView, targetView, plan.TrafficKeys, stage.TargetWeight))
		}
		detail.Stages = append(detail.Stages, sv)
	}
	return detail
}

func toStages(in []StageInput) []Stage {
	out := make([]Stage, len(in))
	for i, s := range in {
		out[i] = Stage{
			Index:           i,
			TargetWeight:    s.TargetWeight,
			MinObservation:  s.MinObservation,
			HealthThreshold: s.HealthThreshold,
			MinSamples:      s.MinSamples,
		}
	}
	return out
}

func findPlan(z *ZoneState, id string) *Plan {
	for _, p := range z.Plans {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func findOutbox(z *ZoneState, id string) *OutboxEntry {
	for _, o := range z.Outbox {
		if o.ID == id {
			return o
		}
	}
	return nil
}

func hashPlanPayload(in CreatePlanInput) (string, error) {
	data, err := json.Marshal(planPayload{ChangeID: in.ChangeID, Stages: in.Stages})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func sameRecords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func minTTL(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}
