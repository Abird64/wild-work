// 客户端骨架：构造 / 传输 / 认证头 / 对话（ChatStream）与 token 续期。
//
// 从 client.go 拆出（结构重排，零行为变更）：本文件只留「一次请求怎么发出去」，
// 积分、签到、错误分类、SSE 解析、工具函数各自独立成文件（对齐 codearts/officeace 的既有布局）。
package lobsterai

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// Client 上游 HTTP 客户端。Base / Version 可覆盖以便测试注入假上游。
type Client struct {
	// HTTP 用于非流式请求（目录 / 积分 / 签到 / 续期 / 版本号），带总超时。
	HTTP *http.Client
	// StreamHTTP 用于对话 SSE：**不设**整体 Timeout，避免长回答在超时点被掐断
	// 且无终止帧（R35 / issue #42）。卡死由 IdleTimeout 空闲看门狗兜底。
	StreamHTTP *http.Client
	// IdleTimeout 流中空闲上限。<=0 时回落到 DefaultIdleTimeout。
	IdleTimeout time.Duration
	// Base 上游 API 基址；空 = APIBase。
	Base string
	// Version 版本号接口地址；空 = VersionURL。测试注入假上游用。
	Version string
}

// defaultClient 包级默认客户端：只给 ClientVersion() 这个包级入口用
// （登录包在 exchange 请求体里要带 version，而它没有自己的 Client）。
var defaultClient = New()

// 编译期自检：本包必须完整实现 provider.Upstream；签到结果结构化上报（CheckinReporter）
// 让调度器能区分「本次领到」与「活动还没上线」（不变式 23 的同款要求）。
var (
	_ provider.Upstream              = (*Client)(nil)
	_ provider.CheckinReporter       = (*Client)(nil)
	_ provider.StreamErrorClassifier = (*StreamError)(nil)
)

// New 生产默认客户端。
func New() *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:        &http.Client{Timeout: RequestTimeout, Transport: tr},
		StreamHTTP:  &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		IdleTimeout: DefaultIdleTimeout,
	}
}

// NewWithBase 测试用：覆盖上游 API 基址。
//
// ⚠️ 测试还必须把 Version 指到假上游：ClientVersion() 会去打版本号接口，
// 不覆盖它就会发**真实网络请求**（本仓纪律：测试零真实请求）。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = base
	return c
}

// base 返回生效的 API 基址。
func (c *Client) base() string {
	if s := strings.TrimRight(strings.TrimSpace(c.Base), "/"); s != "" {
		return s
	}
	return APIBase
}

// streamClient 返回对话用客户端：优先 StreamHTTP（无总超时）。
func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.HTTP
}

// idleTimeout 返回生效的空闲上限（未注入时用本包的 DefaultIdleTimeout=90s）。
//
// 取值口径：**本渠道没有任何实测数据**（协议 §1–§9 未给出超时，也未读过真实流），
// 故照抄本仓 minimax / monkeycode 的 90s 保守值——它只拦「真的一个字节都不出」，
// 不掐长思考。不用 idle.DefaultTimeout 的 300s：那会让真卡死的请求多挂 3 分钟。
func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return DefaultIdleTimeout
}

// ---------------------------------------------------------------------------
// 请求头
// ---------------------------------------------------------------------------

// authHeaders 补齐带 Bearer 的请求头（对话 / 目录 / 积分 / 签到，协议 §2.1）。
//
// withClientMeta 控制是否附上两个 X-LobsterAI-Client-* 头：
//   - 对话与模型列表**必需**（缺 kimi-k3-agentic-v1 会少返回模型，协议 §2.2）；
//   - 积分 / 签到**不要**（协议 §2.1：签到/余额只有 4 个基础头）。多发一个客户端
//     能力头不会让请求失败，但会让「这条请求像谁」变得不可解释，故按协议分开。
func (c *Client) authHeaders(req *http.Request, token, accept string, withClientMeta bool) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if withClientMeta {
		req.Header.Set(HeaderCapabilities, Capabilities)
		req.Header.Set(HeaderClientVersion, c.ClientVersion())
	}
}

// credentialHeaders 补齐凭据类请求头（exchange / refresh，协议 §2.1）：
// **不带 Authorization**，只要 Accept / Content-Type / User-Agent。
func credentialHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
}

// ---------------------------------------------------------------------------
// 请求构造
// ---------------------------------------------------------------------------

