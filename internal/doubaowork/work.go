package doubaowork

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"wild-work/internal/doubaocdp"
)

// 本文件封装「豆包工作」的会话生命周期。
//
// # 为什么需要一整套会话管理
//
// 普通对话（internal/doubao）一条 fetch 就完事；工作任务不行。实测抓包（2026-10-26）
// 表明模型与产品形态都挂在**会话属性**上，而不是请求体里：
//
//	POST /chat/completion                      conversation_id=""  ← 创建会话（用默认模型）
//	POST /im/conversation/modify               conversation_type:3 + model_item_key:"9"
//	                                           + reasoning_effort:"4" + mode_id:"3"
//	POST /im/conversation/batch_del_user_conv  cmd 4171 删除
//
// 而 `modify` 只能作用于**已存在**的 conversation_id —— 服务端在主消息流开始后才分配它
// （实测 completion 发出 0.6s 后客户端才拿它去调 batch_get）。所以想「用工作任务协议 + 指定
// 模型」发一条消息，绕不开：**先用一条占位消息把会话怼出来 → 改成工作任务并设模型 →
// 再用它发真正的消息 → 用完删掉**。
//
// 代价是每条代理请求要 3~4 次页面内 fetch；收益是无状态（不污染用户的会话历史），
// 这也是 HERO476/dsh-doubao-work-connect 那个插件 `autoDeleteConversation` 的同一思路。

const (
	epIMConvModify = baseURL + "/im/conversation/modify"
	epIMConvDelete = baseURL + "/im/conversation/batch_del_user_conv"

	// 工作任务会话类型（实测 conversation_type / bot_conversation_type 同为 3）。
	convTypeWork = 3
	// mode_id "3" = 工作任务的执行模式（实测）。
	modeIDWork = "3"
	// modelItemKeyAuto = 界面上的「自动」模式（实测 id 9）。
	modelItemKeyAuto = "9"
	// reasoningEffort 界面上的思考档位（实测 "4"，对应 UI 里的「中」）。
	reasoningEffortDefault = "4"
)

// newLocalConvID 生成客户端本地会话 ID。
//
// ⚠️ 格式必须是 `local_` + **16 位纯数字**（实测 local_1181741749820556）。
// 早期联调时写成了 local_probe_<时间戳>，服务端不认这个形状、直接回落到最近会话，
// 结果测试消息全部灌进了用户正在用的对话里。这个格式是本渠道的硬约束，不是风格问题。
func newLocalConvID() string {
	return fmt.Sprintf("local_%016d", rand.New(rand.NewSource(time.Now().UnixNano())).Int63n(1e16))
}

// imEnvelope 是所有 /im/* 端点的统一信封（实测）。
type imEnvelope struct {
	Cmd        int            `json:"cmd"`
	UplinkBody map[string]any `json:"uplink_body"`
	SequenceID string         `json:"sequence_id"`
	Channel    int            `json:"channel"`
	Version    string         `json:"version"`
}

func newIMEnvelope(cmd int, body map[string]any) imEnvelope {
	return imEnvelope{
		Cmd:        cmd,
		UplinkBody: body,
		SequenceID: newUUID(),
		Channel:    2,
		Version:    "1",
	}
}

// ackConvID 是「建会话后取回服务端分配的 conversation_id」这一步的返回结构。
//
// 取法与普通对话一致：新 ID 出现在流首的 SSE_ACK 帧里（ack_client_meta.conversation_id）。
// 这里直接在页面脚本里把它捞出来回传，避免把整条 warmup 流搬回 Go 侧再解析。
type ackConvID struct {
	ConvID string `json:"convId"`
	Err    string `json:"err"`
}

