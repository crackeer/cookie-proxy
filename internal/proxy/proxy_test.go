package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/liuhu016/cookie-proxy/internal/config"
	"github.com/liuhu016/cookie-proxy/internal/logging"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// newFrontend 起一个带"伪选路"中间件的代理服务：用 Cookie proxy_name 选路由，
// 这样测试只覆盖 proxy 包自身的行为。
func newFrontend(t *testing.T, cfg *config.Config, logOut io.Writer) *httptest.Server {
	t.Helper()
	if logOut == nil {
		logOut = io.Discard
	}
	router, err := NewRouter(cfg, logging.NewWith(logOut, "text"))
	if err != nil {
		t.Fatalf("NewRouter 失败: %v", err)
	}
	return newFrontendFor(t, router)
}

// newFrontendFor 用给定的 router 起服务，便于测试运行期替换路由表。
func newFrontendFor(t *testing.T, router *Router) *httptest.Server {
	t.Helper()

	e := gin.New()
	e.RedirectTrailingSlash = false
	e.RedirectFixedPath = false
	e.Use(func(c *gin.Context) {
		name, _ := c.Cookie(SelectionCookie)
		route, ok := router.Lookup(name)
		if !ok {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		c.Set(logging.CtxKeyProxy, route.Name)
		c.Set(ContextKeyRoute, route)
		c.Next()
	})
	e.NoRoute(router.Handler())

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, name string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: SelectionCookie, Value: name})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// 不同 Cookie 值必须落到各自的后端，并发时互不串台。
func TestRoutesPerProxyName(t *testing.T) {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("A"))
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("B"))
	}))
	defer backendB.Close()

	front := newFrontend(t, &config.Config{ProxyList: []config.Proxy{
		{Name: "alpha", ProxyPass: backendA.URL},
		{Name: "beta", ProxyPass: backendB.URL},
	}}, nil)

	for _, tc := range []struct{ name, want string }{{"alpha", "A"}, {"beta", "B"}} {
		resp := do(t, http.MethodGet, front.URL+"/", tc.name, nil)
		got, _ := io.ReadAll(resp.Body)
		if string(got) != tc.want {
			t.Errorf("proxy_name %s 得到 %q, 期望 %q", tc.name, got, tc.want)
		}
	}

	// 并发混合请求，确认路由表读取安全且不串台。
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		for _, tc := range []struct{ name, want string }{{"alpha", "A"}, {"beta", "B"}} {
			tc := tc
			go func() {
				req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
				req.AddCookie(&http.Cookie{Name: SelectionCookie, Value: tc.name})
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					errs <- err
					return
				}
				defer resp.Body.Close()
				got, _ := io.ReadAll(resp.Body)
				if string(got) != tc.want {
					errs <- io.ErrUnexpectedEOF
					return
				}
				errs <- nil
			}()
		}
	}
	for i := 0; i < 40; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发请求路由错误: %v", err)
		}
	}
}

// Routes 必须保留配置顺序，选择页依赖它。
func TestRoutesKeepConfigOrder(t *testing.T) {
	router, err := NewRouter(&config.Config{ProxyList: []config.Proxy{
		{Name: "openclacky", ProxyPass: "http://127.0.0.1:7500"},
		{Name: "deepseek", ProxyPass: "http://127.0.0.1:7501"},
		{Name: "download", ProxyPass: "http://127.0.0.1:7502"},
	}}, logging.NewWith(io.Discard, "text"))
	if err != nil {
		t.Fatalf("NewRouter 失败: %v", err)
	}

	want := []string{"openclacky", "deepseek", "download"}
	routes := router.Routes()
	if len(routes) != len(want) || router.Len() != len(want) {
		t.Fatalf("路由数量 = %d/%d, 期望 %d", len(routes), router.Len(), len(want))
	}
	for i, name := range want {
		if routes[i].Name != name {
			t.Errorf("routes[%d].Name = %q, 期望 %q", i, routes[i].Name, name)
		}
	}
}

