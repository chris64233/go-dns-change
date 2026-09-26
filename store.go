package dnschange

import (
	"encoding/json"
	"sort"
	"sync"
)

// Store 持久化区域聚合状态与全部历史。
// 实现必须保证 SaveZone 的原子性：要么完整写入，要么保持原状。
type Store interface {
	// LoadZone 加载区域状态；区域不存在时返回 KindNotFound 错误。
	LoadZone(name string) (*ZoneState, error)
	// SaveZone 原子地保存区域状态。
	SaveZone(z *ZoneState) error
	// ListZones 返回全部区域名（排序后）。
	ListZones() ([]string, error)
}

// MemoryStore 是进程内 Store 实现，读写均返回深拷贝，
// 适合测试与嵌入；需要进程级持久化时请使用 FileStore。
type MemoryStore struct {
	mu    sync.RWMutex
	zones map[string]*ZoneState
}

// NewMemoryStore 创建一个空的内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{zones: map[string]*ZoneState{}}
}

func (m *MemoryStore) LoadZone(name string) (*ZoneState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	z, ok := m.zones[name]
	if !ok {
		return nil, errf(KindNotFound, "zone %q not found", name)
	}
	return cloneZoneState(z)
}

func (m *MemoryStore) SaveZone(z *ZoneState) error {
	c, err := cloneZoneState(z)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.zones[z.Name] = c
	return nil
}

func (m *MemoryStore) ListZones() ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.zones))
	for name := range m.zones {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// cloneZoneState 通过 JSON 往返做深拷贝，避免调用方与存储共享可变状态。
func cloneZoneState(z *ZoneState) (*ZoneState, error) {
	data, err := json.Marshal(z)
	if err != nil {
		return nil, err
	}
	var out ZoneState
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
