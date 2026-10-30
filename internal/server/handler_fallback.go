// 模型降级链的「容量判定」与「备胎选择」。
//
// 从 handler.go 拆出（结构重排，零行为变更）。注意：真正的降级重入循环在
// chatCompletions 的 for fbAttempt 里（R94：只在换号已用尽后出手、只跨渠道、只重入一次），
// 本文件只提供它调用的两个判定函数。
package server

import (
	"strings"
	"time"

	"wild-work/internal/provider"
)

// modelHasCapacity 报告 rt 渠道此刻是否有账号能承载 model。
//
// 一次 Pool.List() 得出答案，口径与 no_healthy_account 的「模型全冷」分支一致：
// 跳过禁用/整号冷却的账号后，看是否还有账号不在该模型的模型级冷却中
// （pool.statusOf 已剔掉过期条目，此处再比一次到期时间属防御）。
//
// 注意本函数对「整渠道全灭」（无任何健康账号）同样返回 false——
// 这正是降级链该顶上的两种形态（模型全冷 / 整渠道全灭），调用方不得
// 再额外要求「渠道内有健康账号」才降级（R54，PR #127 修正的旧判据）。
func modelHasCapacity(rt *Runtime, model string) bool {
	now := time.Now()
	for _, s := range rt.Pool.List() {
		if s.Disabled || s.Cooling {
			continue
		}
		if t, ok := s.ModelCooling[model]; ok && t.After(now) {
			continue
		}
		return true
	}
	return false
}

// pickModelFallback 模型降级链解析：请求模型在本渠道已无账号可承载时
// （两种形态——①「模型全冷」：渠道内有健康账号，但该模型在全部账号上处于
// 模型级限流冷却；②「整渠道全灭」：渠道内已无任何健康账号，含全部禁用与
// pool 的「多模型接连限流升级整号冷却」）时，按规则顺序（规则间按配置顺序、
// 链内按 To 顺序）找第一个有容量的备胎模型全路径 ID；无规则/无可用备胎返回空串。
// 备胎允许跨渠道（To 为 "channel/model" 全路径，经 runtimeForModel 解析）——
// 整渠道全灭时正需要跨渠道备胎顶上，而非直接 503。
//
// skipSameChannel=true：跳过与当前渠道相同的备胎候选（仍会继续往链后找跨渠道的）。
// 只给「请求内重入」用——那里 429 可能只是账号级限流，同渠道兄弟模型共享同一批
// 账号，重打一遍会二次撞墙，并可能把冷却粒度从模型级升级为账号级；入口降级传 false
// （入口已知「该模型在本渠道全冷」，换同渠道兄弟模型正是链配置的本意）。
//
// 第二返回值 primaryAvail = 「原模型此刻有可用账号」，与降级与否无关，只给恢复探针用：
// 旧实现「未降级就算恢复」会把两种情形谎报成已恢复——① 原模型仍全冷但备胎也没容量，
// ② 整渠道无健康账号。仅面板事件记账受影响，但谎报会令「持续时长」失真。
// 未配置规则时不做探测（守住「正常路由零开销」）：此时不会新增事件，
// 代价是删规则后残留的进行中事件不会自动关闭（环形 200 条自然淘汰，重启即清）。
func (h *Handler) pickModelFallback(rt *Runtime, model string, skipSameChannel bool) (string, bool) {
	h.fbMu.RLock()
	rules := h.fallbacks
	h.fbMu.RUnlock()
	if len(rules) == 0 {
		return "", false
	}
	if modelHasCapacity(rt, model) {
		return "", true // 未全冷：走原路由，并告知探针「容量已在」
	}
	// 未全冷之外（模型全冷 / 整渠道全灭）都继续往下遍历规则找备胎：
	// 链内无容量的候选由 modelHasCapacity 跳过；同渠道候选是否跳过由入参决定。
	for _, rule := range rules {
		// 规则里的 from 与请求模型都要按同一套 ID 解析（R59：cline 的 `cline-free/x`
		// 与旧写法 `cline/cline-free/x` 必须都能命中同一条规则）。
		rk, rUp, rok := provider.SplitModelID(rule.From)
		if !rok || rk != rt.Kind || rUp != model {
			continue
		}
		for _, to := range rule.To {
			to = strings.TrimSpace(to)
			if to == "" {
				continue
			}
			rt2, m2, err := h.runtimeForModel(to)
			if err != nil || rt2 == nil {
				continue
			}
			if skipSameChannel && rt2.Kind == rt.Kind {
				// 同渠道备胎共享同一批账号：请求内 429 可能只是账号级限流，
				// 重打一遍必然二次撞墙（详见调用点注释）。继续往链后找跨渠道候选。
				continue
			}
			if modelHasCapacity(rt2, m2) {
				return to, false
			}
		}
	}
	return "", false
}
