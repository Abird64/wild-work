// Package doubaowork 把本机豆包客户端的「工作任务」（侧栏 Ctrl+N / 新工作任务）接入 wild-work。
//
// # 与 internal/doubao 的区别（务必分清）
//
// 同一个豆包客户端里住着两个产品形态，协议不同、模型维度也不同：
//
//	internal/doubao      普通对话（conversation_type 缺省）—— 一条 fetch 完事
//	internal/doubaowork  工作任务（conversation_type=3 + model_item_key + reasoning_effort
//	                     + office 技能/连接器/飞书文档），并且**模型挂在会话属性上**
//
// 后者绕不开会话生命周期：模型只能通过 `/im/conversation/modify` 设到一条**已存在**的
// 会话上，而 conversation_id 要等主消息流开始后由服务端分配。所以每条代理请求是
// 「建会话 → 改属性 → 发消息 → 删会话」四步，详见 work.go 的文件头注释。
//
// # 共用设施
//
// CDP 连接与 SSE 字节级切分在 internal/doubaocdp；本包只持有自己的协议语义（R58/R111）。
package doubaowork

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/doubaocdp"
	"wild-work/internal/provider"
)

// Kind 渠道标识，模型前缀为 `doubaowork/<model>`，状态文件 state-doubaowork.json。
const Kind = "doubaowork"

// 上游地址（在页面上下文里 fetch，必须是绝对 URL）。
const (
	baseURL      = "https://www.doubao.com"
	epCompletion = baseURL + "/chat/completion"
	epQuotaSum   = baseURL + "/alice/commerce/sale/subscription/quota/summary/"
)

// completionQuery 与官方客户端逐字一致（实测）。这些 query 参与请求指纹，不要"顺手美化"。
const completionQuery = "?aid=582478&device_platform=web&doubao_device_platform=desktop" +
	"&client_platform=pc_client&channel=win&version_code=20800&language=zh"

// emitBinding 页面 → Go 的回传通道名（Runtime.addBinding 注册）。
const emitBinding = "__wwDoubaoWorkEmit"

const (
	emitDone = "\x00WW_DONE\x00"
	emitErr  = "\x00WW_ERR\x00"
)

// DefaultBotID 实测可用的 Bot（与 doubao2api 记录一致，交叉验证成立）。
const DefaultBotID = "7338286299411103781"

// ModelAuto 是本渠道唯一暴露的模型。
//
// 界面上的模型选择器是 React 内部状态渲染（DOM 无 data-value、bundle 里搜不到
// model_item_key 的取值表），因此除实测的「自动」(key=9) 之外不编造任何模型
// ——R49：只透传上游真实声明。
const ModelAuto = "auto"

// Client 实现 provider.Upstream。
type Client struct {
	addr string
	mu   sync.Mutex
}

// New 创建客户端；addr 为空时用默认调试端口。
func New(addr string) *Client {
	if strings.TrimSpace(addr) == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", doubaocdp.DefaultDebugPort)
	}
	return &Client{addr: addr}
}

// StaticModels 返回静态模型表。
func StaticModels() []provider.ModelInfo {
	return []provider.ModelInfo{{
		ID:   ModelAuto,
		Name: "豆包工作（工作任务·自动）",
		// 上游未声明上下文窗口；不填占位值，面板如实显示「未声明」
	}}
}

// FetchModels 返回静态表（上游无 OpenAI 式 /models）。
func (c *Client) FetchModels(_ *auth.Auth) ([]provider.ModelInfo, error) { return StaticModels(), nil }

// FetchModelPricing 无按模型计价口径。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) { return nil, nil }

// RefreshToken 无需刷新：凭据在客户端浏览器里。
func (c *Client) RefreshToken(_ *auth.Auth) error { return nil }

// DailyCheckin 每日签到领额度（签到是**账号级**活动，与 internal/doubao 打同一端点）。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.DailyCheckinReport(a)
	return err
}

