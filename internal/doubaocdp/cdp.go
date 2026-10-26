// Package doubaocdp 封装豆包客户端（Chromium 147）的 DevTools 协议。
//
// # 为什么这个渠道跟其余 20 个不一样
//
// 豆包全站接口都受 a_bogus 风控签名保护，而该签名**无法在服务端复现**：
// 现存的三个逆向实现（lza6/doubao-2api、wangchuxiaoji-oss/doubao2api、
// 已被删除的 HERO476/dsh-doubao-work-connect）无一例外都是「在真浏览器里发请求，
// 让字节前端 JS hook 自动注入签名」，没有一个是纯算法计算的。
// 详见 docs/豆包渠道调研.md。
//
// 因此本渠道不直接 net/http 上游，而是通过客户端自带的 DevTools 协议
// （启动参数 --remote-debugging-port=30813）在页面上下文里发起 fetch。
// 代价是运行期依赖一个开着调试端口的豆包客户端；收益是签名、指纹、msToken
// 全部由官方前端自己处理，天然与真机一致。
//
// # 安全边界
//
// 调试端口等于「本机任意进程都能完全控制豆包客户端并读取全部 cookie」。
// 本包只连 127.0.0.1，且**不得**修改 --remote-debugging-address 让它对局域网可见。
package doubaocdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 调试端口与页面地址。端口与官方 CLI 客户端惯例一致（30813）。
const (
	DefaultDebugPort = 30813
)

// 页面 URL 前缀：豆包客户端的内部 scheme。
const (
	schemeChat = "doubao://doubao-chat"
)

// ErrClientUnavailable 表示豆包客户端没在跑，或没带调试端口。
// 上层据此给出「请先启动客户端」这类可行动提示，而不是当成上游故障去罚号。
var ErrClientUnavailable = errors.New("豆包客户端未就绪（需要以 --remote-debugging-port 启动）")

// Target 是 /json/list 返回的一个调试目标。
type Target struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// cdpError 是 CDP 协议层错误（method 返回 error 字段）。
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message)
}

