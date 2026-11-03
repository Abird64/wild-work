package custom

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/backuplink"
	"wild-work/internal/idle"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// authHeader 按渠道声明的鉴权方式拼 Authorization 头值（空串 = 不设头）。
//
// 单一入口：client.go 的对话/请求与 probe.go 的探测必须走同一个函数，否则
// 「探测通过、真调用 401」这类分叉会重现（同 R108 「探针没走同一条链路」的教训）。
//
// Basic 形态的语义：api_key 是 `app_key:app_secret`，按原样 base64 后发 `Basic …`。
// 这里**按第一个冒号切分不做任何校验**（app_secret 自身可能含冒号，原样编码才是对的）；
// 冒号缺失时的拦阻在 ChannelConfig.Validate 里（用户能看懂的地方报错）。
func authHeader(mode AuthMode, key string) string {
	if key == "" {
		return ""
	}
	if mode == AuthBasic {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(key))
	}
	return "Bearer " + key
}

// 超时三兄弟的默认值与下限（R110）。
//
// 为什么等响应头这一段必须比其它渠道宽：自定义渠道挂的多是第三方反代/聚合网关
// （sgproxy、CLIProxy…），大 prompt 的首字节耗时**由上游排队与推理启动决定**，
// 与本机无关。90s 的旧值会让「上下文压缩」这类整段会话重发的大请求必然撞墙：
// 实测 sgproxy + gpt-6.1-sol 在 92 万 token 时 TTFB 常态 20~45s，抖动一下就越线。
const (
	// defaultHeaderTimeout 等响应头的默认上限（与内建渠道口径对齐的宽松值）。
	defaultHeaderTimeout = 180 * time.Second
	// minHeaderTimeout 等响应头的下限：再小会把正常的大 prompt 请求判死。
	minHeaderTimeout = 180 * time.Second
	// defaultIdleTimeout 流中空闲上限（拿到响应头之后才开始计时）。
	//
	// ⚠️ 这一段**刻意不跟着 headerTimeout 放宽**：它防的是「上游连上了、吐了几个
	// 字节之后彻底卡死」——那种情况下多等一分钟只是让用户多等，不会变好。
	defaultIdleTimeout = 90 * time.Second
	// defaultRequestTimeout 非流式整请求上限。
	defaultRequestTimeout = 120 * time.Second
)

// Client 代理自定义 OpenAI 兼容上游的客户端，实现 provider.Upstream 接口。
type Client struct {
	cfg        ChannelConfig
	HTTP       *http.Client // 带总超时，供非流式
	StreamHTTP *http.Client // 无总超时，供长 SSE 流式（R35）
	// baseTr 直连用的 transport。**只在直连连接层失败时**拿去问备用通道要一个
	// 代理版 client（见 doWithFallback），不做默认代理 —— R57「备用通道默认不参与
	// 任何请求，仅按需取用」在自定义渠道上的落地形态。
	baseTr *http.Transport
	idle   time.Duration
}

// New 构造自定义渠道客户端（取默认超时）。
func New(cfg ChannelConfig) *Client { return NewWithTimeout(cfg, 0) }

// NewWithTimeout 构造客户端并注入非流式请求总超时（0 = 取默认 120s）。
//
// 注入点与内建渠道一致（main.go 的 `xxxUp.HTTP.Timeout = cfg.Upstream.TimeoutSeconds`），
// 让 `upstream.timeout_seconds` 这个既有配置项对自定义渠道也生效——R66 引入自定义
// 渠道时漏了这条接线，配置项对它们一直是摆设。
func NewWithTimeout(cfg ChannelConfig, requestTimeout time.Duration) *Client {
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	// 等响应头的上限至少与总超时一样宽：总超时管的是整条请求，若响应头上限比它
	// 还小，就等于总超时被这一段提前截断，用户调大 timeout_seconds 也没用。
	headerTimeout := defaultHeaderTimeout
	if requestTimeout > headerTimeout {
		headerTimeout = requestTimeout
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
	}
	return &Client{
		cfg:        cfg,
		HTTP:       &http.Client{Transport: tr, Timeout: requestTimeout},
		StreamHTTP: &http.Client{Transport: tr},
		baseTr:     tr,
		idle:       defaultIdleTimeout,
	}
}

