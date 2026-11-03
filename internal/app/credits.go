package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// CreditRefreshInterval 积分自动刷新间隔。
// 无签到活动的渠道（如 WorkBuddy 国际版）积分不随签到更新，
// 若不定期刷新：面板长期显示旧值（新账号则一直为 0），
// 且 Pool.Pick() 的最早到期时刻会因此长期陈旧而选错账号。
//
// 2026-10-20 由 30min 调到 10min（用户要求）：代价是每账号的
// UserResourceDetail 请求数与 state.json 落盘次数 ×3，换来「到期时刻已过」更快被纠正
// （过期后该账号必须靠下一次刷新才会从队首退到余额兜底组）。
const CreditRefreshInterval = 10 * time.Minute

// creditSnapshot 实时拉取明细并算出「可消耗余额」「临期额度」「不可消耗余额」三口径，
// 成功即回写 pool 快照 + ledger 差分。三者同源于 UserResourceDetail 的单次响应：remain 即
// pool 路由口径的可消耗余额，临期额度为其中 provider.ExpiringDays 个自然日内到期的部分
// （**仅展示**，R112 起不参与选号），不可消耗部分由条目的 Usable 标记汇总得到
// （渠道不区分专用池时为 0；渠道不下发到期时间时临期为 0，如 Qoder）。
// 同一响应里还算出「最早到期时刻」一并写入 pool——那是 Pick() 的唯一排序键。
// 遇 401 自动刷新 token 并重试一次（见 refreshIfSessionDead）。
//
// 顶层（卡片/汇总条/Pick 选号）与明细弹层从此同源：旧实现里 pool 快照只被「刷新事件」
// （手动刷新/全部刷新/自动循环）更新，而明细是 hover 时的实时值，两者必然相差区间内消耗量
// ——面板「同屏两个数字对不上」实为时差而非上游口径问题（已实测同响应内上游汇总字段==明细池
// 相加）。hover 明细也回写后，弹层展开那一刻全屏数字同源于同一响应。
func (a *App) creditSnapshot(rt *Runtime, au *auth.Auth) (remain int64, items []provider.ResourceItem, expiring, unusable int64, err error) {
	remain, items, err = rt.Upstream.UserResourceDetail(au)
	if err != nil && a.refreshIfSessionDead(rt, au, err) {
		remain, items, err = rt.Upstream.UserResourceDetail(au)
	}
	if err != nil {
		return 0, nil, 0, 0, err
	}
	_, unusable = provider.Summarize(items)
	// 临期额度：自然日口径（provider.ExpiringDays 个自然日内到期，次日 00:00 失效算今日）。
	// R112 起**仅展示**（面板三口径/汇总条），不参与选号——选号用下面的 earliest。
	now := time.Now()
	expiring = provider.ExpiringWithinDays(items, provider.ExpiringDays, now)
	// 最早到期时刻：Pick() 的第一排序键（R112）。ok=false（如 Qoder 不下发到期时间）
	// 时返回零值，pool 会把它排到「有到期信息」的账号之后按余额兜底。
	earliest, _ := provider.EarliestExpire(items, now)
	// 差分记账：与上次快照对比产出 earn/spend/expire 流水（锦上添花，失败不干扰）
	// 积分流水只记「积分口径」的余额变化：token 计量渠道（credits_unit="token"）的 remain
	// 是 token 数，记进积分流水会污染统计（2026-10-17 用户实测报告）。
	// Token 用量本身另有 token 流水（按请求实测计量），不受影响。
	if a.ledger != nil && a.creditsUnit(rt.Kind.String()) == "" {
		a.ledger.DiffCredits(rt.Kind.String(), au.UID, remain, items)
	}
	rt.Pool.SetCreditDetail(au.UID, remain, expiring, unusable, earliest)
	// 百分比配额窗口（Gemini 等）：与余额同源写入，卡片才能在首屏就显示「5 小时 100%」
	// 而不是一个恒为 0 的积分数（2026-10-30 用户实测：额度充足却显示 0）。
	if ws := quotaWindowsFrom(items); len(ws) > 0 {
		rt.Pool.SetQuotaWindows(au.UID, ws)
	}
	return remain, items, expiring, unusable, nil
}

