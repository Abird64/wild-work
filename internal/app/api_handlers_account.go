package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/doubaocdp"
	"wild-work/internal/provider"
)

func (a *App) apiAccountImportLocal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	res, err := a.ImportLocalCredentials(req.Channel)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) apiAccountCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	res, err := a.CheckinAccount(req.UID)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) apiAccountCheckinAll(w http.ResponseWriter, r *http.Request) {
	results := a.CheckinAll()
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (a *App) apiAccountRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	remain, err := a.RefreshCredits(req.UID)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"remain": remain})
}

func (a *App) apiAccountRefreshAll(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.RefreshAll())
}

func (a *App) apiAccountRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.RemoveAccount(req.UID); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiAccountDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID      string `json:"uid"`
		Disabled bool   `json:"disabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.DisableAccount(req.UID, req.Disabled); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiAccountResourceDetail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	remain, items, expiring, _, err := a.ResourceDetail(req.UID)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 可用/不可用小计在服务端算好，前端只负责展示，避免两边口径漂移。
	usable, unusableSum := provider.Summarize(items)
	writeJSON(w, http.StatusOK, map[string]any{
		"remain":          remain,
		"items":           items,
		"usable_remain":   usable,
		"unusable_remain": unusableSum,
		// 临期小计与 pool 快照同源于本次响应：前端回写卡片时三个数字一起更新，
		// 避免「可用已同步、临期还是旧值」的半新半旧展示。
		"expiring_remain": expiring,
	})
}

func (a *App) apiAccountNickname(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := a.SetAccountNickname(req.UID, req.Nickname); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// apiDoubaoConnect 确保豆包客户端在调试端口上就绪（必要时以调试模式重启）。
//
// 为什么要做成显式动作（2026-10-30 用户实测）：
// 本渠道的签名靠客户端前端 JS hook 注入，必须客户端带 --remote-debugging-port 启动；
// 而用户的豆包是**开机自启**、那一路不带该参数，双击图标也带不上 —— 于是每次连接都失败，
// 且错误只说了「需要以 --remote-debugging-port 启动」，用户无法靠自己解决。
//
// 这个端点把那次重启变成一键动作。请求可带 addr（默认 127.0.0.1:30813）。
func (a *App) apiDoubaoConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addr string `json:"addr"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	addr := strings.TrimSpace(req.Addr)
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", doubaocdp.DefaultDebugPort)
	}
	// 30s：重启 + 等端口就绪（EnsureDebugPort 内部最多等 25s）。
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	restarted, err := doubaocdp.EnsureDebugPort(ctx, addr)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	msg := "豆包调试端口已就绪"
	if restarted {
		msg = "已把豆包以调试模式重启，端口已就绪"
	}
	log.Printf("doubao connect: %s (addr=%s)", msg, addr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarted": restarted, "msg": msg})
}
