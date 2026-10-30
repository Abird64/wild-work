// SSE 解析：流式读取、模型名改写、流内业务错误识别。
//
// 从 client.go 拆出（结构重排，零行为变更）：本包刻意不与其它渠道包共享 SSE 代码（R58）。
package lobsterai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// StreamError 上游在 **HTTP 200 的 SSE 流里**下发的业务错误帧（协议 §5 末条）。
//
// 为什么需要它（R40）：流式请求的 HTTP 200 与响应头在读到错误帧**之前**就已发出，
// 协议上无法在同一请求内换号重试；唯一可行的补救是把中招账号冷却掉，让客户端
// **下一次**重试时挑到别的账号。handler 用 errors.As 取 StreamErrorClassifier，
// 故 server 层不必 import 本包。
//
// 「免费额度用尽」正是本渠道最主要的失败模式，且就以这种形态表达（协议 §7.3）。
type StreamError struct {
	// Model 客户端请求的原始模型名（含渠道前缀），便于日志定位。
	Model string
	// Msg 上游 error.message 原文（可能为空 → 用整帧兜底）。
	Msg string
	// Payload 原始 error 帧（排障用，不含凭据）。
	Payload string
	// Status 恒为 200（流内错误），保留字段以便与 HTTP 层错误同形打印。
	Status int
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("lobsterai：上游流内错误（HTTP %d）：%s", e.Status, e.Msg)
}

// Kind 把流内错误分类（协议 §5.1 末条：只能按 message 判，状态码恒 200 无意义）。
func (e *StreamError) Kind() provider.ErrKind { return ClassifyStream(e.Msg) }

// Stream 透传上游 SSE，并把 model 字段回填成客户端请求的原始模型名（R14）。
//
// 流内错误处理（两种"不伪装"：
//  1. 上游那帧 `{"error":{...}}` 经 internal/upstream 的 error-passthrough 分支
//     **原样透出**给客户端（它带 error 键，不会被白名单剥掉）——
//     客户端看到的是真实失败原因，而不是一个干净的空回答；
//  2. 本方法把它转成 *StreamError 返回，handler 据此冷却账号（下一次请求换号）。
//
// 注意上层 streamCore 在流尾会统一补一个 `data: [DONE]`（共享层行为，cline/officeace
// 同款）：错误帧在前、DONE 在后，错误帧本身没有被吞掉，也没有伪造 finish_reason。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	sn := &streamSniffer{src: r}
	err := upstream.StreamCapture(w, rewriteModelReader(sn, model), func(u map[string]any) { usage = u })
	if se := sn.streamError(model, http.StatusOK); se != nil {
		// 流内业务错误优先于传输层错误：它带分类，能驱动账号冷却（换号可解）；
		// 而 streamCore 的截断/空流错误到了这里已经无从分类。
		return usage, se
	}
	return usage, err
}

// Aggregate 把上游 SSE 收敛成单个 OpenAI 响应（客户端要非流式时由 handler 调用）。
//
// 与 Stream 同款嗅探：上游同样可能在 HTTP 200 + 流内 error 帧返回，而
// upstream.Aggregate 会把该帧当成「无正文的成功响应」吞掉（客户端看到空 choices
// 且不报错、账号也不会冷却）——故命中错误帧时**优先返回错误**（R40/R60）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	sn := &streamSniffer{src: r}
	resp, err := upstream.Aggregate(rewriteModelReader(sn, model))
	if se := sn.streamError(model, http.StatusOK); se != nil {
		return nil, se
	}
	if err != nil {
		return nil, err
	}
	if resp != nil && model != "" {
		resp["model"] = model
	}
	return resp, nil
}

// streamSniffer 包在 SSE 读取流下方（**字节级透传、零改写**），只在完整 data 行里
// 记录**第一帧**带顶层 error 键的 payload。
//
// 为什么不用 strings.Contains(line, "error") 判错误帧：正常回答正文里出现 error 字样
// （模型在写代码 / 解释报错）会被误判 → 产出 StreamError → 冷却**健康账号**（实证误伤，
// 见 officeace/streamerr.go 的同款注释）。必须能解析成 JSON **且顶层有 error 键**。
//
// 为什么取第一帧而不是末帧：错误帧的首帧就是根因，后续帧只是重复/噪声。
type streamSniffer struct {
	src     io.Reader
	line    []byte
	payload string
}