// quotaWindowsFrom 把 ResourceItem 里的 **百分比窗口** 提取成 pool.QuotaWindow。
//
// 判据（与 provider.ResourceItem 的既有语义对齐）：
//   - 只取 `Total == 100` 的条目 —— 那是「百分比窗口」的约定形态
//     （gemini 的 parseQuotaItems 就是按 remain/total 都是百分数生成的）；
//   - 普通积分的 total 是分值（几百上千），不会被误收；
//   - total 为 0 的条目跳过（无意义，也防除零）。
//
// 为什么不用 `Unit` 字段判：它是枚举标签，加新枚举要动 provider 契约；
// 而 100 这个值本身就是这类窗口的定义，判据更直接、也不必改既有类型。
func quotaWindowsFrom(items []provider.ResourceItem) []pool.QuotaWindow {
	var out []pool.QuotaWindow
	for _, it := range items {
		if it.Total != 100 || it.Name == "" {
			continue
		}
		pct := int(it.Remain)
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		w := pool.QuotaWindow{Name: it.Name, Percent: pct}
		if t, err := time.Parse(time.RFC3339, it.ExpireAt); err == nil {
			w.ResetAt = t
		}
		out = append(out, w)
	}
	return out
}

// creditTotals 取三口径小计（不含 items），并在同一响应上喂入运行统计的最近临期。
func (a *App) creditTotals(rt *Runtime, au *auth.Auth) (usable, expiring, unusable int64, err error) {
	remain, items, expiring, unusable, err := a.creditSnapshot(rt, au)
	if err != nil {
		return 0, 0, 0, err
	}
	// 运行统计：账号最近临期喂入（复用同一响应的 items，免二次上游调用）
	a.applyExpiryFromItems(au.UID, items)
	return remain, expiring, unusable, nil
}

// refreshIfSessionDead 上游报「登录态失效」时刷新一次 token 并写回，返回是否已刷新。
//
// 为何不能只依赖 NeedsRefresh：它比的是本地 expiresAt，而本地时钟可能是错的——
// 例如 token 已在别处（另一实例/客户端）被轮换、或上次刷新后未落盘，
// 此时本地仍显示「有效」而上游已拒绝 → 不刷新就永久卡在 401（积分恒为 0、明细恒为空）。
// 故必须对 401 本身做一次「刷新 + 重试」。
func (a *App) refreshIfSessionDead(rt *Runtime, au *auth.Auth, err error) bool {
	var ue *provider.Error
	if !errors.As(err, &ue) || ue.Kind != provider.ErrSessionDead {
		return false
	}
	if au.RefreshToken == "" {
		return false
	}
	log.Printf("session dead, refreshing platform=%s uid=%s", rt.Kind, au.UID)
	if rerr := rt.Upstream.RefreshToken(au); rerr != nil {
		log.Printf("session dead refresh failed platform=%s uid=%s err=%v", rt.Kind, au.UID, rerr)
		return false
	}
	// 刷新成功必须落盘：否则下次启动又拿旧 token，重回 401。
	if serr := au.SaveAtomic(); serr != nil {
		log.Printf("session dead refresh save failed platform=%s uid=%s err=%v", rt.Kind, au.UID, serr)
	}
	return true
}