// ChatStream 发起一次对话（流式）。
//
// 返回约定（与其它渠道一致，见 server/handler.go 的消费方式）：
//   - 传输层错误 → (nil, 0, nil, err)：handler 记错并换号；
//   - HTTP >= 400 → (nil, status, 错误体, nil)：交给 Classify 分类后透传原文；
//   - 200 → (带空闲看门狗的响应体, 200, nil, nil)。
//
// **不做换号循环**：参考实现是在渠道内轮换（上限 3 个账号），wild-work 的换号是
// handler 的职责（账号级错误 → Kind.Rotatable() → 换号重试同一请求）。渠道内再套一层
// 循环会造成「双重重试」并把上游错误吞在里面，用户看不到真实首因。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	tok := tokenOf(a)
	if tok == "" {
		// 会话失效语义（不变式 19）：类型化返回，调用方用 errors.As 判定自愈。
		return nil, 0, nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "lobsterai：账号没有 access_token"}
	}
	out, berr := buildChatBody(body)
	if berr != nil {
		// 本地 400 而不是 err：err 会被 handler 当成传输层错误去 NoteError/换号，
		// 那是拿客户端的问题惩罚健康账号（本地 body 带 code=bad_params，见 Classify）。
		return nil, http.StatusBadRequest, localErrorBody(berr), nil
	}
	req, err := http.NewRequest(http.MethodPost, c.base()+EpChatCompletions, bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	c.authHeaders(req, tok, "text/event-stream, application/json", true)

	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		// 上游错误体含「免费额度已用完，请升级套餐」这类**用户必须看到**的文案，
		// 故原样透传（上限 1MB 防呆）。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
}

// buildChatBody 按协议 §4 改写请求体。
//
// 逐条对应协议表格（多余的字段一律不动——上游接受的字段我们没必要删）：
//   - `stream` **恒为 true**：上游只支持 SSE，`stream:false` 直接返回 500；
//   - `tools` 非空才带：空数组/空值是噪声，删掉；
//   - `tool_choice` **不发送**：参考实现的适配器没有该字段（上游是否接受这个字段未确认，
//     透传一个官方客户端从不发的字段风险更大）；
//   - `prompt_cache_key` **不发送**（同上）；
//   - `reasoning_effort` **仅调用方显式传入才透传**，不主动补档。
//
// ⚠️ 协议 §4.1 的坑写在注释里而不是代码里：wire 取值域是
// `off|minimal|low|medium|high|xhigh`，**没有 max**；直接发 `'max'` 与不带参数无差异，
// 发 `'xhigh'` 才触发最高档。本包对外声明的档位表恒为 wire 值（见 models.go），
// 故正常路径不会传进 `'max'`。这里**刻意不做 max→xhigh 改写**——那是「透传」的反面；
// 真要修映射，应该修注入侧（面板/配置），不是在渠道层偷偷改客户端的意图。
func buildChatBody(body []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象：%w", err)
	}
	if obj == nil {
		return nil, errors.New("请求体是空对象")
	}
	obj["stream"] = true
	if v, ok := obj["tools"]; ok && isEmptyList(v) {
		delete(obj, "tools")
	}
	delete(obj, "tool_choice")
	delete(obj, "prompt_cache_key")
	// developer → system：上游对 role 不做白名单校验，而是**内部崩掉**返回
	// `500 {"code":500,"message":"服务器内部错误","data":null}`（实测，见下）。
	// 本渠道请求体不经 upstream.PrepareBody，此前漏做归一 ⇒ DSH 一发 developer
	// 角色，三个健康账号各挨一发 500，被 consecutive errors 冷却 10 分钟，
	// 整条渠道回 503。与 officeace 的 400 ModelArts.81001 是同一个病、不同的崩法。
	//
	// 实测证据（2026-10-18，devdump 抓包，同一账号 123905、同一模型 deepseek-flash）：
	//   role=user      → 200（正常 SSE）
	//   role=developer → 500 空体（唯一变量）
	// 修法统一走 upstream.NormalizeRoles，不再复制一份。
	upstream.NormalizeRoles(obj)
	return json.Marshal(obj)
}

// isEmptyList 报告 JSON 值是否为「空的列表」（nil / 空数组）。
func isEmptyList(v any) bool {
	if v == nil {
		return true
	}
	if arr, ok := v.([]any); ok {
		return len(arr) == 0
	}
	return false
}

// localErrorBody 本地拒绝的错误体（OpenAI 风格）。
// code=bad_params 让 Classify 归到请求级错误：不罚号、不轮换、原文透传给客户端。
func localErrorBody(err error) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "lobsterai：" + err.Error(),
		"type":    "invalid_request_error",
		"code":    "bad_params",
	}})
	return b
}

// ---------------------------------------------------------------------------
// provider.Upstream：续期（协议 §3.2）
// ---------------------------------------------------------------------------

