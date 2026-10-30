package doubao

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"wild-work/internal/doubaocdp"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 渠道标识，用于模型前缀 `doubao/<model>` 与状态文件名。
const Kind = "doubao"

// 上游地址（在页面上下文里 fetch，所以必须是绝对 URL）。
const (
	baseURL      = "https://www.doubao.com"
	epCompletion = baseURL + "/chat/completion"
	epQuotaSum   = baseURL + "/alice/commerce/sale/subscription/quota/summary/"
	epBalance    = baseURL + "/alice/commerce/marketing/card/balance/"
	epProfile    = baseURL + "/alice/profile/self"
)

// completionQuery 是 /chat/completion 的固定查询串（实测抓包口径）。
//
// aid=582478 是豆包客户端的 appId；channel=win / chromium_version 与官方客户端
// 逐字一致——这些是请求指纹的一部分，改了会提高风控命中面，不要"顺手美化"。
const completionQuery = "?aid=582478&device_platform=web&doubao_device_platform=desktop" +
	"&client_platform=pc_client&channel=win&version_code=20800&language=zh"

// emitBinding 是页面 → Go 的回传通道名。
// 通过 Runtime.addBinding 注册后，页面里的 window.<name>(str) 会变成
// 一条 Runtime.bindingCalled 事件，payload 在 params.payload。
const emitBinding = "__wwDoubaoEmit"

// 结束标记：页面在流结束时发这个哨兵值，Go 侧据此关闭 pipe。
const emitDone = "\x00WW_DONE\x00"
const emitErr = "\x00WW_ERR\x00"

// Client 实现 provider.Upstream。
//
// 与其余渠道的本质差别：所有上游调用都在豆包客户端的页面上下文里执行，
// 由前端 JS hook 自动注入 a_bogus 签名与 msToken。Go 侧不持有任何凭据，
// 也不做签名计算。
type Client struct {
	addr string // 调试端口地址，如 127.0.0.1:30813

	// 串行化「取页面 + 建连」这一步。真正的请求各自持有独立 doubaocdp.CDP 连接，
	// 所以不是全局串行——避免一条慢流卡死整个渠道。
	mu sync.Mutex
}

// New 创建客户端。addr 为空时用默认调试端口。
func New(addr string) *Client {
	if strings.TrimSpace(addr) == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", doubaocdp.DefaultDebugPort)
	}
	return &Client{addr: addr}
}

// Addr 返回调试端口地址。
func (c *Client) Addr() string { return c.addr }

// dialPage 找到对话页并建立一条新的 doubaocdp.CDP 连接。
//
// 每条请求独立建连：doubaocdp.CDP 是单读循环模型，共享连接会让并发流互相干扰；
// 本地 WebSocket 的建连成本（毫秒级）远低于这种复杂度。
func (c *Client) dialPage(ctx context.Context) (*doubaocdp.CDP, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	targets, err := doubaocdp.ListTargets(ctx, c.addr)
	if err != nil {
		return nil, err
	}
	t := doubaocdp.PickChatTarget(targets)
	if t == nil {
		return nil, fmt.Errorf("%w: 没找到豆包对话页（客户端可能停在登录页）", doubaocdp.ErrClientUnavailable)
	}
	return doubaocdp.Dial(ctx, t.WebSocketDebuggerURL)
}

// ChatStream 发起一次对话，返回**豆包原始 SSE** 的读端。
//
// 返回值语义与其它渠道一致：(rc, status, respBody, err)。豆包没有 HTTP 状态码
// 可拿（请求发生在页面里），因此统一返回 200；真正的上游错误通过页面上报的
// emitErr 标记进入流内，由 Stream/Aggregate 解析并转成类型化错误（R40：流内
// 业务错误不得伪装成正常收尾）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := c.dialPage(ctx)
	if err != nil {
		return nil, 0, nil, err
	}

	if err := conn.Call(ctx, "Runtime.enable", nil, nil); err != nil {
		conn.Close()
		return nil, 0, nil, fmt.Errorf("%w: Runtime.enable 失败: %v", doubaocdp.ErrClientUnavailable, err)
	}
	if err := conn.Call(ctx, "Runtime.addBinding", map[string]any{"name": emitBinding}, nil); err != nil {
		conn.Close()
		return nil, 0, nil, fmt.Errorf("%w: 注册回传通道失败: %v", doubaocdp.ErrClientUnavailable, err)
	}

	// 组装豆包请求体
	reqBody, err := buildDoubaoBody(body)
	if err != nil {
		conn.Close()
		return nil, 0, nil, err
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		conn.Close()
		return nil, 0, nil, err
	}

	pr, pw := io.Pipe()
	// pump 必须用**独立** ctx：上面的 setup ctx 带 10s 超时且此处 cancel 后即失效，
	// 若共用会让 pump 立刻退出、上层读到空流（这个 bug 首次联调时踩过）。
	go c.pump(conn, pw)

	// 启动脚本用 EvaluateAsync：豆包的对话脚本要等整条 SSE 流读完才 resolve，
	// awaitPromise=true 会把这里阻塞到流结束甚至超时。
	script := startFetchScript(string(reqJSON))
	if err := conn.EvaluateAsync(ctx, script); err != nil {
		// 脚本启动失败：关掉 pipe 让上层立刻看到 EOF + 错误
		pw.CloseWithError(err)
		conn.Close()
		return pr, http.StatusOK, nil, nil
	}
	return pr, http.StatusOK, nil, nil
}

