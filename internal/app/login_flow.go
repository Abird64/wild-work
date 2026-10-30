package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"wild-work/internal/login"
	logincodearts "wild-work/internal/login_codearts"
	logingemini "wild-work/internal/login_gemini"
	loginglm "wild-work/internal/login_glm"
	loginlobsterai "wild-work/internal/login_lobsterai"
	loginloomy "wild-work/internal/login_loomy"
	loginminimax "wild-work/internal/login_minimax"
	loginqoder "wild-work/internal/login_qoder"
	loginqodercn "wild-work/internal/login_qodercn"
	loginqodercom "wild-work/internal/login_qodercom"
	loginqwenwork "wild-work/internal/login_qwenwork"
	loginraccoon "wild-work/internal/login_raccoon"
	logintrae "wild-work/internal/login_trae"
	loginwbai "wild-work/internal/login_wbai"
	loginzcode "wild-work/internal/login_zcode"
	"wild-work/internal/platform"
	"wild-work/internal/provider"
)

const (
	loginTimeout   = 5 * time.Minute
	loginPollEvery = 2 * time.Second
)

// ---------------------------------------------------------------------------
// 渠道登录能力：单一来源表
// ---------------------------------------------------------------------------
//
// 为什么需要这张表（2026-10-18 深档排障，H3/H9 的根因）：
// 改造前「渠道 → 登录能力」被复制成了 5-7 份手工维护的表，全在同一条登录链路上：
//
//	A 表 app.go:634   准入判定（混了 5 种语义：真编排 / 导入型拒绝 / 匿名拒绝 /
//	                  只给链接不轮询 / 两步流程指路）
//	B 表 app.go:683   loginClient 工厂
//	C 表 app.go:719   Start() 调用
//	CancelLogin     6 个 `if kind == provider.X`（各自 Shutdown）
//	pollLogin       13 个 `if kind == provider.X`（各自 Poll + 收尾）+ defer 里 5 个 Shutdown
//
// 同一渠道要在这 5-7 处各登记一次，全靠注释约定「必须与前端对齐」。事故已发生 3 次：
// officeace 只加前端没加后端（死按钮）、7 渠道只加后端没加前端（无 ⟳ 入口）、
// Loomy 的 Shutdown 被误插进 Raccoon 的 if 块（取消 Loomy 时不关引导页）。
// 前两次的「修复」都是再手工对齐一遍 —— 这就是架构问题，不是细心问题。
//
// 本表把「一个渠道的登录能力」收成一条记录。新增渠道 = 加一行；
// 少写一项 = 该字段为 nil，行为由 loginFlows 的调用点统一兜底，不会静默失效。
type loginFlow struct {
	// Start 发起登录并**立即**返回可展示的授权页地址。
	// nil 表示本渠道不通过本表发起（见 noFlowReason）。
	Start func(c *http.Client, statePath string) (string, error)

	// Poll 单次查询授权结果，unfinished 表示「还没完成」（各包用自己的 ErrPending）。
	// done 负责把结果落成账号（即原来的 complete*Login）。
	// nil 表示本渠道不需要轮询（如 GLM 只给链接）。
	Poll func(c *http.Client, statePath string) (done func(*App) error, unfinished bool, err error)

	// Shutdown 释放登录期间占用的本机资源（浏览器 / 回调监听端口）。幂等，可为 nil。
	Shutdown func()

	// clientFactory 创建本渠道的 HTTP 客户端；nil 表示用 login.NewClient()。
	// 改造前这是一整张 switch（B 表），实际上除默认 client 外并无差异。
	clientFactory func() *http.Client

	// openIncognito 表示授权页必须由本工具用**无痕窗口**拉起，而不是交给前端的
	// window.open()。适用面：授权页会复用日常浏览器里已登录会话的渠道——
	// 否则「想添加第二个账号，却总被授权成已登录的那个」（2026-10-17 用户实测）。
	openIncognito bool

	// pollTimeout 轮询登录结果的超时上限；零值 = 用通用 loginTimeout。
	//
	// 为什么需要：部分渠道的本机回调存活时间比通用 5m 长——CodeArts 是 flowTTL（10m）。
	// 以较短者为上限会在用户还在授权页时先放弃并关掉回调口，浏览器随后跳回
	// 127.0.0.1:<port> 只会得到「连接被拒」（2026-10-18 真机实测）。
	//
	// 该特例原先写在 pollLogin 里当成一个 `if a.loginKind == provider.CodeArts`，
	// 属表外第二来源；现收进本表，保持「一个渠道一条记录」的约束。
	pollTimeout func() time.Duration

	// OnlyForRegion 非空时仅在该 region 生效（目前无人使用，留给地区差异渠道）。
	OnlyForRegion string

	// noFlowReason 非空表示本渠道**不通过本表发起登录**，字符串即给用户的说明。
	// 用于统一表达改造前 A 表的四种「拒绝/特例」语义：
	//   - 导入型渠道（凭据来自本机客户端）
	//   - 匿名/免密渠道（无账号可登）
	//   - 两步流程渠道（走专属入口）
	noFlowReason string

	// DisplayURL 非空表示「只把用户引到某个页面，不发起轮询」。
	// 这是 GLM 的形态：凭据由用户从浏览器 Cookie 取出后粘贴（SubmitGLMToken）。
	// 与 Start 的区别是**不置 loginBusy、不起 pollLogin** —— 改造前它靠「返回非空 URL」
	// 伪装成正常登录，前端据此启动了轮询，而轮询见 loginBusy=false 便提示
	// 「登录完成，正在同步账号…」，实际什么都没发生（H9 假成功）。
	DisplayURL string
}

