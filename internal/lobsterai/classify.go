// 错误分类：Classify / ClassifyStream 与「余额不足 / 登录态失效」文本判据。
//
// 从 client.go 拆出（结构重排，零行为变更）：判据串与判定顺序逐字保留——
// 顺序变了会把限流误判成硬余额、或让登录态失效绕过自愈重登。
package lobsterai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"wild-work/internal/provider"
)

// Classify 实现 provider.Upstream：按状态码 + body 语义分类。
//
// 顺序**严格按协议 §5.1**（判定顺序即优先级）：
//
//  1. HTTP 402                    → ErrHardCredit
//  2. body 命中「余额不足」关键词 → ErrHardCredit
//  3. body 命中 40100 / 40101 / token rejected / refresh token was rejected → ErrSessionDead
//  4. HTTP 429                    → ErrSoftRate
//  5. HTTP 404                    → ErrNotFound
//  6. HTTP ≥ 500                  → ErrServer
//  7. HTTP ≥ 400                  → ErrClient
//  8. 否则                        → ErrNone
//
// ⚠️ **唯一一处与字面顺序的偏离，且是仓库不变式要求的**：429 提前到「余额关键词」
// 之前（AGENTS.md 不变式 15）。限流 body 高频带 `quota exceeded` 这类字样，先判余额
// 关键词会把限流误归「余额耗尽」→ 12h 硬冷却，等于用分类器把整条渠道下线。
// 该偏离只影响「429 且同时命中余额关键词」这一种组合，其余组合与 §5.1 逐条一致。
//
// 另一处**分支内**细化：≥400 分支里先看本地自造的 `code=bad_params`
// （出站体不可构造时本包自己产的 400）。它只在没有额外信息可损失时才生效，
// 目的是别让网关本地拒绝去惩罚健康账号（同 minimax / monkeycode 的口径）。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusPaymentRequired:
		return provider.ErrHardCredit
	case status == http.StatusTooManyRequests:
		// 见上方注释：429 必须先于余额关键词（不变式 15）。
		return provider.ErrSoftRate
	case hitsInsufficientCredits(lower):
		return provider.ErrHardCredit
	case hitsSessionDead(lower):
		return provider.ErrSessionDead
	case status == http.StatusNotFound:
		return provider.ErrNotFound
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		if provider.CodeMarker(lower, "bad_params") {
			return provider.ErrBadParams // 本包本地拒绝：请求级，不罚号不轮转
		}
		// 其余 4xx：按协议 §5.1 归 ErrClient（可轮换）。
		// 注意 401/403 若**不带** session-dead 标记也会落到这里（协议没有 401 分支），
		// 这与参考实现一致（"除成功外每一类都换号"）。积分/签到/续期路径另有显式
		// 401/403 判定（见 businessErr），不变式 19 的自愈不受影响。
		return provider.ErrClient
	default:
		return provider.ErrNone
	}
}

// Classify 实现 provider.Upstream（实例方法，便于将来注入渠道级配置）。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// ClassifyStream 流内错误分类（协议 §5.1 末条：classifyLobsteraiStreamError）。
//
// 为什么必须单独一套判据：流内错误的 HTTP 状态恒为 200，**状态码分支全部失效**，
// 只能按 message 文本判。命中 hard-credit / session-dead 时返回对应类别
// （可冷却账号），其余一律 ErrClient（可轮转、不记徽章）。
func ClassifyStream(message string) provider.ErrKind {
	lower := strings.ToLower(message)
	switch {
	case hitsInsufficientCredits(lower):
		return provider.ErrHardCredit
	case hitsSessionDead(lower):
		return provider.ErrSessionDead
	default:
		return provider.ErrClient
	}
}

