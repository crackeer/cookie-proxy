package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	return path
}

// 需求中给出的配置必须能原样加载，并填充可选字段的默认值。
func TestLoadMinimalConfigAppliesDefaults(t *testing.T) {
	path := writeTemp(t, `{
  "port": 8888,
  "proxy_list": [
    {
      "name": "openclacky",
      "proxy_pass": "http://10.33.207.152:7500"
    },
    {
      "name": "deepseek",
      "proxy_pass": "http://10.33.207.152:7501"
    }
  ]
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Port != 8888 {
		t.Errorf("port = %d, 期望 8888", cfg.Port)
	}
	if len(cfg.ProxyList) != 2 {
		t.Fatalf("proxy_list 数量 = %d, 期望 2", len(cfg.ProxyList))
	}
	if cfg.ProxyList[0].Name != "openclacky" || cfg.ProxyList[0].ProxyPass != "http://10.33.207.152:7500" {
		t.Errorf("proxy_list[0] 解析错误: %+v", cfg.ProxyList[0])
	}
	if cfg.ProxyList[1].Name != "deepseek" || cfg.ProxyList[1].ProxyPass != "http://10.33.207.152:7501" {
		t.Errorf("proxy_list[1] 解析错误: %+v", cfg.ProxyList[1])
	}
	if cfg.UpstreamTimeoutSeconds != DefaultUpstreamTimeoutSeconds {
		t.Errorf("upstream_timeout_seconds = %d, 期望默认值 %d", cfg.UpstreamTimeoutSeconds, DefaultUpstreamTimeoutSeconds)
	}
	if cfg.ShutdownTimeoutSeconds != DefaultShutdownTimeoutSeconds {
		t.Errorf("shutdown_timeout_seconds = %d, 期望默认值 %d", cfg.ShutdownTimeoutSeconds, DefaultShutdownTimeoutSeconds)
	}
	if cfg.LogFormat != DefaultLogFormat {
		t.Errorf("log_format = %q, 期望默认值 %q", cfg.LogFormat, DefaultLogFormat)
	}
}

// 仓库自带的示例配置必须始终可加载。
func TestLoadExampleConfig(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "config.example.json")); err != nil {
		t.Fatalf("config.example.json 无法加载: %v", err)
	}
}

func TestLoadNegativeTimeoutMeansUnlimited(t *testing.T) {
	path := writeTemp(t, `{"port":1,"upstream_timeout_seconds":-1,"proxy_list":[{"name":"a","proxy_pass":"http://h"}]}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.UpstreamTimeoutSeconds != 0 {
		t.Errorf("upstream_timeout_seconds = %d, 期望归一化为 0", cfg.UpstreamTimeoutSeconds)
	}
}

func TestLoadFileErrors(t *testing.T) {
	t.Run("文件不存在", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "missing.json"))
		if err == nil {
			t.Fatal("期望报错")
		}
		if !strings.Contains(err.Error(), "missing.json") {
			t.Errorf("错误信息应包含文件路径, 实际: %v", err)
		}
	})

	t.Run("非法 JSON", func(t *testing.T) {
		_, err := Load(writeTemp(t, `{"port": 8888,`))
		if err == nil || !strings.Contains(err.Error(), "解析") {
			t.Fatalf("期望解析错误, 实际: %v", err)
		}
	})

	t.Run("未知字段", func(t *testing.T) {
		_, err := Load(writeTemp(t, `{"port":1,"proxypass":"x","proxy_list":[{"name":"a","proxy_pass":"http://h"}]}`))
		if err == nil || !strings.Contains(err.Error(), "proxypass") {
			t.Fatalf("期望提示未知字段 proxypass, 实际: %v", err)
		}
	})

	// 旧的 users 结构必须直接报错，而不是被静默忽略成空配置。
	t.Run("旧版 users 配置被拒绝", func(t *testing.T) {
		_, err := Load(writeTemp(t, `{"port":8888,"users":[{"user":"a","password":"b","proxy_pass":"http://h"}]}`))
		if err == nil || !strings.Contains(err.Error(), "users") {
			t.Fatalf("期望提示未知字段 users, 实际: %v", err)
		}
	})
}

func TestValidate(t *testing.T) {
	validProxy := Proxy{Name: "a", ProxyPass: "http://127.0.0.1:80"}

	tests := []struct {
		name     string
		cfg      Config
		wantErrs []string // 错误信息必须包含的片段
	}{
		{
			name: "合法配置",
			cfg:  Config{Port: 8888, LogFormat: "text", ProxyList: []Proxy{validProxy}},
		},
		{
			name:     "端口为 0",
			cfg:      Config{Port: 0, LogFormat: "text", ProxyList: []Proxy{validProxy}},
			wantErrs: []string{"port 0 无效"},
		},
		{
			name:     "端口超范围",
			cfg:      Config{Port: 70000, LogFormat: "text", ProxyList: []Proxy{validProxy}},
			wantErrs: []string{"port 70000 无效"},
		},
		{
			name:     "proxy_list 为空",
			cfg:      Config{Port: 8888, LogFormat: "text", ProxyList: nil},
			wantErrs: []string{"proxy_list 不能为空"},
		},
		{
			name:     "name 为空",
			cfg:      Config{Port: 8888, LogFormat: "text", ProxyList: []Proxy{{ProxyPass: "http://h"}}},
			wantErrs: []string{"proxy_list[0].name 不能为空"},
		},
		{
			name:     "name 含非法字符",
			cfg:      Config{Port: 8888, LogFormat: "text", ProxyList: []Proxy{{Name: "a b;c", ProxyPass: "http://h"}}},
			wantErrs: []string{"proxy_list[0].name", "只允许字母、数字"},
		},
		{
			name:     "proxy_pass 缺少 scheme",
			cfg:      Config{Port: 8888, LogFormat: "text", ProxyList: []Proxy{{Name: "a", ProxyPass: "127.0.0.1:80"}}},
			wantErrs: []string{"proxy_list[0].proxy_pass", "http:// 或 https://"},
		},
		{
			name:     "proxy_pass 缺少主机",
			cfg:      Config{Port: 8888, LogFormat: "text", ProxyList: []Proxy{{Name: "a", ProxyPass: "http:///path"}}},
			wantErrs: []string{"proxy_list[0].proxy_pass", "缺少主机部分"},
		},
		{
			name:     "proxy_pass 为空",
			cfg:      Config{Port: 8888, LogFormat: "text", ProxyList: []Proxy{{Name: "a"}}},
			wantErrs: []string{"proxy_list[0].proxy_pass", "不能为空"},
		},
		{
			name: "name 重复",
			cfg: Config{Port: 8888, LogFormat: "text", ProxyList: []Proxy{
				validProxy,
				{Name: "a", ProxyPass: "http://other"},
			}},
			wantErrs: []string{`proxy_list[1].name "a" 重复`},
		},
		{
			name:     "log_format 非法",
			cfg:      Config{Port: 8888, LogFormat: "xml", ProxyList: []Proxy{validProxy}},
			wantErrs: []string{`log_format "xml" 无效`},
		},
		{
			name: "多个问题一次报出",
			cfg: Config{Port: 0, LogFormat: "text", ProxyList: []Proxy{
				{Name: "", ProxyPass: "nope"},
			}},
			wantErrs: []string{"port 0 无效", "proxy_list[0].name 不能为空", "proxy_list[0].proxy_pass"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if len(tc.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("期望校验通过, 实际: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望校验失败, 实际通过")
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误信息缺少 %q, 实际: %v", want, err)
				}
			}
		})
	}
}
