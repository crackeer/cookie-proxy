// Package config 负责加载并校验代理服务的 JSON 配置文件。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// 可选字段的默认值。
const (
	DefaultUpstreamTimeoutSeconds = 30
	DefaultShutdownTimeoutSeconds = 10
	DefaultLogFormat              = "text"
)

// namePattern 限制 name 的取值：它既要放进 Cookie 值，也要出现在页面上，
// 收紧字符集可以避免 Cookie 解析歧义与转义问题。
var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Config 是配置文件的顶层结构。
type Config struct {
	Port      int     `json:"port"`
	ProxyList []Proxy `json:"proxy_list"`

	// UpstreamTimeoutSeconds 为转发到后端的超时时间，0 表示不限制。
	UpstreamTimeoutSeconds int `json:"upstream_timeout_seconds"`
	// ShutdownTimeoutSeconds 为优雅退出的宽限期。
	ShutdownTimeoutSeconds int `json:"shutdown_timeout_seconds"`
	// LogFormat 取 "text" 或 "json"。
	LogFormat string `json:"log_format"`
}

// Proxy 是一条"名称 → 后端"映射，名称即 Cookie proxy_name 的取值。
type Proxy struct {
	Name      string `json:"name"`
	ProxyPass string `json:"proxy_pass"`
}

// Load 读取并校验配置文件。任何问题都返回带文件路径的错误。
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置文件 %s 校验失败: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.UpstreamTimeoutSeconds == 0 {
		c.UpstreamTimeoutSeconds = DefaultUpstreamTimeoutSeconds
	}
	if c.UpstreamTimeoutSeconds < 0 {
		// 负数视为"不限制"，统一归一化为 0。
		c.UpstreamTimeoutSeconds = 0
	}
	if c.ShutdownTimeoutSeconds <= 0 {
		c.ShutdownTimeoutSeconds = DefaultShutdownTimeoutSeconds
	}
	if c.LogFormat == "" {
		c.LogFormat = DefaultLogFormat
	}
}

// Validate 收集所有配置问题后一次性返回，避免"改一处再报一处"。
func (c *Config) Validate() error {
	var errs []error

	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("port %d 无效：必须在 1-65535 之间", c.Port))
	}

	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log_format %q 无效：只支持 \"text\" 或 \"json\"", c.LogFormat))
	}

	if len(c.ProxyList) == 0 {
		errs = append(errs, errors.New("proxy_list 不能为空：至少需要配置一个后端"))
	}

	seen := make(map[string]int, len(c.ProxyList))
	for i, p := range c.ProxyList {
		switch {
		case p.Name == "":
			errs = append(errs, fmt.Errorf("proxy_list[%d].name 不能为空", i))
		case !namePattern.MatchString(p.Name):
			errs = append(errs, fmt.Errorf("proxy_list[%d].name %q 无效：只允许字母、数字、. - _", i, p.Name))
		default:
			if first, dup := seen[p.Name]; dup {
				errs = append(errs, fmt.Errorf("proxy_list[%d].name %q 重复：已在 proxy_list[%d] 中定义", i, p.Name, first))
			} else {
				seen[p.Name] = i
			}
		}

		if err := validateProxyPass(p.ProxyPass); err != nil {
			errs = append(errs, fmt.Errorf("proxy_list[%d].proxy_pass %q 无效：%w", i, p.ProxyPass, err))
		}
	}

	return errors.Join(errs...)
}

func validateProxyPass(raw string) error {
	if raw == "" {
		return errors.New("不能为空")
	}
	target, err := url.Parse(raw)
	if err != nil {
		// 例如 "127.0.0.1:80" 会在这里失败，提示同样落在 scheme 上更有帮助。
		return errors.New("不是合法的 URL，必须是 http:// 或 https:// 开头的绝对地址")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return errors.New("必须是 http:// 或 https:// 开头的绝对地址")
	}
	if target.Host == "" {
		return errors.New("缺少主机部分")
	}
	return nil
}
