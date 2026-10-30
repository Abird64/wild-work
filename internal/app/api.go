package app

import (
	"encoding/json"
	"net"
	"net/http"
)

// HandleAPI 注册管理 API 路由（挂到 server handler 的 /api/* 上）。
// 开启管理密码（config.admin_password 非空）时，除 /api/auth/* 外全部须先登录；
// 密码为空 = 不鉴权，此时仅允许监听环回地址（见 SetListen / SetAdminPassword）。
func (a *App) HandleAPI(mux *http.ServeMux) {
	// 登录/登出/会话探针：挂在 /api/auth/* 上（比 /api/ 更具体，不经过会话鉴权）。
	mux.HandleFunc("POST /api/auth/login", a.apiAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", a.apiAuthLogout)
	mux.HandleFunc("GET /api/auth/state", a.apiAuthState)

	// 其余管理 API 统一挂到私有 mux，最后整体套一层会话鉴权中间件：
	// 路由注册代码零改动，也避免逐个 handler 漏包。
	api := http.NewServeMux()
	mux.Handle("/api/", a.guardMux(api))
	mux = api

	mux.HandleFunc("GET /api/state", a.apiState)
	mux.HandleFunc("POST /api/login/start", a.apiLoginStart)
	// 手动提交授权回调地址（回环回调渠道的远程/局域网兜底）。
	//
	// 背景：回环回调五渠道的回调地址都写死 127.0.0.1/localhost——
	// CodeArts 的 portal 按 `port` 参数把浏览器跳到 `http://127.0.0.1:<port>/oauth/callback`，
	// LobsterAI 的 portal 按 `redirect_uri` 原样跳回 `http://127.0.0.1:<port>/auth/callback`，
	// TraeWork 的回调写死 `http://127.0.0.1:<随机端口>/authorize`（且监听只有 5 分钟寿命），
	// QwenWork 的 redirect_uri 白名单制不可改（`/callback`），
	// Gemini 的 installed-app 政策限 redirect host 为 localhost（`/oauth-callback`）。
	// 五者都只有在「浏览器与网关同一台机器」时授权码才自动回到网关。从局域网别处打开面板时，
	// 浏览器跳到的是**用户自己机器**的 127.0.0.1（连接被拒），流程只能等到超时。
	// 这里提供一条人工通道：把跳水后打不开的那条 URL 原样提交，网关解析出 code 后
	// 走与自动回调**完全相同**的兑换路径（凭据仍由网关侧换，用户不需要交出任何 token）。
	mux.HandleFunc("POST /api/login/callback", a.apiLoginCallback)
	mux.HandleFunc("POST /api/login/cancel", a.apiLoginCancel)
	// 导入型渠道（MonkeyCode）：从本机已登录的官方客户端读取凭据并写入 auths/。
	// 导入型渠道（小浣熊）：从本机已登录的官方客户端读取凭据并写入 auths/。
	// 导入型渠道（Loomy）：从本机已登录的官方客户端读取凭据并写入 auths/。
	mux.HandleFunc("POST /api/account/import_local", a.apiAccountImportLocal)
	mux.HandleFunc("POST /api/login/glm_auto", a.apiLoginGlmAuto)
	mux.HandleFunc("GET /api/login/glm_auto_status", a.apiLoginGlmAutoStatus)
	mux.HandleFunc("POST /api/login/glm_auto_cancel", a.apiLoginGlmAutoCancel)
	mux.HandleFunc("POST /api/login/officeace_auto", a.apiLoginOfficeaceAuto)
	mux.HandleFunc("GET /api/login/officeace_auto_status", a.apiLoginOfficeaceAutoStatus)
	mux.HandleFunc("POST /api/login/officeace_auto_cancel", a.apiLoginOfficeaceAutoCancel)
	mux.HandleFunc("POST /api/login/glm_token", a.apiLoginGlmToken)
	mux.HandleFunc("POST /api/login/cline_key", a.apiLoginClineKey)

	mux.HandleFunc("POST /api/account/checkin", a.apiAccountCheckin)
	mux.HandleFunc("POST /api/account/checkin_all", a.apiAccountCheckinAll)
	mux.HandleFunc("POST /api/account/refresh", a.apiAccountRefresh)
	mux.HandleFunc("POST /api/account/refresh_all", a.apiAccountRefreshAll)
	mux.HandleFunc("POST /api/account/remove", a.apiAccountRemove)
	mux.HandleFunc("POST /api/account/disable", a.apiAccountDisable)
	mux.HandleFunc("POST /api/account/resource_detail", a.apiAccountResourceDetail)
	mux.HandleFunc("POST /api/config/checkin_times", a.apiConfigCheckinTimes)
	mux.HandleFunc("POST /api/config/loop_guard", a.apiConfigLoopGuard)
	mux.HandleFunc("POST /api/config/listen", a.apiConfigListen)
	mux.HandleFunc("POST /api/config/admin_password", a.apiConfigAdminPassword)
	mux.HandleFunc("POST /api/config/api_key", a.apiConfigApiKey)
	mux.HandleFunc("POST /api/config/autostart", a.apiConfigAutostart)
	mux.HandleFunc("POST /api/config/model_fallback", a.apiConfigModelFallback)
	mux.HandleFunc("POST /api/config/reasoning_efforts", a.apiConfigReasoningEfforts)
	// 三场景原子端点（SD-6 唯一写入口）：三场景别名 + 三条降级链 + compat 三项一次落盘。
	mux.HandleFunc("POST /api/config/scene_routing", a.apiConfigSceneRouting)
	mux.HandleFunc("POST /api/config/compat", a.apiConfigCompat)
	mux.HandleFunc("POST /api/config/proxy", a.apiConfigProxy)
	mux.HandleFunc("POST /api/config/body_log", a.apiConfigBodyLog)
	mux.HandleFunc("POST /api/config/oczen_test", a.apiConfigOczenTest)
	mux.HandleFunc("POST /api/config/custom_channels", a.apiConfigCustomChannels)
	mux.HandleFunc("POST /api/config/custom_channel_test", a.apiConfigCustomChannelTest)
	// 面板「点击复制 api-key」：明文按需单条下发，不随渠道列表/费率接口批量返回
	//（否则每次刷新面板都把全部 key 送进浏览器，抓包/历史里全都是）。
	// 口径说明：自定义渠道的 key 是**用户自己填的第三方凭据**，不是本工具的上游会话 token
	//（不变量 2 管的是后者），且本端点同其它 /api/* 一样受 cookie 会话鉴权约束（R4）。
	// 日志只记渠道名，绝不落 key 本身。
	mux.HandleFunc("POST /api/config/custom_channel_key", a.apiConfigCustomChannelKey)
	mux.HandleFunc("POST /api/account/nickname", a.apiAccountNickname)
	mux.HandleFunc("GET /api/fees", a.apiFees)
	mux.HandleFunc("POST /api/fees/refresh", a.apiFeesRefresh)
	mux.HandleFunc("GET /api/usage", a.apiUsage)
	mux.HandleFunc("GET /api/logs", a.apiLogs)
	// 运行统计面板（web 第三个 tab）：快照 = internal/stats 引擎的实时聚合，
	// 载荷字段与 sidecar /capi/stats 对齐（前端 PR2 已按契约轮询）。
	mux.HandleFunc("GET /api/stats", a.apiStats)
	// 请求日志（最近 N 条，默认 15；rows 形状对齐 sidecar /capi/logs）
	mux.HandleFunc("GET /api/stats/logs", a.apiStatsLogs)
	mux.HandleFunc("POST /api/quit", a.apiQuit)
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// apiError 统一错误响应。
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// guardMux 会话鉴权中间件：密码为空（未启用）时原样放行，否则要求有效 cookie 会话。
// 另叠加 stdlib 的跨站请求保护（CSRF）：SameSite=Lax 已在浏览器侧拦跨站 POST，
// 这里是二次防御（非浏览器客户端无 Origin/Sec-Fetch-Site 头，不受影响）。
// 挂在内层 /api/ mux 上（/api/auth/* 由外层更具体的模式直接命中，不经过这里）。
func (a *App) guardMux(inner http.Handler) http.Handler {
	csrf := http.NewCrossOriginProtection()
	return csrf.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.panelAuthEnabled() {
			c, err := r.Cookie(sessionCookie)
			if err != nil || !a.auth.valid(c.Value, passBrief(a.adminPass())) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录或会话已过期", "need_login": true})
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
}

// setSessionCookie 下发/删除会话 cookie。
// HttpOnly 防脚本窃取（前端靠 /api/auth/state 的 auth_session 指纹判登录态，不读 cookie）；
// SameSite=Lax 挡跨站 CSRF；Secure 在 HTTPS 下自动开启（本服务默认 http，故按需）。
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	}
	if token == "" {
		c.MaxAge = -1 // 删除
	}
	http.SetCookie(w, c)
}

// clientIP 取登录限流用的客户端 IP（RemoteAddr 形如 host:port）。
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
