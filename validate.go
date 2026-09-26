package dnschange

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// labelRE 校验单个 DNS 标签：字母数字开头结尾，中间可含连字符，最长 63。
var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// normalizeName 把名称规范化为小写、以 "." 结尾的 FQDN，并做基本合法性校验。
func normalizeName(name string) (string, *Error) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, ".")
	if n == "" {
		return "", errf(KindValidation, "dns name is empty")
	}
	if len(n) > 253 {
		return "", errf(KindValidation, "dns name %q exceeds 253 characters", name)
	}
	for _, label := range strings.Split(n, ".") {
		if label == "*" { // 允许通配符标签
			continue
		}
		if !labelRE.MatchString(label) {
			return "", errf(KindValidation, "dns name %q has invalid label %q", name, label)
		}
	}
	return n + ".", nil
}

// normalizeOps 规范化一次变更中的所有操作（名称小写 FQDN 化、类型大写），
// 并做与区域视图无关的基础校验。
func normalizeOps(ops []ChangeOp) ([]ChangeOp, *Error) {
	if len(ops) == 0 {
		return nil, errf(KindValidation, "change contains no operations")
	}
	out := make([]ChangeOp, len(ops))
	for i, op := range ops {
		switch op.Kind {
		case OpAdd, OpReplace, OpDelete:
		default:
			return nil, errf(KindValidation, "operation %d has unknown kind %q", i, op.Kind)
		}
		if !knownTypes[op.RecordSet.Type] {
			return nil, errf(KindValidation, "operation %d has unknown record type %q", i, op.RecordSet.Type)
		}
		name, verr := normalizeName(op.RecordSet.Name)
		if verr != nil {
			return nil, verr
		}
		op.RecordSet.Name = name
		if op.Kind != OpDelete {
			if len(op.RecordSet.Records) == 0 {
				return nil, errf(KindValidation, "operation %d (%s %s) has no records", i, op.Kind, name)
			}
			recs := make([]string, len(op.RecordSet.Records))
			for j, r := range op.RecordSet.Records {
				recs[j] = strings.TrimSpace(r)
			}
			op.RecordSet.Records = recs
		}
		out[i] = op
	}
	return out, nil
}

// applyOps 把一组操作应用到区域视图副本上，返回变更后的新视图。
// 操作间冲突（同一键重复操作、新增已存在、替换/删除不存在）属于校验错误。
func applyOps(view map[string]RecordSet, ops []ChangeOp) (map[string]RecordSet, *Error) {
	out := make(map[string]RecordSet, len(view)+len(ops))
	for k, v := range view {
		out[k] = v
	}
	seen := make(map[string]bool, len(ops))
	for i, op := range ops {
		key := op.RecordSet.Key()
		if seen[key] {
			return nil, errf(KindValidation, "operation %d duplicates an earlier operation on %s %s",
				i, op.RecordSet.Name, op.RecordSet.Type)
		}
		seen[key] = true
		_, exists := out[key]
		switch op.Kind {
		case OpAdd:
			if exists {
				return nil, errf(KindValidation, "cannot add %s %s: record set already exists",
					op.RecordSet.Name, op.RecordSet.Type)
			}
			out[key] = op.RecordSet
		case OpReplace:
			if !exists {
				return nil, errf(KindValidation, "cannot replace %s %s: record set does not exist",
					op.RecordSet.Name, op.RecordSet.Type)
			}
			out[key] = op.RecordSet
		case OpDelete:
			if !exists {
				return nil, errf(KindValidation, "cannot delete %s %s: record set does not exist",
					op.RecordSet.Name, op.RecordSet.Type)
			}
			delete(out, key)
		}
	}
	return out, nil
}

