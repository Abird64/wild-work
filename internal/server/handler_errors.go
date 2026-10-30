// 响应写出与错误处置：JSON/错误信封、SSE 头清理、错误分类辅助、冷却时长计算。
//
// 从 handler.go 拆出（结构重排，零行为变更）。这里是 gateway 兼容层的**隐式契约**所在：
// writeOpenAIError 的错误信封形状被 responses.go / anthropic.go 直接读取并转换，
// 改形状会静默影响两个兼容端点（见 docs/refactor/02-server.md §4.5）。
package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/provider"
)

// formatRetryAfter 把解冻时刻换算成整数秒（至少 1）。
// 用于 Retry-After 头：与首次全号排队时的 Retry-After: 30 同一量级信号。
func formatRetryAfter(until time.Time) string {
	d := time.Until(until)
	if d < time.Second {
		d = time.Second
	}
	return strconv.Itoa(int(d.Seconds() + 0.5))
}

// formatUnfreeze 解冻时刻的可读格式：24h 内只报时刻（HH:MM），更远带日期。
// 零值返回占位文案（调用方一般已排除零值，防御性兜底）。
func formatUnfreeze(t time.Time) string {
	if t.IsZero() {
		return "稍后（请查看面板冷却原因）"
	}
	if time.Until(t) < 24*time.Hour {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// noLoginChannel 报告渠道是否「无登录概念」（不存在凭证文件、也无重登入口）。
// 用于把「去面板重新登录」这类引导文案限定在真正适用的渠道上，
// 避免免密渠道（oczen / kilo）被误导去找一个不存在的登录按钮。
func noLoginChannel(k provider.Kind) bool { return k == provider.Oczen || k == provider.Kilo }

// keyPasteChannel 报告渠道凭据是否「由用户从官方站点自行创建后粘贴进来」（而非登录编排取得）。
// 这类渠道的引导文案必须指向「粘贴新 Key」而不能说「重新登录」：
// cline 的官方 API Key 不过期、不轮换，被禁用只会是 Key 本身无效/被删。
func keyPasteChannel(k provider.Kind) bool { return k == provider.Cline }

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "api_error", "code": code}})
}

// transparentError 上游错误原文透传：status+body 原样写回，不包装。
func transparentError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// clearSSEHeaders 撑掉渠道 StreamCapture 预先写进 header 的 SSE 头。
// 排队/业务错误帧发现于**未写出任何字节**之时，此时撤掉这些头，后续才能干净地
// 写回 JSON 错误（否则客户端会按 text/event-stream 解析一个 JSON 体）。
func clearSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Del("Content-Type")
	h.Del("Cache-Control")
	h.Del("Connection")
	h.Del("X-Accel-Buffering")
}

// queuedBody 构造「全号排队」的出口错误体：OpenAI 错误信封 + 排队元数据。
// 客户端/agent 读 code=model_queued 即知“不是渠道挂了，是模型拥塞，等 retry_after 再试”。
func queuedBody(q *provider.Queued) []byte {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":             q.Error(),
		"type":                "upstream_error",
		"code":                "model_queued",
		"retry_after_seconds": int(q.Cooldown().Seconds()),
		"queue_count":         q.QueueCount,
		"wait_seconds":        q.WaitSeconds,
	}})
	return payload
}

// isNetworkError 判定是否**本机传输层/DNS**故障（与账号无关）。
// 判据：http.Client.Do 包装的 *url.Error / net.Error，或文案里出现 DNS/拒连特征。
// 用途：refresh 失败要区分「号坏了（换号有效）」与「网络坏了（换号无用）」。
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := err.Error()
	for _, m := range []string{"dial tcp", "no such host", "connection refused",
		"connection reset", "i/o timeout", "timeout awaiting response headers", "network is unreachable"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// softRateResetMaxCap 解析出的重置时长上限：上游时间戳异常（如错给一年后）时不至于把号冻死。
const softRateResetMaxCap = 24 * time.Hour

// softRateCooldown 从限流响应体提取真实重置时间，冷却到该时刻（而非固定 60s）。
// 命中形态：WorkBuddy 6004「...将在 2026-09-23 14:47:19 UTC+8 重置...」——该类限流
// 按重置点解封，60s 软冷却过期后账号会被反复选中反复 429。解析失败/时长为非正值时
// 回退默认 SoftCooldown。时间戳按 UTC+8 墙钟解析，与到期字段口径一致（AGENTS §6.18）。
// 已知形态「2026-09-23 14:47:19」可能带「UTC+8」后缀，解析前剥掉。
// hardCooldown 本次硬冷却（402 余额/权益不足）应持续多久。
// 默认恒为 HardCooldown；装配层注入 HardCooldownFor 时按其计算（当前策略：
// 截到下一个每日发奖窗口，见 config.HardCooldownFor）——qwenwork 每日 00:00
// UTC+8 自动发额度，固定 12h 会把 19:07 的 402 锁到次日 07:07，白锁 7 小时。
func (h *Handler) hardCooldown() time.Duration {
	if h.cfg.HardCooldownFor != nil {
		if d := h.cfg.HardCooldownFor(time.Now()); d > 0 {
			return d
		}
	}
	return h.cfg.HardCooldown
}

func (h *Handler) softRateCooldown(body string) time.Duration {
	ts, ok := softRateResetTs(body)
	if !ok {
		return h.cfg.SoftCooldown
	}
	d := time.Until(ts)
	if d <= 0 {
		return h.cfg.SoftCooldown
	}
	if d > softRateResetMaxCap {
		d = softRateResetMaxCap
	}
	return d
}

// softRateResetTs 提取限流响应体中的重置时间戳（UTC+8 墙钟）；未命中返回 false。
// 命中形态：国内版「...将在 2026-09-23 14:47:19 UTC+8 重置...」；国际版
// workbuddyai 6004「...usage will reset at 2026-10-14 12:00:00 UTC+8, ...」。
func softRateResetTs(body string) (time.Time, bool) {
	if ts, ok := softRateResetAt(body, "将在 ", " 重置"); ok {
		return ts, true
	}
	return softRateResetAt(body, "will reset at ", ",")
}

// softRateResetAt 定位 marker 之后、endMarker 之前的时间戳并按 UTC+8 墙钟解析。
func softRateResetAt(body, marker, endMarker string) (time.Time, bool) {
	i := strings.Index(body, marker)
	if i < 0 {
		return time.Time{}, false
	}
	rest := body[i+len(marker):]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		return time.Time{}, false
	}
	s := strings.TrimSpace(rest[:end])
	// 「UTC+8」是时区说明而非时间一部分（layout 无法解析），剥掉
	if suffix := " UTC+8"; strings.HasSuffix(s, suffix) {
		s = strings.TrimSuffix(s, suffix)
	}
	const layout = "2006-01-02 15:04:05"
	loc := time.FixedZone("UTC+8", 8*3600)
	reset, err := time.ParseInLocation(layout, s, loc)
	if err != nil {
		return time.Time{}, false
	}
	return reset, true
}

// truncateBody 错误日志/聚合 lastErr 用的响应体摘要。
func truncateBody(body []byte) string {
	const max = 200
	s := string(body)
	if len(s) > max {
		return s[:max]
	}
	return s
}