// RefreshCredits 刷新单个账号积分（返回可消耗余额）。
func (a *App) RefreshCredits(uid string) (int64, error) {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return 0, fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		// 匿名通道无积分概念，面板显示「不适用」，无刷新语义。
		return 0, fmt.Errorf("OpenCodeZen 匿名通道无积分，不适用")
	}
	if rt.Kind == provider.Kilo {
		// 免密免费池同样没有积分概念（面板显示「不适用」）。
		return 0, fmt.Errorf("Kilo 免费池无积分，不适用")
	}
	if rt.Custom {
		// 自定义渠道（R66）无积分接口：凭据本体是渠道级 api_key（对话转发用），
		// 不拒绝会落入 creditSnapshot 把 pool 快照写成 0（UserResourceDetail 恒返回空），
		// 让人误读成「额度已用光」。
		return 0, fmt.Errorf("%s 为自定义渠道，无积分概念，不适用", rt.Kind)
	}
	// 注意：ZCode **不**在这里拒绝 —— 它的额度是 token 口径，同样需要刷新才能看到用量；
	// 它的数字由 credits_unit="token" 标注，不进积分小计与流水（见 creditsUnit）。
	log.Printf("credits refresh start platform=%s uid=%s", rt.Kind, uid)
	usable, expiring, unusable, err := a.creditTotals(rt, au)
	if err != nil {
		log.Printf("credits refresh failed platform=%s uid=%s err=%v", rt.Kind, uid, err)
		return 0, err
	}
	log.Printf("credits refresh success platform=%s uid=%s remain=%d expiring=%d unusable=%d", rt.Kind, uid, usable, expiring, unusable)
	return usable, nil
}

// RefreshAll 刷新全部账号积分，返回汇总（供托盘消息框 / Web UI）。
func (a *App) RefreshAll() RefreshSummary {
	a.refreshMu.Lock()
	if a.refreshing {
		a.refreshMu.Unlock()
		return RefreshSummary{Busy: true}
	}
	a.refreshing = true
	a.refreshMu.Unlock()
	defer func() {
		a.refreshMu.Lock()
		a.refreshing = false
		a.refreshMu.Unlock()
	}()
	sum := RefreshSummary{Platforms: map[string]PlatformSummary{}}
	for _, rt := range a.runtimesSnapshot() {
		// 别名渠道复用同一 Pool：计入会重复刷新同一批账号。
		if rt == nil || rt.Pool == nil || rt.Upstream == nil || rt.Alias {
			continue
		}
		// oczen / kilo 无「额度」可刷：纳入只会产生一条无意义的失败记录。
		// （ZCode 要走刷新：它的额度是 token 口径，同样需要更新才能看到用量。）
		if rt.Kind == provider.Oczen || rt.Kind == provider.Kilo {
			continue
		}
		// 自定义渠道（R66）同样无积分概念：凭据本体是渠道级 api_key（对话转发用），
		// 占位账号无 access/refresh token，且 UserResourceDetail 恒返回空。
		// 纳入只会把每个自定义渠道都记成一条「no token」假失败（面板报「失败 N」噪音）。
		if rt.Custom {
			continue
		}
		ps := PlatformSummary{Accounts: []AccountRefresh{}}
		for _, st := range rt.Pool.List() {
			au := rt.Pool.AuthByUID(st.UID)
			// 凭据可用性判据：accessToken 或 refreshToken 任一非空即可。
			// glm 的 accessToken 允许为空（凭据本体是 refresh_token，见 LoadGLMDir），
			// 只查 accessToken 会把它误判成「no token」而永不刷新。
			if au == nil || (au.AccessTokenValue() == "" && au.RefreshTokenValue() == "") {
				ps.Failed++
				ps.Accounts = append(ps.Accounts, AccountRefresh{UID: st.UID, OK: false, Msg: "no token"})
				continue
			}
			if usable, _, _, err := a.creditTotals(rt, au); err == nil {
				ps.OK++
				ps.Accounts = append(ps.Accounts, AccountRefresh{UID: st.UID, OK: true, Remain: usable})
			} else {
				ps.Failed++
				ps.Accounts = append(ps.Accounts, AccountRefresh{UID: st.UID, OK: false, Msg: shortErr(err)})
				log.Printf("credits refresh failed platform=%s uid=%s err=%v", rt.Kind, st.UID, err)
			}
		}
		sum.Total += ps.OK + ps.Failed
		sum.OK += ps.OK
		sum.Failed += ps.Failed
		sum.Platforms[rt.Kind.String()] = ps
	}
	return sum
}

// RefreshSummary 积分刷新汇总（托盘消息框内容）。
type RefreshSummary struct {
	Busy      bool                       `json:"busy"`
	Total     int                        `json:"total"`
	OK        int                        `json:"ok"`
	Failed    int                        `json:"failed"`
	Platforms map[string]PlatformSummary `json:"platforms"`
}

