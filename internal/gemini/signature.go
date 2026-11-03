// signature.go Gemini 4/3 的思考签名（thoughtSignature）回填。
//
// # 为什么需要（2026-11-03 用户实测）
//
// 用 Gemini 跑工具调用时内部报 400：
//
//	Function call is missing a thought_signature in functionCall parts. This is required
//	for tools to work correctly … function call `default_api:bash`, position 39.
//
// 官方 thought-signatures 文档给的规则（逐条照做，不臆测）：
//
//  1. 并行调用只把签名挂在**第一个** functionCall 上，同一步骤后续 FC 不带；
//  2. **当前轮**每一步的第一个 functionCall **必须**回传收到的原签名，否则 400；
//  3. 校验范围只到「从最新往前，最近一条含常规文本的 user 消息」为止——更早的历史轮不校验；
//  4. 官方 OpenCode 兼容层把签名放在 `tool_call.extra_content.google.thought_signature`；
//  5. 客户端没保留签名时（跨模型迁移 / 中间层丢弃 extra_content），
//     官方允许用**占位签名**跳过校验：`skip_thought_signature_validator`。
//
// # 本仓的实际情形
//
// 上一层层转发的客户端（pi / Codex 等）用自己的 OpenAI 结构重放历史，
// `extra_content` 属非标字段、绝大多数会被丢掉。所以：
//   - 有真签名就回真签名（最佳，保住推理上下文）；
//   - 没有就回占位签名（保证 400 不再发生）——**不是编造推理内容，只是跳过校验开关**，
//     与 R49「不编造上游声明」不冲突：这里填的是上游文档明确给出的哨兵值。
package gemini

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// sigSkipValidator 官方文档给出的占位签名：置入即跳过 thought_signature 校验。
//
// 另有一个等价取值 `context_engineering_is_the_way_to_go`，语义相同；
// 只用一个即可，避免两处漂移。
const sigSkipValidator = "skip_thought_signature_validator"

// isValidSignature 校验签名是否是**合法 base64**。
//
// ⚠️ 这是硬约束，不是防御性检查：上游把 `thought_signature` 声明为 **TYPE_BYTES**，
// 实测非法值直接 400（2026-11-03 原样报错）：
//
//	Invalid value at 'request.contents[1].parts[0].thought_signature' (TYPE_BYTES),
//	Base64 decoding failed for "STALE_INVALID_SIGNATURE_XYZ"
//
// 也就是说「invalid argument」这个零信息量的错误，最常见的真因就是签名不是 base64。
// 客户端可能把签名截断、拼接、或塞进自己编码过的值，我们**不能原样转发**。
//
// 四种编码都试：标准/URL 字母表 × 有/无 padding —— 上游接受哪一种没有文档说明，
// 而「能被任意一种解出来」已经足以证明它是一段字节串、不是随手拼的字符串。
func isValidSignature(s string) bool {
	if s == "" {
		return false
	}
	// 哨兵是**特判**：上游先认这个字面量并跳过校验，才轮到 base64 解码。
	// 证据：本机实测该串（含下划线，标准 base64 非法）直接 200。
	if s == sigSkipValidator {
		return true
	}
	// 其余必须是**标准 base64**（带或不带 padding）。
	// 不接受 URL 字母表（`-`/`_`）—— 实测上游对含下划线的签名报
	// `Base64 decoding failed`，用 URL 字母表判定等于放坏值过关。
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if _, err := enc.DecodeString(s); err == nil {
			return true
		}
	}
	return false
}

// sanitizeSignature 返回可安全上行的签名：非法（非 base64）时返回空串，
// 交给 ensureSignatures 用哨兵兜底。
func sanitizeSignature(sig string) string {
	if strings.TrimSpace(sig) == "" {
		return ""
	}
	if !isValidSignature(sig) {
		return ""
	}
	return sig
}

// thoughtSignatureOf 从一条 tool_call（OpenAI 形状）里取 Google 的思考签名。
//
// 取值位置按官方 OpenAI 兼容文档：`extra_content.google.thought_signature`。
// 兼容蛇形键与直接放在顶层 google 段的两种写法（不同客户端实现不一）。
func thoughtSignatureOf(c map[string]any) string {
	if c == nil {
		return ""
	}
	if extra, ok := c["extra_content"].(map[string]any); ok {
		if g, ok := extra["google"].(map[string]any); ok {
			for _, k := range []string{"thought_signature", "thoughtSignature"} {
				if v := strOf(g[k]); v != "" {
					return v
				}
			}
		}
	}
	// 少数实现直接放顶层
	for _, k := range []string{"thought_signature", "thoughtSignature"} {
		if v := strOf(c[k]); v != "" {
			return v
		}
	}
	return ""
}

