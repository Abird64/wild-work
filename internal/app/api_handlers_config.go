package app

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"wild-work/internal/config"
	"wild-work/internal/custom"
)

func (a *App) apiConfigCheckinTimes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Times []string `json:"times"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetCheckinTimes(req.Times); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigLoopGuard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetLoopGuard(req.On); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigListen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host      string  `json:"host"`
		Port      int     `json:"port"`
		AdminPass *string `json:"admin_password"` // 指针：区分「未携带」与「显式清空」
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// 同一请求内先存密码再切监听：面板把「改成 0.0.0.0」与「填密码」合并保存，
	// 避免中间态因缺少密码而被拒。
	if req.AdminPass != nil {
		if err := a.SetAdminPassword(*req.AdminPass); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := a.SetListen(req.Host, req.Port); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigAdminPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetAdminPassword(req.Password); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetAPIKey(req.Key); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigAutostart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetAutostart(req.On); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigModelFallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rules []config.ModelFallbackRule `json:"rules"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetModelFallback(req.Rules); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigReasoningEfforts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Efforts map[string]string `json:"efforts"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetReasoningEfforts(req.Efforts); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigSceneRouting(w http.ResponseWriter, r *http.Request) {
	var req sceneRoutingReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "scene_bad_shape: 请求体不是合法 JSON："+err.Error())
		return
	}
	res, err := a.SetSceneRouting(req)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) apiConfigCompat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultChannel string            `json:"default_channel"`
		MaxTokensCap   int               `json:"max_tokens_cap"`
		ModelMap       map[string]string `json:"model_map"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetCompat(req.DefaultChannel, req.MaxTokensCap, req.ModelMap); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxy       string  `json:"proxy"`         // 备用上游通道地址（空串 = 关闭）
		OczenAPIKey *string `json:"oczen_api_key"` // 指针：区分「未携带」与「显式清空」
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	key := "\u0000unset\u0000"
	if req.OczenAPIKey != nil {
		key = *req.OczenAPIKey
	}
	if err := a.SetBackupProxy(req.Proxy, key); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigBodyLog(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetBodyLog(req.On); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiConfigOczenTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"api_key"` // 空 = 用当前生效凭证（配置中的 key 或匿名）
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	status, body, via, err := a.TestOczenKey(req.APIKey)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": 0, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": status == 200 || status == 429, "status": status, "body": body, "via_backup": via})
}

func (a *App) apiConfigCustomChannels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channels []custom.ChannelConfig `json:"channels"`
		Renames  []customRename         `json:"renames"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "解析请求失败："+err.Error())
		return
	}
	n, err := a.setCustomChannels(req.Channels, req.Renames)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"channels": a.cfg.CustomChannels,
		"renamed":  n,
	})
}

func (a *App) apiConfigCustomChannelTest(w http.ResponseWriter, r *http.Request) {
	var req custom.ChannelConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "解析请求失败："+err.Error())
		return
	}
	if req.BaseURL == "" {
		apiError(w, http.StatusBadRequest, "base_url 不能为空")
		return
	}
	// 面板回显的 api_key 是脱敏值：测试未改动过的渠道时必须用真实密钥，否则测试恒报 401
	//（与 #117 的「探针没走同一条链路」同类）。判定不能只认「…」：脱敏格式改成长头长尾后
	// 中间是四个星号，两种都得认，因为升级瞬间浏览器里可能还渲染着旧脱敏串。
	if isMaskedKey(req.APIKey) {
		req.APIKey = a.resolveCustomKey(req.Name)
	}
	// 探测前先做端点定位（R121）：用户粘的地址常带 /chat/completions 尾巴、漏协议头，
	// 直接拼会请求一个必死地址；Discover 同时回答「哪个根是真的」。
	// 除纯 ID 外还回能力清单（R119）：前端勾选导入时直接拿这些字段写配置；
	// 上游没声明的字段保持零值/nil ⇒ 前端标「未探测」，绝不拿 1M/64k 这类猜测值冒充上游声明（R49 同口径）。
	d, err := custom.Discover(req)
	var caps []custom.ModelCap
	if d != nil {
		caps = d.Caps
	}
	ids := make([]string, 0, len(caps))
	for _, c := range caps {
		ids = append(ids, c.ID)
	}
	resp := map[string]any{"models": ids, "caps": caps} // models 保持旧形状 []string，不破既有契约
	if d != nil {
		resp["base_url"], resp["adjusted"] = d.Base, d.Adjusted
		resp["renormed"], resp["models_ok"] = d.Renormed, d.ModelsOK
		resp["chat_ok"], resp["tries"] = d.ChatOK, d.Tries
		if d.Note != "" {
			resp["note"] = d.Note
		}
	}
	if err != nil {
		resp["ok"], resp["error"] = false, err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["ok"] = true
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) apiConfigCustomChannelKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "解析请求失败："+err.Error())
		return
	}
	key := a.resolveCustomKey(req.Name)
	if key == "" {
		apiError(w, http.StatusNotFound, fmt.Sprintf("自定义渠道 %s 不存在或未配置 api key", req.Name))
		return
	}
	log.Printf("已下发自定义渠道 %s 的 api_key 明文（面板复制）", req.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "api_key": key})
}