func (s *streamSniffer) Read(p []byte) (int, error) {
	n, err := s.src.Read(p)
	for i := 0; i < n; i++ {
		if p[i] == '\n' {
			s.procLine()
			s.line = s.line[:0]
			continue
		}
		s.line = append(s.line, p[i])
	}
	if err != nil && len(s.line) > 0 {
		// 流在最后一帧中途结束（没有尾换行）：仍把残行交给判据 —— 错误帧若恰好是
		// 末行，丢掉它就会让「额度用尽」被当成干净收尾（R36/R40 的同一族问题）。
		s.procLine()
		s.line = s.line[:0]
	}
	return n, err
}

// procLine 处理一行：兼容 `data: {...}` 与 `data:{...}`（协议 §5 明确要求两种都认）。
func (s *streamSniffer) procLine() {
	if s.payload != "" {
		return // 已捕获根因帧，不再覆盖
	}
	line := strings.TrimSpace(strings.TrimRight(string(s.line), "\r"))
	if line == "" {
		return
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	} else if !strings.HasPrefix(line, "{") {
		// event: / id: / 注释行一律不看；裸 JSON 行（部分上游不写 data: 前缀）照看。
		return
	}
	if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &obj) != nil {
		return
	}
	if _, hasErr := obj["error"]; hasErr {
		s.payload = line
	}
}

// streamError 把嗅探到的错误帧转成 *StreamError；没有命中时返回 nil。
func (s *streamSniffer) streamError(model string, status int) *StreamError {
	if s.payload == "" {
		return nil
	}
	return parseStreamError(s.payload, model, status)
}

// parseStreamError 解析一帧 error payload（`{error:{message}}`，也容忍 error 是字符串或
// 只带 code 的形态）。message 取不到时用整帧兜底：宁可给一段 JSON，也不要给空原因。
func parseStreamError(payload, model string, status int) *StreamError {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return nil
	}
	raw, ok := obj["error"]
	if !ok {
		return nil
	}
	msg := strings.TrimSpace(strOfRaw(raw))
	if msg == "" {
		if inner, err := decodeObject(raw); err == nil {
			for _, k := range []string{"message", "msg", "type", "code"} {
				if v := strings.TrimSpace(strOfRaw(inner[k])); v != "" {
					msg = v
					break
				}
			}
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(payload)
	}
	return &StreamError{Model: model, Msg: msg, Payload: payload, Status: status}
}

// ---------------------------------------------------------------------------
// model 字段回填（R14）
// ---------------------------------------------------------------------------

// modelRewriter 逐行把 SSE 帧里的 "model":"<上游裸名>" 换成客户端模型名。
//
// 上游帧经 internal/upstream 的白名单规范化重建后，`model` 字段的序列化形态固定为
// 紧凑的 `"model":"xxx"`（无空格），所以按字节替换即可；**按行**处理是关键——
// 不能把整条流攒起来再替换，否则流式就退化成一次性响应。
type modelRewriter struct {
	src     *bufio.Reader
	pending []byte
	model   string
}

// rewriteModelReader 包装上游流；model 为空时零改动直通。
func rewriteModelReader(r io.Reader, model string) io.Reader {
	if model == "" {
		return r
	}
	return &modelRewriter{src: bufio.NewReaderSize(r, 64*1024), model: model}
}

// Read 一次吐一行（含尾换行），保持 SSE 帧边界完整。
func (m *modelRewriter) Read(p []byte) (int, error) {
	for len(m.pending) == 0 {
		line, err := m.src.ReadBytes('\n')
		if len(line) > 0 {
			m.pending = rewriteModelLine(line, m.model)
		}
		if err != nil {
			if len(m.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, m.pending)
	m.pending = m.pending[n:]
	return n, nil
}

// rewriteModelLine 替换单行内所有 `"model":"…"` 的值。
func rewriteModelLine(line []byte, model string) []byte {
	needle := []byte(`"model":"`)
	out := line
	from := 0
	for {
		i := bytes.Index(out[from:], needle)
		if i < 0 {
			return out
		}
		start := from + i + len(needle)
		end := bytes.IndexByte(out[start:], '"')
		if end < 0 {
			return out
		}
		end += start
		// 已替换段不会再次命中 needle（模型名里不含 `"model":"`），从替换点之后继续扫。
		repl := make([]byte, 0, len(out)-(end-start)+len(model))
		repl = append(repl, out[:start]...)
		repl = append(repl, model...)
		repl = append(repl, out[end:]...)
		out = repl
		from = start + len(model)
	}
}

// ---------------------------------------------------------------------------
// HTTP 小工具与 JSON 取值
// ---------------------------------------------------------------------------