// 路径与查询串必须原样保留；proxy_pass 带前缀时正确拼接。
func TestPathAndQueryPreserved(t *testing.T) {
	var gotURI string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
	}))
	defer backend.Close()

	tests := []struct {
		name      string
		proxyPass string
		request   string
		wantURI   string
	}{
		{"无前缀", backend.URL, "/api/v1/items?page=2&q=a+b", "/api/v1/items?page=2&q=a+b"},
		{"带前缀", backend.URL + "/base", "/x?q=1", "/base/x?q=1"},
		{"前缀带斜杠", backend.URL + "/base/", "/x", "/base/x"},
		{"根路径", backend.URL, "/", "/"},
		{"尾斜杠不被重定向", backend.URL, "/foo/", "/foo/"},
		{"转义字符保留", backend.URL, "/a%2Fb/c", "/a%2Fb/c"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			front := newFrontend(t, &config.Config{ProxyList: []config.Proxy{
				{Name: "u", ProxyPass: tc.proxyPass},
			}}, nil)

			gotURI = ""
			resp := do(t, http.MethodGet, front.URL+tc.request, "u", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
			}
			if gotURI != tc.wantURI {
				t.Errorf("后端收到 %q, 期望 %q", gotURI, tc.wantURI)
			}
		})
	}
}

// 方法、请求体、请求头、响应头与状态码全部透传；X-Forwarded-* 已设置。
func TestForwardsRequestAndResponseFaithfully(t *testing.T) {
	var (
		gotMethod string
		gotBody   string
		gotHeader http.Header
		gotHost   string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotMethod, gotBody, gotHeader, gotHost = r.Method, string(body), r.Header.Clone(), r.Host
		w.Header().Set("X-Backend", "yes")
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("brewed"))
	}))
	defer backend.Close()

	front := newFrontend(t, &config.Config{ProxyList: []config.Proxy{
		{Name: "u", ProxyPass: backend.URL},
	}}, nil)

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/submit", strings.NewReader("payload"))
	req.AddCookie(&http.Cookie{Name: SelectionCookie, Value: "u"})
	req.Header.Set("X-Custom", "keep-me")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer token-for-backend")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if gotMethod != http.MethodPost {
		t.Errorf("后端方法 = %s, 期望 POST", gotMethod)
	}
	if gotBody != "payload" {
		t.Errorf("后端 body = %q, 期望 payload", gotBody)
	}
	if gotHeader.Get("X-Custom") != "keep-me" {
		t.Error("自定义请求头未透传")
	}
	// 本服务不再消费 Authorization，应原样交给后端。
	if gotHeader.Get("Authorization") != "Bearer token-for-backend" {
		t.Errorf("Authorization = %q, 期望原样透传", gotHeader.Get("Authorization"))
	}
	backendHost := strings.TrimPrefix(backend.URL, "http://")
	if gotHost != backendHost {
		t.Errorf("后端 Host = %q, 期望 %q", gotHost, backendHost)
	}
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if gotHeader.Get(h) == "" {
			t.Errorf("缺少 %s 头", h)
		}
	}

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("状态码 = %d, 期望 418", resp.StatusCode)
	}
	if resp.Header.Get("X-Backend") != "yes" {
		t.Error("后端响应头未透传")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "brewed" {
		t.Errorf("响应体 = %q, 期望 brewed", body)
	}
}

// proxy_name 是本服务的内部状态，不外传；其它 Cookie 必须原样保留。
func TestSelectionCookieStrippedFromUpstream(t *testing.T) {
	var gotCookie string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
	}))
	defer backend.Close()

	front := newFrontend(t, &config.Config{ProxyList: []config.Proxy{
		{Name: "u", ProxyPass: backend.URL},
	}}, nil)

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/x", nil)
	req.Header.Set("Cookie", "session=abc; "+SelectionCookie+"=u; theme=dark")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}

	if strings.Contains(gotCookie, SelectionCookie) {
		t.Errorf("后端收到了 %s: %q", SelectionCookie, gotCookie)
	}
	if gotCookie != "session=abc; theme=dark" {
		t.Errorf("后端 Cookie = %q, 期望 %q", gotCookie, "session=abc; theme=dark")
	}
}

