// Package selector 用 Cookie 决定请求转发到哪个后端，并提供一个选择页面。
//
// 约定：Cookie proxy_name 的值就是配置里 proxy_list[].name。
// 没有选择（或选择已失效）时，请求一律被重定向到 /_select。
//
// /_select 下的整个命名空间都由本服务持有，不会转发给后端：
//   - GET  /_select          选择页，列出全部后端
//   - POST /_select          记录选择
//   - POST /_select/add      新增一个后端
//   - POST /_select/update   修改一个后端（可改名）
//   - POST /_select/delete   删除一个后端
//
// 增删改会写回配置文件并立即重建路由表；Store 为 nil 时页面只读，
// 三个写接口不注册（访问返回 404）。
package selector

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/liuhu016/cookie-proxy/internal/config"
	"github.com/liuhu016/cookie-proxy/internal/logging"
	"github.com/liuhu016/cookie-proxy/internal/proxy"
)

const (
	// Path 是选择页的路径，该路径不会被转发到后端。
	Path = "/_select"
	// 管理接口路径，只接受 POST。
	AddPath    = Path + "/add"
	UpdatePath = Path + "/update"
	DeletePath = Path + "/delete"
	// adminPrefix 是 /_select 下保留给本服务的命名空间。
	adminPrefix = Path + "/"

	// CookieName 是承载所选后端名称的 Cookie。
	CookieName = proxy.SelectionCookie
	// cookieMaxAge 是选择的保留时长（秒），默认 30 天。
	cookieMaxAge = 30 * 24 * 60 * 60
)

// 未放行的原因分类，只写入日志。
const (
	ReasonNoSelection = "no_selection"
	ReasonUnknownName = "unknown_proxy_name"
	// ReasonCrossOrigin 表示管理接口收到了来自其它站点的请求。
	ReasonCrossOrigin = "cross_origin"
	// ReasonAdminFailed 表示一次增删改没做成（校验失败、文件写不进去等）。
	ReasonAdminFailed = "admin_failed"
)

// adminActions 是允许的管理动作，用于挡掉 /_select 下的未知子路径。
var adminActions = map[string]bool{
	AddPath:    true,
	UpdatePath: true,
	DeletePath: true,
}

// Selector 持有路由表的只读视图；store 非空时可写（可增删改后端）。
type Selector struct {
	router *proxy.Router
	store  *config.Store
	log    *slog.Logger
}

// New 基于已构建的路由表创建选择器。store 为 nil 表示只读模式：
// 页面不提供增删改，写接口也不注册。
func New(router *proxy.Router, store *config.Store, log *slog.Logger) *Selector {
	return &Selector{router: router, store: store, log: log}
}

// Register 注册选择页与管理接口的路由。其它方法由 Middleware 拦成 405，
// 避免落到兜底转发（那里没有选定的后端）。
func (s *Selector) Register(e *gin.Engine) {
	e.GET(Path, s.page)
	e.HEAD(Path, s.page)
	e.POST(Path, s.choose)

	if s.store == nil {
		// 只读模式不注册写接口；Middleware 会把它们当作未知子路径返回 404。
		return
	}
	e.POST(AddPath, s.addProxy)
	e.POST(UpdatePath, s.updateProxy)
	e.POST(DeletePath, s.deleteProxy)
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
				s.methodNotAllowed(c, "GET, HEAD, POST")
			}
			return
		}
		if path := c.Request.URL.EscapedPath(); strings.HasPrefix(path, adminPrefix) {
			s.serveAdmin(c, path)
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

// serveAdmin 处理 /_select/<action>：只有可写模式下的三个已知动作、且方法为 POST，
// 才会交给对应的处理器。其它情况一律由本服务应答，绝不落到兜底转发。
func (s *Selector) serveAdmin(c *gin.Context, path string) {
	if !adminActions[path] || s.store == nil {
		c.String(http.StatusNotFound, "404 Not Found\n")
		c.Abort()
		return
	}
	if c.Request.Method != http.MethodPost {
		s.methodNotAllowed(c, "POST")
		return
	}
	c.Next()
}

func (s *Selector) methodNotAllowed(c *gin.Context, allow string) {
	c.Header("Allow", allow)
	c.String(http.StatusMethodNotAllowed, "405 Method Not Allowed\n")
	c.Abort()
}

// reject 把未带有效选择的请求（含 curl 等非浏览器客户端）引到选择页，
// 并用 next 记住原地址，选完就能跳回来。
func (s *Selector) reject(c *gin.Context, reason string, clearCookie bool) {
	c.Set(logging.CtxKeyReason, reason)
	if clearCookie {
		s.clearCookie(c)
	}
	c.Header("Cache-Control", "no-store")

	// 非 GET/HEAD 的请求体带不过去，用 303 明确要求改用 GET 访问 next；
	// GET/HEAD 保持 302，避免无谓地改变既有行为。
	status := http.StatusFound
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		status = http.StatusSeeOther
	}

	c.Redirect(status, Path+"?next="+url.QueryEscape(c.Request.URL.RequestURI()))
	c.Abort()
}

