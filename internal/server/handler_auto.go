// handler_auto.go 自动选路（R165）在**内层**的接入点。
//
// 位置与职责边界：
//   - 解析发生在 `resolveAlias` 之后、`runtimeForModel` **之前**——后者要求模型名
//     带明确的 `<channel>/<model>` 前缀，而自动选路的产出正是那个前缀形态。
//   - 决策本身在 internal/auto（纯打分）+ internal/app（快照装配）两层，
//     本文件只做三件事：① 从请求体探测能力要求；② 把决策结果（首选 + 降级全序）
//     接进既有的降级逻辑；③ 全序走空时回落到用户配置的静态降级链（R166）。
//   - 未注入解析器 / 未启用 / 无可用候选 ⇒ 一律回到原路径或给出可诊断的错误，
//     **绝不影响**非 auto 请求（不变量：零行为变化）。
package server

import (
	"strings"

	"wild-work/internal/auto"
	"wild-work/internal/provider"
)

// isAutoModel 报告请求模型名是否命中自动选路的对外别名（大小写与首尾空白不敏感）。
func isAutoModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), auto.Alias)
}

// hasAutoResolver 报告是否已注入自动选路解析器（= 装配层接入了这套能力）。
func (h *Handler) hasAutoResolver() bool {
	h.autoMu.RLock()
	defer h.autoMu.RUnlock()
	return h.autoResolve != nil
}

// autoRequestFromBody 从客户端请求体探测「这次调用需要什么能力」，喂给打分引擎做硬门。
//
// 实现落在 internal/auto（三方共用的唯一口径）：内层拿原始字节，
// 外层兼容层已解析成 map，走 auto.RequestFromMap——同一客户端换协议不会换到别的模型。
func autoRequestFromBody(body []byte) auto.Request {
	return auto.RequestFromBody(body)
}

// pickAutoFallback 自动选路的备胎选择：按决策**全序**（Order）逐个试容量，再回落静态降级链。
//
// 为什么用 Order 而不是 Chain（R166）：Chain 只留 fallback_depth(3) 条给响应头读，
// 早期实现直接拿它当降级链 ⇒ 第 4 个候选起永远用不上。免费池同时限流/额度见底是常态
// （kilo 与 oczen 两条免费渠道一起 429 完全可能发生），此时三条链全冷、请求被透传，
// 而 Order 里通常还有几十个有容量的模型。
//
// 与 pickModelFallback 的分工：
//   - 全序是**本次请求**按质价比算出的跨渠道次序，已含「别打同一批账号」的考虑，
//     所以 skipSameChannel 对它不适用（链本身可能指向同一渠道的兄弟模型）；
//   - 全序走完仍无容量时才走配置的静态降级链（用户手写的兜底，语义不变）。
//
// 第二返回值语义与 pickModelFallback 一致：primaryAvail = 原模型此刻有可用账号。
func (h *Handler) pickAutoFallback(rt *Runtime, model string, order []string, skipSameChannel bool) (string, bool) {
	if modelHasCapacity(rt, model) {
		return "", true
	}
	for _, cand := range order {
		cand = strings.TrimSpace(cand)
		if cand == "" {
			continue
		}
		// 顺序里的当前项就是本次要执行的模型：跳过，避免「自己降级到自己」的空转。
		if k, up, ok := provider.SplitModelID(cand); ok && k == rt.Kind && up == model {
			continue
		}
		rt2, m2, err := h.runtimeForModel(cand)
		if err != nil || rt2 == nil {
			continue
		}
		if skipSameChannel && rt2.Kind == rt.Kind {
			continue
		}
		if modelHasCapacity(rt2, m2) {
			return cand, false
		}
	}
	return h.pickModelFallback(rt, model, skipSameChannel)
}