// DailyCheckinReport 结构化上报签到结果（provider.CheckinReporter）。
//
// 与 internal/doubao 的实现同源（doubaocdp.Checkin），区别只在日志里的渠道名：
// 两个渠道共享同一个客户端与同一份账号额度，签到一次两边都算完成。
func (c *Client) DailyCheckinReport(_ *auth.Auth) (provider.CheckinReport, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	conn, err := c.dialPage(ctx)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	defer conn.Close()
	res, err := doubaocdp.Checkin(ctx, conn)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	if res.Idempotent {
		log.Printf("doubaowork checkin status=%s 日期=%s 连续=%d 天（未重复发放）",
			provider.CheckinAlready, res.CheckinDate, res.ContinuousDays)
		return provider.CheckinReport{
			Status: provider.CheckinAlready,
			Msg:    fmt.Sprintf("今日已领过（连续 %d 天）", res.ContinuousDays),
		}, nil
	}
	log.Printf("doubaowork checkin status=%s +%d %s 日期=%s 连续=%d 天",
		provider.CheckinClaimed, res.RewardAmount, res.RewardCode, res.CheckinDate, res.ContinuousDays)
	return provider.CheckinReport{
		Status: provider.CheckinClaimed,
		Amount: int64(res.RewardAmount),
		Msg:    fmt.Sprintf("签到成功 +%d 额度（连续 %d 天）", res.RewardAmount, res.ContinuousDays),
	}, nil
}

// dialPage 找对话页并建一条独立 CDP 连接。
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

// ChatStream 发起一次工作任务，返回豆包原始 SSE 读端。
//
// 编排（每一步都在页面上下文里执行，签名由前端 hook 自动注入）：
//
//	1. warmup   建会话，从 SSE_ACK 取 conversation_id
//	2. modify   设 conversation_type=3 / model_item_key=9 / reasoning_effort=4
//	3. 发消息   conversation_id 回填，模型设置自此生效
//	4. 流结束后删除会话（尽力而为）
func (c *Client) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	setupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := c.dialPage(setupCtx)
	if err != nil {
		return nil, 0, nil, err
	}

	if err := conn.Call(setupCtx, "Runtime.enable", nil, nil); err != nil {
		conn.Close()
		return nil, 0, nil, fmt.Errorf("%w: Runtime.enable 失败: %v", doubaocdp.ErrClientUnavailable, err)
	}
	if err := conn.Call(setupCtx, "Runtime.addBinding", map[string]any{"name": emitBinding}, nil); err != nil {
		conn.Close()
		return nil, 0, nil, fmt.Errorf("%w: 注册回传通道失败: %v", doubaocdp.ErrClientUnavailable, err)
	}

	localID := newLocalConvID()
	convID, err := c.warmup(setupCtx, conn, localID, DefaultBotID)
	if err != nil {
		conn.Close()
		return nil, 0, nil, err
	}

	// modify 失败不阻断整条请求：最坏退回会话默认模型（仍是一次可用的工作任务），
	// 比让整个请求 500 更符合用户预期。失败原因留在错误链里供诊断。
	if merr := c.modifyConv(setupCtx, conn, convID); merr != nil {
		// 不返回错误；上游若真不可用，主消息流会自己失败。
		_ = merr
	}

	reqBody, err := buildWorkBody(body, convID, localID)
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
	go c.pump(conn, pw, convID)
	if err := conn.EvaluateAsync(setupCtx, startWorkFetchScript(string(reqJSON))); err != nil {
		pw.CloseWithError(err)
		conn.Close()
		return pr, http.StatusOK, nil, nil
	}
	return pr, http.StatusOK, nil, nil
}

// pump 把 binding 事件转成 pipe 字节流；结束时顺手删会话。
func (c *Client) pump(conn *doubaocdp.CDP, pw *io.PipeWriter, convID string) {
	defer func() {
		_ = pw.Close()
		// 用完即焚：这条临时会话对用户没有价值，留着只会在列表里堆垃圾。
		// 用独立 ctx —— pump 已经结束，不能依赖上游请求的 ctx。
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = c.deleteConv(cleanCtx, conn, convID)
		cancel()
		conn.Close()
	}()

	idle := time.NewTimer(120 * time.Second)
	defer idle.Stop()
	hard := time.NewTimer(15 * time.Minute)
	defer hard.Stop()

	for {
		select {
		case <-hard.C:
			_ = pw.CloseWithError(errors.New("doubaowork 流式总时长超限"))
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
			idle.Reset(120 * time.Second)

			switch {
			case p.Payload == emitDone:
				return
			case strings.HasPrefix(p.Payload, emitErr):
				_ = pw.CloseWithError(&provider.Error{
					Kind: provider.ErrServer, Status: 0,
					Msg: strings.TrimPrefix(p.Payload, emitErr),
				})
				return
			}
			if _, err := io.WriteString(pw, p.Payload); err != nil {
				return
			}
		case <-idle.C:
			_ = pw.CloseWithError(errors.New("doubaowork 流式静默超时（120s 无数据）"))
			return
		case <-conn.Done():
			return
		}
	}
}

