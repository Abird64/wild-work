// Package server 暴露 OpenAI 兼容 HTTP 接口，按模型名前缀路由到不同上游。
package server

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/ledger"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	// 别名 statsEngine：与运行统计引擎包名一致（本包无同名局部变量，别名仅为与 app 包对齐可读）
	statsEngine "wild-work/internal/stats"
)

// Runtime 是一个平台的一组运行时资源：pool + upstream + 静态模型兜底。
type Runtime struct {
	Kind         provider.Kind
	Pool         *pool.Pool
	Upstream     provider.Upstream
	StaticModels []provider.ModelInfo

	// NoCooldownOnServerError 声明该渠道的上游 5xx 属于网关/基础设施抖动
	// （账号本身健康），不应计入账号错误触发冷却。
	// WorkBuddy 国际版开启（其 openresty 网关实测间歇性 502/503/504）。
	//
	// ⚠️ 2026-10-18 起本字段**不再决定计数行为**（5xx 已默认不计数，见
	// CountServerErrorAsAccountFault），仅保留作渠道差异的文档标注。
	NoCooldownOnServerError bool

	// CountServerErrorAsAccountFault 显式要求「本渠道的上游 5xx 计入账号错误」。
	//
	// 默认 false（5xx 不计入账号错误）：5xx 的语义是服务端故障，与客户端凭据健康无关。
	// 曾经的默认行为是「计入」，导致一次上游 500 风暴把整池健康账号打成
	// consecutive errors 冷却 10 分钟，整条渠道 503（2026-10-18 lobsterai 实测）。
	//
	// 仅当某渠道**确实用 5xx 表达账号级封禁/欠费**时才置 true —— 当前无一渠道如此。
	CountServerErrorAsAccountFault bool

	// SingleAccount 声明该渠道只有唯一一个、且**无法人工恢复**的账号（当前仅 oczen 匿名）。
	// 语义：任何账号级惩罚（冷却/计数/禁用）都等价于「整条渠道下线」，故一律不适用——
	// 出错就原文透传，把重试交给客户端。区别于 NoCooldownOnServerError（只豁免 5xx）。
	// 生效范围：
	//   - 传输层错误：不累计 errCount（否则 3 次网络抖动即冷却唯一账号）；
	//   - ErrSoftRate（429）：不冷却。单账号无号可轮换，冷却只会把后续请求挡在
	//     挑号阶段（返回 no_healthy_account），连「稍后重试」都做不到；透传 429
	//     才能让客户端按 Retry-After 自行重试；
	//   - 其余分类：统一不罚账号（ErrHardCredit 无余额概念、ErrSessionDead 不可重登）。
	SingleAccount bool

	mu       sync.RWMutex
	models   []provider.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// Config handler 依赖。
type Config struct {
	Runtimes map[provider.Kind]*Runtime
	APIKey   string // 空 = 不鉴权

	// WebUI 为内嵌的静态 Web UI 文件系统（go:embed 产物）；非 nil 时挂载到 /
	WebUI fs.FS
	// AttachAPI 由调用方注册管理 API 路由（internal/app 的 HandleAPI）
	AttachAPI func(mux *http.ServeMux)

	// 兼容旧调用方：只传 Pool/Upstream 时等价于只启用 workbuddy。
	Pool     *pool.Pool
	Upstream provider.Upstream

	// Ledger 用量/积分流水记账器（非 nil 时成功请求记 token 流水）。
	Ledger *ledger.Ledger

	// Stats 运行统计引擎（非 nil 时转发请求插桩 AddUsage/AddRequestRow；见 internal/stats 包注释）。
	Stats *statsEngine.Stats

	// MaxRequestBody 请求体上限（字节）。0 = 用 DefaultMaxRequestBody（128MiB）。
	MaxRequestBody int64

	HardCooldown time.Duration
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	RefreshSkew  time.Duration

	// HardCooldownFor 硬冷却（402 余额/权益不足）时长的计算钩子：nil 时恒用
	// HardCooldown。非 nil 时由装配层注入「截到每日发奖窗口」策略（见
	// config.HardCooldownFor）——放在钩子里而不是 server 直接依赖 config，
	// 是因为 server 包只依赖 provider/pool/ledger，不引入配置层。
	HardCooldownFor func(now time.Time) time.Duration

	// ModelFallbacks 模型降级链初始值（面板保存后经 SetModelFallbacks 热更新）。
	ModelFallbacks []config.ModelFallbackRule

	// ReasoningEfforts 思考强度注入映射初始值（面板保存后经 SetReasoningEfforts 热更新）。
	ReasoningEfforts map[string]string
}

// stickyEntry 粘性路由记录：记录上次路由账号及连续使用次数。
// 不使用 credits（pool 中余额仅在签到/手动刷新时更新，对话后是 stale 数据），
// 改用请求计数：连续请求 maxReqs 次后自动降级换账号。
type stickyEntry struct {
	uid      string
	reqCount int
	maxReqs  int
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	ledger *ledger.Ledger     // 记账器（cfg.Ledger 透传，nil = 不记账）
	stats  *statsEngine.Stats // 运行统计引擎（cfg.Stats 透传，nil = 不插桩）

	apiMu    sync.RWMutex // 保护 cfg.APIKey（面板可运行时修改）
	stickyMu sync.RWMutex
	sticky   map[string]*stickyEntry // runtimeKind → stickyEntry

	fbMu      sync.RWMutex               // 保护 fallbacks（面板可运行时修改）
	fallbacks []config.ModelFallbackRule // 模型降级链（cfg.ModelFallbacks 初始，热更新经 SetModelFallbacks）

	efMu    sync.RWMutex      // 保护 efforts（面板可运行时修改）
	efforts map[string]string // 思考强度注入映射（cfg.ReasoningEfforts 初始，热更新经 SetReasoningEfforts）

	mmMu     sync.RWMutex      // 保护 modelMap（面板可运行时修改）
	modelMap map[string]string // 裸名别名表（compat.model_map 的副本，热更新经 SetModelMap；nil/空 = 不解析别名）

	runtimesMu sync.RWMutex               // 保护 customRts 动态热更新（R66）
	customRts  map[provider.Kind]*Runtime // 自定义渠道运行时
}

// SetCustomRuntimes 热更新自定义渠道运行时（面板保存后由 app 层回调）。
func (h *Handler) SetCustomRuntimes(rts map[provider.Kind]*Runtime) {
	h.runtimesMu.Lock()
	h.customRts = rts
	h.runtimesMu.Unlock()
}

// getRuntime 取渠道运行时。内建渠道优先——自定义渠道名不得与内建同名，
// 该约束由 custom.ChannelConfig.Validate 强制（否则两层会指向不同 Runtime：
// 面板/账号操作打到自定义池，而对话请求走内建渠道）。
func (h *Handler) getRuntime(k provider.Kind) *Runtime {
	if rt := h.cfg.Runtimes[k]; rt != nil {
		return rt
	}
	h.runtimesMu.RLock()
	defer h.runtimesMu.RUnlock()
	return h.customRts[k]
}

// SetModelFallbacks 热更新模型降级链（面板保存后由 app 层回调；nil/空 = 清空禁用）。
func (h *Handler) SetModelFallbacks(rules []config.ModelFallbackRule) {
	h.fbMu.Lock()
	h.fallbacks = rules
	h.fbMu.Unlock()
}

// SetReasoningEfforts 热更新思考强度注入映射（面板保存后由 app 层回调；nil/空 = 清空禁用）。
func (h *Handler) SetReasoningEfforts(efforts map[string]string) {
	h.efMu.Lock()
	h.efforts = efforts
	h.efMu.Unlock()
}

// SetModelMap 热更新内层别名表（= `compat.model_map` 的副本，面板保存后由 app 层回调；
// nil/空 = 不解析别名，行为与改造前逐字等价）。
//
// 为什么必须拷贝：调用方（app/main）持有的是同一份 config 对象，面板保存时可能原地
// 改写并复用该 map；共享同一个 map 会让「拷一份快照」的并发读语义失效（读到半改状态）。
// nil 归一成空 map，使「未配置」与「配置为空」在解析处走同一分支。
func (h *Handler) SetModelMap(m map[string]string) {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	h.mmMu.Lock()
	h.modelMap = cp
	h.mmMu.Unlock()
}

func NewHandler(cfg Config) *Handler {
	if cfg.Runtimes == nil && cfg.Pool != nil && cfg.Upstream != nil {
		cfg.Runtimes = map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: cfg.Pool, Upstream: cfg.Upstream, StaticModels: WorkBuddyStaticModels()},
		}
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.MaxRequestBody > 0 {
		MaxRequestBody = cfg.MaxRequestBody
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sticky: make(map[string]*stickyEntry), ledger: cfg.Ledger, stats: cfg.Stats, fallbacks: cfg.ModelFallbacks, efforts: cfg.ReasoningEfforts}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(h.imagesGenerations))
	h.mux.HandleFunc("POST /v1/images/edits", h.withAuth(h.imagesEdits))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.WebUI != nil {
		// no-cache 强制浏览器每次 revalidate：embed 资源随二进制更新，若不加，
		// 浏览器长期用旧缓存会出现界面与后端版本不匹配（如新字段不渲染）。
		// 文件未变时 FileServer 回 304，开销可忽略。
		webSrv := http.FileServer(http.FS(cfg.WebUI))
		h.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			webSrv.ServeHTTP(w, r)
		}))
	}
	if cfg.AttachAPI != nil {
		cfg.AttachAPI(h.mux)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// stickyKey 粘性路由 key（按渠道独立）
func (h *Handler) stickyKey(kind provider.Kind) string { return kind.String() }

// pickWithSticky 粘性路由选择账号。
// 优先使用上次成功路由的账号，直到：
//   - 账号进入冷却/禁用状态，或对该请求模型处于模型级冷却（429/6004 按模型限额）
//   - 连续成功请求达到 maxReqs 次（默认 50），自动轮换
//
// 任一条件触发则降级为 PickForModel(model) 选新账号并重置粘性记录。
func (h *Handler) pickWithSticky(rt *Runtime, model string) *auth.Auth {
	const defaultMaxReqs = 50

	h.stickyMu.RLock()
	sticky := h.sticky[h.stickyKey(rt.Kind)]
	h.stickyMu.RUnlock()

	// 尝试粘性路由
	if sticky != nil && sticky.uid != "" && sticky.reqCount < sticky.maxReqs {
		acct := rt.Pool.AuthByUID(sticky.uid)
		if acct != nil {
			status, ok := rt.Pool.Status(sticky.uid)
			modelCooled := ok && status.ModelCooling != nil && status.ModelCooling[model].After(time.Now())
			// ⚠️ 2026-10-18 补 SessionDead：本判据曾只查 Cooling/Disabled/模型级冷却，
			// 而 `healthy()`（pool.go:103）明确把 sessionDead 也算不健康。
			// 粘性路由直接调 AuthByUID、**绕过了 healthy()**，于是登录态已失效的账号
			// 仍被粘住 ⇒ 每个请求都白打一次必 401 的上游探测。
			// （MonkeyCode 实测：全日志 16 条 session_dead 401，且一次刷新都没发起。）
			if ok && !status.Cooling && !status.Disabled && !status.SessionDead && !modelCooled {
				log.Printf("sticky route platform=%s uid=%s count=%d/%d",
					rt.Kind, sticky.uid, sticky.reqCount, sticky.maxReqs)
				return acct
			}
			if ok && status.SessionDead {
				log.Printf("sticky route skipped platform=%s uid=%s reason=session_dead",
					rt.Kind, sticky.uid)
			}
		}
	}

	// 降级：选择对当前模型可用的、余额最高的 healthy 账号
	acct := rt.Pool.PickForModel(model)
	if acct == nil {
		return nil
	}

	// 新建粘性记录
	h.stickyMu.Lock()
	h.sticky[h.stickyKey(rt.Kind)] = &stickyEntry{uid: acct.UID, maxReqs: defaultMaxReqs}
	h.stickyMu.Unlock()
	log.Printf("new sticky route platform=%s uid=%s maxReqs=%d", rt.Kind, acct.UID, defaultMaxReqs)
	return acct
}

// stickySuccess 粘性路由成功：递增请求计数。
func (h *Handler) stickySuccess(rt *Runtime) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	key := h.stickyKey(rt.Kind)
	if e := h.sticky[key]; e != nil {
		e.reqCount++
	}
}