// warmup 创建一条会话并返回服务端分配的 conversation_id。
//
// `localID` 由调用方生成并复用：同一条代理请求里的 warmup 与真实消息必须用**同一个**
// local_conversation_id，否则会被当成两条独立会话。
func (c *Client) warmup(ctx context.Context, conn *doubaocdp.CDP, localID string, botID string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": localID,
			"conversation_id":       "",
			"bot_id":                botID,
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
					"text_block":     map[string]any{"text": warmupText, "icon_url": "", "icon_url_dark": "", "summary": ""},
					"pc_event_block": "",
				},
			}},
		}},
	})
	if err != nil {
		return "", err
	}

	// 页面脚本：发请求 → 读到 SSE_ACK 就中止 → 回传 conversation_id。
	// 用 AbortController 主动掐断，避免占位消息把整轮回答跑完（浪费额度与时间）。
	script := fmt.Sprintf(`(async () => {
  const ac = new AbortController();
  try {
    const r = await fetch(%q, {
      method:'POST', credentials:'include',
      headers:{'content-type':'application/json','accept':'text/event-stream'},
      body: %q, signal: ac.signal
    });
    if (!r.body) return {err:'no body, status=' + r.status};
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let acc = '';
    for (let i = 0; i < 40; i++) {
      const { done, value } = await reader.read();
      if (done) break;
      acc += dec.decode(value, { stream: true });
      const m = acc.match(/"ack_client_meta"\s*:\s*\{[^}]*"conversation_id"\s*:\s*"(\d+)"/);
      if (m) { try { ac.abort(); } catch(_) {} return {convId: m[1]}; }
    }
    return {err:'SSE_ACK not found in ' + acc.length + ' bytes'};
  } catch (e) {
    return {err: String(e && e.message ? e.message : e)};
  }
})()`, epCompletion+completionQuery, string(body))

	var res ackConvID
	if err := conn.Evaluate(ctx, script, &res); err != nil {
		return "", err
	}
	if res.Err != "" {
		return "", fmt.Errorf("doubaowork: 创建会话失败: %s", truncate(res.Err, 200))
	}
	if res.ConvID == "" {
		return "", fmt.Errorf("doubaowork: 创建会话后未拿到 conversation_id")
	}
	return res.ConvID, nil
}

// modifyConv 把会话改成工作任务形态并设定模型 / 思考档位。
//
// 三个参数都是实测值（见文件头）：conversation_type=3、model_item_key=9（自动）、
// reasoning_effort=4（中）。⚠️ 其它 model_item_key 的取值表尚未探明 —— 界面上的模型
// 选择器是 React 内部状态渲染，DOM 无 data-value、bundle 里也搜不到，所以本渠道
// 目前只暴露「自动」这一个档，不编造其它模型的 key（R49 同口径）。
func (c *Client) modifyConv(ctx context.Context, conn *doubaocdp.CDP, convID string) error {
	env := newIMEnvelope(1114, map[string]any{
		"modify_conversation_uplink_body": map[string]any{
			"conversation_id":       convID,
			"conversation_type":     convTypeWork,
			"bot_conversation_type": convTypeWork,
			"mode_id":               modeIDWork,
			"model_item_key":        modelItemKeyAuto,
			"reasoning_effort":      reasoningEffortDefault,
		},
	})
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return c.postJSON(ctx, conn, epIMConvModify, string(raw))
}

// deleteConv 删除会话（cmd 4171）。
//
// 「用完即焚」的一环：代理请求结束后立刻删掉，用户会话列表里不会堆满临时任务。
// 删除失败只记日志不影响响应 —— 清理是尽力而为，不该拖垮已成功的回答。
func (c *Client) deleteConv(ctx context.Context, conn *doubaocdp.CDP, convID string) error {
	env := newIMEnvelope(4171, map[string]any{
		"batch_delete_user_conversation_uplink_body": map[string]any{
			"conversation_id":   []string{convID},
			"delete_all":        false,
			"conversation_type": convTypeWork,
		},
	})
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return c.postJSON(ctx, conn, epIMConvDelete, string(raw))
}

// postJSON 在页面上下文里 POST 一个 JSON 并忽略响应体（只关心成败）。
func (c *Client) postJSON(ctx context.Context, conn *doubaocdp.CDP, url, body string) error {
	script := fmt.Sprintf(`(async () => {
  try {
    const r = await fetch(%q, {
      method:'POST', credentials:'include',
      headers:{'content-type':'application/json','accept':'application/json'},
      body: %q
    });
    const t = await r.text();
    return {status: r.status, body: t.slice(0, 300)};
  } catch (e) { return {status: -1, body: String(e)}; }
})()`, url, body)
	var res struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := conn.Evaluate(ctx, script, &res); err != nil {
		return err
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("doubaowork: %s 返回 %d: %s", strings.TrimPrefix(url, baseURL), res.Status, truncate(res.Body, 160))
	}
	return nil
}

// warmupText 是占位消息的正文。
//
// 必须是最短且无副作用的一句话：这条消息会真实产生一次模型调用（计入额度），
// 所以不能用长文本；又不能为空（空 content_block 会被上游判非法）。
const warmupText = "1"
