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
	"sync/atomic"
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

// Router 持有全部路由。选择页增删改后端后会整体重建路由表，因此这里用原子指针
// 发布快照：读的一侧（转发请求、渲染选择页）永远拿到一份完整且不再变化的表。
type Router struct {
	snapshot atomic.Pointer[table]

	// transport 在多次重建之间复用，避免改配置时丢掉已建立的连接池。
	transport *http.Transport
	log       *slog.Logger
}

// table 是一份不可变的路由快照：routes 供按名称查找，order 保留配置里的书写顺序。
type table struct {
	routes  map[string]*Route
	order   []*Route
	timeout time.Duration
}

// NewRouter 为每个后端预构建一个 ReverseProxy。
func NewRouter(cfg *config.Config, log *slog.Logger) (*Router, error) {
	r := &Router{transport: newTransport(), log: log}
	t, err := r.build(cfg)
	if err != nil {
		return nil, err
	}
	r.snapshot.Store(t)
	return r, nil
}

// build 按配置构造一份新的路由快照，不改变当前生效的路由表。
func (r *Router) build(cfg *config.Config) (*table, error) {
	timeout := time.Duration(cfg.UpstreamTimeoutSeconds) * time.Second
	t := &table{
		routes:  make(map[string]*Route, len(cfg.ProxyList)),
		order:   make([]*Route, 0, len(cfg.ProxyList)),
		timeout: timeout,
	}

	for i, p := range cfg.ProxyList {
		target, err := url.Parse(p.ProxyPass)
		if err != nil {
			return nil, fmt.Errorf("proxy_list[%d].proxy_pass %q 解析失败: %w", i, p.ProxyPass, err)
		}
		if _, dup := t.routes[p.Name]; dup {
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
			Transport:      r.transport,
			FlushInterval:  -1, // 立即 flush，支持 SSE 与流式响应
			ModifyResponse: rewriteSetCookieDomain,
			ErrorHandler:   r.errorHandler(route, timeout),
			ErrorLog:       slog.NewLogLogger(r.log.Handler(), slog.LevelError),
		}
		t.routes[p.Name] = route
		t.order = append(t.order, route)
	}

	return t, nil
}

// Replace 用新配置整体替换路由表。构造新表失败时原表保持不变，
// 因此调用方可以把它当作一次"要么全生效、要么不动"的操作。
func (r *Router) Replace(cfg *config.Config) error {
	t, err := r.build(cfg)
	if err != nil {
		return err
	}
	r.snapshot.Store(t)
	return nil
}

// table 返回当前生效的路由快照。零值 Router 没有快照，返回空表而不是 panic。
func (r *Router) table() *table {
	if t := r.snapshot.Load(); t != nil {
		return t
	}
	return &table{routes: map[string]*Route{}}
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

// rewriteSetCookieDomain 去掉后端 Set-Cookie 里的 Domain 属性。
//
// 后端往往按自己的主机名（例如 backend.internal）下发 Domain=...，而客户端是通过
// 本代理的地址访问的，两者不一致会导致浏览器直接丢弃该 Cookie，登录态因此失效。
// 去掉 Domain 后，Cookie 变成"仅限当前主机"（host-only），自动绑定到客户端访问
// 本代理所用的主机，无论代理跑在哪个 host/port 都成立。这与 nginx
// proxy_cookie_domain <backend> off 的效果一致。
//
// 其它属性（Path、Secure、HttpOnly、SameSite、Max-Age 等）保持原样。
func rewriteSetCookieDomain(resp *http.Response) error {
	raw := resp.Header.Values("Set-Cookie")
	if len(raw) == 0 {
		return nil
	}
	rewritten := make([]string, len(raw))
	for i, sc := range raw {
		rewritten[i] = stripCookieDomainAttr(sc)
	}
	resp.Header["Set-Cookie"] = rewritten
	return nil
}

// stripCookieDomainAttr 从单条 Set-Cookie 头里删掉 Domain=... 属性，其余原样保留。
// 属性以 ";" 分隔，属性名大小写不敏感（RFC 6265）。
//
// 第一段是 cookie 的 name=value，必须原样保留——即使 cookie 恰好叫 "domain"，
// 那也是 cookie 名而不是 Domain 属性，只有从第二段起的才是属性。
func stripCookieDomainAttr(setCookie string) string {
	parts := strings.Split(setCookie, ";")
	kept := make([]string, 0, len(parts))
	for i, part := range parts {
		if i > 0 {
			attr := strings.TrimSpace(part)
			if key, _, found := strings.Cut(attr, "="); found &&
				strings.EqualFold(strings.TrimSpace(key), "domain") {
				continue
			}
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, ";")
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

// Lookup 按名称查找当前生效的路由。
func (r *Router) Lookup(name string) (*Route, bool) {
	route, ok := r.table().routes[name]
	return route, ok
}

// Routes 按配置顺序返回当前全部路由，调用方只读。
func (r *Router) Routes() []*Route { return r.table().order }

// Len 返回已加载的路由数量。
func (r *Router) Len() int { return len(r.table().routes) }

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
		if t := r.table(); t.timeout > 0 && !isUpgrade(req) {
			ctx, cancel := context.WithTimeout(req.Context(), t.timeout)
			defer cancel()
			req = req.WithContext(ctx)
		}

		route.proxy.ServeHTTP(c.Writer, req)
	}
}

func (r *Router) errorHandler(route *Route, timeout time.Duration) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, req *http.Request, err error) {
		switch {
		case errors.Is(err, context.Canceled):
			// 客户端已断开，写响应没有意义。
			r.log.Warn("client canceled",
				"proxy", route.Name, "upstream", route.Target, "path", req.URL.Path)
		case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
			r.log.Error("upstream timeout",
				"proxy", route.Name, "upstream", route.Target, "path", req.URL.Path,
				"timeout", timeout.String(), "err", err.Error())
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