func TestStripCookie(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"只有选择 Cookie", []string{"proxy_name=a"}, nil},
		{"位于中间", []string{"x=1; proxy_name=a; y=2"}, []string{"x=1; y=2"}},
		{"没有该 Cookie", []string{"x=1"}, []string{"x=1"}},
		{"前缀相似的名字保留", []string{"proxy_name_x=1; proxy_name=a"}, []string{"proxy_name_x=1"}},
		{"多个 Cookie 头", []string{"proxy_name=a", "x=1"}, []string{"x=1"}},
		{"空值也删除", []string{"proxy_name=; x=1"}, []string{"x=1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for _, h := range tc.in {
				req.Header.Add("Cookie", h)
			}
			stripCookie(req, SelectionCookie)

			got := req.Header.Values("Cookie")
			if len(got) != len(tc.want) {
				t.Fatalf("Cookie 头 = %q, 期望 %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Cookie[%d] = %q, 期望 %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// 后端不可达 → 502，且响应体与日志都不泄露配置细节以外的信息。
func TestUnreachableBackendReturns502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 端口立刻不可用

	var logBuf syncBuffer
	front := newFrontend(t, &config.Config{ProxyList: []config.Proxy{
		{Name: "u", ProxyPass: deadURL},
	}}, &logBuf)

	resp := do(t, http.MethodGet, front.URL+"/x", "u", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), deadURL) {
		t.Errorf("响应体泄露了后端地址: %s", body)
	}
	waitForLog(t, &logBuf, "upstream unreachable")
}

// 后端响应慢于上游超时 → 504。
func TestSlowBackendReturns504(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
			w.Write([]byte("too late"))
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()

	var logBuf syncBuffer
	front := newFrontend(t, &config.Config{
		UpstreamTimeoutSeconds: 1,
		ProxyList: []config.Proxy{
			{Name: "u", ProxyPass: backend.URL},
		},
	}, &logBuf)

	start := time.Now()
	resp := do(t, http.MethodGet, front.URL+"/slow", "u", nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("状态码 = %d, 期望 504", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("耗时 %v，超时未生效", elapsed)
	}
	waitForLog(t, &logBuf, "upstream timeout")
}

// 流式响应必须边到边发，而不是等后端结束才回传。
func TestStreamingResponseIsFlushed(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: second\n\n")
	}))
	defer backend.Close()

	front := newFrontend(t, &config.Config{
		UpstreamTimeoutSeconds: 10,
		ProxyList: []config.Proxy{
			{Name: "u", ProxyPass: backend.URL},
		},
	}, nil)

	resp := do(t, http.MethodGet, front.URL+"/events", "u", nil)
	reader := bufio.NewReader(resp.Body)

	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取第一条流数据失败: %v", err)
	}
	if !strings.Contains(line, "data: first") {
		t.Errorf("第一条流数据 = %q", line)
	}

	close(release)
	rest, _ := io.ReadAll(reader)
	if !strings.Contains(string(rest), "data: second") {
		t.Errorf("后续流数据丢失: %q", rest)
	}
}

// 未选路却走到 Handler（装配错误）应返回 500 而不是 panic。
func TestHandlerWithoutRouteReturns500(t *testing.T) {
	router, err := NewRouter(&config.Config{ProxyList: []config.Proxy{
		{Name: "u", ProxyPass: "http://127.0.0.1:1"},
	}}, logging.NewWith(io.Discard, "text"))
	if err != nil {
		t.Fatalf("NewRouter 失败: %v", err)
	}

	e := gin.New()
	e.NoRoute(router.Handler())
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("状态码 = %d, 期望 500", w.Code)
	}
}

func TestNewRouterRejectsBadProxyPass(t *testing.T) {
	_, err := NewRouter(&config.Config{ProxyList: []config.Proxy{
		{Name: "u", ProxyPass: "http://[::1"},
	}}, logging.NewWith(io.Discard, "text"))
	if err == nil {
		t.Fatal("期望解析失败")
	}
}