// stickyClear 粘性路由失败（错误/冷却）：清除粘性记录，下次请求强制重新选号。
func (h *Handler) stickyClear(rt *Runtime) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	delete(h.sticky, h.stickyKey(rt.Kind))
}

// StickyState 粘性路由快照片段（运行统计喂入用）。
type StickyState struct {
	UID   string
	Count int // 已连续成功请求数
	Max   int // 轮换阈值（默认 50）
}

// StickySnapshot 返回粘性路由快照（key = 渠道名，即 provider.Kind.String()）。
// 供运行统计引擎喂 ApplySticky——进程内直读取代 sidecar 对粘性日志行的解析；
// 无粘性记录的渠道不出现在结果里。
func (h *Handler) StickySnapshot() map[string]StickyState {
	h.stickyMu.RLock()
	defer h.stickyMu.RUnlock()
	out := make(map[string]StickyState, len(h.sticky))
	for k, e := range h.sticky {
		if e == nil || e.uid == "" {
			continue
		}
		out[k] = StickyState{UID: e.uid, Count: e.reqCount, Max: e.maxReqs}
	}
	return out
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key := h.currentAPIKey(); key != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != key {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// SetAPIKey 运行时修改内层 API Key（面板可调用）。
func (h *Handler) SetAPIKey(key string) { h.apiMu.Lock(); defer h.apiMu.Unlock(); h.cfg.APIKey = key }

// CurrentAPIKey 读取当前生效的 API Key（供外层兼容层跟随面板修改）。
func (h *Handler) CurrentAPIKey() string { return h.currentAPIKey() }

func (h *Handler) currentAPIKey() string {
	h.apiMu.RLock()
	defer h.apiMu.RUnlock()
	return h.cfg.APIKey
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	accounts := map[string]any{}
	for _, k := range h.runtimeKinds() {
		rt := h.getRuntime(k)
		if rt != nil && rt.Pool != nil {
			accounts[k.String()] = rt.Pool.List()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (h *Handler) runtimeForModel(model string) (*Runtime, string, error) {
	// 解析与 provider.PublicModelID 同源：既接受 `<渠道>/<上游ID>`，也接受上游 ID 自带命名空间的
	// 直通形态（`cline-free/*`）——后者首段不是渠道名，必须整串当上游模型名（决议 R59）。
	kind, upstream, ok := provider.SplitModelID(model)
	if !ok {
		return nil, "", fmt.Errorf("model must use explicit prefix: workbuddy/<model> / workbuddyai/<model> / traework/<model> / qoder/<model> / qodercn/<model> / qodercom/<model> / qwenwork/<model> / glm/<model> / oczen/<model> / kilo/<model> / minimax/<model> / lobsterai/<model> / zcode/<model> / gemini/<model> / codearts/<model>；Cline 免费池直接用上游 ID（cline-free/<model>）")
	}
	rt := h.getRuntime(kind)
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return nil, "", fmt.Errorf("provider %q is not configured", kind)
	}
	if len(rt.Pool.List()) == 0 {
		return nil, "", fmt.Errorf("provider %q has no account", kind)
	}
	return rt, upstream, nil
}

func (h *Handler) runtimeKinds() []provider.Kind {
	h.runtimesMu.RLock()
	customCopy := make(map[provider.Kind]bool, len(h.customRts))
	for k := range h.customRts {
		customCopy[k] = true
	}
	h.runtimesMu.RUnlock()

	ks := make([]provider.Kind, 0, len(h.cfg.Runtimes)+len(customCopy))
	seen := map[provider.Kind]bool{}
	for k := range h.cfg.Runtimes {
		ks = append(ks, k)
		seen[k] = true
	}
	for k := range customCopy {
		if !seen[k] {
			ks = append(ks, k)
		}
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}
