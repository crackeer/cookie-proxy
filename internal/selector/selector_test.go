package selector

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/liuhu016/cookie-proxy/internal/config"
	"github.com/liuhu016/cookie-proxy/internal/logging"
	"github.com/liuhu016/cookie-proxy/internal/proxy"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// syncBuffer 是并发安全的日志缓冲区：日志由服务端 goroutine 写入，测试主
// goroutine 读取，直接用 bytes.Buffer 会触发 -race 报警。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForLog(t *testing.T, b *syncBuffer, substr string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		out := b.String()
		if strings.Contains(out, substr) {
			return out
		}
		if time.Now().After(deadline) {
			t.Errorf("日志中未出现 %q:\n%s", substr, out)
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stack 是一次装配好的服务：前端地址、后端地址，以及可写模式下的配置文件路径。
type stack struct {
	front   *httptest.Server
	backend string
	cfgPath string // 只读模式下为空
}

// newStack 装配「访问日志 → 选路 → 真实转发」链路。
// editable 为 true 时用一份临时配置文件构造可写的选择器（可增删改后端）。
func newStack(t *testing.T, editable bool, logOut io.Writer) *stack {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend:" + r.URL.RequestURI()))
	}))
	t.Cleanup(backend.Close)

	if logOut == nil {
		logOut = io.Discard
	}
	log := logging.NewWith(logOut, "text")

	cfg := &config.Config{
		Port:                   8888,
		UpstreamTimeoutSeconds: 30,
		LogFormat:              "text",
		ProxyList: []config.Proxy{
			{Name: "openclacky", ProxyPass: backend.URL},
			{Name: "deepseek", ProxyPass: backend.URL + "/ds"},
		},
	}
	router, err := proxy.NewRouter(cfg, log)
	if err != nil {
		t.Fatalf("NewRouter 失败: %v", err)
	}

	var store *config.Store
	cfgPath := ""
	if editable {
		cfgPath = filepath.Join(t.TempDir(), "config.json")
		if err := config.Save(cfgPath, cfg); err != nil {
			t.Fatalf("写入初始配置失败: %v", err)
		}
		loaded, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("加载初始配置失败: %v", err)
		}
		store = config.NewStore(cfgPath, loaded)
	}

	sel := New(router, store, log)
	e := gin.New()
	// 与 main.buildEngine 保持一致：透明代理不改写请求路径。
	e.RedirectTrailingSlash = false
	e.RedirectFixedPath = false
	e.HandleMethodNotAllowed = false
	e.UseRawPath = true
	e.UnescapePathValues = false
	e.Use(logging.AccessLog(log), sel.Middleware())
	sel.Register(e)
	e.NoRoute(router.Handler())

	front := httptest.NewServer(e)
	t.Cleanup(front.Close)
	return &stack{front: front, backend: backend.URL, cfgPath: cfgPath}
}

// loadCfg 读回配置文件，用于断言改动确实落盘了。
func (s *stack) loadCfg(t *testing.T) *config.Config {
	t.Helper()
	if s.cfgPath == "" {
		t.Fatal("只读模式下没有配置文件")
	}
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		t.Fatalf("读回配置失败: %v", err)
	}
	return cfg
}

// noRedirectClient 不自动跟随 3xx，便于断言 Location。
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func request(t *testing.T, method, rawURL string, header http.Header, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// formPost 提交一份表单。
func formPost(t *testing.T, rawURL string, form url.Values) *http.Response {
	t.Helper()
	return request(t, http.MethodPost, rawURL,
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		strings.NewReader(form.Encode()))
}

// 浏览器请求没带 Cookie → 跳到选择页，并记住原地址。
func TestBrowserWithoutCookieRedirectsToSelectPage(t *testing.T) {
	var logBuf syncBuffer
	st := newStack(t, false, &logBuf)

	resp := request(t, http.MethodGet, st.front.URL+"/app/page?x=1",
		http.Header{"Accept": {"text/html,application/xhtml+xml"}}, nil)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("状态码 = %d, 期望 302", resp.StatusCode)
	}
	wantLocation := Path + "?next=" + url.QueryEscape("/app/page?x=1")
	if got := resp.Header.Get("Location"); got != wantLocation {
		t.Errorf("Location = %q, 期望 %q", got, wantLocation)
	}
	waitForLog(t, &logBuf, "reason="+ReasonNoSelection)
}