// loginFlows 渠道登录能力的唯一来源。
//
// 约定：每条记录要么有 Start（真发起登录），要么有 DisplayURL（只引导不轮询），
// 要么有 noFlowReason（明确拒绝并说明原因）。三者至少有一，由 init 校验。
var loginFlows = map[provider.Kind]*loginFlow{}

// newClient 各渠道创建 HTTP 客户端的方式。改造前是一整张 switch，
// 实际上只有 Raccoon 不用 client、其它都是 NewClient()，故用 nil 表示「用默认 client」。
func newLoginClient(k provider.Kind) *http.Client {
	f, ok := loginFlows[k]
	if !ok || f.clientFactory == nil {
		return login.NewClient()
	}
	return f.clientFactory()
}

func init() {
	// —— 走标准 HTTP 授权流（Start + Poll），签名与 login_* 包一一对应 ——
	loginFlows[provider.WorkBuddy] = &loginFlow{
		Start: func(c *http.Client, sp string) (string, error) {
			ep := login.EndpointsForRegion(regionForLogin)
			url, err := login.Start(c, sp, ep)
			if err != nil {
				return "", err
			}
			if resolved, rerr := login.ResolveAuthURL(c, url, ep); rerr == nil && resolved != "" {
				url = resolved
			}
			return url, nil
		},
		Poll: func(c *http.Client, sp string) (func(*App) error, bool, error) {
			r, err := login.Poll(c, sp, login.EndpointsForRegion(regionForLogin))
			if err != nil {
				if errors.Is(err, login.ErrPending) {
					return nil, true, nil
				}
				return nil, false, err
			}
			return func(a *App) error { a.completeLogin(r); return nil }, false, nil
		},
	}
	loginFlows[provider.TraeWork] = &loginFlow{
		Start: logintrae.Start,
		Poll:  wrapPoll(logintrae.Poll, logintrae.ErrPending, func(a *App, r logintrae.Result) error { a.completeTraeLogin(r); return nil }),
	}
	loginFlows[provider.Qoder] = &loginFlow{
		Start: loginqoder.Start,
		Poll:  wrapPoll(loginqoder.Poll, loginqoder.ErrPending, func(a *App, r loginqoder.Result) error { a.completeQoderLogin(r); return nil }),
	}
	loginFlows[provider.QoderCN] = &loginFlow{
		Start: loginqodercn.Start,
		Poll:  wrapPoll(loginqodercn.Poll, loginqodercn.ErrPending, func(a *App, r loginqodercn.Result) error { a.completeQoderCNLogin(r); return nil }),
	}
	loginFlows[provider.QoderCOM] = &loginFlow{
		Start: loginqodercom.Start,
		Poll:  wrapPoll(loginqodercom.Poll, loginqodercom.ErrPending, func(a *App, r loginqodercom.Result) error { a.completeQoderCOMLogin(r); return nil }),
	}
	loginFlows[provider.WorkBuddyAI] = &loginFlow{
		Start: loginwbai.Start,
		Poll:  wrapPoll(loginwbai.Poll, loginwbai.ErrPending, func(a *App, r loginwbai.Result) error { a.completeWbaiLogin(r); return nil }),
	}
	loginFlows[provider.QwenWork] = &loginFlow{
		Start:    loginqwenwork.Start,
		Poll:     wrapPoll(loginqwenwork.Poll, loginqwenwork.ErrPending, func(a *App, r loginqwenwork.Result) error { a.completeQwenWorkLogin(r); return nil }),
		Shutdown: loginqwenwork.Shutdown,
	}
	loginFlows[provider.Minimax] = &loginFlow{
		Start: loginminimax.Start,
		Poll:  wrapPoll(loginminimax.Poll, loginminimax.ErrPending, func(a *App, r loginminimax.Result) error { a.completeMinimaxLogin(r); return nil }),
	}
	loginFlows[provider.LobsterAI] = &loginFlow{
		// LobsterAI 回调端口可固定（config.login.lobsterai_callback_port）：portal 只认
		// http://127.0.0.1:<port>/auth/callback，固定端口才能让「浏览器所在机器」预先
		// 建一条端口转发（ssh -L），从而在局域网/远程访问面板时也自动完成回调（R97）。
		// 由 StartLoginFor 在发起前写入（与 regionForLogin 同款模式，避免改表函数签名）。
		Start: func(c *http.Client, sp string) (string, error) {
			return loginlobsterai.StartWithPort(c, sp, lobsterAICallbackPort)
		},
		Poll:     wrapPoll(loginlobsterai.Poll, loginlobsterai.ErrPending, func(a *App, r loginlobsterai.Result) error { a.completeLobsterAILogin(r); return nil }),
		Shutdown: loginlobsterai.Shutdown,
		// R97：本机回调口存活 flowTTL（10m）也比通用 loginTimeout（5m）长，同款问题
		// （用户还在授权页就先放弃并关掉回调口），超时上限同样收进表。
		pollTimeout: loginlobsterai.FlowTTL,
	}
	loginFlows[provider.ZCode] = &loginFlow{
		Start: loginzcode.Start,
		Poll:  wrapPoll(loginzcode.Poll, loginzcode.ErrPending, func(a *App, r loginzcode.Result) error { a.completeZCodeLogin(r); return nil }),
	}
	loginFlows[provider.Gemini] = &loginFlow{
		Start:    logingemini.Start,
		Poll:     wrapPoll(logingemini.Poll, logingemini.ErrPending, func(a *App, r logingemini.Result) error { a.completeGeminiLogin(r); return nil }),
		Shutdown: logingemini.Shutdown,
	}
	loginFlows[provider.CodeArts] = &loginFlow{
		Start:    logincodearts.Start,
		Poll:     wrapPoll(logincodearts.Poll, logincodearts.ErrPending, func(a *App, r logincodearts.Result) error { a.completeCodeArtsLogin(r); return nil }),
		Shutdown: logincodearts.Shutdown,
		// 授权页会复用日常浏览器里已登录的华为云会话 ⇒ 必须用无痕窗口拉起，
		// 否则「想添加第二个账号却总被授权成已登录的那个」。
		openIncognito: true,
		// 本机回调存活 10m（flowTTL）比通用 loginTimeout（5m）长；超时上限对齐该包，
		// 否则用户还在授权页时就被放弃、回调口已关，浏览器跳回只会「连接被拒」。
		pollTimeout: logincodearts.FlowTTL,
	} // Loomy：本机引导页 + 短信验证码，持有 127.0.0.1 监听端口。
	loginFlows[provider.Loomy] = &loginFlow{
		Start:    loginloomy.Start,
		Poll:     wrapPoll(loginloomy.Poll, loginloomy.ErrPending, func(a *App, r loginloomy.Result) error { a.completeLoomyLogin(r); return nil }),
		Shutdown: loginloomy.Shutdown,
	}
	// Raccoon：网页版 CDP 捕获，签名与其它包同构（两个参数用不上）。
	// clientFactory 为 nil 表示用默认 client（它根本不发 HTTP 轮询）。
	loginFlows[provider.Raccoon] = &loginFlow{
		Start:    loginraccoon.Start,
		Poll:     wrapPoll(loginraccoon.Poll, loginraccoon.ErrPending, func(a *App, r loginraccoon.Result) error { a.completeRaccoonLogin(r); return nil }),
		Shutdown: loginraccoon.Shutdown,
	}

	// —— 不通过本表发起的渠道（改造前 A 表的四种语义，现统一为说明文字）——
	loginFlows[provider.MonkeyCode] = &loginFlow{
		noFlowReason: "MonkeyCode 渠道无需登录：请在面板点「从本机客户端导入」（复用本机已登录的官方客户端凭据）",
	}
	loginFlows[provider.Oczen] = &loginFlow{
		noFlowReason: "OpenCodeZen 为匿名通道，无需也无法添加账号",
	}
	loginFlows[provider.Kilo] = &loginFlow{
		noFlowReason: "Kilo 免费池无需登录，也没有可添加的账号",
	}
	// GLM：无编程式登录，只把用户引到页面，凭据由粘贴 Cookie 获得（见 DisplayURL 注释）。
	loginFlows[provider.GLM] = &loginFlow{
		DisplayURL: loginglm.LoginURL,
	}
	loginFlows[provider.OfficeAce] = &loginFlow{
		noFlowReason: "OfficeAce 登录是两步流程（华为云授权码 + 云端轮询捕获），请用面板的「浏览器登录」入口",
	}

	// 自检：每条记录必须恰好表达一种「发起方式」，防止将来加渠道时漏填。
	for k, f := range loginFlows {
		n := 0
		if f.Start != nil {
			n++
		}
		if f.DisplayURL != "" {
			n++
		}
		if f.noFlowReason != "" {
			n++
		}
		if n != 1 {
			panic(fmt.Sprintf("loginFlows[%s] 必须恰好表达一种发起方式（Start / DisplayURL / noFlowReason），实际 %d 种", k, n))
		}
	}
}

