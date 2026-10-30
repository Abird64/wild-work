package app

import (
	"encoding/json"
	"net/http"

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