// 非浏览器客户端没带 Cookie → 同样跳到选择页。
func TestNonBrowserWithoutCookieRedirectsToSelectPage(t *testing.T) {
	var logBuf syncBuffer
	st := newStack(t, false, &logBuf)

	resp := request(t, http.MethodGet, st.front.URL+"/api/items", nil, nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("状态码 = %d, 期望 302", resp.StatusCode)
	}
	wantLocation := Path + "?next=" + url.QueryEscape("/api/items")
	if got := resp.Header.Get("Location"); got != wantLocation {
		t.Errorf("Location = %q, 期望 %q", got, wantLocation)
	}
	waitForLog(t, &logBuf, "reason="+ReasonNoSelection)
}

// 没带 Cookie 的非 GET/HEAD 请求 → 303，明确让客户端改用 GET 访问 next。
func TestNonGetWithoutCookieRedirectsWithSeeOther(t *testing.T) {
	st := newStack(t, false, nil)

	resp := request(t, http.MethodPost, st.front.URL+"/api/items",
		http.Header{"Content-Type": {"application/json"}}, strings.NewReader(`{"a":1}`))

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", resp.StatusCode)
	}
	wantLocation := Path + "?next=" + url.QueryEscape("/api/items")
	if got := resp.Header.Get("Location"); got != wantLocation {
		t.Errorf("Location = %q, 期望 %q", got, wantLocation)
	}
}

// Cookie 指向配置里已不存在的名称 → 清掉 Cookie 并让用户重选。
func TestUnknownCookieValueIsCleared(t *testing.T) {
	var logBuf syncBuffer
	st := newStack(t, false, &logBuf)

	resp := request(t, http.MethodGet, st.front.URL+"/x",
		http.Header{
			"Accept": {"text/html"},
			"Cookie": {CookieName + "=gone"},
		}, nil)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("状态码 = %d, 期望 302", resp.StatusCode)
	}
	setCookie := resp.Header.Get("Set-Cookie")
	if !strings.Contains(setCookie, CookieName+"=") || !strings.Contains(setCookie, "Max-Age=0") {
		t.Errorf("失效的 Cookie 未被清除: %q", setCookie)
	}
	waitForLog(t, &logBuf, "reason="+ReasonUnknownName)
}

// 带上有效 Cookie → 请求按名称落到对应后端。
func TestValidCookieRoutesToUpstream(t *testing.T) {
	var logBuf syncBuffer
	st := newStack(t, false, &logBuf)

	for _, tc := range []struct{ name, wantBody string }{
		{"openclacky", "backend:/hello"},
		{"deepseek", "backend:/ds/hello"},
	} {
		resp := request(t, http.MethodGet, st.front.URL+"/hello",
			http.Header{"Cookie": {CookieName + "=" + tc.name}}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态码 = %d, 期望 200", tc.name, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != tc.wantBody {
			t.Errorf("%s 响应体 = %q, 期望 %q", tc.name, body, tc.wantBody)
		}
		waitForLog(t, &logBuf, "proxy="+tc.name)
	}
}

