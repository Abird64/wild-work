// Package app 业务编排层：HTTP 管理 API + 托盘动作 + 登录编排 + 日志。
// 由 daemon 入口（cmd/wild-work）装配，替代旧版 wails 绑定层。
package app

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	// 别名 statsEngine：/api/usage handler 内有同名局部变量 stats（遮蔽包名）
	"wild-work/internal/config"
	"wild-work/internal/custom"
	"wild-work/internal/gemini"
	"wild-work/internal/ledger"
	loginglm "wild-work/internal/login_glm"
	"wild-work/internal/officeace"
	"wild-work/internal/platform"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/raccoon"
	"wild-work/internal/scheduler"
	"wild-work/internal/server"
	statsEngine "wild-work/internal/stats"
)

// Version 版本号。
const Version = "2.9.6"

// Runtime 一个渠道的运行时资源。
type Runtime struct {
	Kind      provider.Kind
	Pool      *pool.Pool
	Upstream  provider.Upstream
	Scheduler *scheduler.Scheduler

	// Alias 标记该渠道与另一渠道共用同一账号池（如 TraeCode 复用 TraeWork 的 trPool）。
	// 有别于上游/路由独立性，账号聚合类操作（状态列表、账号计数、批量签到/刷新、
	// uid → 账号反查）必须跳过别名渠道，否则同一账号会被计入两次（见 issue #36）。
	// 别名渠道仍保留独立的模型/费率抓取（各自 upstream 的 function 不同）。
	Alias bool

	// Custom 标记该渠道为用户自定义 OpenAI 兼容第三方渠道（R66）。
	Custom bool
}

// Options 构建 App 的依赖。
type Options struct {
	ConfigPath string
	Config     *config.Config
	Runtimes   map[provider.Kind]*Runtime
	Handler    *server.Handler
	// Ledger 流水记账器（main 装配时创建，与 scheduler 共用同一实例）；nil 时 App 自建。
	Ledger *ledger.Ledger
}

