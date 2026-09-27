package dnschange

import (
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 支持的记录类型。
var supportedTypes = map[string]bool{
	"A": true, "AAAA": true, "CNAME": true, "TXT": true, "MX": true, "NS": true,
}

var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// recordKey 定位区域内的一个记录集。
func recordKey(name, rtype string) string { return name + "|" + rtype }

func refKey(r RecordSetRef) string { return r.Name + "|" + r.Type }

// normalizeZone 规范化区域名为小写 FQDN 并校验语法。
func normalizeZone(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", validationError("zone name is empty")
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	if err := validateLabels(s); err != nil {
		return "", err
	}
	if strings.Count(strings.TrimSuffix(s, "."), ".") < 1 {
		return "", validationError("zone name %q must have at least two labels", s)
	}
	return s, nil
}

// normalizeName 将记录名规范化为区域内的小写 FQDN。
// 裸名称或已经写全的名称都接受；区域外的名称被拒绝。
func normalizeName(raw, zone string) (string, error) {
	s, err := normalizeDomainName(raw, zone)
	if err != nil {
		return "", err
	}
	if s != zone && !strings.HasSuffix(s, "."+zone) {
		return "", validationError("record name %q is outside zone %q", s, zone)
	}
	return s, nil
}

// normalizeDomainName 规范化一个域名形态的输入；允许指向区域外（MX/NS/CNAME 目标）。
func normalizeDomainName(raw, zone string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", validationError("name is empty")
	}
	zoneRoot := strings.TrimSuffix(zone, ".")
	bare := strings.TrimSuffix(s, ".")
	if !strings.HasSuffix(s, ".") {
		// 未以点结尾：恰好等于/落在区域内则视为全名，否则视为相对名拼接到区域。
		if bare == zoneRoot || strings.HasSuffix(bare, "."+zoneRoot) {
			s = bare + "."
		} else {
			s = bare + "." + zone
		}
	}
	if err := validateLabels(s); err != nil {
		return "", err
	}
	return s, nil
}

func validateLabels(fqdn string) error {
	bare := strings.TrimSuffix(fqdn, ".")
	if len(bare) > 253 {
		return validationError("domain name %q exceeds 253 characters", fqdn)
	}
	labels := strings.Split(bare, ".")
	for i, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return validationError("invalid label %q in %q", label, fqdn)
		}
		if label == "*" {
			if i != 0 {
				return validationError("wildcard label only allowed in leftmost position: %q", fqdn)
			}
			continue
		}
		if !labelPattern.MatchString(label) {
			return validationError("invalid label %q in %q", label, fqdn)
		}
	}
	return nil
}

// normalizeRecordSet 规范化并逐条校验一个记录集（不涉及与其他记录集的冲突）。
func normalizeRecordSet(rs RecordSet, zone string) (RecordSet, error) {
	rtype := strings.ToUpper(strings.TrimSpace(rs.Type))
	if !supportedTypes[rtype] {
		return RecordSet{}, validationError("unsupported record type %q", rs.Type)
	}
	name, err := normalizeName(rs.Name, zone)
	if err != nil {
		return RecordSet{}, err
	}
	if len(rs.Values) == 0 {
		return RecordSet{}, validationError("record set %s %s has no values", name, rtype)
	}

	values := make([]string, 0, len(rs.Values))
	seen := make(map[string]bool, len(rs.Values))
	for _, v := range rs.Values {
		nv, err := normalizeValue(rtype, strings.TrimSpace(v), zone)
		if err != nil {
			return RecordSet{}, err
		}
		if seen[nv] {
			return RecordSet{}, validationError("duplicate value %q in %s %s", nv, name, rtype)
		}
		seen[nv] = true
		values = append(values, nv)
	}

	if rtype == "CNAME" && len(values) != 1 {
		return RecordSet{}, validationError("CNAME %s must have exactly one value", name)
	}
	sort.Strings(values)
	return RecordSet{Name: name, Type: rtype, TTL: rs.TTL, Values: values}, nil
}

