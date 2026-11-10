// gateway.go 三接口兼容层：OpenAI Chat Completions / OpenAI Responses / Anthropic Messages。
//
// 设计要点（两层结构）：
//   - 内层（internal/server.Handler）保持原样，只提供 POST /v1/chat/completions；
//     账号池、粘性路由、冷却、token 刷新、渠道改写全部留在那里，本层不感知。
//   - 外层（本包）负责协议转换与模型名路由，通过 in-process 调用内层（见 pipe.go），
//     不经过本地 HTTP 自环，避免二次监听/鉴权/超时。
//
// 数据流：客户端原生请求 → 转 OpenAI Chat 请求 → 内层 → Chat 响应 → 转回客户端原生协议。
package gateway

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auto"
)

// Config 兼容层配置。
type Config struct {
	// Inner 内层 handler（internal/server.Handler），必须非 nil。
	Inner http.Handler
	// APIKey 内层要求的 API Key；为空时内层不鉴权。
	APIKey string
	// Router 模型名路由规则。
	Router Router
	// MaxTokensCap 上游 max_tokens 上限（0 = 不限制）。
	// Anthropic 客户端的 max_tokens 常远大于上游接受值，需在转发前 clamp。
	MaxTokensCap int
	// MaxInnerBody 内层响应体（错误体/非流式响应体）读取上限，单位字节。
	// nil = 用 defaultMaxInnerBody。
	//
	// 用 thunk 而不是快照值：内层上限可在 NewHandler 里按配置
	// （server.max_request_body_mb，R114）覆盖，因此必须**每次取值**。
	// 装配层（cmd/wild-work）负责注入 `func() int64 { return server.MaxRequestBody }`——
	// 这样本包不必直接读 server 的包级可变全局（同一进程里并存两个 handler 时，
	// 全局会互相覆盖，谁生效取决于初始化顺序）。
	MaxInnerBody func() int64
}

// SystemOneHandler 是 systemone 端点的实际处理函数。
// 签名为 func(w, r, state, questions)。网关负责鉴权、解析 body 后调用。
// Handler 返回上游 JSON 响应体的状态码与原始字节供透传；错误表示内部故障（如网络错）。
type SystemOneHandler func(w http.ResponseWriter, r *http.Request, state string, questions map[string]any) (status int, body []byte, err error)

// Gateway 三接口兼容层。零值不可用，须经 New 构造。
type Gateway struct {
	inner        http.Handler
	apiKey       string
	apiKeySource func() string // 非 nil 时优先于 apiKey（面板热改 Key 后立即跟随）
	router       Router
	maxTokensCap int
	sysone       SystemOneHandler // 非 nil 时注册 POST /v1/systemone
	mu           sync.RWMutex     // 保护 router/maxTokensCap 热更新
	// maxBody 读写请求体的上限来源（装配层注入，见 Config.MaxInnerBody）。
	// nil 时用 defaultMaxInnerBody。
	maxBody func() int64

	// autoMu 保护 autoResolve（自动选路解析器，R165）。nil = 未接入。
	//
	// 为什么挂在 Gateway 上而不是 Router 字段里：`SetCompat` 会用新字面量**整体替换**
	// `g.router`（见该函数），任何塞进 Router 的字段都会在每次面板保存 compat 时被清空，
	// 变成「保存一次兼容配置，自动选路就悄悄失效」这类极难定位的回归。
	autoMu      sync.RWMutex
	autoResolve func(req auto.Request) (chosen string, chain, order []string, ok bool)
}

// SetAutoResolver 注入自动选路解析器（R165；main 装配时传 app.AutoResolver()）。
// 注入后兼容层的 `model:"auto"` 才会被就地解析成具体 `<channel>/<model>`。
func (g *Gateway) SetAutoResolver(fn func(req auto.Request) (chosen string, chain, order []string, ok bool)) {
	if g == nil {
		return
	}
	g.autoMu.Lock()
	g.autoResolve = fn
	g.autoMu.Unlock()
}

// autoDecide 取当前解析器并执行一次决策（未注入 ⇒ ok=false）。
func (g *Gateway) autoDecide(req auto.Request) (string, []string, []string, bool) {
	g.autoMu.RLock()
	fn := g.autoResolve
	g.autoMu.RUnlock()
	if fn == nil {
		return "", nil, nil, false
	}
	return fn(req)
}