// App 应用编排。
type App struct {
	cfgPath string
	cfg     *config.Config
	// runtimes 渠道运行时表。启动时装配完成，此后仅在「面板保存自定义渠道」时
	// 经 UpdateCustomRuntimes 增删（R66）——这是唯一一处 post-init 写。
	// 读写一律经 runtimesMu 保护：后台统计喂入器（30s ticker）与 /api/state 轮询
	// 都会遍历此表，若不加锁，与保存操作重叠时即 Go 的
	// `fatal error: concurrent map read and map write`（不可 recover，daemon 直接死）。
	runtimes   map[provider.Kind]*Runtime
	runtimesMu sync.RWMutex
	handler    *server.Handler // 内层（保留强类型，用于 SetAPIKey/ChannelModels 等）
	httpRoot   http.Handler    // 对外暴露的根 handler，可能为外层兼容层 mux（见 SetRootHandler）

	mu         sync.Mutex // 保护 httpSrv / cfg 修改
	httpSrv    *http.Server
	listenAddr string // 当前监听地址；同地址重复 serve 直接复用，避免自撞端口

	muLogin     sync.Mutex
	loginBusy   bool
	loginCtx    context.Context
	loginCancel context.CancelFunc
	loginClient *http.Client
	// loginLabel 用户在登录弹窗里填的「账号标识」（可选，2026-10-17 新增）。
	// 上游不提供稳定身份字段的渠道（当前是 CodeArts）靠它区分「同一账号重新登录」与
	// 「新增第二个账号」——否则方案 B 的复用逻辑会把第二个账号覆盖到第一条上（用户实测报告）。
	loginLabel string
	// loginBackendOpened 本次登录的授权页是否已由本工具（无痕窗口）拉起。
	// 前端据此**不再** window.open —— 否则会在日常浏览器里再开一个带登录态的标签页，
	// 无痕的努力就白费了（授权页照样会复用已登录的华为云账号）。
	loginBackendOpened bool
	loginStateFP       string
	loginKind          provider.Kind
	// loginErr 上一次登录流程的终态错误（成功/取消/超时为空串）。
	// 面板的登录轮询只看 login_busy 就宣告「登录完成」，于是失败也会显示成功——
	// 必须把真实结果带出去（否则用户只看到「登录完成」但账号没加进来）。
	loginErr  string
	pricingFP string

	// 智谱清言自动登录会话（独立于通用 OAuth 登录流程：
	// 它不轮询 state 文件，而是经 CDP 从浏览器 Cookie 捕获凭据）。
	muGLMLogin sync.Mutex
	glmLogin   *loginglm.AutoSession

	muOALogin sync.Mutex
	oaLogin   *officeace.LoginSession
	// oaStarting 表示「已通过入口检查、正在向云端申请 state」的瞬间窗口。
	//
	// 为什么需要它：StartLogin 里的 fetchState 是真实 HTTP，**不能**在 muOALogin 持有期间调用
	// （实测约 1s，期间所有状态轮询全部阻塞）。锁内只留状态变更，就必须另用一个占位标记
	// 防「用户双击/两次请求并发起两条流程」——那是原来靠长持锁顺带解决的问题。
	oaStarting bool

	// auth 管理面板会话（cookie）；详见 session.go
	auth *authState

	logFile *os.File

	// compatSyncer 面板保存 compat 后同步给外层兼容层（热更新路由表）。
	// 由 main.go 注入；nil 时仅写配置不热更（下次启动生效）。
	compatSyncer func(defaultChannel string, maxTokensCap int, modelMap map[string]string)

	// oczenSyncer 面板保存 oczen key 后写入渠道 client（热更新凭证）。
	// 由 main.go 注入；nil 时仅写配置不热更。
	oczenSyncer func(key string)

	// fbSyncer 面板保存模型降级链后推给 server.Handler（热更新降级路由）。
	// 由 main.go 注入；nil 时仅写配置不热更。
	fbSyncer func(rules []config.ModelFallbackRule)

	// efSyncer 面板保存思考强度映射后推给 server.Handler（热更新注入表）。
	// 由 main.go 注入；nil 时仅写配置不热更。
	efSyncer func(efforts map[string]string)

	// customSyncer 面板保存自定义渠道后热更新（R66）。
	// 由 main.go 注入；nil 时仅写配置不热更。
	customSyncer func(channels []custom.ChannelConfig)

	refreshMu  sync.Mutex // 防并发刷新积分
	refreshing bool

	// ledger 用量/积分双流水（data/ledger/）；锦上添花统计，nil = 未启用
	ledger     *ledger.Ledger
	ledgerStop chan struct{}

	// stats 运行统计引擎（data/stats.json）：面板「运行统计」tab 的数据面，
	// 数据注入全部进程内直调（见 internal/stats 包注释与 AGENTS.md R13）；nil = 未启用
	stats     *statsEngine.Stats
	statsStop chan struct{}

	pricingMu      sync.Mutex
	pricingCache   []provider.ModelPricing // 本地缓存
	pricingFetched time.Time
	pricingErr     string // 最近一次拉取错误
	pricingBusy    bool   // 防并发刷新（启动刷新与面板触发可能重叠）
}

// New 构建 App 并接管全局日志（写文件 + 环形缓冲）。
// wireGeminiDiag 装配 gemini 的 400 现场取证落盘。
//
// 与「请求体摘要记录」（config.body_log）**同一开关、同一 data/ 目录**：
// 用户显式开过诊断才写全量请求体；否则只写结构摘要（不含任何消息内容）到日志。
// 这样既尊重既有隐私口径（body_log 是唯一的内容级诊断开关），
// 又能在出问题时一次性拿到可比对的现场。
func wireGeminiDiag(enabled func() bool, dir string) {
	if gemini.DumpFailedBody != nil {
		return
	}
	gemini.DumpFailedBody = func(body, upstreamRaw []byte, status int) {
		if !enabled() {
			return
		}
		p, err := gemini.SaveFailedBody(dir, body, upstreamRaw, status)
		if err != nil {
			log.Printf("gemini 400 请求体落盘失败：%v", err)
			return
		}
		log.Printf("gemini 400 请求体已落盘：%s", p)
	}
}

