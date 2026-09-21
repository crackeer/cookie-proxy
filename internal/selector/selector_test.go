package selector

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// newStack 装配「访问日志 → 选路 → 真实转发」链路，返回前端服务与后端命中地址。
func newStack(t *testing.T, logOut io.Writer) (*httptest.Server, string) {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend:" + r.URL.RequestURI()))
	}))
	t.Cleanup(backend.Close)

	if logOut == nil {
		logOut = io.Discard
	}
	log := logging.NewWith(logOut, "text")
	router, err := proxy.NewRouter(&config.Config{ProxyList: []config.Proxy{
		{Name: "openclacky", ProxyPass: backend.URL},
		{Name: "deepseek", ProxyPass: backend.URL + "/ds"},
	}}, log)
	if err != nil {
		t.Fatalf("NewRouter 失败: %v", err)
	}

	sel := New(router, log)
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
	return front, backend.URL
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

// 浏览器请求没带 Cookie → 跳到选择页，并记住原地址。
func TestBrowserWithoutCookieRedirectsToSelectPage(t *testing.T) {
	var logBuf syncBuffer
	front, _ := newStack(t, &logBuf)

	resp := request(t, http.MethodGet, front.URL+"/app/page?x=1",
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

// 非浏览器客户端没带 Cookie → 403，并给出可读提示。
func TestNonBrowserWithoutCookieGets403(t *testing.T) {
	front, _ := newStack(t, nil)

	resp := request(t, http.MethodGet, front.URL+"/api/items", nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态码 = %d, 期望 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{Path, CookieName} {
		if !strings.Contains(string(body), want) {
			t.Errorf("响应体缺少 %q: %s", want, body)
		}
	}
}

// Cookie 指向配置里已不存在的名称 → 清掉 Cookie 并让用户重选。
func TestUnknownCookieValueIsCleared(t *testing.T) {
	var logBuf syncBuffer
	front, _ := newStack(t, &logBuf)

	resp := request(t, http.MethodGet, front.URL+"/x",
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
	front, _ := newStack(t, &logBuf)

	for _, tc := range []struct{ name, wantBody string }{
		{"openclacky", "backend:/hello"},
		{"deepseek", "backend:/ds/hello"},
	} {
		resp := request(t, http.MethodGet, front.URL+"/hello",
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
	front, backendURL := newStack(t, nil)

	resp := request(t, http.MethodGet, front.URL+Path,
		http.Header{"Cookie": {CookieName + "=deepseek"}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, 期望 text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	for _, want := range []string{"openclacky", "deepseek", backendURL, `method="post"`, `aria-current="true"`} {
		if !strings.Contains(page, want) {
			t.Errorf("选择页缺少 %q", want)
		}
	}
	// 当前选择应该只有一个被标记。
	if got := strings.Count(page, `aria-current="true"`); got != 1 {
		t.Errorf("被标记为当前的项 = %d 个, 期望 1 个", got)
	}
}

// 选择页本身不会被转发到后端，即使已经选好了后端。
func TestSelectPageIsNotProxied(t *testing.T) {
	front, _ := newStack(t, nil)

	resp := request(t, http.MethodGet, front.URL+Path,
		http.Header{"Cookie": {CookieName + "=openclacky"}}, nil)
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "backend:") {
		t.Errorf("%s 被转发到了后端: %s", Path, body)
	}
}

// 提交选择 → 写 Cookie 并 303 跳回原地址。
func TestChooseSetsCookieAndRedirects(t *testing.T) {
	front, _ := newStack(t, nil)

	form := url.Values{"name": {"deepseek"}, "next": {"/app/page?x=1"}}
	resp := request(t, http.MethodPost, front.URL+Path,
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		strings.NewReader(form.Encode()))

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
	front, _ := newStack(t, nil)

	jar := noRedirectClient()
	form := url.Values{"name": {"openclacky"}}
	req, _ := http.NewRequest(http.MethodPost, front.URL+Path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := jar.Do(req)
	if err != nil {
		t.Fatalf("提交选择失败: %v", err)
	}
	resp.Body.Close()

	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("未拿到选择 Cookie")
	}

	follow := request(t, http.MethodGet, front.URL+"/after", http.Header{"Cookie": {cookie}}, nil)
	body, _ := io.ReadAll(follow.Body)
	if follow.StatusCode != http.StatusOK || string(body) != "backend:/after" {
		t.Errorf("选择后转发失败: status=%d body=%q", follow.StatusCode, body)
	}
}

// 未知名称 → 400 并重新展示列表，不写 Cookie。
func TestChooseRejectsUnknownName(t *testing.T) {
	front, _ := newStack(t, nil)

	form := url.Values{"name": {"nope"}}
	resp := request(t, http.MethodPost, front.URL+Path,
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		strings.NewReader(form.Encode()))

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
	front, _ := newStack(t, nil)

	for _, method := range []string{http.MethodPut, http.MethodDelete, "PROPFIND"} {
		resp := request(t, method, front.URL+Path,
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
		{Path, "/"},
		{Path + "?next=/a", "/"},
		{Path + "/sub", "/"},
	}
	for _, tc := range tests {
		if got := safeNext(tc.in); got != tc.want {
			t.Errorf("safeNext(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

// 转义写法不能绕过选择页判断落进兜底转发。
func TestEscapedSelectPathIsNotTreatedAsSelectPage(t *testing.T) {
	front, _ := newStack(t, nil)

	resp := request(t, http.MethodGet, front.URL+"/_%73elect",
		http.Header{"Cookie": {CookieName + "=openclacky"}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（按普通路径转发）", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "backend:") {
		t.Errorf("响应体 = %q, 期望来自后端", body)
	}
}
