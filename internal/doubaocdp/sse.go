package doubaocdp

// SSE 帧扫描：豆包所有流式端点（/chat/completion 及其工作任务变体）共用同一套
// `event:` / `data:` / `id:` + 空行分隔的帧格式，因此放在这里作为传输层工具，
// 由各渠道包自行解释自己认识的 event 名。
//
// 放在公共包而不是各渠道各写一份的理由：它是**传输层**格式，与「哪个渠道、
// 哪种会话类型」无关；R58/R111 禁止的是渠道之间共享**协议语义**（谁的 SSE 里
// 有什么业务字段），不是禁止共享字节级切分。

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// SSEFrame 是一条完整的 SSE 帧（已按空行切分）。
type SSEFrame struct {
	Event string
	Data  []byte
	ID    string
}

// scanSSE 按 SSE 规范切帧并逐帧回调。
//
// 只实现豆包实际用到的子集：event / data / id 三行 + 空行分隔。
// 不做 retry / 多字段拼接 / BOM 处理——上游不发这些，多写只会增加未测路径。
//
// 末尾不带空行的最后一帧也会被回调（很多服务端在流结束时省略空行）。
func ScanSSE(r io.Reader, fn func(SSEFrame) error) error {
	br := bufio.NewReaderSize(r, 32*1024)
	var (
		cur   SSEFrame
		data  bytes.Buffer
		hasFn bool
	)
	flush := func() error {
		if !hasFn {
			return nil
		}
		cur.Data = data.Bytes()
		err := fn(cur)
		cur = SSEFrame{}
		data.Reset()
		hasFn = false
		return err
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				// 空行 = 帧边界
				if ferr := flush(); ferr != nil {
					return ferr
				}
			case bytes.HasPrefix(line, []byte(":")):
				// 注释行（心跳保活），忽略
			case bytes.HasPrefix(line, []byte("event:")):
				cur.Event = strings.TrimSpace(string(line[len("event:"):]))
				hasFn = true
			case bytes.HasPrefix(line, []byte("data:")):
				v := line[len("data:"):]
				// SSE 规范：data: 后的单个前导空格要剥掉
				if len(v) > 0 && v[0] == ' ' {
					v = v[1:]
				}
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(v)
				hasFn = true
			case bytes.HasPrefix(line, []byte("id:")):
				cur.ID = strings.TrimSpace(string(line[len("id:"):]))
			}
		}
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
	}
}