// ensureSignatures 给「当前轮」每一步的第一个 functionCall 补上思考签名。
//
// out 是已经翻译好的 contents（按上游顺序）。就地修改 parts。
//
// 「当前轮」边界：从后往前找到最近一条 **role=user 且含文本 part** 的 content，
// 其后的所有 role=model content 都属于当前轮。找不到（整段历史都是工具结果）时
// 视为全部都是当前轮 —— 宁可多补一个占位签名，也不要漏掉导致 400。
func ensureSignatures(out []content) {
	start := 0
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != "user" {
			continue
		}
		if hasTextPart(out[i].Parts) {
			start = i + 1
			break
		}
	}
	for i := start; i < len(out); i++ {
		c := &out[i]
		if c.Role != "model" {
			continue
		}
		// 每一步（一条 model content）只需第一个 functionCall 带签名。
		for j := range c.Parts {
			fc := c.Parts[j].FunctionCall
			if fc == nil {
				continue
			}
			// 空 → 补哨兵；有值但不是 base64 → 也换成哨兵（否则上游 400 invalid argument）。
			if !isValidSignature(c.Parts[j].ThoughtSignature) {
				c.Parts[j].ThoughtSignature = sigSkipValidator
			}
			break // 只补第一个 FC
		}
	}
}

// hasTextPart 报告 parts 里是否含非空文本（用于识别「用户提问」那条 content）。
func hasTextPart(parts []part) bool {
	for _, p := range parts {
		if strings.TrimSpace(p.Text) != "" {
			return true
		}
	}
	return false
}

// forceSentinelSignatures 把已有签名**全部**换成哨兵值（降级重试用）。
//
// 触发场景：客户端回传的签名不是本次会话产生的（换过模型、换了渠道、或中间层缓存了
// 旧值），上游不会报「missing」而是报 `400 Request contains an invalid argument` ——
// 后者信息量为零，只能靠「换成哨兵再试一次」来区分并自愈。
func forceSentinelSignatures(out []content) {
	for i := range out {
		for j := range out[i].Parts {
			if out[i].Parts[j].FunctionCall == nil {
				continue
			}
			if strings.TrimSpace(out[i].Parts[j].ThoughtSignature) != "" {
				out[i].Parts[j].ThoughtSignature = sigSkipValidator
			}
		}
	}
}

// hasRealSignature 报告请求体里是否含有**非哨兵**的签名（即客户端回传的真签名）。
//
// 用于决定要不要在 400 时降级重试：只有真签名才可能「无效」，
// 全是哨兵时再重试一次结果不会变，白多打一次上游。
func hasRealSignature(body []byte) bool {
	var probe struct {
		Request struct {
			Contents []struct {
				Parts []struct {
					ThoughtSignature string `json:"thoughtSignature"`
				} `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	for _, c := range probe.Request.Contents {
		for _, p := range c.Parts {
			sig := strings.TrimSpace(p.ThoughtSignature)
			if sig != "" && sig != sigSkipValidator {
				return true
			}
		}
	}
	return false
}

// isInvalidArgument 上游「参数无效」判据。
//
// 文案是 `400 Request contains an invalid argument.`（status=INVALID_ARGUMENT）。
// ⚠️ 只认这一种泛化文案：它信息量为零，正是我们需要「换哨兵再试一次」来区分
// 「签名失效」与「别的参数错」的场景；带更具体原因的 400（如 missing thought_signature、
// User location is not supported）各有专门处置，不能一并重试。
func isInvalidArgument(raw []byte) bool {
	low := strings.ToLower(string(raw))
	if !strings.Contains(low, "invalid_argument") && !strings.Contains(low, "invalid argument") {
		return false
	}
	// 排除有具体归因的形态，避免误吞
	for _, known := range []string{
		"missing a thought_signature", // 没带签名 → 由 ensureSignatures 兜底，不该走到这
		"user location is not supported",
		"exceeds the maximum",
	} {
		if strings.Contains(low, known) {
			return false
		}
	}
	return true
}
