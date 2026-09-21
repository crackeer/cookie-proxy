// Package selector 用 Cookie 决定请求转发到哪个后端，并提供一个选择页面。
//
// 约定：Cookie proxy_name 的值就是配置里 proxy_list[].name。
// 没有选择（或选择已失效）时，浏览器请求被重定向到 /_select，其它客户端收到 403。
package selector

import (
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/liuhu016/cookie-proxy/internal/logging"
	"github.com/liuhu016/cookie-proxy/internal/proxy"
)

const (
	// Path 是选择页的路径，该路径不会被转发到后端。
	Path = "/_select"
	// CookieName 是承载所选后端名称的 Cookie。
	CookieName = proxy.SelectionCookie
	// cookieMaxAge 是选择的保留时长（秒），默认 30 天。
	cookieMaxAge = 30 * 24 * 60 * 60
)

// 未放行的原因分类，只写入日志。
const (
	ReasonNoSelection = "no_selection"
	ReasonUnknownName = "unknown_proxy_name"
)

// Selector 持有路由表的只读视图。
type Selector struct {
	router *proxy.Router
	log    *slog.Logger
}

// New 基于已构建的路由表创建选择器。
func New(router *proxy.Router, log *slog.Logger) *Selector {
	return &Selector{router: router, log: log}
}

// Register 注册选择页的路由。其它方法由 Middleware 拦成 405，
// 避免落到兜底转发（那里没有选定的后端）。
func (s *Selector) Register(e *gin.Engine) {
	e.GET(Path, s.page)
	e.HEAD(Path, s.page)
	e.POST(Path, s.choose)
}

// Middleware 读取 Cookie 并把命中的路由写入 gin.Context。
func (s *Selector) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if isSelectPath(c.Request) {
			// 选择页自身不需要（也不能要求）已有选择。
			switch c.Request.Method {
			case http.MethodGet, http.MethodHead, http.MethodPost:
				c.Next()
			default:
				c.Header("Allow", "GET, HEAD, POST")
				c.String(http.StatusMethodNotAllowed, "405 Method Not Allowed\n")
				c.Abort()
			}
			return
		}

		name, err := c.Cookie(CookieName)
		if err != nil || name == "" {
			s.reject(c, ReasonNoSelection, false)
			return
		}

		route, ok := s.router.Lookup(name)
		if !ok {
			// 配置里已经不存在的名称：清掉失效 Cookie，让用户重选。
			s.reject(c, ReasonUnknownName, true)
			return
		}

		c.Set(proxy.ContextKeyRoute, route)
		c.Set(logging.CtxKeyProxy, route.Name)
		c.Next()
	}
}

// reject 让浏览器跳到选择页，其它客户端拿到一条可读的 403。
func (s *Selector) reject(c *gin.Context, reason string, clearCookie bool) {
	c.Set(logging.CtxKeyReason, reason)
	if clearCookie {
		s.clearCookie(c)
	}
	c.Header("Cache-Control", "no-store")

	if wantsHTML(c.Request) {
		c.Redirect(http.StatusFound, Path+"?next="+url.QueryEscape(c.Request.URL.RequestURI()))
		c.Abort()
		return
	}

	c.String(http.StatusForbidden,
		"403 Forbidden: 未选择代理目标。请先在浏览器打开 %s 选择，或在请求中携带 Cookie %s=<name>\n",
		Path, CookieName)
	c.Abort()
}

// page 展示可选后端列表。
func (s *Selector) page(c *gin.Context) {
	s.render(c, http.StatusOK, safeNext(c.Query("next")), "")
}

// choose 记录选择并跳回原来的地址。
func (s *Selector) choose(c *gin.Context) {
	next := safeNext(c.PostForm("next"))

	route, ok := s.router.Lookup(c.PostForm("name"))
	if !ok {
		c.Set(logging.CtxKeyReason, ReasonUnknownName)
		s.render(c, http.StatusBadRequest, next, "未知的代理名称，请从下面的列表中选择。")
		return
	}

	s.setCookie(c, route.Name)
	c.Set(logging.CtxKeyProxy, route.Name)
	// 303 让浏览器用 GET 访问 next，而不是重复 POST。
	c.Redirect(http.StatusSeeOther, next)
}

func (s *Selector) setCookie(c *gin.Context, name string) {
	c.SetSameSite(http.SameSiteLaxMode)
	// 明文 HTTP 下不能加 Secure，否则 Cookie 直接被丢弃。
	c.SetCookie(CookieName, name, cookieMaxAge, "/", "", c.Request.TLS != nil, true)
}

func (s *Selector) clearCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(CookieName, "", -1, "/", "", c.Request.TLS != nil, true)
}

// option 是选择页上的一项。
type option struct {
	Name        string
	Target      string
	Selected    bool
	AriaCurrent string
}

type pageData struct {
	Action  string
	Next    string
	Current string
	Error   string
	Options []option
}