// wrapPoll 把「各包各形的 Poll + complete」适配成表中统一的形状。
// 各包的 Result 类型互不相同，只能在这里用泛型收口，避免 13 份重复的
// 「err == nil / ErrPending / 不报错就 continue」样板。
func wrapPoll[R any](
	poll func(*http.Client, string) (R, error),
	pending error,
	done func(*App, R) error,
) func(*http.Client, string) (func(*App) error, bool, error) {
	return func(c *http.Client, sp string) (func(*App) error, bool, error) {
		r, err := poll(c, sp)
		if err != nil {
			if errors.Is(err, pending) {
				return nil, true, nil
			}
			return nil, false, err
		}
		return func(a *App) error { return done(a, r) }, false, nil
	}
}

// regionForLogin 登录时使用的地区（workbuddy 系授权端点分地区）。
// 由 App 在发起登录前写入，避免把 region 塞进每张表的函数签名。
var regionForLogin = "cn"

// lobsterAICallbackPort LobsterAI 回调端口（0=随机）；由 App 发起登录前写入。
// 写入点与 regionForLogin 同款（见 loginFlows[provider.LobsterAI] 的 Start）。
var lobsterAICallbackPort int

// lookupLoginFlow 取某渠道的登录能力；不存在时返回 nil（调用方报「未知渠道」）。
func lookupLoginFlow(kind string) (*loginFlow, bool) {
	f, ok := loginFlows[provider.Kind(kind)]
	return f, ok
}

