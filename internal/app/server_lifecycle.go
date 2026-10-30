package app

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"wild-work/internal/server"
	"wild-work/internal/systray"
)

// SetHandler 注入内层 HTTP handler（在 HandleAPI 注册后调用）。
func (a *App) SetHandler(h *server.Handler) {
	a.handler = h
	if a.httpRoot == nil {
		a.httpRoot = h
	}
}

// SetRootHandler 注入对外服务的根 handler（通常是「兼容层 mux + 内层 handler」的组合）。
// httpRoot 为 nil 时回退到内层 handler。
func (a *App) SetRootHandler(h http.Handler) { a.httpRoot = h }

// serveHandler 返回实际对外服务的 handler（优先外层组合，其次内层）。
func (a *App) serveHandler() http.Handler {
	// 最外层统一加 Host 校验（未启用管理密码时只接受环回 Host，防 DNS rebinding 读写面板）；
	// 挂在最外层而非只挂 /api/：面板静态页面与 /v1/* 同样不该被跨站域名访问。
	if a.httpRoot != nil {
		return a.hostGuard(a.httpRoot)
	}
	if a.handler == nil { // 避免 typed-nil 接口导致 http.Server 请求时 panic
		return nil
	}
	return a.hostGuard(a.handler)
}

// StartServer 按当前配置启动 HTTP 服务。
func (a *App) StartServer() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serveLocked(a.cfg.Listen.Addr())
}

// serveLocked 在 newAddr 上启动新服务并切换；调用方需持有 a.mu。
func (a *App) serveLocked(addr string) error {
	// 地址未变时直接复用现有 listener：否则「先 Listen 再 Shutdown 旧」在同端口上
	// 会撞自身（Only one usage of each socket address），导致面板保存映射/密钥时报 400。
	if a.httpSrv != nil && a.listenAddr == addr {
		return nil
	}
	// 先停旧服务再绑新地址：避免同端口切换时新旧 listener 短暂重叠报「端口被占用」。
	if old := a.httpSrv; old != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = old.Shutdown(shutdownCtx)
		cancel()
		a.httpSrv = nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           a.serveHandler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	a.httpSrv = srv
	a.listenAddr = addr
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("http serve %s: %v", addr, err)
		}
	}()
	return nil
}

// Stop 停止 HTTP 服务（退出时调用）。
func (a *App) Stop() {
	a.mu.Lock()
	srv := a.httpSrv
	a.httpSrv = nil
	a.listenAddr = ""
	a.mu.Unlock()
	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	a.Close()
}

// Quit 退出整个程序（Web UI“退出”按钮调用）。
func (a *App) Quit() {
	// 先摘托盘图标：回收发生在托盘消息循环里，放在停机之后会被 Close 关掉日志、
	// 也可能被 os.Exit 抢在前面，Windows 任务栏就会留下幽灵图标。
	systray.Quit(3 * time.Second)
	a.Stop()
	os.Exit(0)
}

// ServerRunning 是否正在监听。
func (a *App) ServerRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.httpSrv != nil
}

// PanelAuthEnabled 管理面板是否启用密码鉴权（供启动提示使用）。
func (a *App) PanelAuthEnabled() bool { return a.panelAuthEnabled() }

// AdminPassIsWeak 管理员密码是否过弱（< 8 位）：仅用于启动告警，不阻断。
func (a *App) AdminPassIsWeak() bool {
	p := a.adminPass()
	return p != "" && len(p) < 8
}