// page 展示可选后端列表。
func (s *Selector) page(c *gin.Context) {
	s.render(c, http.StatusOK, cleanNext(c.Query("next")), "")
}

// choose 记录选择并跳回原来的地址。
func (s *Selector) choose(c *gin.Context) {
	// 没有可用的 next 时回到站点根路径；选择页自身会被 cleanNext 过滤掉，
	// 免得选完又跳回选择页。
	next := cleanNext(c.PostForm("next"))
	if next == "" {
		next = "/"
	}

	route, ok := s.router.Lookup(c.PostForm("name"))
	if !ok {
		c.Set(logging.CtxKeyReason, ReasonUnknownName)
		s.render(c, http.StatusBadRequest, cleanNext(c.PostForm("next")),
			"未知的代理名称，请从下面的列表中选择。")
		return
	}

	s.setCookie(c, route.Name)
	c.Set(logging.CtxKeyProxy, route.Name)
	// 303 让浏览器用 GET 访问 next，而不是重复 POST。
	c.Redirect(http.StatusSeeOther, next)
}

// addProxy 新增一条后端：校验 → 落盘 → 重建路由表 → 跳回选择页。
func (s *Selector) addProxy(c *gin.Context) {
	if !s.checkOrigin(c) {
		return
	}

	name := strings.TrimSpace(c.PostForm("name"))
	target := strings.TrimSpace(c.PostForm("proxy_pass"))
	if err := config.ValidateName(name); err != nil {
		s.fail(c, "新增失败："+err.Error())
		return
	}
	if err := config.ValidateProxyPass(target); err != nil {
		s.fail(c, fmt.Sprintf("新增失败：后端地址 %q 无效：%s", target, err))
		return
	}

	next, err := s.store.Update(func(cfg *config.Config) error {
		if indexOf(cfg.ProxyList, name) >= 0 {
			return fmt.Errorf("名称 %q 已存在", name)
		}
		cfg.ProxyList = append(cfg.ProxyList, config.Proxy{Name: name, ProxyPass: target})
		return nil
	})
	if err != nil {
		s.fail(c, "新增失败："+err.Error())
		return
	}

	s.publish(next)
	s.log.Info("proxy added", "name", name, "upstream", target)
	s.done(c)
}