// insufficientMarkers 余额/权益不足关键词（中英双表）。
//
// ⚠️ **未确认**：协议 §5.1 只说「body 命中『余额不足』关键词（中英双表）」，
// 关键词表本体在参考实现 lobsterai-errors.ts:54-66，**未收录进本地文档**；
// 文档只额外提到 2026-09 补充的「免费额度已用完，请升级套餐」类文案。
// 下表是按该描述整理的**推断**表：宁可多列几条已知说法，也不要漏判——
// 漏判会把余额耗尽的账号当健康账号反复重试（而它是本渠道最主要的失败模式）。
var insufficientMarkers = []string{
	// 中文
	"余额不足", "额度不足", "积分不足", "余额已用完", "额度已用完",
	"免费额度已用完", "免费额度用完", "请升级套餐", "升级套餐", "欠费",
	// 英文
	"insufficient", "not enough credits", "not enough balance", "no credits",
	"out of credits", "credit limit reached", "quota exhausted", "quota exceeded",
	"exceeded your current quota", "please upgrade", "upgrade your plan",
}

// sessionDeadMarkers 会话失效关键词（协议 §5.1 第 3 条逐字照抄）。
var sessionDeadTexts = []string{"token rejected", "refresh token was rejected"}

// sessionDeadCodes 会话失效业务码（协议 §5.1 第 3 条）。
var sessionDeadCodes = []string{"40100", "40101"}

// hitsInsufficientCredits 报告（已小写化的）文本是否命中余额不足关键词。
func hitsInsufficientCredits(lower string) bool {
	for _, m := range insufficientMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// hitsSessionDead 报告（已小写化的）文本是否命中会话失效判据。
//
// 数字码用 provider.CodeMarker 而不是裸 Contains：裸 Contains 会被 request_id /
// 时间戳里的「40100」误命中（见 internal/provider/codemarker.go 的口径说明）。
func hitsSessionDead(lower string) bool {
	for _, c := range sessionDeadCodes {
		if provider.CodeMarker(lower, c) {
			return true
		}
	}
	for _, m := range sessionDeadTexts {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// businessErr 把业务端点（积分 / 签到 / 续期）的 HTTP 层失败转成类型化错误。
//
// 与 Classify 的差别：这里对 **HTTP 401/403 额外加一条显式判定**，归 ErrSessionDead
// ——协议 §5.1 的状态码表里没有 401 分支，但不变式 19/23 要求「登录态失效必须能被
// errors.As 识别」，否则 401 自愈（refresh + 落盘 + 重试一次）永远不触发。
// 另：HTTP 200 + 业务码非 0 由调用方经 envelopeError 处理，本函数不管。
func (c *Client) businessErr(status int, raw []byte) error {
	if status == 0 {
		return nil
	}
	lower := strings.ToLower(string(raw))
	if status == http.StatusUnauthorized || status == http.StatusForbidden || hitsSessionDead(lower) {
		return &provider.Error{Kind: provider.ErrSessionDead, Status: status, Msg: snippet(raw)}
	}
	if status >= 400 {
		return &provider.Error{Kind: Classify(status, string(raw)), Status: status, Msg: snippet(raw)}
	}
	return nil
}

// envelopeError 把「HTTP 200 但统一信封 code 非 0」的响应转成类型化错误。
//
// 先判 session-dead 标记（同一份 body 里可能带 40100/40101），其余归 ErrClient：
// 可轮转、不致命，且不会把「业务拒绝」误当成会话失效去禁用账号。
func (c *Client) envelopeError(status, code int, msg string, raw []byte) error {
	kind := provider.ErrClient
	if hitsSessionDead(strings.ToLower(string(raw))) {
		kind = provider.ErrSessionDead
	}
	return &provider.Error{Kind: kind, Status: status,
		Msg: fmt.Sprintf("lobsterai：业务码 %d %s", code, strings.TrimSpace(msg))}
}

// envelopeCode 取统一信封的 code（`{code,msg,data}`）。ok=false 表示响应里没有 code 键
// （部分端点可能直接给裸对象，此时不按业务码判失败）。
func envelopeCode(top map[string]json.RawMessage) (code int, msg string, ok bool) {
	raw, has := top["code"]
	if !has {
		return 0, "", false
	}
	return int(intOfRaw(raw)), strings.TrimSpace(strOfRaw(top["msg"])), true
}

// ---------------------------------------------------------------------------
// provider.Upstream：流式透传 / 本地聚合（含流内错误识别）
// ---------------------------------------------------------------------------
