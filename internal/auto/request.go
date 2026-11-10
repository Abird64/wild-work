// request.go 从客户端请求体探测「这次调用需要什么能力」，供打分引擎做硬门。
//
// 为什么放在 auto 包而不是各协议层：同一份探测必须被三方共用——
// 内层 `POST /v1/chat/completions`（handler 直接拿原始字节）、
// 外层 `/v1/responses` 与 `/v1/messages`（兼容层已把 body 解析成 map）。
// 探测口径若分家，同一个客户端换个协议就会被路由到不同的模型（最难查的那类 bug）。
//
// 探测口径刻意保守（宁可少判、不可错判）：
//   - Tools：`tools` 是非空数组，或显式给了 `tool_choice`，或旧版 `functions` 非空。
//     历史消息里的 tool 角色不触发——工具流是普通对话之后的**后续轮**才出现的。
//   - Images：递归扫整个结构，出现 `image_url` / `input_image` 键，或 `{"type":"image"}`
//     块 ⇒ 要求视觉。多判的代价是错失免费模型，漏判的代价是路由到不支持图像的模型后报错；
//     权衡下宁可要求视觉能力。
//   - Reasoning：`reasoning_effort` 非空，或 `reasoning` / `thinking` 存在且非空。
//   - MaxTokens：`max_tokens` → `max_completion_tokens` → `max_output_tokens`，取第一个正数。
//   - ContextTokens：**不估**。无本地分词器，字符数→token 的换算误差足以误杀候选
//     （中文 1 字≈1~2 token，代码/JSON 差得更远）。故硬门的上下文判定对自动选路
//     只在「调用方显式声明」时生效，当前恒不生效——这是刻意的取舍，不是遗漏。
package auto

import "encoding/json"

// RequestFromBody 从原始 JSON 请求体探测能力要求（解析失败 ⇒ 零值 = 不设任何要求）。
func RequestFromBody(body []byte) Request {
	if len(body) == 0 {
		return Request{}
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return Request{}
	}
	return RequestFromMap(obj)
}

// RequestFromMap 从已解析的请求体探测能力要求（兼容层复用此入口，避免二次序列化）。
func RequestFromMap(obj map[string]any) Request {
	var req Request
	if len(obj) == 0 {
		return req
	}
	if nonEmptyArray(obj["tools"]) || nonEmpty(obj["tool_choice"]) || nonEmptyArray(obj["functions"]) {
		req.Tools = true
	}
	if s, ok := obj["reasoning_effort"].(string); ok && trimSpace(s) != "" {
		req.Reasoning = true
	}
	if nonEmpty(obj["reasoning"]) || nonEmpty(obj["thinking"]) {
		req.Reasoning = true
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if n, ok := asInt64(obj[k]); ok && n > 0 {
			req.MaxTokens = n
			break
		}
	}
	req.Images = hasImageBlock(obj, 0)
	return req
}

// hasImageBlock 递归扫描结构里的图像内容块（深度上限 8，防止异常深层嵌套拖慢解析）。
func hasImageBlock(v any, depth int) bool {
	if depth > 8 {
		return false
	}
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			switch k {
			case "image_url", "input_image", "image":
				if nonEmpty(val) {
					return true
				}
			}
		}
		if s, ok := t["type"].(string); ok && trimSpace(s) == "image" {
			return true
		}
		for _, val := range t {
			if hasImageBlock(val, depth+1) {
				return true
			}
		}
	case []any:
		for _, val := range t {
			if hasImageBlock(val, depth+1) {
				return true
			}
		}
	}
	return false
}

// nonEmpty 报告任意 JSON 值是否「不是空」（缺失/空串/null/空数组/空对象皆为空）。
func nonEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return trimSpace(t) != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// nonEmptyArray 报告值是否为非空数组。
func nonEmptyArray(v any) bool {
	arr, ok := v.([]any)
	return ok && len(arr) > 0
}

// asInt64 宽松取整数（JSON 数字统一解成 float64，兼容直接传 int 的调用方）。
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	}
	return 0, false
}

// trimSpace 是 strings.TrimSpace 的等价（只为让本文件不引入 strings 依赖，保持包面最窄）。
func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end {
		c := s[start]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '\v' && c != '\f' {
			break
		}
		start++
	}
	for end > start {
		c := s[end-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '\v' && c != '\f' {
			break
		}
		end--
	}
	return s[start:end]
}