// updateProxy 修改一条后端，允许改名；原名称通过 original_name 指定。
func (s *Selector) updateProxy(c *gin.Context) {
	if !s.checkOrigin(c) {
		return
	}

	original := strings.TrimSpace(c.PostForm("original_name"))
	name := strings.TrimSpace(c.PostForm("name"))
	target := strings.TrimSpace(c.PostForm("proxy_pass"))
	if err := config.ValidateName(name); err != nil {
		s.fail(c, "修改失败："+err.Error())
		return
	}
	if err := config.ValidateProxyPass(target); err != nil {
		s.fail(c, fmt.Sprintf("修改失败：后端地址 %q 无效：%s", target, err))
		return
	}

	next, err := s.store.Update(func(cfg *config.Config) error {
		idx := indexOf(cfg.ProxyList, original)
		if idx < 0 {
			return fmt.Errorf("名称 %q 不存在（列表可能已被其它操作改动，请刷新页面）", original)
		}
		if name != original && indexOf(cfg.ProxyList, name) >= 0 {
			return fmt.Errorf("名称 %q 已存在", name)
		}
		cfg.ProxyList[idx] = config.Proxy{Name: name, ProxyPass: target}
		return nil
	})
	if err != nil {
		s.fail(c, "修改失败："+err.Error())
		return
	}

	s.publish(next)
	// 改名后指向的还是同一个后端，顺手把 Cookie 改过去，免得用户被迫重选。
	if cur, _ := c.Cookie(CookieName); cur == original && name != original {
		s.setCookie(c, name)
	}
	s.log.Info("proxy updated", "old_name", original, "name", name, "upstream", target)
	s.done(c)
}

// deleteProxy 删除一条后端。
func (s *Selector) deleteProxy(c *gin.Context) {
	if !s.checkOrigin(c) {
		return
	}

	name := strings.TrimSpace(c.PostForm("name"))
	next, err := s.store.Update(func(cfg *config.Config) error {
		idx := indexOf(cfg.ProxyList, name)
		if idx < 0 {
			return fmt.Errorf("名称 %q 不存在（列表可能已被其它操作改动，请刷新页面）", name)
		}
		cfg.ProxyList = append(cfg.ProxyList[:idx], cfg.ProxyList[idx+1:]...)
		return nil
	})
	if err != nil {
		s.fail(c, "删除失败："+err.Error())
		return
	}

	s.publish(next)
	// 被删掉的正是当前选择：直接清掉 Cookie，省一次"选择已失效"的跳转。
	if cur, _ := c.Cookie(CookieName); cur == name {
		s.clearCookie(c)
	}
	s.log.Info("proxy deleted", "name", name)
	s.done(c)
}

// publish 用新配置重建路由表。Update 已经过完整校验，这里失败只可能是内部错误；
// 此时配置已落盘，重启后仍会按新配置生效，所以只记录不回滚。
func (s *Selector) publish(cfg *config.Config) {
	if err := s.router.Replace(cfg); err != nil {
		s.log.Error("rebuild routes failed after config saved, restart to apply",
			"err", err.Error())
	}
}

// checkOrigin 挡住浏览器发起的跨站表单提交（CSRF）：浏览器跨站 POST 必定带
// Origin，与本站不一致就拒绝。不带这些头的客户端（curl、脚本）直接放行——
// 它们本来就能直接改配置文件，拦在这里没有意义。
func (s *Selector) checkOrigin(c *gin.Context) bool {
	if sameOrigin(c.Request) {
		return true
	}
	c.Set(logging.CtxKeyReason, ReasonCrossOrigin)
	s.log.Warn("cross-origin edit request rejected",
		"path", c.Request.URL.Path, "origin", c.Request.Header.Get("Origin"))
	c.String(http.StatusForbidden, "403 Forbidden: 修改请求必须来自本服务页面\n")
	c.Abort()
	return false
}

func sameOrigin(req *http.Request) bool {
	if origin := req.Header.Get("Origin"); origin != "" {
		return sameHost(origin, req.Host)
	}
	if referer := req.Header.Get("Referer"); referer != "" {
		return sameHost(referer, req.Host)
	}
	return true
}