// needsPollFor 该渠道发起登录后是否需要前端轮询结果。
//
// 判据只有一条且来自单一来源表：**只给链接、不跑流程**的渠道（DisplayURL 形态，如 GLM）
// 不需要轮询；其余真发起登录流程的渠道都需要。
// 这是 H9（GLM 假成功）的唯一判定处——改造前前端靠「auth_url 是否非空」猜，
// 而 GLM 恰恰返回了非空 URL，于是轮询读不到 login_busy 便谎报「登录完成」。
//
// 未知渠道返回 false：既然连登录能力都没登记，本就不该进入轮询。
func (a *App) needsPollFor(kind string) bool {
	f, ok := loginFlows[provider.Kind(kind)]
	if !ok {
		return false
	}
	return f.DisplayURL == "" && f.Start != nil
}

// manualCallbackKind 报告该渠道的授权回调是否**写死回环地址**，因而需要一条
// 「把浏览器地址栏 URL 粘回来」的人工兜底通道。
//
// 为什么要有这个单一来源：/api/login/start 用它决定 `manual_callback`（前端显隐输入行），
// /api/login/callback 用它决定是否接受提交。两处各写一份条件，漏改一处就是
// 「输入框显示了、提交却 400」或「后端支持、前端没入口」的半接线状态。
//
// 判据（不是所有渠道都需要）：portal 侧只能把浏览器跳回 `http://127.0.0.1:<port>/…`
// 的，才是「浏览器必须与网关同机」——
//   - CodeArts：授权页只接受 `port` 参数，跳转目标由 portal 写死（见 internal/login_codearts）；
//   - LobsterAI：`redirect_uri` 由本工具给出，但回调口绝不监听非环回（见 internal/login_lobsterai）；
//   - TraeWork：回调写死 `http://127.0.0.1:<随机端口>/authorize`，且监听只有 5 分钟寿命，
//     局域网/超时场景下凭证都到不了网关（见 internal/login_trae，issue #98）；
//   - QwenWork：PKCE(Hydra) 回环回调（`/callback`），redirect_uri 白名单制不可改（R104 前置）；
//   - Gemini：installed-app 政策限 redirect host 为 localhost（不可改，见 internal/login_gemini）。
//
// 其余渠道的回调**不是**这种形状：有的由网关侧轮询/捕获（glm、小浣熊、OfficeAce），
// 有的是本机引导页（Loomy），有的是纯服务端轮询无回调环节（workbuddy/qoder/zcode/minimax 系），
// 不接受也不知道怎么消费用户粘回来的 URL，故一律为 false。
func manualCallbackKind(kind provider.Kind) bool {
	switch kind {
	case provider.CodeArts, provider.LobsterAI, provider.TraeWork, provider.QwenWork, provider.Gemini:
		return true
	}
	return false
}