// Replace 立刻换掉整张路由表：旧名称消失、新名称可用、顺序按新配置。
func TestReplaceSwapsRoutes(t *testing.T) {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("A"))
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("B"))
	}))
	defer backendB.Close()

	router, err := NewRouter(&config.Config{ProxyList: []config.Proxy{
		{Name: "one", ProxyPass: backendA.URL},
	}}, logging.NewWith(io.Discard, "text"))
	if err != nil {
		t.Fatalf("NewRouter 失败: %v", err)
	}
	front := newFrontendFor(t, router)

	body := func(name string) (int, string) {
		resp := do(t, http.MethodGet, front.URL+"/", name, nil)
		got, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(got)
	}

	if status, got := body("one"); status != http.StatusOK || got != "A" {
		t.Fatalf("替换前: status=%d body=%q", status, got)
	}

	if err := router.Replace(&config.Config{ProxyList: []config.Proxy{
		{Name: "two", ProxyPass: backendB.URL},
		{Name: "three", ProxyPass: backendA.URL},
	}}); err != nil {
		t.Fatalf("Replace 失败: %v", err)
	}

	if _, ok := router.Lookup("one"); ok {
		t.Error("旧名称仍然存在")
	}
	if status, _ := body("one"); status == http.StatusOK {
		t.Error("旧名称仍然可以转发")
	}
	if status, got := body("two"); status != http.StatusOK || got != "B" {
		t.Errorf("新名称未生效: status=%d body=%q", status, got)
	}

	routes := router.Routes()
	if len(routes) != 2 || router.Len() != 2 || routes[0].Name != "two" || routes[1].Name != "three" {
		t.Errorf("路由表 = %+v (Len=%d), 期望 [two three]", routeNames(routes), router.Len())
	}
}

// Replace 构造失败时必须保持原路由表可用。
func TestReplaceFailureKeepsOldTable(t *testing.T) {
	router, err := NewRouter(&config.Config{ProxyList: []config.Proxy{
		{Name: "one", ProxyPass: "http://127.0.0.1:7500"},
	}}, logging.NewWith(io.Discard, "text"))
	if err != nil {
		t.Fatalf("NewRouter 失败: %v", err)
	}

	bad := &config.Config{ProxyList: []config.Proxy{
		{Name: "dup", ProxyPass: "http://127.0.0.1:7501"},
		{Name: "dup", ProxyPass: "http://127.0.0.1:7502"},
	}}
	if err := router.Replace(bad); err == nil {
		t.Fatal("期望重复名称导致失败")
	}

	route, ok := router.Lookup("one")
	if !ok || route.Target != "http://127.0.0.1:7500" {
		t.Errorf("失败后原路由表被破坏: ok=%v route=%+v", ok, route)
	}
	if _, ok := router.Lookup("dup"); ok {
		t.Error("失败的表不应生效")
	}
}

// 上游超时来自当前生效的那张表，替换后立即按新值生效。
func TestReplaceAppliesNewTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
			w.Write([]byte("late"))
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	router, err := NewRouter(&config.Config{
		UpstreamTimeoutSeconds: 0, // 不限制
		ProxyList:              []config.Proxy{{Name: "u", ProxyPass: slow.URL}},
	}, logging.NewWith(io.Discard, "text"))
	if err != nil {
		t.Fatalf("NewRouter 失败: %v", err)
	}
	front := newFrontendFor(t, router)

	if err := router.Replace(&config.Config{
		UpstreamTimeoutSeconds: 1,
		ProxyList:              []config.Proxy{{Name: "u", ProxyPass: slow.URL}},
	}); err != nil {
		t.Fatalf("Replace 失败: %v", err)
	}

	start := time.Now()
	resp := do(t, http.MethodGet, front.URL+"/slow", "u", nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("状态码 = %d, 期望 504", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("耗时 %v，新的超时未生效", elapsed)
	}
}

func routeNames(routes []*Route) []string {
	names := make([]string, 0, len(routes))
	for _, r := range routes {
		names = append(names, r.Name)
	}
	return names
}

func TestIsUpgrade(t *testing.T) {
	tests := []struct {
		conn, upgrade string
		want          bool
	}{
		{"Upgrade", "websocket", true},
		{"keep-alive, Upgrade", "websocket", true},
		{"keep-alive", "websocket", false},
		{"Upgrade", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.conn != "" {
			req.Header.Set("Connection", tc.conn)
		}
		if tc.upgrade != "" {
			req.Header.Set("Upgrade", tc.upgrade)
		}
		if got := isUpgrade(req); got != tc.want {
			t.Errorf("isUpgrade(Connection=%q, Upgrade=%q) = %v, 期望 %v", tc.conn, tc.upgrade, got, tc.want)
		}
	}
}