// validateZoneView 针对变更后的完整区域视图做校验：
// 名称规范、TTL 边界、记录数据格式与记录冲突（CNAME 排他、顶点约束等）。
func validateZoneView(zone string, view map[string]RecordSet, p Policy) *Error {
	var issues []string

	// 每个名称下出现的类型集合，用于 CNAME 排他检查。
	typesAt := make(map[string]map[RRType]bool)

	for _, rs := range view {
		if !strings.HasSuffix(rs.Name, zone) {
			issues = append(issues, fmt.Sprintf("%s %s: name is outside zone %s", rs.Name, rs.Type, zone))
		}
		if p.MinTTL > 0 && rs.TTL < p.MinTTL {
			issues = append(issues, fmt.Sprintf("%s %s: ttl %d below zone minimum %d", rs.Name, rs.Type, rs.TTL, p.MinTTL))
		}
		if p.MaxTTL > 0 && rs.TTL > p.MaxTTL {
			issues = append(issues, fmt.Sprintf("%s %s: ttl %d above zone maximum %d", rs.Name, rs.Type, rs.TTL, p.MaxTTL))
		}
		if len(rs.Records) == 0 {
			issues = append(issues, fmt.Sprintf("%s %s: record set is empty", rs.Name, rs.Type))
		}
		seenRec := make(map[string]bool, len(rs.Records))
		for _, r := range rs.Records {
			if r == "" {
				issues = append(issues, fmt.Sprintf("%s %s: empty record data", rs.Name, rs.Type))
				continue
			}
			if seenRec[r] {
				issues = append(issues, fmt.Sprintf("%s %s: duplicate record %q", rs.Name, rs.Type, r))
			}
			seenRec[r] = true
			if msg := checkRdata(rs); msg != "" {
				issues = append(issues, msg)
				break // 同类型只报一次格式错误
			}
		}
		if rs.Type == TypeCNAME && len(rs.Records) > 1 {
			issues = append(issues, fmt.Sprintf("%s CNAME: must have exactly one record", rs.Name))
		}
		if rs.Type == TypeCNAME && rs.Name == zone {
			issues = append(issues, "CNAME is not allowed at the zone apex")
		}
		if typesAt[rs.Name] == nil {
			typesAt[rs.Name] = map[RRType]bool{}
		}
		typesAt[rs.Name][rs.Type] = true
	}

	for name, types := range typesAt {
		if types[TypeCNAME] && len(types) > 1 {
			issues = append(issues, fmt.Sprintf("%s: CNAME cannot coexist with other record types", name))
		}
	}

	apex := typesAt[zone]
	if !apex[TypeSOA] {
		issues = append(issues, "zone apex must have an SOA record set")
	}
	if !apex[TypeNS] {
		issues = append(issues, "zone apex must have an NS record set")
	}

	if len(issues) > 0 {
		sort.Strings(issues)
		return validationErr(issues)
	}
	return nil
}

// checkRdata 对记录数据做按类型的基础格式校验，返回空串表示通过。
func checkRdata(rs RecordSet) string {
	for _, r := range rs.Records {
		switch rs.Type {
		case TypeA:
			addr, err := netip.ParseAddr(r)
			if err != nil || !addr.Is4() {
				return fmt.Sprintf("%s A: %q is not a valid IPv4 address", rs.Name, r)
			}
		case TypeAAAA:
			addr, err := netip.ParseAddr(r)
			if err != nil || !addr.Is6() {
				return fmt.Sprintf("%s AAAA: %q is not a valid IPv6 address", rs.Name, r)
			}
		}
	}
	return ""
}

// sortedSets 把区域视图展开为按键排序的记录集切片。
func sortedSets(view map[string]RecordSet) []RecordSet {
	out := make([]RecordSet, 0, len(view))
	for _, rs := range view {
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// setsEqual 比较两个记录集是否内容一致（记录顺序无关）。
func setsEqual(a, b RecordSet) bool {
	if a.Name != b.Name || a.Type != b.Type || a.TTL != b.TTL || len(a.Records) != len(b.Records) {
		return false
	}
	as := append([]string(nil), a.Records...)
	bs := append([]string(nil), b.Records...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// diffOps 计算把 head 视图变为 target 视图所需的操作集合（按键排序，保证确定性）。
func diffOps(head, target map[string]RecordSet) []ChangeOp {
	var ops []ChangeOp
	for key, hs := range head {
		ts, ok := target[key]
		if !ok {
			ops = append(ops, ChangeOp{Kind: OpDelete, RecordSet: RecordSet{Name: hs.Name, Type: hs.Type}})
		} else if !setsEqual(hs, ts) {
			ops = append(ops, ChangeOp{Kind: OpReplace, RecordSet: ts})
		}
	}
	for key, ts := range target {
		if _, ok := head[key]; !ok {
			ops = append(ops, ChangeOp{Kind: OpAdd, RecordSet: ts})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].RecordSet.Key() < ops[j].RecordSet.Key() })
	return ops
}