// RefreshToken 用 refreshToken 续期。
//
// 协议要点（每条都有坑）：
//   - `POST {APIBase}/api/auth/refresh`，**不带 Authorization**（凭据在请求体里）；
//   - 请求体 = keyfrom 身份载荷 + `refreshToken`；
//   - ⚠️ `uuid` / `userId` 缺省时**删键**而不是发空串（keyfromPayload 只在非空时 Set）；
//   - ⚠️ `latestKeyfrom` **刻意不更新为当前时刻**：严格对齐外部参考实现
//     （`lobsterai.ts:311-315`、`:456-461`；applyLobsteraiRefresh 也只改 token 与过期时间）。
//     这也意味着本方法**绝不回写 Extra 里的 keyfrom 四元组**；
//   - 终态：HTTP 401/403 或响应体命中 session-dead 标记 → `*provider.Error{ErrSessionDead}`；
//     其余（网络抖动 / 5xx / 429）→ 普通分类错误，走可重试路径；
//   - 成功后**必须落盘**（不变式 20）：refresh_token 会轮换，不落盘 = 下次启动用旧的。
func (c *Client) RefreshToken(a *auth.Auth) error {
	if a == nil {
		return &provider.Error{Kind: provider.ErrSessionDead, Msg: "lobsterai：没有账号"}
	}
	rt := strings.TrimSpace(a.RefreshTokenValue())
	if rt == "" {
		return &provider.Error{Kind: provider.ErrSessionDead, Msg: "lobsterai：账号没有 refresh_token"}
	}
	payload := c.keyfromPayload(a)
	payload["refreshToken"] = rt
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("lobsterai：续期请求体编码失败：%w", err)
	}
	status, respRaw, err := c.postJSON(c.base()+EpAuthRefresh, "", raw, false)
	if err != nil {
		return err
	}
	if e := c.businessErr(status, respRaw); e != nil {
		return e
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string          `json:"accessToken"`
			RefreshToken string          `json:"refreshToken"`
			ExpiresIn    json.RawMessage `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respRaw, &env); err != nil {
		return fmt.Errorf("lobsterai：续期响应解析失败：%w", err)
	}
	if env.Code != 0 {
		return c.envelopeError(status, env.Code, env.Msg, respRaw)
	}
	access := strings.TrimSpace(env.Data.AccessToken)
	if access == "" {
		// 缺 accessToken 即报错，不落半成品凭据（协议 §3.1 的同款纪律，续期同理）。
		return errors.New("lobsterai：续期响应缺 accessToken")
	}
	newRT := strings.TrimSpace(env.Data.RefreshToken)
	a.Lock()
	a.AccessToken = access
	if newRT != "" {
		// 上游没回新 refresh_token 时保留旧的（不回退成空串）。
		a.RefreshToken = newRT
	}
	if exp := intOfRaw(env.Data.ExpiresIn); exp > 0 {
		// expiresAt 优先用 expiresIn（**单位秒**，协议 §3.1）。
		a.ExpiresAt = time.Now().Add(time.Duration(exp) * time.Second).Unix()
	}
	a.Unlock()
	// expiresIn 缺失时回退 access token 的 JWT exp（协议 §3.1 的兜底口径）。
	// AdoptJWTExpiry 只在 exp 更晚时覆盖，故不会把正常值改坏；两者都拿不到时
	// 保留原 expiresAt —— 那会让 NeedsRefresh 可能恒真（反复续期），
	// ⚠️ 这是**已知限制**：上游不给任何到期信息时无法自行判定，只能靠下一次 401 纠正。
	a.AdoptJWTExpiry()

	// 不变式 20：refresh 轮换必须落盘。handler 之后还会再 SaveAtomic 一次，幂等无害。
	// 日志零 token（AGENTS.md §0.2）：只记 err，绝不打印任何 token 值。
	if err := a.SaveAtomic(); err != nil {
		log.Printf("lobsterai：续期落盘失败（仅影响下次启动）：%v", err)
	}
	return nil
}

// keyfromPayload 构造 keyfrom 身份载荷（请求体形态，协议 §3.2）。
//
// 与 keyfromQuery 同源但用途不同：query 给模型列表，body 给续期。
// 两处都遵守「空值键不发送」——这正是协议 §3.2 ⚠️ 要求的「uuid/userId 缺省时删键」。
//
// 注意 `latestKeyfrom` 取的是凭据里存的**原值**（Extra），不是当前时刻。
func (c *Client) keyfromPayload(a *auth.Auth) map[string]any {
	p := map[string]any{"version": c.ClientVersion()}
	if v := extraOf(a, ExtraFirstKeyfrom); v != "" {
		p["firstKeyfrom"] = v
	}
	if v := extraOf(a, ExtraLatestKeyfrom); v != "" {
		p["latestKeyfrom"] = v
	}
	if v := extraOf(a, ExtraUUID); v != "" {
		p["uuid"] = v
	}
	if v := extraOf(a, ExtraUserID); v != "" {
		p["userId"] = v
	}
	return p
}

// ---------------------------------------------------------------------------
// provider.Upstream：积分（协议 §7.2）
// ---------------------------------------------------------------------------
