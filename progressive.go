package dnschange

import (
	"fmt"
	"sort"
	"time"
)

// 本文件实现分阶段流量切换（灰度发布）：
//
// 对一个包含「可分流记录」（同一记录集在当前线上视图与目标修订中都存在，
// 且目标侧含有新增记录值）的已审批变更，可以创建一份不可变的阶段计划。
// 每个阶段声明目标权重、最短观察时间与健康门槛；阶段权重必须构成
// 0(当前线上) -> ... -> 100(目标修订) 的严格递增合法路径。
//
// 每次推进发布当前阶段的完整区域视图（占用一个新修订号）并写出唯一 outbox；
// 健康样本绑定区域、修订与阶段，只有当前计划当前阶段、且修订匹配的样本参与
// 判定，相同样本重放幂等。健康不达标时在同一原子操作内创建并发布一个恢复到
// 上一稳定权重的新修订，历史不改写。计划执行期间允许以当前已发布阶段为基准
// 提交并发布新变更；一旦发生，旧计划被标记为 superseded，不能再推进或覆盖
// 后来发布的修订。

// autoRecoverySubmitter 是系统自动创建恢复变更时使用的提交者。
const autoRecoverySubmitter = "system:auto-recovery"

// CreatePlan 为已审批变更创建分阶段发布计划。计划一经创建不可修改：
// 权重必须严格递增且以 100 结束，每个权重生成的完整区域视图都必须合法。
func (s *Service) CreatePlan(in PlanInput) (*RolloutPlan, error) {
	zone, verr := normalizeName(in.Zone)
	if verr != nil {
		return nil, verr
	}
	if verr := validateStageSpecs(in.Stages); verr != nil {
		return nil, verr
	}
	var out *RolloutPlan
	err := s.mutate(zone, func(z *ZoneState) error {
		chg := findChange(z, in.ChangeID)
		if chg == nil {
			return errf(KindNotFound, "change %q not found in zone %s", in.ChangeID, z.Name)
		}
		if chg.Status != StatusApproved {
			return errf(KindState, "change %s is %s; only approved changes can start a rollout plan",
				in.ChangeID, chg.Status)
		}
		if p := activePlan(z); p != nil {
			return errf(KindState, "zone %s already has an active rollout plan %s", z.Name, p.ID)
		}
		// 目标修订之后不能还压着未落定（未撤销）的变更；已撤销的修订只保留历史编号，不影响。
		for _, other := range z.Changes {
			if other.Revision > chg.Revision &&
				other.Status != StatusWithdrawn && other.Status != StatusPublished {
				return errf(KindState,
					"change %s has a newer unsettled change %s (revision %d); resolve it before planning a rollout",
					in.ChangeID, other.ID, other.Revision)
			}
		}
		if chg.Revision <= z.PublishedRevision {
			return errf(KindState, "change %s (revision %d) is not ahead of published revision %d",
				in.ChangeID, chg.Revision, z.PublishedRevision)
		}
		baseView := viewAt(z, z.PublishedRevision)
		targetView := viewAt(z, chg.Revision)
		if len(splittableKeys(baseView, targetView)) == 0 {
			return errf(KindValidation,
				"change %s contains no splittable record sets; use Publish for an immediate release",
				in.ChangeID)
		}
		// 预生成并校验每个阶段的完整视图，保证权重构成从线上配置到目标修订的合法路径。
		stages := make([]StageRuntime, len(in.Stages))
		for i, spec := range in.Stages {
			view := buildStageView(baseView, targetView, spec.TargetWeight)
			if verr := validateZoneView(z.Name, view, z.Policy); verr != nil {
				return verr
			}
			if verr := validateWeightedView(view); verr != nil {
				return verr
			}
			stages[i] = StageRuntime{Spec: spec}
		}

		now := s.now()
		z.Seq++
		plan := &RolloutPlan{
			ID:             planID(z.Name, z.Seq),
			Zone:           z.Name,
			ChangeID:       chg.ID,
			TargetRevision: chg.Revision,
			BaseRevision:   z.PublishedRevision,
			Stages:         stages,
			Status:         PlanActive,
			StableRevision: z.PublishedRevision,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		z.Plans = append(z.Plans, plan)
		out = plan
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AdvanceStage 推进计划一次。整个判定与发布在区域锁内原子完成，
// 因此健康判定、人工停止与并发推进之间只会有一个结果生效；
// 失败不留下部分记录集，也不消耗修订号。可能的结果：
//   - OutcomeStagePublished：发布了当前（或下一个）阶段的完整区域视图；
//   - OutcomeCompleted：末阶段健康通过，计划完成，线上即目标修订内容；
//   - OutcomeRecovered：健康不达标，已原子创建并发布恢复修订。
//
// 观察时间不足或样本不足时返回状态错误，不产生任何记录。
func (s *Service) AdvanceStage(zone, planID string) (*AdvanceResult, error) {
	zname, verr := normalizeName(zone)
	if verr != nil {
		return nil, verr
	}
	var out *AdvanceResult
	err := s.mutate(zname, func(z *ZoneState) error {
		plan := findPlan(z, planID)
		if plan == nil {
			return errf(KindNotFound, "plan %q not found in zone %s", planID, z.Name)
		}
		if plan.Status != PlanActive {
			return errf(KindState, "plan %s is %s and cannot advance", planID, plan.Status)
		}
		chg := findChange(z, plan.ChangeID)
		if chg == nil {
			return errf(KindNotFound, "target change %q of plan %s vanished", plan.ChangeID, planID)
		}
		stage := &plan.Stages[plan.CurrentStage]
		// 计划执行期间若有人基于已发布阶段提交了更新的变更，必须先让它落定
		// （发布则旧计划随即失效；撤销则计划可继续），阶段视图不能越过它。
		// 唯一例外：计划刚创建、首阶段未发布时，头部就是计划自己的目标修订。
		ownTargetPending := plan.CurrentStage == 0 && !stage.Published && z.HeadRevision == plan.TargetRevision
		if !ownTargetPending && hasPendingAhead(z, plan.ChangeID) {
			return errf(KindState,
				"plan %s cannot advance: a newer change is pending ahead of published revision %d; publish or withdraw it first",
				plan.ID, z.PublishedRevision)
		}

		now := s.now()

		// 情况一：发布当前阶段。线上必须仍是本计划的稳定点。
		if !stage.Published {
			if z.PublishedRevision != plan.StableRevision {
				markSuperseded(z, plan, chg, now)
				return errf(KindState, "plan %s was superseded by published revision %d and has been stopped",
					plan.ID, z.PublishedRevision)
			}
			rev := s.publishStageView(z, plan, plan.CurrentStage, stage, chg, now)
			plan.UpdatedAt = now
			out = &AdvanceResult{Outcome: OutcomeStagePublished, Plan: plan,
				Stage: plan.CurrentStage, Revision: rev}
			return nil
		}

		// 情况二：当前阶段已发布，做健康判定。线上必须仍是本阶段视图。
		if z.PublishedRevision != stage.Revision {
			markSuperseded(z, plan, chg, now)
			return errf(KindState, "plan %s was superseded by published revision %d and has been stopped",
				plan.ID, z.PublishedRevision)
		}
		stats := healthStats(z, plan.ID, stage.Revision, plan.CurrentStage)
		if elapsed := now.Sub(stage.PublishedAt); elapsed < stage.Spec.MinObservation {
			return errf(KindState,
				"plan %s stage %d still in observation: %s elapsed of minimum %s",
				plan.ID, plan.CurrentStage, elapsed, stage.Spec.MinObservation)
		}
		if stats.Total < stage.Spec.MinSampleCount {
			return errf(KindState,
				"plan %s stage %d has %d samples, minimum is %d",
				plan.ID, plan.CurrentStage, stats.Total, stage.Spec.MinSampleCount)
		}
		plan.UpdatedAt = now
		if stats.Ratio < stage.Spec.HealthThreshold {
			// 情况三：健康不达标 → 在同一原子操作内恢复到上一稳定权重。
			rec := s.recoverPlan(z, plan, chg, &stats, now)
			out = &AdvanceResult{Outcome: OutcomeRecovered, Plan: plan,
				Stage: plan.CurrentStage, Revision: rec.RecoveryRevision, Recovery: rec}
			return nil
		}

		// 健康达标：观察通过，稳定点前移。
		stage.ObservedAt = now
		plan.StableRevision = stage.Revision
		if plan.CurrentStage == len(plan.Stages)-1 {
			// 末阶段（权重 100）视图即目标修订内容，计划完成。
			plan.CurrentStage++
			plan.Status = PlanCompleted
			z.Seq++
			chg.Status = StatusPublished
			chg.Seq = z.Seq
			chg.UpdatedAt = now
			out = &AdvanceResult{Outcome: OutcomeCompleted, Plan: plan,
				Stage: plan.CurrentStage - 1, Revision: stage.Revision}
			return nil
		}

		// 情况四：发布下一阶段。
		plan.CurrentStage++
		next := &plan.Stages[plan.CurrentStage]
		rev := s.publishStageView(z, plan, plan.CurrentStage, next, chg, now)
		out = &AdvanceResult{Outcome: OutcomeStagePublished, Plan: plan,
			Stage: plan.CurrentStage, Revision: rev}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordHealthSample 记录一条健康样本。样本必须绑定区域、修订与阶段；
// 相同 SampleID 重放幂等（同键但内容不同返回幂等错误）。
// 旧阶段或其他修订的样本会被留存为历史证据，但不参与任何当前判定。
func (s *Service) RecordHealthSample(sample HealthSample) (*HealthSample, error) {
	zone, verr := normalizeName(sample.Zone)
	if verr != nil {
		return nil, verr
	}
	if sample.SampleID == "" {
		return nil, errf(KindValidation, "sample id is required")
	}
	if sample.At.IsZero() {
		sample.At = s.now()
	}
	sample.Zone = zone
	var out *HealthSample
	err := s.mutate(zone, func(z *ZoneState) error {
		for _, ex := range z.HealthSamples {
			if ex.SampleID == sample.SampleID {
				if ex.Revision != sample.Revision || ex.Stage != sample.Stage || ex.Healthy != sample.Healthy {
					return errf(KindIdempotency,
						"sample %q was already recorded with a different revision/stage/result", sample.SampleID)
				}
				out = ex // 重放幂等
				return nil
			}
		}
		if findRevision(z, sample.Revision) == nil {
			return errf(KindNotFound, "revision %d not found in zone %s", sample.Revision, z.Name)
		}
		cp := sample
		z.HealthSamples = append(z.HealthSamples, &cp)
		out = &cp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StopRollout 人工停止计划：流量停在当前已发布阶段，计划不再推进。
// 重复停止幂等；已结束（完成/恢复）的计划不能停止。
func (s *Service) StopRollout(zone, planID, actor string) (*RolloutPlan, error) {
	if actor == "" {
		return nil, errf(KindValidation, "actor is required")
	}
	zname, verr := normalizeName(zone)
	if verr != nil {
		return nil, verr
	}
	var out *RolloutPlan
	err := s.mutate(zname, func(z *ZoneState) error {
		plan := findPlan(z, planID)
		if plan == nil {
			return errf(KindNotFound, "plan %q not found in zone %s", planID, z.Name)
		}
		switch plan.Status {
		case PlanStopped:
			out = plan // 幂等
			return nil
		case PlanCompleted, PlanRecovered:
			return errf(KindState, "plan %s is already %s", planID, plan.Status)
		}
		now := s.now()
		plan.Status = PlanStopped
		plan.UpdatedAt = now
		if chg := findChange(z, plan.ChangeID); chg != nil {
			z.Seq++
			chg.Status = StatusStopped
			chg.Seq = z.Seq
			chg.UpdatedAt = now
		}
		out = plan
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetPlan 查询计划详情：各阶段实际区域内容、健康依据、传播状态与恢复链。
func (s *Service) GetPlan(zone, planID string) (*PlanDetail, error) {
	zname, verr := normalizeName(zone)
	if verr != nil {
		return nil, verr
	}
	var out *PlanDetail
	err := s.read(zname, func(z *ZoneState) error {
		plan := findPlan(z, planID)
		if plan == nil {
			return errf(KindNotFound, "plan %q not found in zone %s", planID, z.Name)
		}
		detail := &PlanDetail{
			Plan:             plan,
			BaseRecordSets:   sortedSets(viewAt(z, plan.BaseRevision)),
			TargetRecordSets: sortedSets(viewAt(z, plan.TargetRevision)),
		}
		for i := range plan.Stages {
			st := &plan.Stages[i]
			sv := StageView{
				PlanID: plan.ID, Index: i, Spec: st.Spec,
				Status: plan.Status, Stage: *st,
			}
			if i == 0 {
				sv.StableWeight = 0
			} else {
				sv.StableWeight = plan.Stages[i-1].Spec.TargetWeight
			}
			if rev := findStageRevision(z, plan.ID, i); rev != nil {
				sv.RecordSets = rev.RecordSets
			}
			detail.Stages = append(detail.Stages, sv)
		}
		if cur := plan.CurrentStage; cur < len(plan.Stages) && plan.Stages[cur].Published {
			detail.Health = healthStats(z, plan.ID, plan.Stages[cur].Revision, cur)
		} else {
			detail.Health = HealthStats{Stage: plan.CurrentStage}
		}
		detail.RecoveryChain = recoveryChain(z, plan)
		out = detail
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPlans 按创建顺序列出区域的全部分阶段计划。
func (s *Service) ListPlans(zone string) ([]*RolloutPlan, error) {
	zname, verr := normalizeName(zone)
	if verr != nil {
		return nil, verr
	}
	var out []*RolloutPlan
	err := s.read(zname, func(z *ZoneState) error {
		out = append(out, z.Plans...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AdvanceOutcome 标识一次推进的结果类型。
type AdvanceOutcome string

const (
	// OutcomeStagePublished 发布了（下一）阶段视图。
	OutcomeStagePublished AdvanceOutcome = "stage_published"
	// OutcomeCompleted 末阶段健康通过，计划完成，线上即目标修订内容。
	OutcomeCompleted AdvanceOutcome = "completed"
	// OutcomeRecovered 健康不达标，已创建并发布恢复修订。
	OutcomeRecovered AdvanceOutcome = "recovered"
)

// AdvanceResult 是一次推进的结果。
type AdvanceResult struct {
	Outcome  AdvanceOutcome
	Plan     *RolloutPlan
	Stage    int
	Revision int64 // 本次发布（或检测到的覆盖）修订号
	Recovery *RecoveryLink
}

// --- 内部实现 ---

func planID(zone string, seq int64) string { return fmt.Sprintf("%s/plan/%d", zone, seq) }

func findPlan(z *ZoneState, id string) *RolloutPlan {
	for _, p := range z.Plans {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// planOfChange 返回以指定变更为目标的计划（不论计划状态）。
func planOfChange(z *ZoneState, changeID string) *RolloutPlan {
	for _, p := range z.Plans {
		if p.ChangeID == changeID {
			return p
		}
	}
	return nil
}

// activePlan 返回区域当前仍可推进的计划（不存在则 nil）。
func activePlan(z *ZoneState) *RolloutPlan {
	for _, p := range z.Plans {
		if p.Status == PlanActive {
			return p
		}
	}
	return nil
}

// hasPendingAhead 报告线上修订之后是否还压着未落定（未发布也未撤销）的变更。
func hasPendingAhead(z *ZoneState, excludeChangeID string) bool {
	for _, c := range z.Changes {
		if c.ID == excludeChangeID {
			continue
		}
		if c.Revision > z.PublishedRevision &&
			c.Status != StatusPublished && c.Status != StatusWithdrawn &&
			c.Status != StatusStopped && c.Status != StatusRecovered {
			return true
		}
	}
	return false
}

func findStageRevision(z *ZoneState, planID string, stage int) *Revision {
	for _, r := range z.Revisions {
		if r.PlanID == planID && r.StageIndex == stage {
			return r
		}
	}
	return nil
}

// validateStageSpecs 校验阶段声明：权重严格递增、末阶段 100，门槛合法。
func validateStageSpecs(stages []StageSpec) *Error {
	if len(stages) == 0 {
		return errf(KindValidation, "rollout plan requires at least one stage")
	}
	for i, st := range stages {
		if i < len(stages)-1 {
			if st.TargetWeight < 1 || st.TargetWeight > 99 {
				return errf(KindValidation, "stage %d weight must be in 1..99 (only the final stage may be 100)", i)
			}
		} else if st.TargetWeight != 100 {
			return errf(KindValidation, "final stage weight must be 100, got %d", st.TargetWeight)
		}
		if i > 0 && st.TargetWeight <= stages[i-1].TargetWeight {
			return errf(KindValidation, "stage weights must be strictly increasing: stage %d weight %d is not above %d",
				i, st.TargetWeight, stages[i-1].TargetWeight)
		}
		if st.MinObservation < 0 {
			return errf(KindValidation, "stage %d min observation must be >= 0", i)
		}
		if st.HealthThreshold <= 0 || st.HealthThreshold > 1 {
			return errf(KindValidation, "stage %d health threshold must be in (0,1]", i)
		}
		if st.MinSampleCount < 1 {
			return errf(KindValidation, "stage %d requires at least 1 sample", i)
		}
	}
	return nil
}

// splittableKeys 返回可分流记录集的键：基视图与目标视图都存在、类型允许多值
// （CNAME/SOA 只能有一条记录，无法按权重并存）、内容不同，且目标侧至少有一个
// 新增记录值（旧记录值才有地方让出新权重）。
func splittableKeys(base, target map[string]RecordSet) []string {
	var keys []string
	for key, trs := range target {
		brs, ok := base[key]
		if !ok || setsEqual(brs, trs) {
			continue
		}
		if trs.Type == TypeCNAME || trs.Type == TypeSOA {
			continue
		}
		old := map[string]bool{}
		for _, r := range brs.Records {
			old[r] = true
		}
		for _, r := range trs.Records {
			if !old[r] {
				keys = append(keys, key)
				break
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// buildStageView 生成目标权重 W 下的完整区域视图（蓝绿模型）：
//   - W == 0：等同基视图；W == 100：等同目标视图；
//   - 中间权重：可分流记录集取新旧记录值并集；当前线上（基）记录值分摊 100-W，
//     目标修订新增的记录值分摊 W；目标侧已删除的旧值在末阶段前继续保留。
//   - 无法分流的变更（整集删除、纯删除记录值、CNAME/SOA 等单值类型）
//     在末阶段前保持基视图内容，因此中间视图必然通过区域校验。
func buildStageView(base, target map[string]RecordSet, w int) map[string]RecordSet {
	if w == 0 {
		return cloneView(base)
	}
	if w == 100 {
		return cloneView(target)
	}
	splittable := map[string]bool{}
	for _, k := range splittableKeys(base, target) {
		splittable[k] = true
	}
	out := map[string]RecordSet{}
	for key, brs := range base {
		trs, inTarget := target[key]
		if !inTarget || !splittable[key] {
			out[key] = cloneRS(brs) // 整集删除 / 无法分流：末阶段前保持旧内容
			continue
		}
		newSet := map[string]bool{} // 目标侧新增值
		for _, r := range trs.Records {
			newSet[r] = true
		}
		var newOnly []string
		for _, r := range brs.Records {
			delete(newSet, r) // 共有值不再算新增
		}
		for r := range newSet {
			newOnly = append(newOnly, r)
		}
		sort.Strings(newOnly)

		// 记录值并集：全部旧值（按基视图顺序）+ 排序后的新增值。
		records := make([]string, 0, len(brs.Records)+len(newOnly))
		records = append(records, brs.Records...)
		records = append(records, newOnly...)

		oldW := distributeWeight(100-w, len(brs.Records))
		newW := distributeWeight(w, len(newOnly))
		weights := make([]WeightedRecord, 0, len(brs.Records)+len(newOnly))
		for i, r := range brs.Records {
			weights = append(weights, WeightedRecord{Record: r, Weight: oldW[i]})
		}
		for i, r := range newOnly {
			weights = append(weights, WeightedRecord{Record: r, Weight: newW[i]})
		}
		sort.Slice(weights, func(i, j int) bool { return weights[i].Record < weights[j].Record })
		out[key] = RecordSet{
			Name: brs.Name, Type: brs.Type, TTL: trs.TTL,
			Records: records, Weights: weights,
		}
	}
	// 目标侧全新增的记录集只在末阶段（w==100，已提前返回）出现。
	return out
}

// distributeWeight 把 total 尽可能均分给 n 条记录，余数给靠前的记录，保证总和精确。
func distributeWeight(total, n int) []int {
	if n <= 0 {
		return nil
	}
	out := make([]int, n)
	base, rem := total/n, total%n
	for i := range out {
		out[i] = base
		if i < rem {
			out[i]++
		}
	}
	return out
}

// validateWeightedView 校验中间阶段视图的权重：加权记录必须存在、不重复、
// 权重非负，且每个记录集的权重合计 100。
func validateWeightedView(view map[string]RecordSet) *Error {
	var issues []string
	for _, rs := range view {
		if len(rs.Weights) == 0 {
			continue
		}
		known := map[string]bool{}
		for _, r := range rs.Records {
			known[r] = true
		}
		sum := 0
		seen := map[string]bool{}
		for _, wr := range rs.Weights {
			switch {
			case !known[wr.Record]:
				issues = append(issues, fmt.Sprintf("%s %s: weight for unknown record %q", rs.Name, rs.Type, wr.Record))
			case wr.Weight < 0:
				issues = append(issues, fmt.Sprintf("%s %s: negative weight for %q", rs.Name, rs.Type, wr.Record))
			case seen[wr.Record]:
				issues = append(issues, fmt.Sprintf("%s %s: duplicate weight for %q", rs.Name, rs.Type, wr.Record))
			}
			seen[wr.Record] = true
			sum += wr.Weight
		}
		if sum != 100 {
			issues = append(issues, fmt.Sprintf("%s %s: weights sum to %d, want 100", rs.Name, rs.Type, sum))
		}
	}
	if len(issues) > 0 {
		sort.Strings(issues)
		return validationErr(issues)
	}
	return nil
}

// publishStageView 在区域锁内分配新修订号、写阶段完整视图与唯一 outbox，
// 并把变更状态置为 rolling。全部为内存操作，由外层 mutate 统一原子持久化，
// 因此失败不会留下部分记录集或跳过修订号。
func (s *Service) publishStageView(z *ZoneState, plan *RolloutPlan, idx int,
	stage *StageRuntime, chg *Change, now time.Time) int64 {
	baseView := viewAt(z, plan.BaseRevision)
	targetView := viewAt(z, plan.TargetRevision)
	view := buildStageView(baseView, targetView, stage.Spec.TargetWeight)

	z.Seq++
	revNum := z.HeadRevision + 1
	rev := &Revision{
		Zone: z.Name, Number: revNum, ChangeID: chg.ID, Base: z.PublishedRevision,
		RecordSets: sortedSets(view), PlanID: plan.ID, StageIndex: idx,
		Seq: z.Seq, CreatedAt: now,
	}
	z.HeadRevision = revNum
	z.Revisions = append(z.Revisions, rev)

	z.Seq++
	entry := &OutboxEntry{
		ID: outboxID(z.Name, revNum), Zone: z.Name, Revision: revNum,
		RecordSets: rev.RecordSets, PlanID: plan.ID, StageIndex: idx,
		Seq: z.Seq, CreatedAt: now,
	}
	z.Outbox = append(z.Outbox, entry)
	z.PublishedRevision = revNum

	stage.Published = true
	stage.PublishedAt = now
	stage.Revision = revNum
	stage.OutboxID = entry.ID

	if chg.Status == StatusApproved {
		chg.Status = StatusRolling
	}
	chg.UpdatedAt = now
	return revNum
}

// recoverPlan 在健康不达标时原子地创建并发布一个恢复到上一稳定权重的新变更，
// 不改写任何历史；恢复内容是稳定点修订（基线或上一健康阶段）的完整视图快照，
// 因此权重也精确回退。计划、阶段与目标变更同时进入终态。
func (s *Service) recoverPlan(z *ZoneState, plan *RolloutPlan, chg *Change,
	stats *HealthStats, now time.Time) *RecoveryLink {
	stableSets := viewAt(z, plan.StableRevision)
	headView := viewAt(z, z.HeadRevision)
	ops := diffOps(headView, stableSets)
	for i := range ops {
		ops[i].RecordSet.Weights = nil // 权重只属于阶段视图快照，Ops 中不携带
	}

	z.Seq++
	revNum := z.HeadRevision + 1
	rec := &Change{
		ID:           changeID(z.Name, revNum),
		Zone:         z.Name,
		Revision:     revNum,
		BaseRevision: z.HeadRevision,
		Ops:          ops,
		Submitter:    autoRecoverySubmitter,
		Comment: fmt.Sprintf("auto-recovery for plan %s stage %d (health %.3f < %.3f, %d/%d samples)",
			plan.ID, plan.CurrentStage, stats.Ratio,
			plan.Stages[plan.CurrentStage].Spec.HealthThreshold,
			stats.HealthyCount, stats.Total),
		Policy:     z.Policy,
		Status:     StatusPublished, // 系统自动恢复：创建即审批通过并发布
		PlanID:     plan.ID,
		RollbackOf: plan.StableRevision,
		Seq:        z.Seq,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	z.HeadRevision = revNum
	z.Changes = append(z.Changes, rec)
	stableView := sortedSets(stableSets)
	z.Revisions = append(z.Revisions, &Revision{
		Zone: z.Name, Number: revNum, ChangeID: rec.ID, Base: rec.BaseRevision,
		RecordSets: stableView, RollbackOf: plan.StableRevision,
		PlanID: plan.ID, StageIndex: -1, // -1 表示计划附属的恢复修订，不是任何阶段
		Seq: z.Seq, CreatedAt: now,
	})

	z.Seq++
	z.Outbox = append(z.Outbox, &OutboxEntry{
		ID: outboxID(z.Name, revNum), Zone: z.Name, Revision: revNum,
		RecordSets: stableView, PlanID: plan.ID,
		Seq: z.Seq, CreatedAt: now,
	})
	z.PublishedRevision = revNum

	stage := &plan.Stages[plan.CurrentStage]
	stage.Reverted = true
	stage.RevertedToRevision = revNum
	plan.Status = PlanRecovered
	plan.RecoveryChangeID = rec.ID
	plan.UpdatedAt = now
	chg.Status = StatusRecovered
	chg.RecoveredBy = rec.ID
	chg.UpdatedAt = now

	return &RecoveryLink{
		PlanID: plan.ID, FromStage: plan.CurrentStage,
		FromRevision: stage.Revision, StableRevision: plan.StableRevision,
		RecoveryChangeID: rec.ID, RecoveryRevision: revNum,
		PlanStatus: PlanRecovered,
	}
}

// markSuperseded 在计划被后来的发布挤占时把它置为不可推进（保留审计痕迹）。
func markSuperseded(z *ZoneState, plan *RolloutPlan, chg *Change, now time.Time) {
	plan.Status = PlanStopped
	plan.Superseded = true
	plan.UpdatedAt = now
	if chg != nil {
		chg.Status = StatusStopped
		chg.UpdatedAt = now
	}
}

// healthStats 统计绑定到指定计划、阶段修订的样本；
// 修订必须属于该计划，其他修订/阶段（含旧阶段重放）的样本天然被排除。
func healthStats(z *ZoneState, planID string, revision int64, stage int) HealthStats {
	stats := HealthStats{Stage: stage, Revision: revision}
	rev := findRevision(z, revision)
	if rev == nil || rev.PlanID != planID || rev.StageIndex != stage {
		return stats
	}
	for _, sm := range z.HealthSamples {
		if sm.Zone != z.Name || sm.Revision != revision || sm.Stage != stage {
			continue
		}
		stats.Total++
		if sm.Healthy {
			stats.HealthyCount++
		}
		if stats.EarliestAt.IsZero() || sm.At.Before(stats.EarliestAt) {
			stats.EarliestAt = sm.At
		}
		if stats.LatestAt.IsZero() || sm.At.After(stats.LatestAt) {
			stats.LatestAt = sm.At
		}
	}
	if stats.Total > 0 {
		stats.Ratio = float64(stats.HealthyCount) / float64(stats.Total)
	}
	return stats
}

// recoveryChain 沿恢复链行走：本计划 -> 恢复修订 -> 以该修订为基线的下一个计划 -> ...
func recoveryChain(z *ZoneState, start *RolloutPlan) []RecoveryLink {
	var links []RecoveryLink
	plan := start
	for plan != nil {
		if plan.RecoveryChangeID == "" {
			break
		}
		link := RecoveryLink{
			PlanID: plan.ID, PlanStatus: plan.Status,
			RecoveryChangeID: plan.RecoveryChangeID,
		}
		if chg := findChange(z, plan.RecoveryChangeID); chg != nil {
			link.RecoveryRevision = chg.Revision
			link.StableRevision = chg.RollbackOf
		}
		if i := plan.revertedStage(); i >= 0 {
			link.FromStage = i
			link.FromRevision = plan.Stages[i].Revision
		}
		links = append(links, link)

		var next *RolloutPlan
		for _, p := range z.Plans {
			if p.ID != plan.ID && p.BaseRevision == link.RecoveryRevision {
				next = p
				break
			}
		}
		plan = next
	}
	return links
}

func (p *RolloutPlan) revertedStage() int {
	for i := range p.Stages {
		if p.Stages[i].Reverted {
			return i
		}
	}
	return -1
}

// cloneRS 深拷贝单个记录集。
func cloneRS(rs RecordSet) RecordSet {
	cp := RecordSet{Name: rs.Name, Type: rs.Type, TTL: rs.TTL}
	cp.Records = append([]string(nil), rs.Records...)
	if rs.Weights != nil {
		cp.Weights = append([]WeightedRecord(nil), rs.Weights...)
	}
	return cp
}

// cloneView 深拷贝一个区域视图。
func cloneView(view map[string]RecordSet) map[string]RecordSet {
	out := make(map[string]RecordSet, len(view))
	for k, v := range view {
		out[k] = cloneRS(v)
	}
	return out
}
