package dnschange

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// FileStore 以每区域一个 JSON 文件的方式持久化状态与历史。
// 写入通过 临时文件 + rename 保证原子性。
type FileStore struct {
	dir string
	mu  sync.Mutex
}

// NewFileStore 创建以 dir 为存储目录的文件存储，目录不存在时自动创建。
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

func zoneFileName(zone string) string {
	var b strings.Builder
	for _, r := range zone {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String() + ".json"
}

func (f *FileStore) path(zone string) string {
	return filepath.Join(f.dir, zoneFileName(zone))
}

func (f *FileStore) LoadZone(name string) (*ZoneState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errf(KindNotFound, "zone %q not found", name)
		}
		return nil, fmt.Errorf("read zone %q: %w", name, err)
	}
	var z ZoneState
	if err := json.Unmarshal(data, &z); err != nil {
		return nil, fmt.Errorf("decode zone %q: %w", name, err)
	}
	return &z, nil
}

func (f *FileStore) SaveZone(z *ZoneState) error {
	data, err := json.MarshalIndent(z, "", "  ")
	if err != nil {
		return fmt.Errorf("encode zone %q: %w", z.Name, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tmp, err := os.CreateTemp(f.dir, ".zone-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, f.path(z.Name)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename zone file: %w", err)
	}
	return nil
}

func (f *FileStore) ListZones() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, fmt.Errorf("list zones: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.dir, e.Name()))
		if err != nil {
			continue
		}
		var z ZoneState
		if err := json.Unmarshal(data, &z); err == nil && z.Name != "" {
			out = append(out, z.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}
