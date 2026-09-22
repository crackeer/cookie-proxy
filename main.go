// cookie-proxy 是一个用 Cookie 区分后端的 HTTP 反向代理：
// Cookie proxy_name 的值对应配置里 proxy_list[].name，请求被转发到该条目的 proxy_pass。
// 访问 /_select 可以在页面上选择后端。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/liuhu016/cookie-proxy/internal/config"
	"github.com/liuhu016/cookie-proxy/internal/logging"
	"github.com/liuhu016/cookie-proxy/internal/proxy"
	"github.com/liuhu016/cookie-proxy/internal/selector"
)

func main() {
	cfgPath := flag.String("c", "", "JSON 配置文件路径（必填）")
	readonly := flag.Bool("readonly", false, "选择页只读：不注册 /_select 的增删改接口")
	flag.Parse()

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "错误: 必须通过 -c 指定 JSON 配置文件")
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}

	log := logging.New(cfg.LogFormat)

	router, err := proxy.NewRouter(cfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}

	// 可写模式下把配置文件交给 Store 统一读写；只读模式传 nil。
	var store *config.Store
	if !*readonly {
		store = config.NewStore(*cfgPath, cfg)
	}
	log.Info("config loaded", "path", *cfgPath, "proxies", len(cfg.ProxyList), "editable", store != nil)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           buildEngine(router, store, log),
		ReadHeaderTimeout: 20 * time.Second,
	}

	// SIGINT/SIGTERM 触发优雅退出；stop 后第二次信号恢复默认行为（强制结束）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, stop, srv, cfg, log); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

// buildEngine 组装中间件链：访问日志 → panic 恢复 → Cookie 选路 → 转发。
// store 为 nil 时 /_select 只读。
func buildEngine(router *proxy.Router, store *config.Store, log *slog.Logger) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	e := gin.New()
	// 透明代理不能改写请求：这些自动重定向/路径修正必须关掉。
	e.RedirectTrailingSlash = false
	e.RedirectFixedPath = false
	e.HandleMethodNotAllowed = false
	e.UseRawPath = true
	e.UnescapePathValues = false
	// 不信任入站 X-Forwarded-For，ClientIP 取真实对端地址。
	_ = e.SetTrustedProxies(nil)

	sel := selector.New(router, store, log)
	e.Use(logging.AccessLog(log), gin.Recovery(), sel.Middleware())
	// /_select 是本服务自己的页面，不转发。
	sel.Register(e)
	// 代理要接受任意方法与任意路径，因此用 NoRoute 兜底而不是注册路由。
	e.NoRoute(router.Handler())
	return e
}

// run 监听并在 ctx 结束时优雅退出。ctx 由调用方绑定信号，测试可直接传入可取消的 ctx。
func run(ctx context.Context, stop func(), srv *http.Server, cfg *config.Config, log *slog.Logger) error {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", srv.Addr, err)
	}

	log.Info("server started", "addr", ln.Addr().String(), "proxies", len(cfg.ProxyList),
		"select_path", selector.Path, "upstream_timeout_seconds", cfg.UpstreamTimeoutSeconds)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("服务异常退出: %w", err)
		}
		return nil
	case <-ctx.Done():
		stop() // 恢复默认信号行为，第二次 Ctrl-C 可强制结束
	}

	log.Info("shutting down", "grace_seconds", cfg.ShutdownTimeoutSeconds)
	shutdownCtx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.ShutdownTimeoutSeconds)*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out, forcing close", "err", err.Error())
		if err := srv.Close(); err != nil {
			return fmt.Errorf("强制关闭失败: %w", err)
		}
	}

	log.Info("server stopped")
	return nil
}