func (s *Selector) render(c *gin.Context, status int, next, errMsg string) {
	current, _ := c.Cookie(CookieName)
	if _, ok := s.router.Lookup(current); !ok {
		current = ""
	}

	routes := s.router.Routes()
	data := pageData{
		Action:  Path,
		Next:    next,
		Current: current,
		Error:   errMsg,
		Options: make([]option, 0, len(routes)),
	}
	for _, route := range routes {
		selected := route.Name == current
		data.Options = append(data.Options, option{
			Name:        route.Name,
			Target:      route.Target,
			Selected:    selected,
			AriaCurrent: boolAttr(selected),
		})
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	c.Status(status)

	if c.Request.Method == http.MethodHead {
		return
	}
	if err := pageTmpl.Execute(c.Writer, data); err != nil {
		// 响应已经开始写出，只能记录。
		s.log.Error("render select page failed", "err", err.Error())
	}
}

func boolAttr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// isSelectPath 用与 Gin 路由一致的原始路径比较，避免 /_%73elect 这类
// 转义写法绕过判断后落进兜底转发。
func isSelectPath(req *http.Request) bool {
	return req.URL.EscapedPath() == Path
}

func wantsHTML(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	return strings.Contains(req.Header.Get("Accept"), "text/html")
}

// safeNext 只接受本站的绝对路径，杜绝跳转到外部站点，也避免选完又跳回选择页。
func safeNext(raw string) string {
	const fallback = "/"
	if raw == "" || raw[0] != '/' {
		return fallback
	}
	// "//host" 与 "/\host" 都会被浏览器解析成外部地址。
	if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, `/\`) {
		return fallback
	}
	if strings.ContainsAny(raw, "\r\n") {
		return fallback
	}
	if raw == Path || strings.HasPrefix(raw, Path+"?") || strings.HasPrefix(raw, Path+"/") {
		return fallback
	}
	return raw
}

var pageTmpl = template.Must(template.New("select").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>选择代理目标</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
    padding: 2rem 1rem; background: #f5f6f8; color: #1b1d21;
    font: 15px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", "Helvetica Neue", Arial,
      "PingFang SC", "Microsoft YaHei", sans-serif;
  }
  main { width: 100%; max-width: 32rem; }
  h1 { margin: 0 0 .4rem; font-size: 1.35rem; }
  .hint { margin: 0 0 1.25rem; color: #5b6069; font-size: .875rem; }
  .hint code { background: #e7e9ee; border-radius: 4px; padding: .1em .35em; font-size: .9em; }
  .error {
    margin: 0 0 1rem; padding: .6rem .8rem; border-radius: 8px;
    background: #fdeaea; border: 1px solid #f0b4b4; color: #8a1f1f;
  }
  ul { list-style: none; margin: 0; padding: 0; display: grid; gap: .6rem; }
  button {
    width: 100%; display: flex; align-items: center; gap: .75rem; text-align: left; cursor: pointer;
    padding: .85rem 1rem; border: 1px solid #d7dae1; border-radius: 10px; background: #fff;
    color: inherit; font: inherit;
  }
  button:hover { border-color: #9aa1ad; }
  button:focus-visible { outline: 3px solid #2f6feb; outline-offset: 2px; }
  button.selected { border-color: #2f6feb; box-shadow: inset 0 0 0 1px #2f6feb; }
  .name { font-weight: 600; }
  .target { margin-left: auto; color: #5b6069; font-size: .8125rem; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  .badge {
    flex: none; padding: .1rem .45rem; border-radius: 999px;
    background: #2f6feb; color: #fff; font-size: .75rem;
  }
  @media (prefers-color-scheme: dark) {
    body { background: #16181d; color: #e6e8ec; }
    .hint { color: #9aa1ad; }
    .hint code { background: #2a2e36; }
    .error { background: #3a1e1e; border-color: #6b2c2c; color: #f4c2c2; }
    button { background: #1f2229; border-color: #343943; }
    button:hover { border-color: #4d5462; }
    .target { color: #9aa1ad; }
  }
</style>
</head>
<body>
<main>
  <h1>选择代理目标</h1>
  <p class="hint">选择后会写入 Cookie <code>proxy_name</code>，此后本站请求都转发到该后端。随时回到本页可切换。</p>
  {{with .Error}}<p class="error" role="alert">{{.}}</p>{{end}}
  <ul>
  {{range .Options}}
    <li>
      <form method="post" action="{{$.Action}}">
        <input type="hidden" name="name" value="{{.Name}}">
        <input type="hidden" name="next" value="{{$.Next}}">
        <button type="submit" class="{{if .Selected}}selected{{end}}" aria-current="{{.AriaCurrent}}">
          <span class="name">{{.Name}}</span>
          <span class="target">{{.Target}}</span>
          {{if .Selected}}<span class="badge">当前</span>{{end}}
        </button>
      </form>
    </li>
  {{end}}
  </ul>
</main>
</body>
</html>
`))