// New 构造兼容层。inner 为 nil 时返回 nil（调用方据此跳过兼容层，保持旧行为）。
func New(cfg Config) *Gateway {
	if cfg.Inner == nil {
		return nil
	}
	return &Gateway{
		inner:        cfg.Inner,
		apiKey:       cfg.APIKey,
		router:       cfg.Router,
		maxTokensCap: cfg.MaxTokensCap,
		maxBody:      cfg.MaxInnerBody,
	}
}

// SetAPIKeySource 注入 API Key 实时读取函数（通常为 server.Handler.CurrentAPIKey）。
// 注入后无需在面板改 Key 时同步本层状态。
func (g *Gateway) SetAPIKeySource(fn func() string) {
	if g != nil {
		g.apiKeySource = fn
	}
}

// SetSystemOneHandler 注入 systemone 端点处理器（非 nil 时注册路由 /v1/systemone）。
func (g *Gateway) SetSystemOneHandler(h SystemOneHandler) {
	if g != nil {
		g.sysone = h
	}
}

// withRouter 在读锁内执行 fn（Router 指针语义，不可拷贝）。
func (g *Gateway) withRouter(fn func(rt *Router)) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	fn(&g.router)
}

// SetCompat 热更新路由配置（default_channel / max_tokens_cap / model_map）。
// 面板保存 compat 时调用；不刷新的话新映射要到下次重启才生效。
// channels 用于校验映射目标的渠道前缀，调用方应传入当前已接入渠道。
func (g *Gateway) SetCompat(defaultChannel string, maxTokensCap int, modelMap map[string]string, channels []string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.router = Router{
		Default:  defaultChannel,
		Map:      modelMap,
		Channels: SortChannels(channels),
	}
	g.maxTokensCap = maxTokensCap
	g.mu.Unlock()
}

// key 返回当前生效的 API Key。
func (g *Gateway) key() string {
	if g.apiKeySource != nil {
		if k := g.apiKeySource(); k != "" {
			return k
		}
	}
	return g.apiKey
}

