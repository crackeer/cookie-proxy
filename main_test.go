package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/liuhu016/cookie-proxy/internal/config"
	"github.com/liuhu016/cookie-proxy/internal/logging"
	"github.com/liuhu016/cookie-proxy/internal/proxy"
	"github.com/liuhu016/cookie-proxy/internal/selector"
)

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func testConfig() *config.Config {
	return &config.Config{
		Port:                   0,
		UpstreamTimeoutSeconds: 30,
		ShutdownTimeoutSeconds: 5,
		LogFormat:              "text",
		ProxyList: []config.Proxy{
			{Name: "simple", ProxyPass: "http://127.0.0.1:9"},
		},
	}
}

// 端口被占用时必须报错退出，而不是静默失败。
func TestRunFailsWhenPortIsBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用端口失败: %v", err)
	}
	defer busy.Close()

	srv := &http.Server{Addr: busy.Addr().String(), Handler: http.NotFoundHandler()}
	err = run(context.Background(), func() {}, srv, testConfig(), logging.NewWith(io.Discard, "text"))
	if err == nil {
		t.Fatal("期望监听失败")
	}
	if !strings.Contains(err.Error(), "监听") {
		t.Errorf("错误信息应说明监听失败, 实际: %v", err)
	}
}

// 收到关闭信号时，进行中的请求必须先跑完。
func TestGracefulShutdownWaitsForInflightRequests(t *testing.T) {
	addr := freePort(t)
	started := make(chan struct{})

	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			time.Sleep(400 * time.Millisecond)
			w.Write([]byte("done"))
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, cancel, srv, testConfig(), logging.NewWith(io.Discard, "text"))
	}()

	// 等服务真正开始监听
	waitForListen(t, addr)

	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			respCh <- nil
			return
		}
		respCh <- resp
	}()

	<-started // 请求已进入 handler
	cancel()  // 模拟 SIGTERM

	select {
	case resp := <-respCh:
		if resp == nil {
			t.Fatal("进行中的请求被中断")
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(body) != "done" {
			t.Errorf("进行中的请求未正常完成: status=%d body=%q", resp.StatusCode, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("进行中的请求未在预期时间内完成")
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("run 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run 未退出")
	}

	// 关闭后不再接受新连接
	if _, err := http.Get("http://" + addr + "/after"); err == nil {
		t.Error("服务关闭后仍接受新请求")
	}
}

func waitForListen(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("服务未在预期时间内监听 %s", addr)
}

// buildEngine 的装配必须满足：未选择时不转发、选择页可用、选好后任意方法与路径都转发。
func TestBuildEngineWiring(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Method + " " + r.URL.RequestURI()))
	}))
	defer backend.Close()

	log := logging.NewWith(io.Discard, "text")
	router, err := proxy.NewRouter(&config.Config{
		UpstreamTimeoutSeconds: 10,
		ProxyList: []config.Proxy{
			{Name: "simple", ProxyPass: backend.URL},
		},
	}, log)
	if err != nil {
		t.Fatalf("NewRouter 失败: %v", err)
	}

	front := httptest.NewServer(buildEngine(router, log))
	defer front.Close()

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	t.Run("未选择时不转发", func(t *testing.T) {
		resp, err := client.Get(front.URL + "/whatever")
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("状态码 = %d, 期望 403", resp.StatusCode)
		}
	})

	t.Run("浏览器被引导到选择页", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/whatever", nil)
		req.Header.Set("Accept", "text/html")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("状态码 = %d, 期望 302", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, selector.Path) {
			t.Errorf("Location = %q, 期望以 %s 开头", loc, selector.Path)
		}
	})

	t.Run("选择页列出后端", func(t *testing.T) {
		resp, err := client.Get(front.URL + selector.Path)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "simple") {
			t.Errorf("选择页异常: status=%d body=%s", resp.StatusCode, body)
		}
	})

	t.Run("任意方法与路径都被转发", func(t *testing.T) {
		for _, tc := range []struct{ method, path string }{
			{http.MethodGet, "/"},
			{http.MethodDelete, "/a/b/c?x=1"},
			{http.MethodPut, "/dir/"},
			{"PROPFIND", "/webdav"},
		} {
			req, _ := http.NewRequest(tc.method, front.URL+tc.path, nil)
			req.AddCookie(&http.Cookie{Name: selector.CookieName, Value: "simple"})
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s 请求失败: %v", tc.method, tc.path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s %s 状态码 = %d, 期望 200", tc.method, tc.path, resp.StatusCode)
			}
			want := tc.method + " " + tc.path
			if string(body) != want {
				t.Errorf("后端收到 %q, 期望 %q", body, want)
			}
		}
	})
}
