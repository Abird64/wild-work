package doubao

import (
	"strings"
)

// 豆包 /chat/completion 的 SSE 事件名（实测捕获）。
//
// 抓包样本（docs/豆包渠道调研.md 二·补）：
//
//	event: SSE_HEARTBEAT
//	data: {}
//
//	event: SSE_ACK
//	data: {"query_list":[...],"ack_client_meta":{"conversation_id":"...","section_id":"..."},
//	       "timeout_conf":{"answer_first_pending_time":180000,...}}
//
//	event: FULL_MSG_NOTIFY
//	data: {"message":{"conversation_id":"...","message_id":"...","content_type":9999,
//	       "content_block":[{"block_type":10000,"content":{"text_block":{"text":"1"}},
//	                         "is_finish":false}], "index_in_conv":12}}
//
// ⚠️ 已知局限（如实标注，见 AGENTS.md §0.1 的判据分层）：
// 上面三种事件名是**实测**；文本增量到底是推 FULL_MSG_NOTIFY 还是另有
// STREAM_MSG_NOTIFY 之类的帧，**未实证**——抓包时只读到流的前 13KB 就中止了。
// 因此本文件对「没见过的 event 名」一律不报错、不静默丢弃，而是转交上层
// 计入 unknownEvents 统计，以便后续调试时能立刻看出漏了什么。
const (
	evHeartbeat = "SSE_HEARTBEAT"
	evAck       = "SSE_ACK"
	evFullMsg   = "FULL_MSG_NOTIFY"
	// evStreamMsg 正文帧：**助手的回答在这里**（实测）。
	// 注意它的载荷是 {"content":{"content_block":[...]}}，**不是** message 字段
	// —— 与 FULL_MSG_NOTIFY 的形状不同，首次实现时按 message 解析导致
	// 一条回答都取不到（联调实录）。
	evStreamMsg = "STREAM_MSG_NOTIFY"
	// evStreamChunk 增量补丁帧（实测）。载荷 {"message_id":"...","patch_op":[{...}]}，
	// patch_value.content_block 为主题；实测出现的帧里 text_block 为空、只带
	// is_finish:true 收官，因此它主要价值是「结束信号 + 可能的增量文本」。
	evStreamChunk = "STREAM_CHUNK"
	// evReplyEnd 回答结束（实测）。载荷 {"end_type":1,"msg_finish_attr":{"msgid","brief"}}，
	// brief 是答完后的摘要（短回答时等于全文）。
	evReplyEnd = "SSE_REPLY_END"
)

// 文本块类型。10000 = 纯文本（实测）。
const blockTypeText = 10000

// ackPayload 对应 SSE_ACK 的 data。
type ackPayload struct {
	QueryList []struct {
		QuestionID     string `json:"question_id"`
		LocalMessageID string `json:"local_message_id"`
		MessageIndex   int    `json:"message_index"`
	} `json:"query_list"`
	AckClientMeta struct {
		ConversationID   string `json:"conversation_id"`
		SectionID        string `json:"section_id"`
		ConversationType int    `json:"conversation_type"`
	} `json:"ack_client_meta"`
	TimeoutConf struct {
		AnswerFirstPendingTime int64 `json:"answer_first_pending_time"`
		PacketIntervalTime     int64 `json:"packet_interval_time"`
		MaxRetryCount          int   `json:"max_retry_count"`
	} `json:"timeout_conf"`
}

// contentBlock 是消息内容块。block_type 10000 = 文本（实测）。
//
// 同一 block_id 会随生成推进被反复推送（is_finish false → true），
// text 是**累积全文**而非增量——这一点由 textDiffer 负责还原成 delta。
type contentBlock struct {
	BlockType int    `json:"block_type"`
	BlockID   string `json:"block_id"`
	Content   struct {
		TextBlock struct {
			Text string `json:"text"`
		} `json:"text_block"`
		// 思考链/推理内容（未实证字段名，留通道供容错）
		ThinkingBlock struct {
			Text string `json:"text"`
		} `json:"thinking_block"`
	} `json:"content"`
	IsFinish bool `json:"is_finish"`
}

