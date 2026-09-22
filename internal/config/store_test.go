package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig() *Config {
	return &Config{
		Port:                   8888,
		UpstreamTimeoutSeconds: 30,
		ShutdownTimeoutSeconds: 10,
		LogFormat:              "text",
		ProxyList: []Proxy{
			{Name: "alpha", ProxyPass: "http://127.0.0.1:7500"},
		},
	}
}

// Save 写出的文件必须能被 Load 原样读回。
func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg := testConfig()
	cfg.ProxyList = append(cfg.ProxyList, Proxy{Name: "beta", ProxyPass: "http://127.0.0.1:7501/base"})
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got.Port != cfg.Port || got.UpstreamTimeoutSeconds != cfg.UpstreamTimeoutSeconds ||
		got.ShutdownTimeoutSeconds != cfg.ShutdownTimeoutSeconds || got.LogFormat != cfg.LogFormat {
		t.Errorf("顶层字段不一致: %+v", got)
	}
	if len(got.ProxyList) != 2 || got.ProxyList[1] != cfg.ProxyList[1] {
		t.Errorf("proxy_list = %+v, 期望 %+v", got.ProxyList, cfg.ProxyList)
	}

	// 文件应当是可读的 JSON 文本，并以换行结尾。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}
	if !strings.HasSuffix(string(raw), "}\n") {
		t.Errorf("文件应以换行结尾:\n%q", raw)
	}
	if !strings.Contains(string(raw), "\"proxy_list\"") {
		t.Errorf("文件缺少 proxy_list 字段:\n%s", raw)
	}
}

// 覆盖已有文件时要保留原有权限位，并补齐默认值。
func TestSaveKeepsFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"port":8888,"proxy_list":[{"name":"a","proxy_pass":"http://h"}]}`), 0o600); err != nil {
		t.Fatalf("写入初始文件失败: %v", err)
	}

	cfg := &Config{Port: 8888, ProxyList: []Proxy{{Name: "a", ProxyPass: "http://h"}}}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("权限 = %o, 期望 600", got)
	}

	// 落盘的内容应当是补过默认值的完整配置。
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if loaded.UpstreamTimeoutSeconds != DefaultUpstreamTimeoutSeconds || loaded.LogFormat != DefaultLogFormat {
		t.Errorf("默认值未补齐: %+v", loaded)
	}
}

// 校验不通过时不能碰原文件。
func TestSaveRejectsInvalidConfigWithoutTouchingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"port":8888,"proxy_list":[{"name":"a","proxy_pass":"http://h"}]}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("写入初始文件失败: %v", err)
	}

	err := Save(path, &Config{Port: 8888, ProxyList: []Proxy{{Name: "a b", ProxyPass: "nope"}}})
	if err == nil {
		t.Fatal("期望校验失败")
	}
	if !strings.Contains(err.Error(), "只允许字母、数字") || !strings.Contains(err.Error(), "http:// 或 https://") {
		t.Errorf("错误信息应说明全部问题, 实际: %v", err)
	}

	raw, _ := os.ReadFile(path)
	if string(raw) != original {
		t.Errorf("校验失败不应改写文件:\n%s", raw)
	}
	// 也不该留下临时文件。
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("残留文件: %s", e.Name())
		}
	}
}

// 目录不可写时要给出可操作的错误。
func TestSaveReportsUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	if os.Getuid() == 0 {
		t.Skip("以 root 运行时权限位不起作用")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod 失败: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := Save(filepath.Join(dir, "config.json"), testConfig())
	if err == nil {
		t.Fatal("期望写入失败")
	}
	if !strings.Contains(err.Error(), "创建临时文件失败") {
		t.Errorf("错误信息应说明目录不可写, 实际: %v", err)
	}
}

// 深拷贝之后改副本不能影响原对象。
func TestCloneIsDeep(t *testing.T) {
	cfg := testConfig()
	dup := cfg.Clone()

	dup.Port = 9999
	dup.ProxyList[0].Name = "changed"
	if cfg.Port == 9999 || cfg.ProxyList[0].Name == "changed" {
		t.Errorf("修改副本影响到了原对象: %+v", cfg)
	}
}

// Update 成功时落盘并更新内存；失败时两者都不变。
func TestStoreUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, testConfig()); err != nil {
		t.Fatalf("写入初始配置失败: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	store := NewStore(path, loaded)

	got, err := store.Update(func(c *Config) error {
		c.ProxyList = append(c.ProxyList, Proxy{Name: "beta", ProxyPass: "http://127.0.0.1:7501"})
		return nil
	})
	if err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	if len(got.ProxyList) != 2 {
		t.Errorf("返回的配置 = %+v", got.ProxyList)
	}

	fromDisk, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(fromDisk.ProxyList) != 2 || fromDisk.ProxyList[1].Name != "beta" {
		t.Errorf("磁盘上的配置 = %+v", fromDisk.ProxyList)
	}
	if snap := store.Snapshot(); len(snap.ProxyList) != 2 {
		t.Errorf("内存中的配置 = %+v", snap.ProxyList)
	}

	t.Run("fn 报错", func(t *testing.T) {
		_, err := store.Update(func(*Config) error { return os.ErrInvalid })
		if err == nil {
			t.Fatal("期望报错")
		}
		if snap := store.Snapshot(); len(snap.ProxyList) != 2 {
			t.Errorf("失败后内存配置被改动: %+v", snap.ProxyList)
		}
	})

	t.Run("校验失败", func(t *testing.T) {
		_, err := store.Update(func(c *Config) error {
			c.ProxyList = nil
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "proxy_list 不能为空") {
			t.Fatalf("期望校验失败, 实际: %v", err)
		}
		if snap := store.Snapshot(); len(snap.ProxyList) != 2 {
			t.Errorf("失败后内存配置被改动: %+v", snap.ProxyList)
		}
		if fromDisk, _ := Load(path); len(fromDisk.ProxyList) != 2 {
			t.Errorf("失败后磁盘配置被改动: %+v", fromDisk.ProxyList)
		}
	})
}

// Snapshot 返回的是副本，调用方改了不能影响 Store。
func TestStoreSnapshotIsCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, testConfig()); err != nil {
		t.Fatalf("写入初始配置失败: %v", err)
	}
	store := NewStore(path, testConfig())

	snap := store.Snapshot()
	snap.ProxyList[0].Name = "mutated"

	if got := store.Snapshot().ProxyList[0].Name; got != "alpha" {
		t.Errorf("Store 内部配置被外部改动: %q", got)
	}
}

// 并发修改不能互相覆盖：每个改动都必须体现在最终配置里。
func TestStoreUpdateIsSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, testConfig()); err != nil {
		t.Fatalf("写入初始配置失败: %v", err)
	}
	store := NewStore(path, testConfig())

	const n = 8
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		name := string(rune('a' + i))
		go func() {
			_, err := store.Update(func(c *Config) error {
				c.ProxyList = append(c.ProxyList, Proxy{Name: name, ProxyPass: "http://127.0.0.1:7600"})
				return nil
			})
			done <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("并发 Update 失败: %v", err)
		}
	}

	if got := len(store.Snapshot().ProxyList); got != n+1 {
		t.Errorf("proxy_list 数量 = %d, 期望 %d（有改动被覆盖）", got, n+1)
	}
	if got := len(loadMust(t, path).ProxyList); got != n+1 {
		t.Errorf("磁盘上 proxy_list 数量 = %d, 期望 %d", got, n+1)
	}
}

func loadMust(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	return cfg
}