// HeaderTimeout 返回实际生效的「等响应头」上限（供测试与日志断言）。
func (c *Client) HeaderTimeout() time.Duration {
	if c.StreamHTTP != nil {
		if tr, ok := c.StreamHTTP.Transport.(*http.Transport); ok {
			return tr.ResponseHeaderTimeout
		}
	}
	return 0
}

// IdleTimeout 返回实际生效的流中空闲上限。
func (c *Client) IdleTimeout() time.Duration {
	if c.idle > 0 {
		return c.idle
	}
	return defaultIdleTimeout
}

// Config 返回当前配置副本。
func (c *Client) Config() ChannelConfig {
	return c.cfg
}

// ---------------------------------------------------------------------------
// provider.Upstream 必需接口实现
// ---------------------------------------------------------------------------

func (c *Client) RefreshToken(_ *auth.Auth) error { return nil }

func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

func (c *Client) UserResource(_ *auth.Auth) (int64, error) {
	return 0, nil
}

func (c *Client) UserResourceDetail(_ *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

func (c *Client) DailyCheckin(_ *auth.Auth) error { return nil }

func (c *Client) Classify(status int, _ string) provider.ErrKind {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return provider.ErrClient
	case status >= http.StatusInternalServerError:
		return provider.ErrServer
	default:
		return provider.ErrClient
	}
}

// doWithFallback 发请求；**仅当直连在连接层失败**且已配置备用通道时，换代理重试一次。
//
// 为什么需要（2026-10-30 现场）：自定义渠道的 transport 用的是
// `http.ProxyFromEnvironment` —— 它只读 `HTTPS_PROXY` 环境变量，**完全不看本工具的
// 「备用上游通道」**（那是 backuplink 的事），也**不读 Windows 的系统代理设置**
// （Go 在 Windows 上不查注册表）。于是用户明明在面板里配好并验证过代理，
// 自定义渠道打境外 API 仍然 DNS/连接超时，且看不出原因。
//
// 为什么不干脆默认走代理：R57 定的语义是「备用通道默认不参与任何请求，仅渠道按需取用」。
// 自定义渠道的 base_url 是用户手填的，可能是国内地址 —— 一律走代理会把它绕出国、
// 变慢甚至失败。所以按「需要时才用」：只有直连**连接层**失败才回落。
//
// 判据只认连接层（DNS/超时/拒绝/handshake）：HTTP 4xx/5xx 说明网络是通的，
// 换出口没有意义，重试只会掩盖真实错误。
func (c *Client) doWithFallback(req *http.Request, hc *http.Client, stream bool) (*http.Response, error) {
	return doWithFallbackTr(req, hc, c.baseTr)
}

// doWithFallbackTr 与 doWithFallback 同义，但接受显式 transport ——
// 端点探测（probe.go 的 getAt）是包级函数、没有 Client 实例，走这一版。
func doWithFallbackTr(req *http.Request, hc *http.Client, base *http.Transport) (*http.Response, error) {
	resp, err := hc.Do(req)
	if err == nil || !isConnectFailure(err) || !backuplink.Enabled() {
		return resp, err
	}
	if base == nil {
		return resp, err
	}
	bc := backuplink.Client(base)
	if bc == nil {
		return resp, err
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		b, gerr := req.GetBody()
		if gerr != nil {
			return resp, err
		}
		retry.Body = b
	}
	log.Printf("custom: 直连失败（%v），改用备用通道 %s 重试一次", err, backuplink.Raw())
	return bc.Do(retry)
}

