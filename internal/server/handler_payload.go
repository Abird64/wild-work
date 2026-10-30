// 入站请求体处理：限流读取 → 模型别名解析 → 模型名改写 → 思考强度注入。
//
// 从 handler.go 拆出（结构重排，零行为变更）。调用顺序是行为约束（MS-006）：
// resolveAlias 必须在 runtimeForModel 之前；rewriteModel 必须在 TraceBody 之后。
// 搬文件不改顺序。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// resolveAlias 把客户端模型名里的「裸名别名」解析为 `compat.model_map` 里的全路径。
//
// 语义与 gateway.Router.Resolve（internal/gateway/router.go:25）**同源**：
// 精确匹配 → 通配（key 以 `*` 结尾，取最长匹配前缀）→ 未命中原样返回。
// 刻意与 gateway 不同的两点（MS-006 设计口径 1/3，均为「零行为变化」要求）：
//   - 只解析裸名：含 `/` 的模型名**原样返回**，不查表（`channel/model` 与
//     `cline-free/x` 这类上游命名空间直通形态都零变化）；
//   - **不做 default_channel 回退**：内层若回退，会把现在「裸名报 400」变成
//     「静默路由到默认渠道」，属行为变更，本轮只加别名查表。
//
// 未命中/映射目标为空一律原样返回，让 runtimeForModel 报出与改造前逐字相同的
// "model must use explicit prefix…" 错误（口径 5：空表 = 完全 no-op）。
func (h *Handler) resolveAlias(model string) string {
	if strings.ContainsRune(model, '/') {
		return model // 全路径：不查表、原样返回（含前导/尾随空白的原值也保持不变）
	}
	name := strings.TrimSpace(model)
	if name == "" {
		return model
	}
	h.mmMu.RLock()
	m := h.modelMap
	target := strings.TrimSpace(m[name])
	if target == "" {
		// 通配匹配：与 gateway 同一条判据（key 以 * 结尾时按前缀比，取最长 key）
		if k := longestAliasWildcardKey(m, name); k != "" {
			target = strings.TrimSpace(m[k])
		}
	}
	h.mmMu.RUnlock()
	if target == "" {
		return model // 未命中（或映射目标为空 = 配置残留）：原样返回，错误串不变
	}
	return target
}

// longestAliasWildcardKey 返回最长的匹配通配 key（无则空串），与
// gateway.longestWildcardKey 逐字同源：仅 key 以 `*` 结尾时按前缀匹配，
// `*` 出现在中间视为普通字符。大小写敏感（Go map 查表语义，与 gateway 一致）。
func longestAliasWildcardKey(m map[string]string, model string) string {
	best := ""
	for k := range m {
		if !strings.HasSuffix(k, "*") {
			continue
		}
		prefix := strings.TrimSuffix(k, "*")
		if strings.HasPrefix(model, prefix) && len(k) > len(best) {
			best = k
		}
	}
	return best
}

// rewriteModel 改写发往上游的请求体：修补工具轮的空 content（见
// normalizeToolTurnContent），并把 model 覆盖为路由后的真实模型名。
func rewriteModel(body []byte, model string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	normalizeToolTurnContent(obj)
	obj["model"] = model
	return json.Marshal(obj)
}

// applyEffort 思考强度档位注入收口：未传且命中面板配置（lookupEffort 精确未命中回退
// 通配 `*`）时填入 reasoning_effort；键已存在（任意值含 null，客户端显式意图）不碰；
// 未配置（空 map，未配置 = 常态）零开销直通。注入失败不中断——原样返回旧 body，
// 让上游给出权威结果（同模型降级链「仅触发才干活」哲学，也与 rewriteModel 失败
// 处理同套路）。
func (h *Handler) applyEffort(body []byte, model string) []byte {
	h.efMu.RLock()
	ef, ok := lookupEffort(h.efforts, model)
	h.efMu.RUnlock()
	if !ok {
		return body
	}
	nb, changed, err := injectEffortField(body, ef)
	if err != nil {
		log.Printf("effort 注入失败 %s: %v", model, err)
		return body
	}
	if changed {
		log.Printf("effort inject: %s -> %s", model, ef)
		return nb
	}
	return body
}

// lookupEffort 思考强度映射查询：精确命中优先，未命中回退 `*` 全局默认；皆无 = 不注入。
// 空 map / 空模型名一律未命中（调用方无需再加前置判断）。
func lookupEffort(efforts map[string]string, model string) (string, bool) {
	if len(efforts) == 0 || model == "" {
		return "", false
	}
	if ef, ok := efforts[model]; ok {
		return ef, true
	}
	ef, ok := efforts["*"]
	return ef, ok
}