// StartLoginFor 发起指定渠道登录：workbuddy / workbuddyai / traework / qoder / qodercn。
func (a *App) StartLoginFor(kind string) (string, error) {
	k := provider.Kind(strings.TrimSpace(kind))
	if k == "" {
		k = provider.WorkBuddy
	}

	// 准入与形态判定：全部来自 loginFlows 这一张表。
	flow, known := loginFlows[k]
	if !known {
		return "", fmt.Errorf("unknown login provider %s", kind)
	}
	// 不通过本表发起的渠道：统一用 noFlowReason 说明原因（改造前是 4 个手写 case）。
	if flow.noFlowReason != "" {
		return "", errors.New(flow.noFlowReason)
	}
	// 只引导不轮询的渠道（GLM）：返回地址但**不置 loginBusy、不起 pollLogin**。
	// 前端据 /api/state 下发的 capabilities 判定「是否需要轮询」，
	// 不再靠「返回值非空」猜（改造前正是这个猜法导致 H9 假成功）。
	if flow.DisplayURL != "" {
		return flow.DisplayURL, nil
	}
	regionForLogin = a.cfg.Region
	a.muLogin.Lock()
	if a.loginBusy {
		a.muLogin.Unlock()
		return "", errors.New("已有登录流程进行中，请先完成或取消")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.loginCtx, a.loginCancel = ctx, cancel
	a.loginKind = k
	// B 表已并入 loginFlows：Raccoon 不发 HTTP 轮询，clientFactory 为 nil 时
	// 仍给一个默认 client（它用不到，但保持非 nil 可避免各处 nil 判断）。
	a.loginClient = newLoginClient(k)
	a.loginBusy = true
	a.loginErr = ""
	a.muLogin.Unlock()

	// C 表已并入 loginFlows：一条记录一个 Start，结构上不可能再出现
	// 「两行挤进同一个 case、前者被覆盖」（H2）或「case 只有注释」（H1）。
	// LobsterAI 的回调端口配置在发起前写入（R97：portal 只认 127.0.0.1:<port>，
	// 固定端口才能让浏览器侧预建 ssh -L 转发实现全自动回调）。
	lobsterAICallbackPort = a.cfg.Login.LobsterAICallbackPort
	authURL, err := flow.Start(a.loginClient, a.loginStateFP)

	if err != nil {
		a.finishLogin()
		return "", err
	}
	// 浏览器打开通常由前端 Web UI 通过 window.open() 完成；但 CodeArts 例外：
	// 它的授权页会**复用日常浏览器里已登录的华为云会话**，于是「想添加第二个账号，
	// 却总被授权成已登录的那个」（2026-10-17 用户实测报告）。故由本工具用**无痕窗口**拉起。
	if flow.openIncognito {
		a.openAuthURLIncognito(authURL)
	}
	go a.pollLogin(ctx)
	log.Printf("%s 登录流程已发起", k)
	return authURL, nil
}

// openAuthURLIncognito 用无痕窗口打开授权页（用于必须避开日常浏览器登录态的渠道）。
//
// 成功时置 loginBackendOpened，让前端**不再** window.open —— 否则会在日常浏览器里再开一个
// 带登录态的标签页，无痕那一步就白做了。
// 探测不到支持无痕的浏览器 / 启动失败时**退回系统默认打开**：可用性优先于隔离性，
// 并在日志里说明原因，便于排查「为什么还是登录成了老账号」。
func (a *App) openAuthURLIncognito(url string) {
	exe, flag, ok := platform.DefaultBrowserIncognito()
	if !ok {
		log.Printf("登录：未探测到支持无痕的默认浏览器，退回系统默认打开（可能带上已有登录态）")
		_ = platform.OpenURL(url)
		return
	}
	if err := platform.LaunchIncognito(exe, flag, url); err != nil {
		log.Printf("登录：无痕窗口启动失败（%v），退回系统默认打开", err)
		_ = platform.OpenURL(url)
		return
	}
	a.muLogin.Lock()
	a.loginBackendOpened = true
	a.muLogin.Unlock()
	log.Printf("登录：已用无痕窗口打开授权页（避免复用日常浏览器的登录态）")
}

// CancelLogin 取消当前登录流程。
func (a *App) CancelLogin() error {
	a.muLogin.Lock()
	cancel := a.loginCancel
	kind := a.loginKind
	a.muLogin.Unlock()
	if cancel == nil {
		return errors.New("没有进行中的登录")
	}
	cancel()
	_ = os.Remove(a.loginStateFP)
	// 释放本机资源（浏览器 / 127.0.0.1 回调监听端口）：由 loginFlows 的 Shutdown 统一表达。
	// 改造前这里是 6 个平铺的 `if kind == provider.X`，且注释记载「Loomy 的 Shutdown 曾被
	// 误插进 Raccoon 的 if 块里，取消 Loomy 时引导页不关」—— 同一个坑的第三次复发。
	a.shutdownLoginFlow(provider.Kind(kind))
	log.Printf("登录已取消")
	return nil
}

// shutdownLoginFlow 释放某渠道登录期间占用的本机资源（幂等，未登记或无 Shutdown 则无操作）。
// 取代了改造前散落在 CancelLogin 与 pollLogin defer 里的 11 个手工 if 分支
// （两处各写一遍，且曾因插错层级而漏关）。
func (a *App) shutdownLoginFlow(k provider.Kind) {
	if f, ok := loginFlows[k]; ok && f.Shutdown != nil {
		f.Shutdown()
	}
}

// pollLogin 后台轮询登录结果，写日志。
func (a *App) pollLogin(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("login poll panic: %v", r)
		}
		// 释放本机资源：由 loginFlows 的 Shutdown 统一表达（幂等）。
		// 改造前这里是 5 个平铺的逐渠道 if，且与 CancelLogin 里那 6 个各写一遍 ——
		// 两处不一致就会漏关（Loomy 引导页就漏过一次）。
		a.shutdownLoginFlow(a.loginKind)
		a.finishLogin()
	}()
	// 超时上限由 loginFlows 的 pollTimeout 决定（零值用通用 loginTimeout）。
	// 原先这里是 `if a.loginKind == provider.CodeArts` 的表外特例，现收进表。
	// R97：LobsterAI 的回调口存活 flowTTL（10m）也比通用 loginTimeout（5m）长，
	// 同款问题（用户还在授权页就先放弃并关掉回调口），同样收进表。
	pollBudget := loginTimeout
	if f, ok := loginFlows[a.loginKind]; ok && f.pollTimeout != nil {
		pollBudget = f.pollTimeout()
	}
	deadline := time.Now().Add(pollBudget)
	log.Printf("登录流程已发起，等待浏览器完成…")
	t := time.NewTicker(loginPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("登录已取消")
			a.setLoginError("登录已取消")
			return
		case <-t.C:
		}
		if time.Now().After(deadline) {
			log.Printf("登录超时，请重新发起")
			a.setLoginError("登录超时（未收到授权回调），请重新发起登录")
			return
		}
		// 分派：由 loginFlows 表决定，不再逐渠道 if。
		// 改造前这里是 13 个几乎相同的块（每块 5-8 行样板：Poll → 判 err → 判 ErrPending
		// → 不报错就 continue → setLoginError），且块序与 A/B/C 三张表的顺序无关，
		// 新增渠道时极易漏写其中一处（漏了就是「登录永远等超时」）。
		flow, known := loginFlows[a.loginKind]
		if !known || flow.Poll == nil {
			// 表里没登记 Poll：DisplayURL / noFlowReason 的渠道已在 StartLoginFor 提前返回，
			// 走不到这里。兜底成「立即失败」而不是死循环等超时。
			a.setLoginError("登录失败：该渠道未提供轮询能力")
			return
		}
		done, unfinished, perr := flow.Poll(a.loginClient, a.loginStateFP)
		// 分派契约（loginFlow.Poll / wrapPoll 的三态，2026-10-18 9f8f4a95 回归教训）：
		//   pending  → (nil, true, nil)：继续轮询。**不得**调 done —— 该回归曾写成
		//              `if perr == nil` 就调 done(a)，而 pending 的 done 恒为 nil，
		//              首个 tick（2s）必 nil 闭包 panic；panic 被 recover 吞掉后
		//              finishLogin 清 busy，表现是「所有轮询渠道发起登录 2 秒后静默死亡，
		//              面板无任何响应」。
		//   成功     → (done, false, nil)：落账号收尾。
		//   终态错误 → (nil, false, err)：立即结束轮询（继续只会重复报同一个错）。
		switch {
		case unfinished:
			// 还没完成：下一跳再看。
		case perr == nil:
			if err := done(a); err != nil {
				a.setLoginError("登录失败：" + err.Error())
			}
			return
		default:
			// 终态错误（下发验证码失败 / 引导页起不来 / 未捕获到账号 / 换取令牌失败）：
			// 立即结束轮询，由 defer 释放资源。
			log.Printf("%s login poll failed: %v", a.loginKind, perr)
			a.setLoginError("登录失败：" + perr.Error())
			return
		}
	}
}

func (a *App) finishLogin() {
	a.muLogin.Lock()
	a.loginBusy = false
	a.loginCtx, a.loginCancel, a.loginClient = nil, nil, nil
	a.loginKind = ""
	a.muLogin.Unlock()
}

// setLoginError 记录本次登录的终态错误（供面板展示）。调用方需持有 a.muLogin 或在其外调用。
func (a *App) setLoginError(msg string) {
	a.muLogin.Lock()
	a.loginErr = msg
	a.muLogin.Unlock()
}

// peekLoginError 读取本次登录的终态错误（不清空）。
//
// 故意**非破坏式**：`/api/state` 有多个调用方（页面加载、其他轮询），
// 若读取即清空，面板的登录轮询可能拿不到而被别的调用偷走，toast 就永远不显示了。
// 由下一次 startLogin 置空。
func (a *App) peekLoginError() string {
	a.muLogin.Lock()
	defer a.muLogin.Unlock()
	return a.loginErr
}

// LoginBusy 是否登录中。
func (a *App) LoginBusy() bool {
	a.muLogin.Lock()
	defer a.muLogin.Unlock()
	return a.loginBusy
}