// isConnectFailure 连接层失败判据（不是 HTTP 错误，而是根本没通）。
func isConnectFailure(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var netErr net.Error // 超时 / 连接被拒 / 网络不可达
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"connection refused", "no such host", "i/o timeout",
		"tls handshake", "network is unreachable", "connectex", "eof"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// ChatStream 发送聊天完成请求到上游。
func (c *Client) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if h := authHeader(c.cfg.Auth, c.cfg.APIKey); h != "" {
		req.Header.Set("Authorization", h)
	}

	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)
	if peek.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	req, cancel := idle.WithCancel(req)
	hc := c.HTTP
	if peek.Stream && c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}

	resp, err := c.doWithFallback(req, hc, peek.Stream)
	if err != nil {
		cancel()
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, resp.StatusCode, respBody, nil
	}

	bodyReader := idle.Monitor(resp.Body, c.IdleTimeout(), cancel)
	return bodyReader, http.StatusOK, nil, nil
}

// Stream 透传并规范化上游 SSE 帧，同时将响应 model 回填为客户端请求名（含渠道前缀）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := upstream.StreamCapture(w, rewriteModelReader(r, model), func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 读取上游响应，同时支持非流式 JSON 与流式 SSE 聚合。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	br := bufio.NewReader(r)
	for {
		b, err := br.Peek(1)
		if err != nil {
			return nil, err
		}
		if b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n' {
			_, _ = br.ReadByte()
			continue
		}
		if b[0] == '{' {
			var resp map[string]any
			if err := json.NewDecoder(br).Decode(&resp); err != nil {
				return nil, err
			}
			if model != "" {
				resp["model"] = model
			}
			return resp, nil
		}
		break
	}
	resp, err := upstream.Aggregate(br)
	if err != nil {
		return nil, err
	}
	if model != "" {
		resp["model"] = model
	}
	return resp, nil
}

// FetchModels 返回该渠道配置的模型列表。
// 规则：用户配的是什么就返回什么，运行期绝不向上游拉取（面板主动探测走 TestConnection），绝不做任何启发式推测。
func (c *Client) FetchModels(_ *auth.Auth) ([]provider.ModelInfo, error) {
	return c.StaticModels(), nil
}

// StaticModels 返回用户配置的模型信息（配什么返回什么，字段如实映射）。
func (c *Client) StaticModels() []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(c.cfg.Models))
	for _, m := range c.cfg.Models {
		if m.ID == "" {
			continue
		}
		efforts := provider.CleanEfforts(m.ReasoningEfforts)
		hasReasoning := m.SupportsReasoning || len(efforts) > 0 || m.DefaultReasoningEffort != ""
		mi := provider.ModelInfo{
			ID:                     m.ID,
			Name:                   m.ID,
			ContextWindow:          m.ContextWindow,
			MaxTokens:              m.MaxTokens,
			SupportsImages:         m.SupportsImages,
			SupportsReasoning:      hasReasoning,
			ReasoningEfforts:       efforts,
			DefaultReasoningEffort: m.DefaultReasoningEffort,
			SupportsTools:          m.ToolsEnabled(),
			// 上下文的「来源」按 CapSource 如实上报：以前这里两个标志都不置，
			// 导致自定义渠道面板上下文列永远好显示「未知」（消费方判据 has_context 永远为假）。
			ContextFromAPI:  m.CapFromAPI(),
			ContextFromUser: m.CapFromUser(),
		}
		out = append(out, mi)
	}
	return out
}

// ---------------------------------------------------------------------------
// SSE 模型名回填器
// ---------------------------------------------------------------------------

type modelRewriter struct {
	src     *bufio.Reader
	pending []byte
	model   string
}

func rewriteModelReader(r io.Reader, model string) io.Reader {
	if model == "" {
		return r
	}
	return &modelRewriter{src: bufio.NewReaderSize(r, 64*1024), model: model}
}

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

func rewriteModelLine(line []byte, model string) []byte {
	needle := []byte(`"model":"`)
	idx := bytes.Index(line, needle)
	if idx < 0 {
		return line
	}
	start := idx + len(needle)
	end := bytes.IndexByte(line[start:], '"')
	if end < 0 {
		return line
	}
	end += start
	var out bytes.Buffer
	out.Grow(len(line) + len(model))
	out.Write(line[:start])
	out.WriteString(model)
	out.Write(line[end:])
	return out.Bytes()
}
