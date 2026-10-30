package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	logincodearts "wild-work/internal/login_codearts"
	logingemini "wild-work/internal/login_gemini"
	loginlobsterai "wild-work/internal/login_lobsterai"
	loginqwenwork "wild-work/internal/login_qwenwork"
	logintrae "wild-work/internal/login_trae"
	"wild-work/internal/provider"
)

func (a *App) apiAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !a.panelAuthEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": ""})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	token, brief, ok, msg := a.panelLogin(req.Password, clientIP(r))
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": msg})
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": brief})
}

func (a *App) apiAuthLogout(w http.ResponseWriter, r *http.Request) {
	a.panelLogout(r)
	setSessionCookie(w, r, "") // 置空 + MaxAge<0 删除 cookie
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiAuthState(w http.ResponseWriter, r *http.Request) {
	// 会话探针：不返回 401，未登录时只回鉴权元信息（绝不回传 API-Key/账号/代理等
	// 已鉴权内容），供前端在首次加载时就弹出登录层。
	a.mu.Lock()
	pass, listen, version := a.cfg.AdminPass, a.cfg.Listen, Version
	a.mu.Unlock()
	// 未登录时只回鉴权元信息（结构体故意最小化：不回传 API-Key/账号/代理等任何已鉴权内容）
	authOn := strings.TrimSpace(pass) != ""
	probe := struct {
		AuthEnabled  bool   `json:"auth_enabled"`
		AuthSession  string `json:"auth_session"`
		AuthRequired bool   `json:"auth_required"`
		Version      string `json:"version"`
	}{authOn, "", requireRemoteAuth(listen), version}
	if authOn {
		if c, err := r.Cookie(sessionCookie); err == nil && a.auth.valid(c.Value, passBrief(pass)) {
			st := a.GetState() // 已持有有效会话：与 /api/state 等价
			writeJSON(w, http.StatusOK, st)
			return
		}
	}
	writeJSON(w, http.StatusOK, probe)
}

func (a *App) apiLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
		// AccountLabel 用户指定的「账号标识」（可选）。
		// 只对上游不提供稳定身份字段的渠道有意义（当前是 CodeArts）：填了就是独立账号，
		// 留空则按「同一账号重新登录」处理（覆盖该渠道已有条目）。
		AccountLabel string `json:"account_label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	a.muLogin.Lock()
	a.loginLabel = strings.TrimSpace(req.AccountLabel)
	a.loginBackendOpened = false
	a.muLogin.Unlock()
	url, err := a.StartLoginFor(req.Channel)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.muLogin.Lock()
	opened := a.loginBackendOpened
	kind := a.loginKind
	a.muLogin.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url":       url,
		"backend_opened": opened,
		// needs_poll（2026-10-18 H9）：告知前端这次登录是否需要轮询结果。
		// GLM 的形态是「只给链接、不跑流程」（凭据靠粘贴 Cookie），后端不置 loginBusy、
		// 不起 pollLogin；改造前前端只看 auth_url 非空就无条件 startLoginPoll()，
		// 于是轮询看到 login_busy=false 便提示「登录完成，正在同步账号…」——假成功。
		"needs_poll": a.needsPollFor(req.Channel),
		// manual_callback：该渠道的授权回调写死回环地址时（当前 CodeArts 与 LobsterAI，
		// 判据见 manualCallbackKind 单一来源，R96③/R97），浏览器与网关不同机
		// （局域网/远程访问面板）就收不到授权码，只能靠用户把地址栏 URL 粘回来，
		// 前端据此显示输入框。
		"manual_callback": manualCallbackKind(kind),
	})
}

func (a *App) apiLoginCallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	a.muLogin.Lock()
	busy, kind := a.loginBusy, a.loginKind
	a.muLogin.Unlock()
	if !busy {
		apiError(w, http.StatusBadRequest, "没有进行中的登录流程，请先发起登录")
		return
	}
	// 白名单与 /api/login/start 的 manual_callback 同源（manualCallbackKind），
	// 避免出现「前端显示了输入框、这里却 400」的半接线状态。
	if !manualCallbackKind(kind) {
		apiError(w, http.StatusBadRequest, fmt.Sprintf(
			"%s 渠道不需要手动回调：它的凭据由网关侧轮询/捕获，不接受回调地址", kind))
		return
	}
	switch kind {
	case provider.CodeArts:
		if err := logincodearts.SubmitCallbackURL(req.URL); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	case provider.LobsterAI:
		if err := loginlobsterai.SubmitCallbackURL(req.URL); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	case provider.TraeWork:
		// 状态在文件（login-state.json）而非内存会话，故把路径传进去；
		// 凭证解析/写入与自动回调同一条 state 通道，Poll 零分叉。
		if err := logintrae.SubmitCallbackURL(a.loginStateFP, req.URL); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	case provider.QwenWork:
		// 内存 pending 通道（与自动回调同一条，见 internal/login_qwenwork SubmitCallbackURL）。
		if err := loginqwenwork.SubmitCallbackURL(req.URL); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	case provider.Gemini:
		// 内存 session 通道（state 逐字比对，见 internal/login_gemini SubmitCallbackURL）。
		if err := logingemini.SubmitCallbackURL(req.URL); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// 提交成功必须回 JSON：PR #163 的合并曾在 multi-parent 合并时把这一行弄丢，
	// handler 变成空 200（前端 api() 容错空 body 故测试全绿，用户却看到「已提交」后
	// 无任何响应）。合并不丢行：成功/失败都要给前端一个可判定的响应体。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiLoginCancel(w http.ResponseWriter, r *http.Request) {
	if err := a.CancelLogin(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiLoginGlmAuto(w http.ResponseWriter, r *http.Request) {
	if err := a.StartGLMAutoLogin(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiLoginGlmAutoStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.GetGLMAutoLoginStatus())
}

func (a *App) apiLoginGlmAutoCancel(w http.ResponseWriter, r *http.Request) {
	if err := a.CancelGLMAutoLogin(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiLoginOfficeaceAuto(w http.ResponseWriter, r *http.Request) {
	url, err := a.StartOfficeAceLogin()
	if err != nil {
		// 已有流程进行中时也回传 URL，便于前端继续打开/轮询
		if url != "" {
			writeJSON(w, http.StatusOK, map[string]any{"auth_url": url, "warning": err.Error()})
			return
		}
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth_url": url})
}

func (a *App) apiLoginOfficeaceAutoStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.GetOfficeAceLoginStatus())
}

func (a *App) apiLoginOfficeaceAutoCancel(w http.ResponseWriter, r *http.Request) {
	if err := a.CancelOfficeAceLogin(); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiLoginGlmToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	uid, err := a.SubmitGLMToken(req.RefreshToken)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
}

func (a *App) apiLoginClineKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	uid, warn, err := a.SubmitClineKey(req.APIKey)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "warning": warn})
}
