// Package proxy 负责把"已选定的后端名称"映射到对应后端并完成请求转发。
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/liuhu016/cookie-proxy/internal/config"
	"github.com/liuhu016/cookie-proxy/internal/logging"
)

// ContextKeyRoute 是选择中间件写入 gin.Context 的路由键。
const ContextKeyRoute = "proxy.route"

// SelectionCookie 是承载后端名称的 Cookie 名，转发前会从 Cookie 头中摘掉。
const SelectionCookie = "proxy_name"

// Route 是一个 proxy_list 条目编译后的转发单元，启动后只读。
type Route struct {
	Name string
	// Target 是配置中的 proxy_pass 原文，用于日志与选择页展示。
	Target string

	proxy *httputil.ReverseProxy
}

// Router 持有全部路由，运行期只读，因此并发访问无需加锁。
type Router struct {
	routes map[string]*Route
	// order 保留配置里的书写顺序，供选择页按序展示。
	order   []*Route
	timeout time.Duration
	log     *slog.Logger
}

// NewRouter 为每个后端预构建一个 ReverseProxy。
func NewRouter(cfg *config.Config, log *slog.Logger) (*Router, error) {
	transport := newTransport()
	r := &Router{
		routes:  make(map[string]*Route, len(cfg.ProxyList)),
		order:   make([]*Route, 0, len(cfg.ProxyList)),
		timeout: time.Duration(cfg.UpstreamTimeoutSeconds) * time.Second,
		log:     log,
	}

	for i, p := range cfg.ProxyList {
		target, err := url.Parse(p.ProxyPass)
		if err != nil {
			return nil, fmt.Errorf("proxy_list[%d].proxy_pass %q 解析失败: %w", i, p.ProxyPass, err)
		}
		if _, dup := r.routes[p.Name]; dup {
			return nil, fmt.Errorf("proxy_list[%d].name %q 重复", i, p.Name)
		}

		route := &Route{Name: p.Name, Target: p.ProxyPass}
		route.proxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				// SetURL 负责拼接 proxy_pass 的路径前缀与原始路径，并保留查询串。
				pr.SetURL(target)
				pr.Out.Host = target.Host
				// 覆盖入站可能伪造的 X-Forwarded-*，而不是信任它们。
				pr.SetXForwarded()
				// 选择状态是本代理的内部实现，没必要让后端看到。
				stripCookie(pr.Out, SelectionCookie)
			},
			Transport:     transport,
			FlushInterval: -1, // 立即 flush，支持 SSE 与流式响应
			ErrorHandler:  r.errorHandler(route),
			ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelError),
		}
		r.routes[p.Name] = route
		r.order = append(r.order, route)
	}

	return r, nil
}

// stripCookie 从请求的 Cookie 头里摘掉指定的一个 Cookie，其余原样保留。
// 直接在原始文本上过滤，避免重新序列化改动其它 Cookie 的字节。
func stripCookie(req *http.Request, name string) {
	raw := req.Header.Values("Cookie")
	if len(raw) == 0 {
		return
	}

	kept := make([]string, 0, len(raw))
	for _, header := range raw {
		pairs := strings.Split(header, ";")
		keptPairs := make([]string, 0, len(pairs))
		for _, pair := range pairs {
			trimmed := strings.TrimSpace(pair)
			if trimmed == "" {
				continue
			}
			if key, _, found := strings.Cut(trimmed, "="); found && strings.TrimSpace(key) == name {
				continue
			}
			keptPairs = append(keptPairs, trimmed)
		}
		if len(keptPairs) > 0 {
			kept = append(kept, strings.Join(keptPairs, "; "))
		}
	}

	if len(kept) == 0 {
		req.Header.Del("Cookie")
		return
	}
	req.Header["Cookie"] = kept
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// Lookup 按名称查找路由。
func (r *Router) Lookup(name string) (*Route, bool) {
	route, ok := r.routes[name]
	return route, ok
}

// Routes 按配置顺序返回全部路由，调用方只读。
func (r *Router) Routes() []*Route { return r.order }

// Len 返回已加载的路由数量。
func (r *Router) Len() int { return len(r.routes) }

// Handler 是兜底处理器：接受任意方法与路径，转发到已选定的后端。
func (r *Router) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		v, exists := c.Get(ContextKeyRoute)
		route, ok := v.(*Route)
		if !exists || !ok {
			// 选择中间件未放行却走到这里，说明装配有误。
			r.log.Error("missing selected route in context", "path", c.Request.URL.Path)
			c.String(http.StatusInternalServerError, "500 Internal Server Error\n")
			return
		}

		c.Set(logging.CtxKeyUpstream, route.Target)

		req := c.Request
		// 协议升级（WebSocket 等）是长连接，套用上游超时会误杀。
		if r.timeout > 0 && !isUpgrade(req) {
			ctx, cancel := context.WithTimeout(req.Context(), r.timeout)
			defer cancel()
			req = req.WithContext(ctx)
		}

		route.proxy.ServeHTTP(c.Writer, req)
	}
}

func (r *Router) errorHandler(route *Route) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, req *http.Request, err error) {
		switch {
		case errors.Is(err, context.Canceled):
			// 客户端已断开，写响应没有意义。
			r.log.Warn("client canceled",
				"proxy", route.Name, "upstream", route.Target, "path", req.URL.Path)
		case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
			r.log.Error("upstream timeout",
				"proxy", route.Name, "upstream", route.Target, "path", req.URL.Path,
				"timeout", r.timeout.String(), "err", err.Error())
			// 响应体保持通用，不泄露后端地址。
			http.Error(w, "504 Gateway Timeout", http.StatusGatewayTimeout)
		default:
			r.log.Error("upstream unreachable",
				"proxy", route.Name, "upstream", route.Target, "path", req.URL.Path,
				"err", err.Error())
			http.Error(w, "502 Bad Gateway", http.StatusBadGateway)
		}
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isUpgrade(req *http.Request) bool {
	if req.Header.Get("Upgrade") == "" {
		return false
	}
	for _, token := range strings.Split(req.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}