func New(opts Options) (*App, error) {
	a := &App{
		cfgPath:  opts.ConfigPath,
		cfg:      opts.Config,
		runtimes: opts.Runtimes,
		handler:  opts.Handler,
		auth:     newAuthState(),
	}
	a.loginStateFP = filepath.Join(filepath.Dir(opts.Config.StateFile), "login-state.json")
	a.pricingFP = filepath.Join(filepath.Dir(opts.Config.StateFile), "pricing-cache.json")

	// 用量/积分双流水（data/ledger/）：优先复用 main 装配的实例
	if opts.Ledger != nil {
		a.ledger = opts.Ledger
	} else if lg, err := ledger.New(filepath.Join(filepath.Dir(opts.Config.StateFile), "ledger")); err == nil {
		a.ledger = lg
	} else {
		log.Printf("ledger init failed（统计功能不可用）: %v", err)
	}
	if a.ledger != nil {
		a.ledgerStop = make(chan struct{})
		go a.ledger.AutoFlush(a.ledgerStop)
		// 请求体摘要开关（诊断用，默认关）：进程级全局，启动时与 config.body_log 同步
		ledger.SetBodyTrace(a.cfg.BodyLog)
		// gemini 的 400 现场取证：同一开关控制「是否落盘全量请求体」，
		// 目录与请求体摘要同源（data/ 下），默认关时不写任何内容。
		wireGeminiDiag(func() bool { return ledger.BodyTraceEnabled() },
			filepath.Join(filepath.Dir(opts.Config.StateFile), "gemini-400"))
	}

	// 运行统计引擎（data/stats.json）：初始化失败不致命——端点回 disabled、插桩/喂入全判 nil
	if st, err := statsEngine.New(filepath.Dir(opts.Config.StateFile), log.Printf); err == nil {
		a.stats = st
		a.statsStop = make(chan struct{})
		go st.RunFlusher(a.statsStop)
	} else {
		log.Printf("stats init failed（运行统计不可用）: %v", err)
	}

	// 日志文件 data/app.log
	logFP := filepath.Join(filepath.Dir(opts.Config.StateFile), "app.log")
	if err := os.MkdirAll(filepath.Dir(logFP), 0o755); err == nil {
		f, err := os.OpenFile(logFP, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			a.logFile = f
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(&logWriter{app: a})
	a.loadPricingCache()
	a.healRaccoonProtocol()
	return a, nil
}

// stateDir 数据目录（loginStateFP / pricingFP 都在这里）。
func (a *App) stateDir() string { return filepath.Dir(a.cfg.StateFile) }

// healRaccoonProtocol 启动自愈：小浣熊授权登录会在 data/ 留一份注册表备份，
// 正常结束时会自行删除。若启动时它还在，说明上次登录中途退出/崩溃/被强杀，
// 此时立刻把 office-raccoon 注册表恢复原状，避免协议长期指向本程序。
func (a *App) healRaccoonProtocol() {
	dir := a.stateDir()
	if !raccoon.HasPendingHijack(dir) {
		return
	}
	restored, err := raccoon.RestoreProtocol(raccoon.BackupPath(dir))
	switch {
	case err != nil:
		log.Printf("启动自愈：小浣熊协议注册表恢复失败：%v（可手动检查 HKCU\\%s）", err, raccoon.ProtocolKeyPath)
	case restored:
		log.Printf("启动自愈：检测到上次登录残留的协议注册改写，已恢复 %s 注册表", raccoon.ProtocolScheme)
	default:
		log.Printf("启动自愈：残留备份存在但注册表已被官方客户端重写，仅清理备份文件")
	}
	_ = raccoon.ClearCallback(dir)
}

// Close 关闭日志文件。
func (a *App) Close() {
	// 停 ledger 自动落盘并冲刷句柄（丢最后几秒流水可接受，但不能一直丢）
	if a.ledgerStop != nil {
		select {
		case <-a.ledgerStop:
		default:
			close(a.ledgerStop)
		}
	}
	if a.ledger != nil {
		a.ledger.Close()
	}
	// 停统计引擎并同步冲刷：RunFlusher 的 stop 分支是异步收尾，
	// 进程退出路径必须 FlushNow 兜底（os.Exit 抢跑会丢最后几秒快照）
	if a.statsStop != nil {
		select {
		case <-a.statsStop:
		default:
			close(a.statsStop)
		}
	}
	if a.stats != nil {
		a.stats.FlushNow()
	}
	if a.logFile != nil {
		_ = a.logFile.Close()
		a.logFile = nil
	}
}

// Ledger 返回流水记账器（未启用时 nil；供 main 装配传给 server handler）。
func (a *App) Ledger() *ledger.Ledger { return a.ledger }

// Stats 返回运行统计引擎（未启用时 nil；供 main 装配传给 server handler 插桩）。
func (a *App) Stats() *statsEngine.Stats { return a.stats }

func (a *App) runtime(kind provider.Kind) *Runtime {
	a.runtimesMu.RLock()
	defer a.runtimesMu.RUnlock()
	if a.runtimes == nil {
		return nil
	}
	return a.runtimes[kind]
}

// runtimesSnapshot 返回运行时表的浅拷贝（调用方据此遍历，期间不受热更新影响）。
// 热更新只增删 map 键、不修改 Runtime 本体，故拷贝元素指针安全。
func (a *App) runtimesSnapshot() []*Runtime {
	a.runtimesMu.RLock()
	defer a.runtimesMu.RUnlock()
	out := make([]*Runtime, 0, len(a.runtimes))
	for _, rt := range a.runtimes {
		out = append(out, rt)
	}
	return out
}

// OpenLogFile 用系统默认编辑器打开日志文件。
func (a *App) OpenLogFile() error {
	fp := filepath.Join(filepath.Dir(a.cfg.StateFile), "app.log")
	if _, err := os.Stat(fp); err != nil {
		_ = os.WriteFile(fp, []byte("(empty)\n"), 0o644)
	}
	abs, err := filepath.Abs(fp)
	if err != nil {
		return err
	}
	return platform.OpenWithTextEditor(abs)
}

// NotifyCheckin 记录自动签到结果（调度器观察器）。
func (a *App) NotifyCheckin(platformName string, r scheduler.CheckinResult) {
	log.Printf("checkin platform=%s uid=%s ok=%t msg=%s remain=%d has_remain=%t",
		platformName, r.UID, r.OK, r.Msg, r.Remain, r.HasRemain)
}

// NotifyRefresh 记录 token 刷新结果（调度器观察器）。
func (a *App) NotifyRefresh(platformName, uid string, ok bool, msg string) {
	log.Printf("refresh platform=%s uid=%s ok=%t msg=%s", platformName, uid, ok, msg)
}

// logWriter 写日志文件。
type logWriter struct{ app *App }

func (w *logWriter) Write(p []byte) (int, error) {
	if w.app.logFile != nil {
		_, _ = w.app.logFile.Write(p)
	}
	// 运行统计：签到事件实时喂入（log 库单次 Write 即完整一行；非事件行
	// 只做至多 5 个锚定正则匹配即跳过，开销可忽略。与启动时的全量
	// RescanEventLog 按 uid|day 幂等去重，不会双计）
	if w.app.stats != nil {
		w.app.stats.ObserveLogLine(string(p))
	}
	return len(p), nil
}

// safeGo 带 panic 兜底的 goroutine 启动器。
func (a *App) safeGo(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC: %v", r)
			}
		}()
		f()
	}()
}