type cdpResult struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *cdpError       `json:"error"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Event 是一条 CDP 事件（method + params 原文）。
type Event struct {
	Method string
	Params json.RawMessage
}

// CDP 是一个 page 级 DevTools 连接。
//
// 只支持「一个连接、一个读循环、并发 Call」这一种用法：读循环是唯一的读端，
// 所有请求靠 id 关联，事件走 Events()。这避免了多 goroutine 抢读 socket。
type CDP struct {
	conn *websocket.Conn

	writeMu sync.Mutex // 串行化写，gorilla 不支持并发写
	nextID  int64

	mu      sync.Mutex
	pending map[int64]chan cdpResult

	events chan Event
	closed chan struct{}
	err    error // 读循环退出原因（首次错误）
}

// dialer 只允许环回地址。见包注释的安全边界。
var dialer = websocket.Dialer{
	HandshakeTimeout: 5 * time.Second,
	NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("doubao: 拒绝连接非环回地址 %q（调试端口不得跨机使用）", host)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	},
}

// ListTargets 查询该调试端口上的目标列表。
//
// 用 HTTP 而不是 CDP：/json/list 是 DevTools 的固定 HTTP 端点，
// 比先连 browser ws 再 Target.getTargets 少一次握手。
func ListTargets(ctx context.Context, addr string) ([]Target, error) {
	url := "http://" + addr + "/json/list"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrClientUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: /json/list 返回 %d", ErrClientUnavailable, resp.StatusCode)
	}
	var out []Target
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("doubao: 解析 /json/list 失败: %w", err)
	}
	return out, nil
}

// PickChatTarget 从目标列表里挑出主对话页面。
//
// 选 doubao://doubao-chat（主 UI）。退而求其次用 launcher —— 两者加载的是同一份
// 前端 bundle，cookie/signature 能力相同，只是视图不同。
func PickChatTarget(targets []Target) *Target {
	var fallback *Target
	for i := range targets {
		t := &targets[i]
		if t.Type != "page" || t.WebSocketDebuggerURL == "" {
			continue
		}
		if strings.HasPrefix(t.URL, schemeChat) {
			return t
		}
		if fallback == nil && strings.Contains(t.URL, "doubao-launcher") {
			fallback = t
		}
	}
	return fallback
}

// Dial 连接一个 page 目标的 DevTools WebSocket。
func Dial(ctx context.Context, wsURL string) (*CDP, error) {
	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, fmt.Errorf("%w: 连接 %s 失败(status=%d): %v", ErrClientUnavailable, wsURL, status, err)
	}
	c := &CDP{
		conn:    conn,
		pending: make(map[int64]chan cdpResult),
		events:  make(chan Event, 256),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// readLoop 是唯一的读端：把带 id 的响应分发到等待者，其余作为事件广播。
func (c *CDP) readLoop() {
	defer close(c.closed)
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.mu.Lock()
			if c.err == nil {
				c.err = err
			}
			// 唤醒所有等待者，避免 Call 永久阻塞
			for id, ch := range c.pending {
				delete(c.pending, id)
				select {
				case ch <- cdpResult{ID: id, Error: &cdpError{Code: -1, Message: "connection closed"}}:
				default:
				}
			}
			c.mu.Unlock()
			return
		}
		var msg cdpResult
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- msg:
				default:
				}
			}
			continue
		}
		if msg.Method == "" {
			continue
		}
		// 事件：丢弃而不是阻塞。本包只关心少数事件，
		// 让无人在读时拖死读循环是更坏的失败模式。
		select {
		case c.events <- Event{Method: msg.Method, Params: msg.Params}:
		default:
		}
	}
}

// Events 返回事件通道。通道在连接关闭时不再产生新值（未关闭，避免读端 panic）。
func (c *CDP) Events() <-chan Event { return c.events }

// Done 在连接断开时关闭。
func (c *CDP) Done() <-chan struct{} { return c.closed }

// Err 返回读循环退出原因（连接正常时返回 nil）。
func (c *CDP) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close 关闭连接。
func (c *CDP) Close() error {
	err := c.conn.Close()
	select {
	case <-c.closed:
	case <-time.After(2 * time.Second):
	}
	return err
}

// Call 发一个 CDP 方法调用，把 result 解到 out（out 可为 nil）。
func (c *CDP) Call(ctx context.Context, method string, params any, out any) error {
	var raw json.RawMessage
	if err := c.call(ctx, method, params, &raw); err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("doubao: 解析 %s 响应失败: %w", method, err)
	}
	return nil
}

func (c *CDP) call(ctx context.Context, method string, params any, rawOut *json.RawMessage) error {
	select {
	case <-c.closed:
		return fmt.Errorf("%w: 连接已断开: %v", ErrClientUnavailable, c.Err())
	default:
	}

	c.writeMu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan cdpResult, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	err := c.conn.WriteJSON(payload)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%w: 写 CDP 请求失败: %v", ErrClientUnavailable, err)
	}

	select {
	case res := <-ch:
		if res.Error != nil {
			return res.Error
		}
		if rawOut != nil {
			*rawOut = res.Result
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.closed:
		return fmt.Errorf("%w: 连接已断开: %v", ErrClientUnavailable, c.Err())
	}
}

// evalResult 是 Runtime.evaluate 的返回。
type evalResult struct {
	Result struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception *struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

// Evaluate 在页面上下文执行一段 JS，把返回值解到 out（out 可为 nil）。
//
// awaitPromise=true ⇒ 可以直接传 async 表达式；returnByValue=true ⇒ 拿结构化值
// 而不是 remote object 句柄（后者需要再一次 Runtime.callFunctionOn，且要管释放）。
func (c *CDP) Evaluate(ctx context.Context, expr string, out any) error {
	var res evalResult
	err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &res)
	if err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		desc := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception != nil && res.ExceptionDetails.Exception.Description != "" {
			desc = res.ExceptionDetails.Exception.Description
		}
		return fmt.Errorf("doubao: 页面脚本异常: %s", truncate(desc, 300))
	}
	if out == nil {
		return nil
	}
	if len(res.Result.Value) == 0 {
		return nil // undefined
	}
	if err := json.Unmarshal(res.Result.Value, out); err != nil {
		return fmt.Errorf("doubao: 解析页面返回值失败: %w", err)
	}
	return nil
}

// EvaluateAsync 执行一段 JS 后**立即返回**，不等 promise 完成。
//
// 用于「启动一个长跑脚本」的场景：豆包的对话脚本要等整个 SSE 流读完才 resolve，
// 若用 Evaluate（awaitPromise=true）会把调用方阻塞到流结束甚至超时。
// 脚本内部的错误由脚本自己通过 emitErr 回传，不在这里抛。
func (c *CDP) EvaluateAsync(ctx context.Context, expr string) error {
	var res evalResult
	err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":        expr,
		"returnByValue":     false,
		"awaitPromise":      false,
		"silent":            true,
	}, &res)
	if err != nil {
		return err
	}
	// 仅有同步异常（脚本语法错误/立即抛错）会在此体现；异步 rejection 不阻塞这里。
	if res.ExceptionDetails != nil {
		desc := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception != nil && res.ExceptionDetails.Exception.Description != "" {
			desc = res.ExceptionDetails.Exception.Description
		}
		return fmt.Errorf("doubao: 页面脚本异常: %s", truncate(desc, 300))
	}
	return nil
}

// Cookie 是 CDP Network.Cookie 的投影（只取本包用得到的字段）。
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
}

// AllCookies 读取全部 cookie（含 httpOnly）。
//
// 关键点：豆包的登录态全在 **httpOnly** cookie 里，`document.cookie` 读出来是空的
// （实测 0 字符）。必须走 Network.getAllCookies，这也是「读客户端 cookie」的真实含义。
func (c *CDP) AllCookies(ctx context.Context) ([]Cookie, error) {
	if err := c.Call(ctx, "Network.enable", nil, nil); err != nil {
		return nil, err
	}
	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := c.Call(ctx, "Network.getAllCookies", nil, &out); err != nil {
		return nil, err
	}
	return out.Cookies, nil
}

// CookieHeader 把 cookie 拼成 Cookie 请求头，并限定到指定域名后缀。
//
// 只取与 domainSuffix 匹配的那些（如 ".doubao.com" / "www.doubao.com"），
// 避免把 byteimg、bytedance 等无关域的 cookie 一起塞进请求。
func CookieHeader(cookies []Cookie, domainSuffix string) string {
	var b strings.Builder
	for _, ck := range cookies {
		if !DomainMatches(ck.Domain, domainSuffix) {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(ck.Name)
		b.WriteByte('=')
		b.WriteString(ck.Value)
	}
	return b.String()
}

func DomainMatches(cookieDomain, want string) bool {
	d := strings.TrimPrefix(strings.ToLower(cookieDomain), ".")
	w := strings.TrimPrefix(strings.ToLower(want), ".")
	return d == w || strings.HasSuffix(d, "."+w) || strings.HasSuffix(w, "."+d)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
