package logging

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// 一次请求应产出恰好一条 access 记录，且字段齐全。
func TestAccessLogFields(t *testing.T) {
	var buf bytes.Buffer
	log := NewWith(&buf, "text")

	e := gin.New()
	e.Use(AccessLog(log))
	e.GET("/api/items", func(c *gin.Context) {
		c.Set(CtxKeyProxy, "deepseek")
		c.Set(CtxKeyUpstream, "http://127.0.0.1:80")
		c.String(http.StatusOK, "hello")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/items?page=2", nil)
	req.Header.Set("Cookie", "proxy_name=deepseek; session=secret-session")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	out := buf.String()
	if got := strings.Count(out, "msg=access"); got != 1 {
		t.Fatalf("期望 1 条 access 日志, 实际 %d 条:\n%s", got, out)
	}
	for _, want := range []string{
		"client_ip=", "method=GET", "/api/items?page=2", "status=200",
		"duration_ms=", "bytes=5", "proxy=deepseek", "upstream=http://127.0.0.1:80",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("日志缺少 %q:\n%s", want, out)
		}
	}
}

// 未选择后端的请求：proxy 用占位符，upstream 为空，reason 出现。
func TestAccessLogRequestWithoutSelection(t *testing.T) {
	var buf bytes.Buffer
	log := NewWith(&buf, "text")

	e := gin.New()
	e.Use(AccessLog(log))
	e.GET("/", func(c *gin.Context) {
		c.Set(CtxKeyReason, "no_selection")
		c.String(http.StatusForbidden, "403 Forbidden")
	})

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	out := buf.String()
	for _, want := range []string{"status=403", "proxy=-", "reason=no_selection", `upstream=""`} {
		if !strings.Contains(out, want) {
			t.Errorf("日志缺少 %q:\n%s", want, out)
		}
	}
}

// 日志中不得出现 Authorization 头或 Cookie 原文。
func TestAccessLogNeverLeaksCredentials(t *testing.T) {
	var buf bytes.Buffer
	log := NewWith(&buf, "json")

	e := gin.New()
	e.Use(AccessLog(log))
	e.POST("/login", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("body"))
	req.SetBasicAuth("simple", "hddhsd")
	req.Header.Set("Cookie", "session=secret-session")
	rawAuth := req.Header.Get("Authorization")
	e.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Contains(out, "hddhsd") {
		t.Errorf("日志泄露了密码明文:\n%s", out)
	}
	if strings.Contains(out, rawAuth) || strings.Contains(strings.ToLower(out), "authorization") {
		t.Errorf("日志泄露了 Authorization 头:\n%s", out)
	}
	if strings.Contains(out, "secret-session") || strings.Contains(strings.ToLower(out), "cookie") {
		t.Errorf("日志泄露了 Cookie:\n%s", out)
	}
}

func TestNewWithFormats(t *testing.T) {
	var buf bytes.Buffer
	NewWith(&buf, "json").Info("hi", "k", "v")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Errorf("json 格式应输出 JSON, 实际: %s", buf.String())
	}

	buf.Reset()
	NewWith(&buf, "text").Info("hi", "k", "v")
	if !strings.Contains(buf.String(), "msg=hi k=v") {
		t.Errorf("text 格式输出异常: %s", buf.String())
	}
}