// Routes 在 mux 上注册三接口路由 + 可选的 SystemOne 端点。
//
// 说明：仅注册内层没有的路径。POST /v1/chat/completions 与 GET /v1/models 由内层直接服务，
// 无需本层介入，这样旧客户端的调用栈完全不变。
func (g *Gateway) Routes(mux *http.ServeMux) {
	if g == nil {
		return
	}
	mux.HandleFunc("POST /v1/responses", g.withAuth(g.handleResponses))
	mux.HandleFunc("POST /v1/messages", g.withAuth(g.handleAnthropicMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", g.withAuth(g.handleCountTokens))
	if g.sysone != nil {
		mux.HandleFunc("POST /v1/systemone", g.withAuth(g.handleSystemOne))
	}
}

// withAuth 校验客户端凭据：同时接受 Anthropic 风格的 x-api-key 与标准 Bearer。
// Key 为空（内层不鉴权）时直接放行。
func (g *Gateway) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := g.key()
		if key == "" {
			next(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.TrimSpace(token) != key {
			if strings.TrimSpace(r.Header.Get("x-api-key")) != key {
				// 错误体形状按协议区分：Anthropic 客户端只认 {"type":"error",...}
				if strings.Contains(r.URL.Path, "/messages") {
					writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
						"invalid x-api-key or Authorization header")
				} else {
					writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key",
						"missing or invalid API key")
				}
				return
			}
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// 共用工具
// ---------------------------------------------------------------------------

// chatRequest 是发往内层的请求体（OpenAI Chat Completions 子集 + 透传字段）。
// 用 map 而非结构体：上游渠道会读取各自私有字段（如 reasoning_effort/thinking），
// 结构体能承载的字段有限，map 可原样保真转发。
type chatRequest = map[string]any

// callChat 把 Chat 请求投递给内层，返回其响应（流式 raw body / 非流式已解析）。
//
// 降级标注头必须在此统一转写：两个兼容端点都经由本函数拿内层响应，而外层响应体
// 是**重建**的（model 字段按客户端请求名回填，见 R15 口径），不搬该头就等于静默换模型。
// 调用时机在设计上就安全：本函数在两条路径的首个 WriteHeader 之前。
func (g *Gateway) callChat(w http.ResponseWriter, r *http.Request, req chatRequest, stream bool, autoOrder []string) (*innerResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// 自动选路的降级全序必须**带进内层**（R167）：本层已把 `auto` 换成裸模型名，
	// 内层若拿不到这个头就只能走静态降级链，而那条链对 auto 选出的模型一条都不命中。
	var extras []string
	if hdr := strings.Join(autoOrder, ","); strings.TrimSpace(hdr) != "" {
		extras = []string{auto.OrderHeader, hdr}
	}
	res, err := g.call(r.Context(), "/v1/chat/completions", body, g.key(), extras...)
	if err != nil {
		return nil, err
	}
	if v := res.Header.Get("X-Model-Fallback"); v != "" {
		w.Header().Set("X-Model-Fallback", v)
	}
	return res, nil
}

// readChatResponse 读取内层非流式响应，返回 (状态码, 原始 body)。
func readChatResponse(res *innerResult) (int, []byte, error) {
	raw, err := res.ReadAll()
	return res.Status, raw, err
}

// logf 统一日志前缀，便于在 data/app.log 中与内层日志区分。
func logf(format string, args ...any) {
	log.Printf("[compat] "+format, args...)
}

// nowUnix 便于测试注入（当前仅用于响应 created 字段）。
var nowUnix = func() int64 { return time.Now().Unix() }

// parseJSONBody 读取并解析请求体，失败时按协议形状回错误。
// anthropicShape=true 时错误体为 Anthropic 形状。
func parseJSONBody(w http.ResponseWriter, r *http.Request, anthropicShape bool, limit func() int64) (map[string]any, bool) {
	raw, err := readLimited(r, limit())
	if err != nil {
		status, code := http.StatusBadRequest, "invalid_request"
		if errors.Is(err, errBodyTooLarge) || strings.Contains(err.Error(), "exceeds limit") {
			status, code = http.StatusRequestEntityTooLarge, "request_too_large"
		}
		if anthropicShape {
			writeAnthropicError(w, status, "invalid_request_error", err.Error())
		} else {
			writeOpenAIError(w, status, code, err.Error())
		}
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		if anthropicShape {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		}
		return nil, false
	}
	return obj, true
}

// resolveModel 解析模型名（无能力要求版本：常驻客户端传的普通模型名）。
func (g *Gateway) resolveModel(w http.ResponseWriter, model string, anthropicShape bool) (string, []string, bool) {
	return g.resolveModelReq(w, model, anthropicShape, auto.Request{})
}

// resolveModelReq 解析模型名，失败时按协议形状回错误；`model:"auto"` 走自动选路（R165）。
//
// 自动选路必须在 Router 之前拦截：`auto` 既不是渠道名、也不该进 model_map
// （否则用户配一条 `auto -> x` 就能把整套选路静默架空）。
// 解析失败语义与内层 handler 对齐：未注入/未启用 ⇒ 400 invalid_model；
// 已启用但无候选 ⇒ 400 invalid_model 并指向面板预览（兼容层没有 503 的语义位）。
func (g *Gateway) resolveModelReq(w http.ResponseWriter, model string, anthropicShape bool, req auto.Request) (string, []string, bool) {
	if strings.EqualFold(strings.TrimSpace(model), auto.Alias) {
		chosen, chain, order, ok := g.autoDecide(req)
		if !ok {
			if anthropicShape {
				writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
					"model \"auto\" 未启用或当前没有可用候选：请在面板「自动选路」开启并预览候选剔除原因")
			} else {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_model",
					"model \"auto\" 未启用或当前没有可用候选：请在面板「自动选路」开启并预览候选剔除原因")
			}
			return "", nil, false
		}
		w.Header().Set("X-Model-Origin", "auto")
		w.Header().Set("X-Model-Auto-Chosen", chosen)
		if len(chain) > 1 {
			w.Header().Set("X-Model-Auto-Chain", strings.Join(chain, ","))
		}
		logf("auto: model=auto chosen=%s chain=%s order=%d", chosen, strings.Join(chain, ","), len(order))
		return chosen, order, true
	}
	var resolved string
	var rerr error
	g.withRouter(func(rt *Router) {
		resolved, rerr = rt.Resolve(model)
	})
	if rerr != nil {
		if anthropicShape {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", rerr.Error())
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_model", rerr.Error())
		}
		return "", nil, false
	}
	return resolved, nil, true
}

// handleSystemOne 处理 POST /v1/systemone —— Jev 结构化决策端点。
// 请求体：{"state": "...", "questions": {...}} → 透传到 OpenCode Zen 上游。
// 网关负责 API key 鉴权与解析；model 固定为 jev-1.13-free（无需客户端传）。
func (g *Gateway) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	body, ok := parseJSONBody(w, r, false, g.bodyLimit)
	if !ok {
		return
	}
	state := asString(body["state"])
	if state == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: state")
		return
	}
	questions, ok := body["questions"].(map[string]any)
	if !ok || len(questions) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: questions")
		return
	}
	status, respBody, err := g.sysone(w, r, state, questions)
	if err != nil {
		logf("systemone upstream error: %v", err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}
