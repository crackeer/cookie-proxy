// Package logging 提供 slog 日志器与 Gin 访问日志中间件。
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// gin.Context 中的字段键，由 selector 与 proxy 写入，访问日志中间件读取。
const (
	CtxKeyProxy    = "access.proxy"
	CtxKeyUpstream = "access.upstream"
	CtxKeyReason   = "access.reason"
)

// New 按 format（"text" 或 "json"）构造写往标准输出的日志器。
func New(format string) *slog.Logger {
	return NewWith(os.Stdout, format)
}

// NewWith 允许指定输出目标，便于测试。
func NewWith(w io.Writer, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// AccessLog 在每次请求结束后输出一条访问日志。
// 绝不记录 Authorization 头、Cookie 原文或凭据。
func AccessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		// RequestURI 在中间件链中可能被下游改写，先留存一份。
		target := c.Request.URL.RequestURI()

		c.Next()

		attrs := []any{
			"client_ip", c.ClientIP(),
			"method", c.Request.Method,
			"path", target,
			"status", c.Writer.Status(),
			"duration_ms", float64(time.Since(start).Microseconds()) / 1000,
			"bytes", c.Writer.Size(),
			"proxy", stringValue(c, CtxKeyProxy, "-"),
			"upstream", stringValue(c, CtxKeyUpstream, ""),
		}
		if reason := stringValue(c, CtxKeyReason, ""); reason != "" {
			attrs = append(attrs, "reason", reason)
		}

		log.Info("access", attrs...)
	}
}

func stringValue(c *gin.Context, key, fallback string) string {
	if v, ok := c.Get(key); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}