// 选择页要列出全部后端，并标出当前选择。
func TestSelectPageListsProxies(t *testing.T) {
	st := newStack(t, false, nil)

	resp := request(t, http.MethodGet, st.front.URL+Path,
		http.Header{"Cookie": {CookieName + "=deepseek"}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, 期望 text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	for _, want := range []string{"openclacky", "deepseek", st.backend, `method="post"`, `aria-current="true"`} {
		if !strings.Contains(page, want) {
			t.Errorf("选择页缺少 %q", want)
		}
	}
	// 当前选择应该只有一个被标记。
	if got := strings.Count(page, `aria-current="true"`); got != 1 {
		t.Errorf("被标记为当前的项 = %d 个, 期望 1 个", got)
	}
}

// 可写模式下页面提供增删改入口；只读模式下只给选择按钮。
func TestSelectPageEditability(t *testing.T) {
	t.Run("可写", func(t *testing.T) {
		st := newStack(t, true, nil)
		body, _ := io.ReadAll(request(t, http.MethodGet, st.front.URL+Path, nil, nil).Body)
		page := string(body)
		for _, want := range []string{AddPath, UpdatePath, DeletePath, "新增后端", "编辑", "删除"} {
			if !strings.Contains(page, want) {
				t.Errorf("可写模式页面缺少 %q", want)
			}
		}
	})

	t.Run("只读", func(t *testing.T) {
		st := newStack(t, false, nil)
		body, _ := io.ReadAll(request(t, http.MethodGet, st.front.URL+Path, nil, nil).Body)
		page := string(body)
		if strings.Contains(page, AddPath) {
			t.Errorf("只读模式页面不应出现增删改入口:\n%s", page)
		}
		if !strings.Contains(page, "-readonly") {
			t.Error("只读模式页面缺少说明")
		}
	})
}

// 选择页本身不会被转发到后端，即使已经选好了后端。
func TestSelectPageIsNotProxied(t *testing.T) {
	st := newStack(t, false, nil)

	resp := request(t, http.MethodGet, st.front.URL+Path,
		http.Header{"Cookie": {CookieName + "=openclacky"}}, nil)
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "backend:") {
		t.Errorf("%s 被转发到了后端: %s", Path, body)
	}
}

// 提交选择 → 写 Cookie 并 303 跳回原地址。
func TestChooseSetsCookieAndRedirects(t *testing.T) {
	st := newStack(t, false, nil)

	resp := formPost(t, st.front.URL+Path, url.Values{"name": {"deepseek"}, "next": {"/app/page?x=1"}})

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/app/page?x=1" {
		t.Errorf("Location = %q, 期望 /app/page?x=1", got)
	}

	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			found = c
		}
	}
	if found == nil {
		t.Fatalf("未写入 %s Cookie: %v", CookieName, resp.Header.Values("Set-Cookie"))
	}
	if found.Value != "deepseek" {
		t.Errorf("Cookie 值 = %q, 期望 deepseek", found.Value)
	}
	if found.Path != "/" || !found.HttpOnly || found.MaxAge <= 0 {
		t.Errorf("Cookie 属性异常: path=%q httpOnly=%v maxAge=%d", found.Path, found.HttpOnly, found.MaxAge)
	}
}

// 选好之后原来的请求就能直达后端：模拟浏览器带上新 Cookie 重放。
func TestSelectThenRequestReachesUpstream(t *testing.T) {
	st := newStack(t, false, nil)

	resp := formPost(t, st.front.URL+Path, url.Values{"name": {"openclacky"}})
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("未拿到选择 Cookie")
	}

	follow := request(t, http.MethodGet, st.front.URL+"/after", http.Header{"Cookie": {cookie}}, nil)
	body, _ := io.ReadAll(follow.Body)
	if follow.StatusCode != http.StatusOK || string(body) != "backend:/after" {
		t.Errorf("选择后转发失败: status=%d body=%q", follow.StatusCode, body)
	}
}

// 未知名称 → 400 并重新展示列表，不写 Cookie。
func TestChooseRejectsUnknownName(t *testing.T) {
	st := newStack(t, true, nil)

	resp := formPost(t, st.front.URL+Path, url.Values{"name": {"nope"}})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			t.Errorf("不应写入 Cookie: %v", c)
		}
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "未知的代理名称") {
		t.Errorf("响应缺少错误提示: %s", body)
	}
}

// 选择页只接受 GET/HEAD/POST，其它方法不能落到兜底转发。
func TestSelectPathRejectsOtherMethods(t *testing.T) {
	st := newStack(t, false, nil)

	for _, method := range []string{http.MethodPut, http.MethodDelete, "PROPFIND"} {
		resp := request(t, method, st.front.URL+Path,
			http.Header{"Cookie": {CookieName + "=openclacky"}}, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s 状态码 = %d, 期望 405", method, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "backend:") {
			t.Errorf("%s %s 被转发到了后端", method, Path)
		}
	}
}

