package doubaowork

import (
	"encoding/json"
	"strings"
)

// 本文件持有本渠道自己的 SSE 语义。
//
// 为什么要抄一份而不是 import internal/doubao：R58/R111 的纪律是**渠道包之间不共享
// 协议语义**（谁的流里有什么业务字段是各渠道自己的知识）。字节级切分已抽到
// internal/doubaocdp.ScanSSE；这里只保留「哪些 event 是助手回答」这层判断。
//
// 实测结论（2026-10-26，工作任务会话）：事件集与普通对话**完全一致**
// —— STREAM_MSG_NOTIFY 承载助手正文、STREAM_CHUNK 是增量补丁、SSE_REPLY_END 收尾、
// FULL_MSG_NOTIFY 是用户提问回显。故两份结构目前同形；但刻意分开持有，
// 以便将来任一侧的上游发生分化时不必牵连对方。

const (
	evHeartbeat   = "SSE_HEARTBEAT"
	evAck         = "SSE_ACK"
	evFullMsg     = "FULL_MSG_NOTIFY"
	evStreamMsg   = "STREAM_MSG_NOTIFY"
	evStreamChunk = "STREAM_CHUNK"
	evReplyEnd    = "SSE_REPLY_END"

	blockTypeText = 10000
	// userTypeSelf 用户提问回显的 user_type（实测）。工作任务下同样出现，同样必须跳过。
	userTypeSelf = 1
)

// contentBlock 消息内容块。
type contentBlock struct {
	BlockType int    `json:"block_type"`
	BlockID   string `json:"block_id"`
	Content   struct {
		TextBlock struct {
			Text string `json:"text"`
		} `json:"text_block"`
	} `json:"content"`
	IsFinish bool `json:"is_finish"`
}

// streamMsgNotify 是 STREAM_MSG_NOTIFY 载荷（注意是 content，不是 message）。
type streamMsgNotify struct {
	Content struct {
		ContentBlock  []contentBlock `json:"content_block"`
		ContentStatus int            `json:"content_status"`
	} `json:"content"`
}

// streamChunk 是 STREAM_CHUNK 载荷（增量补丁）。
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

// Blocks 汇总补丁里的内容块。
func (c *streamChunk) Blocks() []contentBlock {
	var out []contentBlock
	for _, op := range c.PatchOp {
		out = append(out, op.PatchValue.ContentBlock...)
	}
	return out
}

// textDiffer 把「同一 block 被反复推送全文」还原成增量 delta。
// 与 internal/doubao 同构：新文本以已发内容为前缀则只发尾差；相等则不发；
// 不以已发内容为前缀说明上游改写历史，此时不重发（避免客户端出现重复段落）。
type textDiffer struct {
	sent map[string]string
}

func newTextDiffer() *textDiffer { return &textDiffer{sent: make(map[string]string)} }

// Delta 返回该 block 相对上次的增量；rewritten 表示上游改写了已发内容。
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
	d.sent[blockID] = full
	return "", true
}

// collectBlocks 从一帧里取出文本并做差分，把增量交给 emit。
func collectBlocks(differ *textDiffer, blocks []contentBlock, emit func(string) error) error {
	for _, blk := range blocks {
		if blk.BlockType != blockTypeText {
			continue
		}
		delta, _ := differ.Delta(blk.BlockID, blk.Content.TextBlock.Text)
		if delta == "" {
			continue
		}
		if err := emit(delta); err != nil {
			return err
		}
	}
	return nil
}

// parseStreamMsg 解析 STREAM_MSG_NOTIFY。
func parseStreamMsg(data []byte) ([]contentBlock, bool) {
	var m streamMsgNotify
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	return m.Content.ContentBlock, true
}

// parseStreamChunk 解析 STREAM_CHUNK。
func parseStreamChunk(data []byte) ([]contentBlock, bool) {
	var c streamChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, false
	}
	return c.Blocks(), true
}
