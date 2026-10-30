package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a *App) apiState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.GetState())
}

func (a *App) apiFees(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.FeesInfo())
}

func (a *App) apiFeesRefresh(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.FeesInfo())
	go a.safeGo(func() { a.RefreshPricing() })
}

func (a *App) apiUsage(w http.ResponseWriter, r *http.Request) {
	if a.ledger == nil {
		writeJSON(w, http.StatusOK, map[string]any{"disabled": true})
		return
	}
	days := 7
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil {
		days = d
	}
	// enrich：uid → 昵称/渠道（查 pool 状态，仅一次遍历）
	nameOf := map[string]string{}
	chOf := map[string]string{}
	for _, rt := range a.runtimesSnapshot() {
		if rt == nil || rt.Pool == nil || rt.Alias {
			continue
		}
		for _, st := range rt.Pool.List() {
			nameOf[st.UID] = st.Nickname
			chOf[st.UID] = rt.Kind.String()
		}
	}
	stats := a.ledger.Query(days, func(uid string) (string, string) { return nameOf[uid], chOf[uid] })
	writeJSON(w, http.StatusOK, stats)
}

func (a *App) apiLogs(w http.ResponseWriter, r *http.Request) {
	fp := filepath.Join(filepath.Dir(a.cfg.StateFile), "app.log")
	raw, _ := os.ReadFile(fp)
	lines := strings.Split(string(raw), "\n")
	if len(lines) > 300 {
		lines = lines[len(lines)-300:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (a *App) apiStats(w http.ResponseWriter, r *http.Request) {
	if a.stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{"disabled": true})
		return
	}
	writeJSON(w, http.StatusOK, a.stats.Snapshot(time.Now()))
}

func (a *App) apiStatsLogs(w http.ResponseWriter, r *http.Request) {
	if a.stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
		return
	}
	limit := 15
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": a.stats.Logs(limit)})
}

func (a *App) apiQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	go a.safeGo(func() { a.Quit() })
}