// PlatformSummary 单个渠道汇总。
type PlatformSummary struct {
	OK       int              `json:"ok"`
	Failed   int              `json:"failed"`
	Accounts []AccountRefresh `json:"accounts"`
}

// AccountRefresh 单账号刷新结果。
type AccountRefresh struct {
	UID    string `json:"uid"`
	OK     bool   `json:"ok"`
	Remain int64  `json:"remain"`
	Msg    string `json:"msg,omitempty"`
}

// ResourceDetail 查询单个账号积分明细（含可用/不可用/临期小计）。
// 返回的 remain 与 items 同时给出：remain 供 pool 口径对账，items 供 UI 明细展示。
// 取数经 creditSnapshot：实时拉取的同时把同一响应回写 pool 快照，故面板展开明细后
// 卡片顶层与弹层同源一致，不再依赖刷新按钮弥合时差；401 自愈亦在 creditSnapshot 内。
func (a *App) ResourceDetail(uid string) (remain int64, items []provider.ResourceItem, expiring, unusable int64, err error) {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil || rt.Upstream == nil {
		return 0, nil, 0, 0, fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		return 0, nil, 0, 0, fmt.Errorf("OpenCodeZen 匿名通道无积分，不适用")
	}
	if rt.Kind == provider.Kilo {
		return 0, nil, 0, 0, fmt.Errorf("Kilo 免费池无积分，不适用")
	}
	if rt.Custom {
		// 自定义渠道（R66）无明细接口：UserResourceDetail 恒返回空 items，
		// 不拒绝会让前端弹出一张空白明细层。
		return 0, nil, 0, 0, fmt.Errorf("%s 为自定义渠道，无积分明细，不适用", rt.Kind)
	}
	return a.creditSnapshot(rt, au)
}

// StartCreditAutoRefresh 后台定期刷新指定渠道的账号积分。
// 启动立即刷一次，之后每隔 interval 一次；单账号失败不影响其他账号。
//
// 范围限制在传入的 kinds：国内版 workbuddy 有自己的签到刷新途径，
// 不纳入此循环，以免改变其行为。
func (a *App) StartCreditAutoRefresh(ctx context.Context, kinds []provider.Kind, interval time.Duration) {
	if interval <= 0 {
		interval = CreditRefreshInterval
	}
	go func() {
		refresh := func() {
			for _, k := range kinds {
				rt := a.runtime(k)
				// 别名渠道复用主渠道 Pool：纳入会重复刷新同一批账号。
				if rt == nil || rt.Pool == nil || rt.Upstream == nil || rt.Alias {
					continue
				}
				for _, st := range rt.Pool.List() {
					if st.Disabled {
						// 停用账号不再刷资源：清掉引擎里的临期残留（sidecar 对 disabled 账号
						// 仍轮询 resource；此处写空值达到同等效果，防「最近临期」行冻结旧值）
						a.applyExpiryFromItems(st.UID, nil)
						continue
					}
					au := rt.Pool.AuthByUID(st.UID)
					if au == nil {
						continue
					}
					// token 临近过期时先刷新（国际版 token 有效期长，通常不触发）
					if au.NeedsRefresh(10 * time.Minute) {
						if err := rt.Upstream.RefreshToken(au); err != nil {
							log.Printf("credit auto-refresh token refresh failed platform=%s uid=%s err=%v", k, st.UID, err)
							continue
						}
						if err := au.SaveAtomic(); err != nil {
							log.Printf("credit auto-refresh token save failed platform=%s uid=%s err=%v", k, st.UID, err)
						}
					}
					usable, expiring, unusable, err := a.creditTotals(rt, au)
					if err != nil {
						log.Printf("credit auto-refresh failed platform=%s uid=%s err=%v", k, st.UID, err)
						continue
					}
					log.Printf("credit auto-refresh platform=%s uid=%s remain=%d expiring=%d unusable=%d", k, st.UID, usable, expiring, unusable)
				}
			}
		}
		refresh() // 启动即刷：新账号不必等下一个周期
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
}