// 新增后端：写回配置文件，并立刻能用于转发。
func TestAddProxyWritesConfigAndTakesEffect(t *testing.T) {
	var logBuf syncBuffer
	st := newStack(t, true, &logBuf)

	resp := formPost(t, st.front.URL+AddPath,
		url.Values{"name": {"added"}, "proxy_pass": {st.backend + "/new"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != Path {
		t.Errorf("Location = %q, 期望 %q", got, Path)
	}

	cfg := st.loadCfg(t)
	if len(cfg.ProxyList) != 3 || cfg.ProxyList[2].Name != "added" || cfg.ProxyList[2].ProxyPass != st.backend+"/new" {
		t.Fatalf("配置文件未正确更新: %+v", cfg.ProxyList)
	}

	// 新后端立即生效，无需重启。
	body, _ := io.ReadAll(request(t, http.MethodGet, st.front.URL+"/hey",
		http.Header{"Cookie": {CookieName + "=added"}}, nil).Body)
	if string(body) != "backend:/new/hey" {
		t.Errorf("响应体 = %q, 期望 backend:/new/hey", body)
	}
	waitForLog(t, &logBuf, "proxy added")
}

// 新增时带 next → 跳回原地址。
func TestAddProxyRedirectsToNext(t *testing.T) {
	st := newStack(t, true, nil)

	resp := formPost(t, st.front.URL+AddPath,
		url.Values{"name": {"added"}, "proxy_pass": {st.backend}, "next": {"/app/page?x=1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/app/page?x=1" {
		t.Errorf("Location = %q, 期望 /app/page?x=1", got)
	}
}

// 新增时的各种非法输入都要被挡住，并且不改动配置文件。
func TestAddProxyValidation(t *testing.T) {
	tests := []struct {
		name     string
		form     url.Values
		wantErrs []string
	}{
		{
			name:     "名称为空",
			form:     url.Values{"name": {"  "}, "proxy_pass": {"http://127.0.0.1:7500"}},
			wantErrs: []string{"名称不能为空"},
		},
		{
			name:     "名称含非法字符",
			form:     url.Values{"name": {"a b"}, "proxy_pass": {"http://127.0.0.1:7500"}},
			wantErrs: []string{"只允许字母、数字"},
		},
		{
			name:     "名称重复",
			form:     url.Values{"name": {"openclacky"}, "proxy_pass": {"http://127.0.0.1:7500"}},
			wantErrs: []string{"已存在"},
		},
		{
			name:     "后端地址缺少 scheme",
			form:     url.Values{"name": {"ok"}, "proxy_pass": {"127.0.0.1:7500"}},
			wantErrs: []string{"http:// 或 https://"},
		},
		{
			name:     "后端地址为空",
			form:     url.Values{"name": {"ok"}, "proxy_pass": {" "}},
			wantErrs: []string{"不能为空"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newStack(t, true, nil)
			before, err := io.ReadAll(mustOpen(t, st.cfgPath))
			if err != nil {
				t.Fatalf("读取配置失败: %v", err)
			}

			resp := formPost(t, st.front.URL+AddPath, tc.form)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			for _, want := range tc.wantErrs {
				if !strings.Contains(string(body), want) {
					t.Errorf("页面缺少提示 %q:\n%s", want, body)
				}
			}

			after, err := io.ReadAll(mustOpen(t, st.cfgPath))
			if err != nil {
				t.Fatalf("读取配置失败: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("校验失败时不应改动配置文件:\n%s", after)
			}
		})
	}
}

// 修改后端：改名 + 换地址，Cookie 跟着改过去。
func TestUpdateProxyRenamesAndRetargets(t *testing.T) {
	var logBuf syncBuffer
	st := newStack(t, true, &logBuf)

	resp := request(t, http.MethodPost, st.front.URL+UpdatePath,
		http.Header{
			"Content-Type": {"application/x-www-form-urlencoded"},
			"Cookie":       {CookieName + "=openclacky"},
		},
		strings.NewReader(url.Values{
			"original_name": {"openclacky"},
			"name":          {"alpha"},
			"proxy_pass":    {st.backend + "/alpha"},
		}.Encode()))

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", resp.StatusCode)
	}

	var renamed *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			renamed = c
		}
	}
	if renamed == nil || renamed.Value != "alpha" {
		t.Errorf("改名后 Cookie 未跟随: %v", resp.Header.Values("Set-Cookie"))
	}

	cfg := st.loadCfg(t)
	if cfg.ProxyList[0].Name != "alpha" || cfg.ProxyList[0].ProxyPass != st.backend+"/alpha" {
		t.Fatalf("配置文件未正确更新: %+v", cfg.ProxyList[0])
	}
	if len(cfg.ProxyList) != 2 {
		t.Errorf("条目数量 = %d, 期望 2（改名不应新增）", len(cfg.ProxyList))
	}

	// 旧名称立刻失效，新名称生效。
	if resp := request(t, http.MethodGet, st.front.URL+"/x",
		http.Header{"Cookie": {CookieName + "=alpha"}}, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("改名后新名称不可用: 状态码 = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(request(t, http.MethodGet, st.front.URL+"/hi",
		http.Header{"Cookie": {CookieName + "=alpha"}}, nil).Body)
	if string(body) != "backend:/alpha/hi" {
		t.Errorf("响应体 = %q, 期望 backend:/alpha/hi", body)
	}
	waitForLog(t, &logBuf, "proxy updated")
}

// 修改成一条不存在的记录、或撞上已有名称 → 400，配置不变。
func TestUpdateProxyRejectsBadInput(t *testing.T) {
	tests := []struct {
		name     string
		form     url.Values
		wantErrs []string
	}{
		{
			name:     "原名称不存在",
			form:     url.Values{"original_name": {"ghost"}, "name": {"ghost"}, "proxy_pass": {"http://127.0.0.1:7500"}},
			wantErrs: []string{"不存在"},
		},
		{
			name:     "改名撞上已有名称",
			form:     url.Values{"original_name": {"openclacky"}, "name": {"deepseek"}, "proxy_pass": {"http://127.0.0.1:7500"}},
			wantErrs: []string{"已存在"},
		},
		{
			name:     "地址非法",
			form:     url.Values{"original_name": {"openclacky"}, "name": {"openclacky"}, "proxy_pass": {"nope"}},
			wantErrs: []string{"http:// 或 https://"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newStack(t, true, nil)
			before, _ := io.ReadAll(mustOpen(t, st.cfgPath))

			resp := formPost(t, st.front.URL+UpdatePath, tc.form)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			for _, want := range tc.wantErrs {
				if !strings.Contains(string(body), want) {
					t.Errorf("页面缺少提示 %q:\n%s", want, body)
				}
			}

			after, _ := io.ReadAll(mustOpen(t, st.cfgPath))
			if !bytes.Equal(before, after) {
				t.Errorf("校验失败时不应改动配置文件:\n%s", after)
			}
		})
	}
}

// 删除后端：写回配置文件，被删掉的正好是当前选择时清掉 Cookie。
func TestDeleteProxyRemovesEntry(t *testing.T) {
	var logBuf syncBuffer
	st := newStack(t, true, &logBuf)

	resp := request(t, http.MethodPost, st.front.URL+DeletePath,
		http.Header{
			"Content-Type": {"application/x-www-form-urlencoded"},
			"Cookie":       {CookieName + "=deepseek"},
		},
		strings.NewReader(url.Values{"name": {"deepseek"}}.Encode()))

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", resp.StatusCode)
	}
	if sc := resp.Header.Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") {
		t.Errorf("被删除的当前选择应清掉 Cookie: %q", sc)
	}

	cfg := st.loadCfg(t)
	if len(cfg.ProxyList) != 1 || cfg.ProxyList[0].Name != "openclacky" {
		t.Fatalf("配置文件未正确更新: %+v", cfg.ProxyList)
	}
	// 删掉的名称立刻不再可用。
	if resp := request(t, http.MethodGet, st.front.URL+"/x",
		http.Header{"Cookie": {CookieName + "=deepseek"}}, nil); resp.StatusCode == http.StatusOK {
		t.Error("已删除的后端仍可用")
	}
	waitForLog(t, &logBuf, "proxy deleted")
}

// 删除最后一条会被拒绝：配置要求至少保留一个后端。
func TestDeleteLastProxyRejected(t *testing.T) {
	st := newStack(t, true, nil)

	if resp := formPost(t, st.front.URL+DeletePath, url.Values{"name": {"deepseek"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("第一次删除状态码 = %d, 期望 303", resp.StatusCode)
	}

	resp := formPost(t, st.front.URL+DeletePath, url.Values{"name": {"openclacky"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "proxy_list 不能为空") {
		t.Errorf("页面缺少提示:\n%s", body)
	}
	if cfg := st.loadCfg(t); len(cfg.ProxyList) != 1 {
		t.Errorf("配置文件被改动: %+v", cfg.ProxyList)
	}
}

// 来自其它站点的表单提交（CSRF）必须被拒绝。
func TestAdminRejectsCrossOrigin(t *testing.T) {
	st := newStack(t, true, nil)
	before, _ := io.ReadAll(mustOpen(t, st.cfgPath))

	resp := request(t, http.MethodPost, st.front.URL+AddPath,
		http.Header{
			"Content-Type": {"application/x-www-form-urlencoded"},
			"Origin":       {"https://evil.example.com"},
		},
		strings.NewReader(url.Values{"name": {"evil"}, "proxy_pass": {"http://evil.example.com"}}.Encode()))

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态码 = %d, 期望 403", resp.StatusCode)
	}
	after, _ := io.ReadAll(mustOpen(t, st.cfgPath))
	if !bytes.Equal(before, after) {
		t.Errorf("跨站请求不应改动配置文件:\n%s", after)
	}
}

// 同源的 Origin（含端口不同即算不同源）行为要正确。
func TestSameOriginRules(t *testing.T) {
	build := func(origin, referer string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, AddPath, nil)
		req.Host = "proxy.example.com:8888"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		return req
	}

	tests := []struct {
		origin, referer string
		want            bool
	}{
		{"", "", true}, // curl 等不带头的客户端
		{"http://proxy.example.com:8888", "", true},
		{"HTTP://PROXY.EXAMPLE.COM:8888", "", true},
		{"https://evil.example.com", "", false},
		{"http://proxy.example.com:9999", "", false},
		{"null", "", false},
		{"", "http://proxy.example.com:8888/foo", true},
		{"", "https://evil.example.com/foo", false},
		{"https://evil.example.com", "http://proxy.example.com:8888/foo", false}, // Origin 优先
	}
	for _, tc := range tests {
		if got := sameOrigin(build(tc.origin, tc.referer)); got != tc.want {
			t.Errorf("sameOrigin(origin=%q, referer=%q) = %v, 期望 %v", tc.origin, tc.referer, got, tc.want)
		}
	}
}

// 管理接口只接受 POST，且未知子路径不会被转发给后端。
func TestAdminPathsStrictness(t *testing.T) {
	st := newStack(t, true, nil)

	t.Run("错误方法", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut} {
			resp := request(t, method, st.front.URL+AddPath,
				http.Header{"Cookie": {CookieName + "=openclacky"}}, nil)
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s 状态码 = %d, 期望 405", method, resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); allow != "POST" {
				t.Errorf("%s Allow = %q, 期望 POST", method, allow)
			}
		}
	})

	t.Run("未知子路径", func(t *testing.T) {
		resp := formPost(t, st.front.URL+adminPrefix+"whatever", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("状态码 = %d, 期望 404", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "backend:") {
			t.Errorf("/_select 下的未知路径被转发到了后端: %s", body)
		}
	})
}

// 只读模式下写接口不存在，页面也没有入口。
func TestReadonlyModeHasNoWriteEndpoints(t *testing.T) {
	st := newStack(t, false, nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, AddPath},
		{http.MethodPost, UpdatePath},
		{http.MethodPost, DeletePath},
	} {
		resp := request(t, tc.method, st.front.URL+tc.path,
			http.Header{
				"Content-Type": {"application/x-www-form-urlencoded"},
				"Cookie":       {CookieName + "=openclacky"},
			},
			strings.NewReader("name=x&proxy_pass=http://h&original_name=x"))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s 状态码 = %d, 期望 404", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// next 只能是本站路径，且不能把用户又送回选择页。
func TestSafeNext(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/app/page?x=1", "/app/page?x=1"},
		{"/a%2Fb", "/a%2Fb"},
		{"https://evil.example.com/", "/"},
		{"//evil.example.com/", "/"},
		{`/\evil.example.com/`, "/"},
		{"app/page", "/"},
		{"/ok\r\nX-Injected: 1", "/"},
	}
	for _, tc := range tests {
		if got := safeNext(tc.in); got != tc.want {
			t.Errorf("safeNext(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

// cleanNext 进一步把首页与选择页自身视为"没有 next"。
func TestCleanNext(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"/", ""},
		{"/app/page?x=1", "/app/page?x=1"},
		{Path, ""},
		{Path + "?next=/a", ""},
		{Path + "/sub", ""},
		{AddPath, ""},
		{"https://evil.example.com/", ""},
		{"//evil.example.com/", ""},
	}
	for _, tc := range tests {
		if got := cleanNext(tc.in); got != tc.want {
			t.Errorf("cleanNext(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

// 转义写法不能绕过选择页判断落进兜底转发。
func TestEscapedSelectPathIsNotTreatedAsSelectPage(t *testing.T) {
	st := newStack(t, false, nil)

	resp := request(t, http.MethodGet, st.front.URL+"/_%73elect",
		http.Header{"Cookie": {CookieName + "=openclacky"}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（按普通路径转发）", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "backend:") {
		t.Errorf("响应体 = %q, 期望来自后端", body)
	}
}

// mustOpen 打开配置文件；测试失败即终止。
func mustOpen(t *testing.T, path string) io.ReadCloser {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
