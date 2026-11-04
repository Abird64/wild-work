// quota_windows_test.go 百分比配额窗口的提取与下发（2026-10-30 用户实测驱动）。
//
// 现场：Gemini 账号额度明明满格（5 小时 100% / 周 100%），面板却显示「0 可用积分」——
// 因为这类渠道的 `UserResource` 恒返回 0（额度是窗口百分比，不是可消耗积分）。
// 用户原话：「是不是要单独给它写一种新的表现形式，为这种百分比配额制度的渠道做一个前端展示」。
package app

import (
	"path/filepath"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// TestQuotaWindowsFromExtractsPercentItems 只有 Total==100 的条目才是百分比窗口。
func TestQuotaWindowsFromExtractsPercentItems(t *testing.T) {
	items := []provider.ResourceItem{
		{Name: "5 小时窗口（%）", Remain: 100, Total: 100, ExpireAt: "2026-10-30T18:58:55+08:00"},
		{Name: "周窗口（%）", Remain: 63, Total: 100, ExpireAt: "2026-11-06T13:58:55+08:00"},
		// 普通积分条目（total 是分值）不得被误收成窗口
		{Name: "每日积分", Remain: 900, Total: 1000},
		// total=0 的残缺条目跳过
		{Name: "残缺", Remain: 5, Total: 0},
	}
	ws := quotaWindowsFrom(items)
	if len(ws) != 2 {
		t.Fatalf("应只提取 2 个百分比窗口，得 %d：%+v", len(ws), ws)
	}
	if ws[0].Name != "5 小时窗口（%）" || ws[0].Percent != 100 {
		t.Errorf("窗口 1 错误：%+v", ws[0])
	}
	if ws[1].Percent != 63 {
		t.Errorf("窗口 2 百分比错误：%+v", ws[1])
	}
	if ws[0].ResetAt.IsZero() {
		t.Error("resetTime 应被解析出来（面板要显示何时重置）")
	}
}

// TestQuotaWindowsFromClampsPercent 百分比必须落在 0~100（上游抖动不该溢出）。
func TestQuotaWindowsFromClampsPercent(t *testing.T) {
	ws := quotaWindowsFrom([]provider.ResourceItem{
		{Name: "a", Remain: 150, Total: 100},
		{Name: "b", Remain: -5, Total: 100},
	})
	if ws[0].Percent != 100 || ws[1].Percent != 0 {
		t.Fatalf("应钳到 0~100，得 %+v", ws)
	}
}

// TestQuotaWindowsFromEmpty 没有窗口（普通积分渠道）时必须返回空，不得造出假窗口。
func TestQuotaWindowsFromEmpty(t *testing.T) {
	if ws := quotaWindowsFrom([]provider.ResourceItem{{Name: "x", Remain: 5, Total: 500}}); len(ws) != 0 {
		t.Fatalf("不该产生窗口：%+v", ws)
	}
}

// TestQuotaWindowsPersistAndCopy 窗口必须能落盘读回，且 getter 返回副本（防外部改动）。
func TestQuotaWindowsPersistAndCopy(t *testing.T) {
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")
	p := pool.New(stateFp)
	p.Add(&auth.Auth{UID: "u1"})
	ws := []pool.QuotaWindow{{Name: "5h", Percent: 80, ResetAt: time.Unix(1793340000, 0)}}
	p.SetQuotaWindows("u1", ws)

	got := p.QuotaWindows("u1")
	if len(got) != 1 || got[0].Percent != 80 {
		t.Fatalf("读回失败：%+v", got)
	}
	got[0].Percent = 1 // 改副本不得影响池内
	if p.QuotaWindows("u1")[0].Percent != 80 {
		t.Fatal("QuotaWindows 必须返回副本")
	}

	// 落盘 → 重新加载仍要有（否则重启后回到「待刷新」，而余额却还在，自相矛盾）
	p2 := pool.New(stateFp)
	if got := p2.QuotaWindows("u1"); len(got) != 1 || got[0].Percent != 80 {
		t.Fatalf("重启后窗口丢失：%+v", got)
	}
}

// TestGeminiCreditsUnitIsPercent 守门：Gemini 必须被标成 percent 口径。
//
// 这条守的是「别把它改回积分口径」——一改回去，面板又会显示「0 可用积分」。
func TestGeminiCreditsUnitIsPercent(t *testing.T) {
	a := &App{}
	if got := a.creditsUnit(provider.Gemini.String()); got != "percent" {
		t.Fatalf("Gemini 必须是 percent 口径，得 %q", got)
	}
	// 反向护栏：普通积分渠道不得被误标
	if got := a.creditsUnit(provider.QwenWork.String()); got != "" {
		t.Fatalf("普通渠道应为积分口径，得 %q", got)
	}
}

