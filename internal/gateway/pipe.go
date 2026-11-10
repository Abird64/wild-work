// pipe.go 把现有 server.Handler 当作 http.Handler 在进程内调用，
// 并用 io.Pipe 保留流式（SSE）语义。不经过本地 HTTP 自环，无二次监听/鉴权/超时问题。
package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// pipeRW 是一个把响应体写入 io.Pipe 的 ResponseWriter，供内层 handler 使用。
//
// 与真实 ResponseWriter 的差异与理由：
//   - Flush() 是空操作：io.Pipe 无缓冲，每次 Write 都会阻塞到读端消费，
//     天然具备「写即送达」语义，无需显式冲刷。
//   - statusCh 用于握手：调用方必须先拿到状态码，才能决定按哪种协议转换响应体，
//     而内层 handler 可能先 WriteHeader(err) 再写错误体。状态码通过 buffered
//     channel 回传，Once 保证只发一次。
type pipeRW struct {
	hdr      http.Header
	pw       *io.PipeWriter
	statusCh chan int
	once     sync.Once
}

func newPipeRW(pw *io.PipeWriter) *pipeRW {
	return &pipeRW{hdr: http.Header{}, pw: pw, statusCh: make(chan int, 1)}
}

func (p *pipeRW) Header() http.Header { return p.hdr }

// setStatus 记录并回传状态码；Once 保证「显式 WriteHeader 优先于隐式 200」。
// statusCh 容量为 1，即使调用方已提前返回也不会阻塞写端 goroutine。
func (p *pipeRW) setStatus(code int) {
	p.once.Do(func() { p.statusCh <- code })
}

func (p *pipeRW) WriteHeader(code int) { p.setStatus(code) }

func (p *pipeRW) Write(b []byte) (int, error) {
	p.setStatus(http.StatusOK) // 未显式 WriteHeader 时按 HTTP 约定补 200
	return p.pw.Write(b)
}

// Flush 空实现：io.Pipe 的每次 Write 即为一次同步交付。
func (p *pipeRW) Flush() {}

// innerResult 是内层 handler 的响应：状态码 + 响应头 + 可流式读取的 body。
type innerResult struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
	cancel context.CancelFunc
	// limit 响应体读取上限的来源（装配层注入的 thunk，见 Gateway.maxInnerBody）。
	limit func() int64
}

// Close 释放管道与内层请求 context。调用方必须在读完后调用，否则内层 goroutine 可能阻塞在 Write 上。
func (r *innerResult) Close() {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	if r.cancel != nil {
		r.cancel()
	}
}

// ReadAll 读取完整响应体（非流式路径用），随后自动 Close。
func (r *innerResult) ReadAll() ([]byte, error) {
	defer r.Close()
	return io.ReadAll(io.LimitReader(r.Body, r.limit()))
}

// defaultMaxInnerBody 内层响应体读取上限的缺省值（128MiB，与
// server.DefaultMaxRequestBody 同值）。仅在装配层未注入 MaxInnerBody 时生效。
const defaultMaxInnerBody int64 = 128 << 20

// bodyLimit 读写上限（内层错误体/非流式响应体，以及解析入站请求体）。
// 优先用装配层注入的 thunk——它每次返回内层当前生效的上限
// （可被 NewHandler 按配置覆盖，故不能快照）；未注入时退回缺省值。
func (g *Gateway) bodyLimit() int64 {
	if g.maxBody != nil {
		if v := g.maxBody(); v > 0 {
			return v
		}
	}
	return defaultMaxInnerBody
}

// call 在进程内调用内层 handler（按 path 路由），返回其状态码与响应流。
//
// apiKey 非空时以 `Authorization: Bearer <apiKey>` 形式注入，让内层 withAuth 正常放行；
// 调用方应确保该 key 已经过校验（或直接来自请求方，由内层自行拒绝）。
func (g *Gateway) call(parent context.Context, path string, body []byte, apiKey string, extraHdr ...string) (*innerResult, error) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(parent)
	p := newPipeRW(pw)

	// host 仅作为占位，内层 ServeMux 只按 URL.Path 匹配。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://wild-work"+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	req.RemoteAddr = "127.0.0.1:0" // 内层若记录来源地址，避免空串
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// extraHdr 是 key/value 交替的内部头（R167：Layer→内层的 auto 降级全序）。
	// 只在本层已经认过的调用点传入，取值一律由本层自己生成，不搬客户端头族。
	for i := 0; i+1 < len(extraHdr); i += 2 {
		if k := extraHdr[i]; k != "" {
			req.Header.Set(k, extraHdr[i+1])
		}
	}

	go func() {
		// 兜底：内层 handler 未写任何内容就返回时，补 200 让调用方不至于阻塞在握手。
		defer func() {
			p.setStatus(http.StatusOK)
			_ = pw.Close()
		}()
		g.inner.ServeHTTP(p, req)
	}()

	select {
	case status := <-p.statusCh:
		// channel 收/发构成 happens-before，此后读 p.hdr 是安全的。
		return &innerResult{Status: status, Header: p.hdr, Body: pr, cancel: cancel, limit: g.bodyLimit}, nil
	case <-ctx.Done():
		// 客户端断连或调用方取消：关闭读端让内层写端拿到 ErrClosedPipe 退出，避免 goroutine 泄漏。
		_ = pr.Close()
		cancel()
		return nil, fmt.Errorf("inner call cancelled: %w", ctx.Err())
	}
}