// sameHost 比较一个绝对 URL（Origin 或 Referer）的主机与请求的 Host。
func sameHost(raw, host string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// fail 把一次失败的修改渲染回选择页，并说明原因。
func (s *Selector) fail(c *gin.Context, msg string) {
	c.Set(logging.CtxKeyReason, ReasonAdminFailed)
	s.log.Warn("proxy edit failed", "path", c.Request.URL.Path, "err", msg)
	s.render(c, http.StatusBadRequest, cleanNext(c.PostForm("next")), msg)
}

// done 用 303 结束一次修改（PRG：刷新页面不会重复提交）。
// 表单带了 next 就回到用户原来想访问的地址，否则留在选择页。
func (s *Selector) done(c *gin.Context) {
	target := Path
	if next := cleanNext(c.PostForm("next")); next != "" {
		target = next
	}
	c.Redirect(http.StatusSeeOther, target)
}

// indexOf 返回名称在列表中的下标，找不到返回 -1。
func indexOf(list []config.Proxy, name string) int {
	for i, p := range list {
		if p.Name == name {
			return i
		}
	}
	return -1
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

	// Editable 为 false 时页面不提供增删改。
	Editable     bool
	AddAction    string
	UpdateAction string
	DeleteAction string
}

func (s *Selector) render(c *gin.Context, status int, next, errMsg string) {
	current, _ := c.Cookie(CookieName)
	if _, ok := s.router.Lookup(current); !ok {
		current = ""
	}

	routes := s.router.Routes()
	data := pageData{
		Action:       Path,
		Next:         next,
		Current:      current,
		Error:        errMsg,
		Editable:     s.store != nil,
		AddAction:    AddPath,
		UpdateAction: UpdatePath,
		DeleteAction: DeletePath,
		Options:      make([]option, 0, len(routes)),
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
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; form-action 'self'")
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

// safeNext 只接受本站的绝对路径与查询串，杜绝跳转到外部站点。
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
	return raw
}

// cleanNext 归一化表单里的 next：只保留"用户原来想访问的地址"，
// 首页与选择页自身（含带查询串、子路径的写法）都视为没有 next（空串）。
func cleanNext(raw string) string {
	next := safeNext(raw)
	if next == "/" {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil {
		return ""
	}
	if u.Path == Path || strings.HasPrefix(u.Path, adminPrefix) {
		return ""
	}
	return next
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
  main { width: 100%; max-width: 46rem; }
  .page-head { display: flex; align-items: center; justify-content: space-between; gap: 1rem; }
  h1 { margin: 0 0 .4rem; font-size: 1.35rem; }
  h2 { margin: 0 0 .6rem; font-size: 1rem; }
  .hint { margin: 0 0 1.25rem; color: #5b6069; font-size: .875rem; }
  .hint code { background: #e7e9ee; border-radius: 4px; padding: .1em .35em; font-size: .9em; }
  .error {
    margin: 0 0 1rem; padding: .6rem .8rem; border-radius: 8px;
    background: #fdeaea; border: 1px solid #f0b4b4; color: #8a1f1f;
  }
  table {
    width: 100%; border-collapse: collapse; background: #fff;
    border: 1px solid #d7dae1; border-radius: 10px; overflow: hidden;
  }
  thead th {
    text-align: left; font-size: .75rem; font-weight: 600; letter-spacing: .02em;
    color: #5b6069; text-transform: uppercase; padding: .6rem .85rem;
    background: #f2f4f8; border-bottom: 1px solid #d7dae1;
  }
  tbody td { padding: .7rem .85rem; border-bottom: 1px solid #eceef2; vertical-align: middle; }
  tbody tr:last-child td { border-bottom: 0; }
  tbody tr.selected { background: #eef3fe; }
  .badge {
    margin-left: .4rem; padding: .05rem .4rem; border-radius: 999px;
    background: #2f6feb; color: #fff; font-size: .7rem; vertical-align: middle;
  }
  .target { color: #5b6069; font-size: .8125rem; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
  td.actions { white-space: nowrap; text-align: right; }
  .pick { display: inline; margin: 0; }
  .pick .name {
    border: 0; background: transparent; padding: 0; font: inherit; font-weight: 600;
    color: #2f6feb; cursor: pointer; text-align: left;
  }
  .pick .name:hover { text-decoration: underline; }
  .pick .name:focus-visible { outline: 3px solid #2f6feb; outline-offset: 2px; border-radius: 4px; }
  .btn {
    padding: .35rem .75rem; border-radius: 8px; font: inherit; font-size: .8125rem;
    cursor: pointer; border: 1px solid transparent;
  }
  .btn-primary { border-color: #2f6feb; background: #2f6feb; color: #fff; }
  .btn-ghost { border-color: #d7dae1; background: transparent; color: inherit; }
  .btn-ghost:hover { background: #f2f4f8; }
  .btn-link { border: 0; background: transparent; color: #2f6feb; padding: .35rem .5rem; }
  .btn-link:hover { text-decoration: underline; }
  .btn-danger-link { border: 0; background: transparent; color: #a13232; padding: .35rem .5rem; cursor: pointer; font: inherit; font-size: .8125rem; }
  .btn-danger-link:hover { text-decoration: underline; }
  .btn:focus-visible, .btn-link:focus-visible, .btn-danger-link:focus-visible {
    outline: 3px solid #2f6feb; outline-offset: 2px;
  }
  form.stack { display: grid; gap: .6rem; }
  form.stack label { display: grid; gap: .25rem; color: #5b6069; font-size: .8125rem; }
  input {
    width: 100%; padding: .5rem .6rem; border: 1px solid #d7dae1; border-radius: 8px;
    background: #fff; color: inherit; font: inherit; font-size: .875rem;
  }
  input:focus-visible { outline: 3px solid #2f6feb; outline-offset: 1px; }
  form.stack .btn-primary { justify-self: start; }
  dialog {
    border: 1px solid #d7dae1; border-radius: 12px; padding: 0; width: min(92vw, 30rem);
    background: #fff; color: #1b1d21;
  }
  dialog::backdrop { background: rgba(0,0,0,.45); }
  .dialog-body { padding: 1.25rem; }
  .dialog-body h2 { margin: 0 0 1rem; }
  .dialog-actions { display: flex; justify-content: flex-end; gap: .5rem; margin-top: 1.1rem; }
  @media (prefers-color-scheme: dark) {
    body { background: #16181d; color: #e6e8ec; }
    .hint, form.stack label, thead th, .target { color: #9aa1ad; }
    .hint code { background: #2a2e36; }
    .error { background: #3a1e1e; border-color: #6b2c2c; color: #f4c2c2; }
    table { background: #1f2229; border-color: #343943; }
    thead th { background: #23262e; border-color: #343943; }
    tbody td { border-color: #2a2e36; }
    tbody tr.selected { background: #1c2740; }
    .btn-ghost { border-color: #343943; }
    .btn-ghost:hover { background: #272b33; }
    input, dialog { background: #16181d; border-color: #343943; }
    dialog { color: #e6e8ec; }
    .btn-danger-link { color: #f4a2a2; }
  }
</style>
</head>
<body>
<main>
  <div class="page-head">
    <h1>选择代理目标</h1>
    {{if .Editable}}<button type="button" class="btn btn-primary" data-add>新增</button>{{end}}
  </div>
  <p class="hint">选择后会写入 Cookie <code>proxy_name</code>，此后本站请求都转发到该后端。随时回到本页可切换。</p>
  {{with .Error}}<p class="error" role="alert">{{.}}</p>{{end}}
  <table>
    <thead>
      <tr>
        <th scope="col">名称</th>
        <th scope="col">后端地址</th>
        {{if .Editable}}<th scope="col" style="text-align:right">操作</th>{{end}}
      </tr>
    </thead>
    <tbody>
    {{range .Options}}
      <tr class="{{if .Selected}}selected{{end}}" aria-current="{{.AriaCurrent}}">
        <td>
          <form class="pick" method="post" action="{{$.Action}}">
            <input type="hidden" name="name" value="{{.Name}}">
            <input type="hidden" name="next" value="{{$.Next}}">
            <button type="submit" class="name" title="切换到该后端">{{.Name}}</button>
          </form>
          {{if .Selected}}<span class="badge">当前</span>{{end}}
        </td>
        <td class="target">{{.Target}}</td>
        {{if $.Editable}}
        <td class="actions">
          <button type="button" class="btn-link"
            data-edit
            data-name="{{.Name}}"
            data-target="{{.Target}}">编辑</button>
          <form method="post" action="{{$.DeleteAction}}" style="display:inline"
            data-delete data-name="{{.Name}}">
            <input type="hidden" name="name" value="{{.Name}}">
            <input type="hidden" name="next" value="{{$.Next}}">
            <button type="submit" class="btn-danger-link">删除</button>
          </form>
        </td>
        {{end}}
      </tr>
    {{end}}
    </tbody>
  </table>
  {{if .Editable}}
  <dialog id="add-dialog">
    <form class="stack" method="post" action="{{.AddAction}}" style="margin:0">
      <div class="dialog-body">
        <h2>新增后端</h2>
        <input type="hidden" name="next" value="{{.Next}}">
        <label>名称
          <input name="name" required pattern="[A-Za-z0-9._-]+" placeholder="alpha">
        </label>
        <label>后端地址
          <input name="proxy_pass" required placeholder="http://127.0.0.1:7500">
        </label>
        <div class="dialog-actions">
          <button type="button" class="btn btn-ghost" data-close-add>取消</button>
          <button type="submit" class="btn btn-primary">添加</button>
        </div>
      </div>
    </form>
  </dialog>

  <dialog id="edit-dialog">
    <form class="stack" method="post" action="{{.UpdateAction}}" style="margin:0">
      <div class="dialog-body">
        <h2>编辑后端</h2>
        <input type="hidden" name="original_name" id="edit-original">
        <input type="hidden" name="next" value="{{.Next}}">
        <label>名称
          <input name="name" id="edit-name" required pattern="[A-Za-z0-9._-]+">
        </label>
        <label>后端地址
          <input name="proxy_pass" id="edit-target" required placeholder="http://127.0.0.1:7500">
        </label>
        <div class="dialog-actions">
          <button type="button" class="btn btn-ghost" data-close>取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </div>
    </form>
  </dialog>
  <script>
    (function () {
      function open(dlg) {
        if (typeof dlg.showModal === 'function') { dlg.showModal(); }
        else { dlg.setAttribute('open', ''); }
      }
      function close(dlg) {
        if (typeof dlg.close === 'function') { dlg.close(); }
        else { dlg.removeAttribute('open'); }
      }

      var addDialog = document.getElementById('add-dialog');
      document.querySelectorAll('[data-add]').forEach(function (btn) {
        btn.addEventListener('click', function () { open(addDialog); });
      });
      addDialog.querySelectorAll('[data-close-add]').forEach(function (btn) {
        btn.addEventListener('click', function () { close(addDialog); });
      });

      var dialog = document.getElementById('edit-dialog');
      var origInput = document.getElementById('edit-original');
      var nameInput = document.getElementById('edit-name');
      var targetInput = document.getElementById('edit-target');

      document.querySelectorAll('[data-edit]').forEach(function (btn) {
        btn.addEventListener('click', function () {
          origInput.value = btn.getAttribute('data-name');
          nameInput.value = btn.getAttribute('data-name');
          targetInput.value = btn.getAttribute('data-target');
          open(dialog);
        });
      });

      dialog.querySelectorAll('[data-close]').forEach(function (btn) {
        btn.addEventListener('click', function () { close(dialog); });
      });

      document.querySelectorAll('form[data-delete]').forEach(function (form) {
        form.addEventListener('submit', function (e) {
          var name = form.getAttribute('data-name');
          if (!window.confirm('确定删除后端 "' + name + '" 吗？此操作不可撤销。')) {
            e.preventDefault();
          }
        });
      });
    })();
  </script>
  {{else}}
  <p class="hint">本服务以只读模式启动（<code>-readonly</code>），不能在页面上增删改后端。</p>
  {{end}}
</main>
</body>
</html>
`))
