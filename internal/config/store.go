package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Clone 返回配置的深拷贝，调用方可以随意修改而不影响原对象。
func (c *Config) Clone() *Config {
	dup := *c
	dup.ProxyList = make([]Proxy, len(c.ProxyList))
	copy(dup.ProxyList, c.ProxyList)
	return &dup
}

// Save 把配置写回 path：先写同目录下的临时文件，落盘后再 rename 覆盖。
// rename 在同一文件系统内是原子的，因此读配置的一方不会看到写了一半的文件。
// 写之前会补默认值并校验，校验不通过时不会碰原文件。
func Save(path string, cfg *Config) error {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	data = append(data, '\n')

	// 覆盖写入不经过原文件的权限位，这里显式继承，避免把 0640 变成 0600。
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("在 %s 创建临时文件失败（配置文件所在目录需要可写）: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("设置临时文件权限失败: %w", err)
	}
	// Sync 之后再 rename，避免断电后留下一个空文件。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("刷盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	tmpName = "" // 已改名成功，不需要再清理
	return nil
}

// Store 持有配置文件路径与当前配置，并把"读-改-写"串行化，
// 避免两个并发修改互相覆盖（后写的会丢掉先写的改动）。
type Store struct {
	path string
	mu   sync.Mutex
	cfg  *Config
}

// NewStore 基于已加载的配置创建 Store，内部保存一份副本。
func NewStore(path string, cfg *Config) *Store {
	return &Store{path: path, cfg: cfg.Clone()}
}

// Snapshot 返回当前配置的副本。
func (s *Store) Snapshot() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Clone()
}

// Update 把 fn 应用到当前配置的副本上，校验并原子落盘成功后才替换内存中的配置，
// 返回落盘后的新配置。任何一步失败都不会改动文件与内存状态。
func (s *Store) Update(fn func(*Config) error) (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cfg.Clone()
	if err := fn(next); err != nil {
		return nil, err
	}
	if err := Save(s.path, next); err != nil {
		return nil, err
	}
	s.cfg = next
	return next.Clone(), nil
}