func normalizeValue(rtype, v, zone string) (string, error) {
	if v == "" {
		return "", validationError("empty %s value", rtype)
	}
	switch rtype {
	case "A":
		ip := net.ParseIP(v)
		if ip == nil || ip.To4() == nil {
			return "", validationError("invalid A record address %q", v)
		}
		return ip.String(), nil
	case "AAAA":
		ip := net.ParseIP(v)
		if ip == nil || ip.To4() != nil || ip.To16() == nil {
			return "", validationError("invalid AAAA record address %q", v)
		}
		return ip.String(), nil
	case "CNAME", "NS":
		name, err := normalizeDomainName(v, zone)
		if err != nil {
			return "", err
		}
		return name, nil
	case "TXT":
		if len(v) > 4096 {
			return "", validationError("TXT value exceeds 4096 characters")
		}
		return v, nil
	case "MX":
		fields := strings.Fields(v)
		if len(fields) != 2 {
			return "", validationError("MX value %q must be \"preference exchange\"", v)
		}
		pref, err := strconv.Atoi(fields[0])
		if err != nil || pref < 0 || pref > 65535 {
			return "", validationError("MX preference in %q must be 0..65535", v)
		}
		host, err := normalizeDomainName(fields[1], zone)
		if err != nil {
			return "", err
		}
		return strconv.Itoa(pref) + " " + host, nil
	default:
		return "", validationError("unsupported record type %q", rtype)
	}
}

// normalizeRef 规范化删除引用。
func normalizeRef(r RecordSetRef, zone string) (RecordSetRef, error) {
	rtype := strings.ToUpper(strings.TrimSpace(r.Type))
	if !supportedTypes[rtype] {
		return RecordSetRef{}, validationError("unsupported record type %q", r.Type)
	}
	name, err := normalizeName(r.Name, zone)
	if err != nil {
		return RecordSetRef{}, err
	}
	return RecordSetRef{Name: name, Type: rtype}, nil
}

// validateFullView 针对变更后的完整区域视图做校验：TTL 边界与记录冲突。
func validateFullView(records map[string]RecordSet, cfg ZoneConfig) error {
	names := make(map[string]map[string]bool)
	for _, rs := range records {
		if cfg.MinTTL > 0 && rs.TTL < cfg.MinTTL {
			return validationError("record set %s %s TTL %d below minimum %d", rs.Name, rs.Type, rs.TTL, cfg.MinTTL)
		}
		if cfg.MaxTTL > 0 && rs.TTL > cfg.MaxTTL {
			return validationError("record set %s %s TTL %d above maximum %d", rs.Name, rs.Type, rs.TTL, cfg.MaxTTL)
		}
		if rs.Type == "CNAME" && rs.Values[0] == rs.Name {
			return validationError("CNAME %s cannot point to itself", rs.Name)
		}
		if names[rs.Name] == nil {
			names[rs.Name] = make(map[string]bool)
		}
		names[rs.Name][rs.Type] = true
	}
	// CNAME 与同名其他任何记录集冲突。
	for name, types := range names {
		if types["CNAME"] && len(types) > 1 {
			return validationError("CNAME at %s conflicts with other record types at the same name", name)
		}
	}
	return nil
}

// applyChange 在基准完整视图上应用一次混合变更，返回新的完整视图（map 形态）。
// 同一批次内重复 upsert/delete、upsert 与 delete 撞键、删除不存在的记录集都会报错。
func applyChange(base []RecordSet, upserts []RecordSet, deletes []RecordSetRef) (map[string]RecordSet, error) {
	view := make(map[string]RecordSet, len(base)+len(upserts))
	for _, rs := range base {
		view[recordKey(rs.Name, rs.Type)] = rs
	}

	upserted := make(map[string]bool, len(upserts))
	for _, rs := range upserts {
		k := recordKey(rs.Name, rs.Type)
		if upserted[k] {
			return nil, validationError("duplicate upsert for %s %s", rs.Name, rs.Type)
		}
		upserted[k] = true
	}
	deleted := make(map[string]bool, len(deletes))
	for _, d := range deletes {
		k := refKey(d)
		if deleted[k] {
			return nil, validationError("duplicate delete for %s %s", d.Name, d.Type)
		}
		if upserted[k] {
			return nil, validationError("record set %s %s is both upserted and deleted in one change", d.Name, d.Type)
		}
		deleted[k] = true
		if _, ok := view[k]; !ok {
			return nil, validationError("cannot delete %s %s: record set does not exist", d.Name, d.Type)
		}
		delete(view, k)
	}
	for _, rs := range upserts {
		view[recordKey(rs.Name, rs.Type)] = rs
	}
	return view, nil
}

// sortedRecords 将视图导出为按 name、type 排序的切片。
func sortedRecords(view map[string]RecordSet) []RecordSet {
	out := make([]RecordSet, 0, len(view))
	for _, rs := range view {
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out
}