// pump 把 doubaocdp.CDP 的 bindingCalled 事件转成 pipe 字节流，直到 sentinel 或连接断开。
func (c *Client) pump(conn *doubaocdp.CDP, pw *io.PipeWriter) {
	defer func() {
		// 兜底：异常路径也要让读端退出，否则上层 handler 会挂住。
		_ = pw.Close()
		conn.Close()
	}()

	// 保护性超时：豆包 timeout_conf 里 answer_first_pending_time=180s，
	// 这里给到 3 倍余量。doubaocdp.CDP 没有 net/http 的 Timeout 可用（R35 的
	// StreamHTTP 语义在此渠道不适用），只能靠事件静默 + 总时长双闸。
	idle := time.NewTimer(90 * time.Second)
	defer idle.Stop()
	hard := time.NewTimer(10 * time.Minute)
	defer hard.Stop()

	for {
		select {
		case <-hard.C:
			_ = pw.CloseWithError(errors.New("doubao 流式总时长超限"))
			return
		case ev, ok := <-conn.Events():
			if !ok {
				return
			}
			if ev.Method != "Runtime.bindingCalled" {
				continue
			}
			var p struct {
				Name    string `json:"name"`
				Payload string `json:"payload"`
			}
			if err := json.Unmarshal(ev.Params, &p); err != nil || p.Name != emitBinding {
				continue
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(90 * time.Second)

			switch {
			case p.Payload == emitDone:
				return
			case strings.HasPrefix(p.Payload, emitErr):
				_ = pw.CloseWithError(&provider.Error{
					Kind:   provider.ErrServer,
					Status: 0,
					Msg:    strings.TrimPrefix(p.Payload, emitErr),
				})
				return
			}
			if _, err := io.WriteString(pw, p.Payload); err != nil {
				return
			}
		case <-idle.C:
			_ = pw.CloseWithError(errors.New("doubao 流式静默超时（90s 无数据）"))
			return
		case <-conn.Done():
			return
		}
	}
}

// startFetchScript 生成「在页面里发请求并把 SSE 分片回传」的 JS。
//
// 关键点：
//   - credentials:'include' 让浏览器自动带上全部 cookie（含 httpOnly 的 sessionid）
//   - 不手工加任何签名头：a_bogus / msToken 由豆包前端 hook 自动注入
//   - 每读到一片就调 window[emitBinding](text)，Go 侧即时收到（真流式）
//   - 结束/异常都发 sentinel，保证 Go 侧一定退出
func startFetchScript(bodyJSON string) string {
	return fmt.Sprintf(`(async () => {
  const EMIT = %q, DONE = %q, ERR = %q;
  const emit = (s) => { try { window[EMIT](s); } catch(_) {} };
  try {
    const r = await fetch(%q, {
      method: 'POST',
      credentials: 'include',
      headers: { 'content-type': 'application/json', 'accept': 'text/event-stream' },
      body: %q
    });
    if (!r.body) { emit(ERR + 'no response body (status ' + r.status + ')'); return; }
    if (r.status >= 400) {
      let t = ''; try { t = await r.text(); } catch(_) {}
      emit(ERR + 'upstream http ' + r.status + ' ' + t.slice(0, 400)); return;
    }
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      emit(dec.decode(value, { stream: true }));
    }
    const tail = dec.decode();
    if (tail) emit(tail);
    emit(DONE);
  } catch (e) {
    emit(ERR + 'fetch failed: ' + (e && e.message ? e.message : String(e)));
  }
})()`, emitBinding, emitDone, emitErr, epCompletion+completionQuery, bodyJSON)
}

// buildDoubaoBody 把入站 OpenAI 请求体转成豆包 /chat/completion 的 body。
//
// 请求体形状来自实测抓包（见 docs 二·补）。几个约束：
//   - conversation_id 传空串表示开新会话；但**实测服务端会忽略**它并复用
//     最近会话（抓包见 SSE_ACK.ack_client_meta.conversation_id 仍是旧会话）。
//     这是已知限制，见 docs「已知问题」。
//   - block_type 10000 = 文本块。
//   - bot_id 必须是有效的 Bot，实测 7338286299411103781 可用。
func buildDoubaoBody(openaiBody []byte) (map[string]any, error) {
	var in struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(openaiBody, &in); err != nil {
		return nil, fmt.Errorf("doubao: 解析入站请求失败: %w", err)
	}
	if len(in.Messages) == 0 {
		return nil, &provider.Error{Kind: provider.ErrBadParams, Status: 400, Msg: "messages 为空"}
	}

	// 只取最后一条 user 消息作为本轮输入。
	// 豆包客户端是"单轮 send"模型：历史由 conversation_id 在服务端维护，
	// 把整段 OpenAI 历史平铺成 messages[] 会被当成多轮并发提问。
	var text string
	for i := len(in.Messages) - 1; i >= 0; i-- {
		if in.Messages[i].Role != "user" {
			continue
		}
		text = extractText(in.Messages[i].Content)
		break
	}
	if text == "" {
		return nil, &provider.Error{Kind: provider.ErrBadParams, Status: 400, Msg: "最后一条 user 消息无可提取文本"}
	}

	blocks := []map[string]any{{
		"block_type": blockTypeText,
		"block_id":   newUUID(),
		"content": map[string]any{
			"text_block": map[string]any{
				"text": text, "icon_url": "", "icon_url_dark": "", "summary": "",
			},
			"pc_event_block": "",
		},
	}}

	return map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": "local_" + newUUID(),
			"conversation_id":       "",
			"bot_id":                DefaultBotID,
			"last_section_id":       "",
			"last_message_index":    nil,
			"local_permissions":     []any{},
		},
		"messages": []map[string]any{{
			"local_message_id": newUUID(),
			"content_block":    blocks,
		}},
	}, nil
}

// extractText 从 OpenAI 的 content 字段取纯文本。
// 兼容字符串与多模态数组（[{type:"text",text:"..."}]）。
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