// buildWorkBody 构造工作任务的消息体。
//
// 与普通对话的唯一实质差别：conversation_id 必须回填（指向刚 modify 过的会话），
// 否则服务端会当成又一次"新建"、模型设置白做。
func buildWorkBody(openaiBody []byte, convID, localID string) (map[string]any, error) {
	var in struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(openaiBody, &in); err != nil {
		return nil, fmt.Errorf("doubaowork: 解析入站请求失败: %w", err)
	}
	if len(in.Messages) == 0 {
		return nil, &provider.Error{Kind: provider.ErrBadParams, Status: 400, Msg: "messages 为空"}
	}

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

	return map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": localID,
			"conversation_id":       convID,
			"bot_id":                DefaultBotID,
			"last_section_id":       "",
			"last_message_index":    nil,
			"local_permissions":     []any{},
		},
		"messages": []map[string]any{{
			"local_message_id": newUUID(),
			"content_block": []map[string]any{{
				"block_type": blockTypeText,
				"block_id":   newUUID(),
				"content": map[string]any{
					"text_block":     map[string]any{"text": text, "icon_url": "", "icon_url_dark": "", "summary": ""},
					"pc_event_block": "",
				},
			}},
		}},
	}, nil
}

// startWorkFetchScript 在页面里发消息并把 SSE 分片经 binding 回传。
func startWorkFetchScript(bodyJSON string) string {
	return fmt.Sprintf(`(async () => {
  const EMIT = %q, DONE = %q, ERR = %q;
  const emit = (s) => { try { window[EMIT](s); } catch(_) {} };
  try {
    const r = await fetch(%q, {
      method:'POST', credentials:'include',
      headers:{'content-type':'application/json','accept':'text/event-stream'},
      body: %q
    });
    if (!r.body) { emit(ERR + 'no body, status=' + r.status); return; }
    if (r.status >= 400) {
      let t=''; try { t = await r.text(); } catch(_) {}
      emit(ERR + 'upstream http ' + r.status + ' ' + t.slice(0,400)); return;
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

// Stream 把豆包 SSE 转成 OpenAI 流式响应。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	differ := newTextDiffer()
	flusher, _ := w.(http.Flusher)
	wrote := false

	emit := func(delta string) error {
		if delta == "" {
			return nil
		}
		wrote = true
		chunk := map[string]any{
			"id": "chatcmpl-doubaowork", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}}},
		}
		b, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	err := doubaocdp.ScanSSE(r, func(f doubaocdp.SSEFrame) error {
		switch f.Event {
		case evStreamMsg:
			if blocks, ok := parseStreamMsg(f.Data); ok {
				return collectBlocks(differ, blocks, emit)
			}
		case evStreamChunk:
			if blocks, ok := parseStreamChunk(f.Data); ok {
				return collectBlocks(differ, blocks, emit)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !wrote {
		return nil, fmt.Errorf("doubaowork: 上游未返回任何文本")
	}

	stop := map[string]any{
		"id": "chatcmpl-doubaowork", "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}
	sb, _ := json.Marshal(stop)
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", sb)
	if flusher != nil {
		flusher.Flush()
	}
	return nil, nil
}

// Aggregate 聚合成 OpenAI 非流式响应。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	differ := newTextDiffer()
	var full strings.Builder
	collect := func(blocks []contentBlock) {
		_ = collectBlocks(differ, blocks, func(d string) error {
			full.WriteString(d)
			return nil
		})
	}
	err := doubaocdp.ScanSSE(r, func(f doubaocdp.SSEFrame) error {
		switch f.Event {
		case evStreamMsg:
			if blocks, ok := parseStreamMsg(f.Data); ok {
				collect(blocks)
			}
		case evStreamChunk:
			if blocks, ok := parseStreamChunk(f.Data); ok {
				collect(blocks)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if full.Len() == 0 {
		return nil, fmt.Errorf("doubaowork: 上游未返回任何文本")
	}
	return map[string]any{
		"id": "chatcmpl-doubaowork", "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": full.String()},
			"finish_reason": "stop",
		}},
	}, nil
}

// UserResource 可消耗余额（窗口剩余百分比之和）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	_, items, err := c.UserResourceDetail(a)
	if err != nil {
		return 0, err
	}
	var remain int64
	for _, it := range items {
		if it.Usable && it.Remain > 0 {
			remain += it.Remain
		}
	}
	return remain, nil
}

// quotaSummary 额度接口响应投影（与 internal/doubao 同源语义）。
type quotaSummary struct {
	Data struct {
		WindowLimitSection struct {
			WindowLimitGroups []struct {
				FeatureGroup string `json:"feature_group"`
				WindowLimits []struct {
					WindowType  int   `json:"window_type"`
					UsedPercent int   `json:"used_percent"`
					EndTime     int64 `json:"end_time"`
				} `json:"window_limits"`
			} `json:"window_limit_groups"`
		} `json:"window_limit_section"`
		IncentiveQuotaSection struct {
			WindowLimitGroups []struct {
				FeatureGroupName string `json:"feature_group_name"`
				WindowLimits     []struct {
					WindowType  int   `json:"window_type"`
					UsedPercent int   `json:"used_percent"`
					EndTime     int64 `json:"end_time"`
				} `json:"window_limits"`
			} `json:"window_limit_groups"`
		} `json:"incentive_quota_section"`
	} `json:"data"`
}

func windowTypeName(t int) string {
	switch t {
	case 1:
		return "5 小时窗口"
	case 2:
		return "7 天窗口"
	case 3:
		return "活动赠送额度"
	}
	return fmt.Sprintf("窗口 type=%d", t)
}

// UserResourceDetail 读取窗口额度明细。额度是**账号级**的，与普通对话渠道共用同一份数据。
func (c *Client) UserResourceDetail(_ *auth.Auth) (int64, []provider.ResourceItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := c.dialPage(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()

	var sum quotaSummary
	expr := fmt.Sprintf(`(async () => {
		const r = await fetch(%q, {method:'POST', credentials:'include',
			headers:{'accept':'application/json','content-type':'application/json'}, body:'{}'});
		if (!r.ok) throw new Error('quota http ' + r.status);
		return await r.json();
	})()`, epQuotaSum)
	if err := conn.Evaluate(ctx, expr, &sum); err != nil {
		return 0, nil, err
	}

	var items []provider.ResourceItem
	var remain int64
	add := func(name string, used int, endMs int64) {
		left := int64(100 - used)
		if left < 0 {
			left = 0
		}
		items = append(items, provider.ResourceItem{
			Name: name, Total: 100, Used: int64(used), Remain: left, Usable: true,
			ExpireAt: unixMilliToRFC3339(endMs),
		})
		remain += left
	}
	for _, g := range sum.Data.WindowLimitSection.WindowLimitGroups {
		for _, w := range g.WindowLimits {
			add(fmt.Sprintf("%s (type=%d)", windowTypeName(w.WindowType), w.WindowType), w.UsedPercent, w.EndTime)
		}
	}
	for _, g := range sum.Data.IncentiveQuotaSection.WindowLimitGroups {
		for _, w := range g.WindowLimits {
			n := g.FeatureGroupName
			if n == "" {
				n = windowTypeName(w.WindowType)
			}
			add(fmt.Sprintf("%s (type=%d)", n, w.WindowType), w.UsedPercent, w.EndTime)
		}
	}
	return remain, items, nil
}

// Classify 错误分类（与普通对话同口径）。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	low := strings.ToLower(body)
	switch {
	case status == 401 || status == 403,
		strings.Contains(low, "not login"), strings.Contains(low, "未登录"):
		return provider.ErrSessionDead
	case status == 429, strings.Contains(low, "rate limit"), strings.Contains(low, "too many requests"),
		strings.Contains(low, "请求过于频繁"):
		return provider.ErrSoftRate
	case strings.Contains(low, "quota"), strings.Contains(low, "额度不足"), strings.Contains(low, "usage_exhausted"):
		return provider.ErrHardCredit
	case strings.Contains(low, "content"), strings.Contains(low, "审核"), strings.Contains(low, "违规"):
		return provider.ErrContentBlocked
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		return provider.ErrClient
	}
	return provider.ErrNone
}

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

func unixMilliToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