// injectEffortField 思考强度档位注入：请求体顶层**无** reasoning_effort 键时填入配置
// 档位。整体 map 解析 → 增键 → Marshal，保留其余全部字段；顶层键已存在（任意值含
// null——客户端显式意图）不碰，返回 changed=false。失败返回 error，调用方保持原样
// 转发（绝不因注入失败中断请求）。
func injectEffortField(body []byte, ef string) (nb []byte, changed bool, err error) {
	var obj map[string]any
	if err = json.Unmarshal(body, &obj); err != nil {
		return nil, false, err
	}
	if obj == nil {
		return nil, false, errors.New("body 不是 JSON 对象")
	}
	if _, ok := obj["reasoning_effort"]; ok {
		return nil, false, nil
	}
	obj["reasoning_effort"] = ef
	nb, err = json.Marshal(obj)
	if err != nil {
		return nil, false, err
	}
	return nb, true, nil
}

// normalizeToolTurnContent 把「content 为 null 或缺失」的 assistant / tool 消息补成空串。
//
// 上游（实测 qoder/deepseek-flash，2026-09-24）要求带 tool_calls 的 assistant
// 消息必须携带**字符串** content，null 或字段缺失会让它整请求拒答，并且报一个
// 完全误导的错：
//
//	Messages with role 'tool' must be a response to a preceding message with 'tool_calls'
//
// 实测矩阵（同一份客户端真实请求，只改这一处）：
//
//	assistant.content = null  → 200 + provider_error 帧（空流）
//	assistant.content = ""    → 正常出流
//	content 字段缺失          → 200 + provider_error 帧（空流）
//
// 危害在于**中毒进历史**：客户端在「模型只回工具调用、没输出正文」时会把该
// assistant 消息的 content 落成 null，之后这一轮永远留在 messages 里，于是该
// 会话的每一发请求都被上游拒（重启客户端也不恢复，只能新建会话）。
//
// 放宽到 tool 角色一并处理：工具返回空内容时客户端同样可能给 null，上游对工具
// 轮是同一套校验。
//
// 2026-10-11 追加（traecode/SOLO 实测）：**普通 assistant 消息**（无 tool_calls）
// content 缺失同样会被整请求拒答，错误是 SOLO 流内 event:error 的
// `4001 the param is invalid`。成因是 CodeBuddy 客户端在「思考中途被打断」时，
// 会把半截思考存成 `{"role":"assistant","reasoning":"…"}`（无 content、无
// tool_calls）；此后该会话每轮重放都带上这条空壳，于是每发必拒（同「中毒进
// 历史」）。重放对照坐实：原样 → 0.6s 即 4001；删掉该条或补 content:"" → 均
// 正常出流。故 assistant 一律补空串，不再以 tool_calls 为前提。
func normalizeToolTurnContent(obj map[string]any) {
	msgs, _ := obj["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "assistant", "tool":
		default:
			continue
		}
		if c, exists := m["content"]; !exists || c == nil {
			m["content"] = ""
		}
	}
}

// DefaultMaxRequestBody 请求体默认上限（128MiB）。
//
// 沿革：8MiB → 32MiB（issue #30，多模态大图 base64 后易超限）→ 128MiB（issue #176）。
// 提不动的原因不是「图大」，而是**无状态客户端每轮重放整段历史**：Codex 的一个会话
// 累积 12 张 2048px 截图（33.01MiB base64，文本仅 0.43MiB），请求体 33.44MiB 直接撞
// 32MiB 硬限回 413，会话报废。gateway 侧另有 `imageURLBlock` 在转发前把图缩到 1568px
// / JPEG（同 12 张 → 约 2.9MiB），所以上游实际收到的仍很小；此处上限只需容纳
// **客户端原始体**。仍可经 config.json 的 `server.max_request_body_mb` 调整。
const DefaultMaxRequestBody = 128 << 20

// MaxRequestBody 当前生效的请求体上限。默认 DefaultMaxRequestBody，
// 启动时按配置覆盖（见 NewHandler）。保留为包级变量是为兼容既有测试与调用方。
var MaxRequestBody int64 = DefaultMaxRequestBody

// ReadBodyLimited 读取请求体：超过 MaxRequestBody 时回 413 并返回错误。
// 用 LimitReader(max+1) 多读 1 字节以区分「恰好 max」与「超限」。
func ReadBodyLimited(r *http.Request) ([]byte, error) {
	limit := MaxRequestBody
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		_ = r.Body.Close()
		return nil, tooLargeError(limit)
	}
	return raw, nil
}

// errTooLarge 超限哨兵（调用方用 errors.Is 判定后回 413）。
var errTooLarge = errors.New("request body too large")

// IsBodyTooLarge 供 gateway 层判定「入站请求体超限」（跨包无法引用未导出哨兵）。
func IsBodyTooLarge(err error) bool { return errors.Is(err, errTooLarge) }

// tooLargeError 构造超限错误：包住哨兵以便 errors.Is 命中，文案带实际上限。
// MiB 数字由生效上限推导，避免改上限后文案不同步。
func tooLargeError(limit int64) error {
	return fmt.Errorf("%w: exceeds limit of %d bytes (%d MiB); please reduce request size",
		errTooLarge, limit, limit>>20)
}