// msgNotify 对应 FULL_MSG_NOTIFY / STREAM_MSG_NOTIFY 的 data。
type msgNotify struct {
	Message struct {
		ConversationID string         `json:"conversation_id"`
		MessageID      string         `json:"message_id"`
		ContentType    int            `json:"content_type"`
		ContentBlock   []contentBlock `json:"content_block"`
		IndexInConv    int            `json:"index_in_conv"`
		SenderID       string         `json:"sender_id"`
		UserType       int            `json:"user_type"`
	} `json:"message"`
}

// textDiffer 把「同一 block 被反复推送累积全文」还原成增量 delta。
//
// 三种情形必须区分开，否则会重复吐字（这是流式代理最常见的 bug）：
//  1. 新文本以已发内容为前缀 → 只发多出来的尾巴（正常增量）
//  2. 新文本等于已发内容 → 什么都不发（重复推送）
//  3. 新文本不以已发内容为前缀 → 上游改写了历史（重生成/审核回退），
//     此时**整体重发**会让客户端出现重复段落，正确做法是只发这条 block
//     的当前全文一次，并标记由调用方决定是否重置。本实现选择「只发差异无法
//     表达的尾部为空」→ 即不重发、记录一次 rewrite 计数，交由上层决定。
type textDiffer struct {
	sent map[string]string // block_id → 已发出的全文
}

func newTextDiffer() *textDiffer {
	return &textDiffer{sent: make(map[string]string)}
}

// Delta 返回相对上次该 block 的增量；rewritten 表示上游改写了已发内容。
func (d *textDiffer) Delta(blockID, full string) (delta string, rewritten bool) {
	prev, seen := d.sent[blockID]
	if !seen {
		d.sent[blockID] = full
		return full, false
	}
	if full == prev {
		return "", false
	}
	if strings.HasPrefix(full, prev) {
		d.sent[blockID] = full
		return full[len(prev):], false
	}
	// 上游改写：不重发历史，只把「新全文」记下来。返回值交给上层决定。
	d.sent[blockID] = full
	return "", true
}

// streamMsgNotify 是 STREAM_MSG_NOTIFY 的载荷。
//
// ⚠️ 与 FULL_MSG_NOTIFY 的区别是本渠道最容易踩的坑：
//   - FULL_MSG_NOTIFY  → {"message": {...}}   ← 用户提问的回显
//   - STREAM_MSG_NOTIFY → {"content": {...}}  ← 助手的回答
// 两者 content_block 形状相同，只差外层字段名。
type streamMsgNotify struct {
	Content struct {
		ContentBlock  []contentBlock `json:"content_block"`
		ContentStatus int            `json:"content_status"`
	} `json:"content"`
}

// streamChunk 是 STREAM_CHUNK 的载荷（增量补丁）。
type streamChunk struct {
	MessageID string `json:"message_id"`
	PatchOp   []struct {
		PatchObject int `json:"patch_object"`
		PatchType   int `json:"patch_type"`
		PatchValue  struct {
			ContentBlock []contentBlock `json:"content_block"`
		} `json:"patch_value"`
	} `json:"patch_op"`
}

// Blocks 汇总补丁里所有内容块，便于调用方统一处理。
func (c *streamChunk) Blocks() []contentBlock {
	var out []contentBlock
	for _, op := range c.PatchOp {
		out = append(out, op.PatchValue.ContentBlock...)
	}
	return out
}

// replyEnd 是 SSE_REPLY_END 的载荷。
type replyEnd struct {
	EndType       int `json:"end_type"`
	MsgFinishAttr struct {
		MsgID string `json:"msgid"`
		Brief string `json:"brief"`
	} `json:"msg_finish_attr"`
}
