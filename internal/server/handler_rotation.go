// 账号轮转主链路（serveRotation）：选号 → 令牌刷新 → 上游调用 → 错误分类与惩罚 → 写回/换号 → 候选耗尽出口。
//
// 从 handler.go 拆出（结构重排，零行为变更）。**本文件是全仓风险最高的一段代码**，
// 三条没有类型约束的时序不变量必须原样保留（见 docs/refactor/02-server.md §2.4）：
//
//	① statRow.Status 的写读契约：进入惩罚分支时写、轮转内换号成功时必须清零，
//	   出口 defer 只在 Status==0 时补 200；
//	② sw.written 是「能不能换号」的唯一分界线，且 clearSSEHeaders 必须在判定为零字节之后才执行；
//	③ defer rc.Close() 注册在循环体内，但所有分支都在本函数内 return ⇒ 每次调用恰好一次。
package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"wild-work/internal/ledger"
	"wild-work/internal/loopguard"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	// 别名 statsEngine：与运行统计引擎包名一致（本包无同名局部变量，别名仅为与 app 包对齐可读）
	statsEngine "wild-work/internal/stats"
)

// serveRotation：chatCompletions 的轮转选号 + 上游交互 + 耗尽出口段（原代码整体
// 平移，零缩进变化；结果经命名返回值承载——done 默认 true，既有裸 return 语义
// 原样保留：所有提前返回的出口都已把响应写完）。轮转中「账号级可轮换」错误
// （kind.Rotatable()）换号续试并置位 lastKind；请求级/内容类错误在循环内提前
// 透传返回。方案 B 重入时由调用方完成全部降级状态同步后再次调用本方法（tried
// 随调用重建，备胎渠道天然全新）。
func (h *Handler) serveRotation(
	w http.ResponseWriter, sw *statWriter, rt *Runtime, model string, body []byte,
	clientModel, dispModel, acctModel string, statRow *statsEngine.ReqRow, stream bool,
) (res rotationResult) {
	res.done = true // 默认：既有 return 出口都已把响应写完（成功/请求级错误/诊断引导）
	tried := map[string]bool{}
	var lastErr error
	// 最后一次上游错误的原始 status+body：候选耗尽时原样透传（而非包装成 503），
	// 客户端才能看到 6004 重置时间等上游细节。内容类错误在循环内已提前透传，不受影响。
	var lastStatus int
	var lastBody []byte
	// lastKind：轮转中最后一个「账号级可轮换」错误的类别（Rotatable 分支置位）——
	// 候选耗尽透传前交回调用方做方案 B 判定；成功/请求级错误不置位（零值 ErrNone）。
	var lastKind provider.ErrKind
	// attempts：本次请求最多试几个号 = 渠道账号数（原 MaxRotate 已删）。
	// 每个号最多试一次（tried），是否真的换下一个由错误类别决定——
	// kind.Rotatable() 为假、或传输层/5xx 这类「换号必撞同一面墙」的错误直接返回，
	// 这样「试满所有号」与「立即失败」两种语义都由错误类型决定，不再有魔法数字上限。
	attempts := len(rt.Pool.List())
	if attempts <= 0 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		acct := h.pickWithSticky(rt, model)
		if acct == nil {
			break
		}
		if tried[acct.UID] {
			// 粘性路由选回已尝试的账号，清除粘性记录后重试
			h.stickyClear(rt)
			acct = rt.Pool.PickExcludingForModel(tried, model)
			if acct == nil {
				break
			}
		}
		tried[acct.UID] = true
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			log.Printf("refresh start platform=%s uid=%s reason=request", rt.Kind, acct.UID)
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				log.Printf("refresh failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
				lastErr = err
				h.stickyClear(rt)
				// 本机网络/DNS 挂着时（`dial tcp: lookup ...: no such host`、connection refused），
				// 所有账号都刷不出来，换号必撞同一面墙 ⇒ 直接 502，别把 N×30s 超时堆满。
				if isNetworkError(err) {
					log.Printf("refresh failed by network platform=%s uid=%s（不换号）err=%v", rt.Kind, acct.UID, err)
					statRow.Status = http.StatusBadGateway
					writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
					return
				}
				// 账号侧问题（refresh token 失效 / 刷新接口 5xx）：只有「恰好要刷新」的号
				// 受影响，其余号 token 仍然有效 ⇒ 换下一个号，不设次数上限（试满所有号）。
				var ue *provider.Error
				if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
					rt.Pool.MarkSessionDead(acct.UID, "refresh: "+err.Error())
				} else {
					rt.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("refresh save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
			}
			log.Printf("refresh success platform=%s uid=%s expires_at=%d", rt.Kind, acct.UID, acct.ExpiresAt)
		}
		rc, status, respBody, terr := rt.Upstream.ChatStream(acct, body)
		if terr != nil {
			lastErr = terr
			h.stickyClear(rt)
			// 传输层错误（连接超时 / 拒连 / 等响应头超时）：同一渠道所有账号打的是**同一个
			// host**，网络层故障与账号无关 ⇒ 换号必然撞同一面墙，而且单次可长达 120s。
			// 直接 502 把重试交给客户端（原逻辑：多号渠道轮 3 次 = 最长 6 分钟白等）。
			// 也不计入账号错误（2026-10-20）：一次本机网络抖动不该把整池账号打成冷却。
			// 单账号渠道（oczen）本来就是这条语义，现在统一。
			log.Printf("upstream transport error platform=%s uid=%s（不换号、不计账号错误）err=%v",
				rt.Kind, acct.UID, terr)
			statRow.Status = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", terr.Error())
			return
		}
		if status >= 400 {
			statRow.Status = status // 所有透传出口（单账号/请求级/惩罚后）共用
			h.stickyClear(rt)
			kind := rt.Upstream.Classify(status, string(respBody))
			// 单账号渠道：任何账号级惩罚都等于整条渠道下线，故一律原文透传、不罚账号。
			// 尤其在 429（唯一需要的背压）上：冷却后后续请求会在挑号阶段被挡成
			// no_healthy_account，反而不如透传 429 让客户端按 Retry-After 自行重试。
			if rt.SingleAccount {
				// 402（权益不足）例外：账号本身健康，同号其他模型照常可用，只冷却「账号×该模型」
				// ——下一次请求挑号阶段即「模型全冷」，自动触发降级链顶替；本次仍原文透传。
				if kind == provider.ErrHardCredit {
					rt.Pool.CooldownModel(acct.UID, model, h.hardCooldown(), "402 权益不足")
					log.Printf("model cooldown (single-account 402) platform=%s uid=%s model=%s cooldown=%s",
						rt.Kind, acct.UID, model, h.hardCooldown())
				}
				// 日志点明错误性质与上游原文：旧文案（「不罚账号」）会让用户看到 403 时
				// 误以为「工具把这个错误当成了请求级问题」，掩盖了真实原因（issue #117）。
				log.Printf("upstream error platform=%s uid=%s status=%d kind=%s body=%s（单账号渠道无号可轮换，故原样透传、不冷却）",
					rt.Kind, acct.UID, status, kind, truncateBody(respBody))
				transparentError(w, status, respBody)
				return
			}
			switch kind {
			case provider.ErrHardCredit:
				rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.hardCooldown(), "余额/权益不足")
			case provider.ErrSoftRate:
				// 429/6004 上游按模型限额（「您也可以切换其他模型继续使用」）：
				// 只冷却该账号的该模型，同号其他模型继续参与选号；
				// 短时间内第二个模型也撞墙时 CooldownModel 会升级整号冷却（防上游按账号计时）。
				// 冷却时长按 6004 响应体的真实重置点解析（#82）：固定 60s 与上游「按重置点解封」
				// 不符，解析失败/非正回退 SoftCooldown；升级时整号冷却到最晚重置点。
				escalated := rt.Pool.CooldownModel(acct.UID, model, h.softRateCooldown(string(respBody)), "429 rate limit")
				if escalated {
					log.Printf("model cooldown escalated to account platform=%s uid=%s model=%s", rt.Kind, acct.UID, model)
				} else {
					log.Printf("model cooldown platform=%s uid=%s model=%s", rt.Kind, acct.UID, model)
				}
			case provider.ErrSessionDead:
				// 登录态失效不再混入「停用」：标记 SessionDead（挑号跳过 + 面板徽标 +
				// 重新登录入口），与用户手动停用（Disable）分离（#134）。
				rt.Pool.MarkSessionDead(acct.UID, truncateBody(respBody))
				log.Printf("session dead marked platform=%s uid=%s", rt.Kind, acct.UID)
			case provider.ErrNotFound:
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
			case provider.ErrContentBlocked, provider.ErrPromptTooLong, provider.ErrImageInvalid,
				provider.ErrBadParams, provider.ErrPassthrough:
				// 内容拦截 / 上下文超限 / 图片无效 / 出站 body 不合格 / 请求级拒绝：
				// 不冷却不熔断不计错，直接透传原文回客户端。
				// 这些是请求内容或请求形态问题，与账号健康无关，轮转白费时间且浪费好号配额。
				//
				// ⚠️ ErrImageInvalid 与 ErrBadParams 是 2026-10-17 补入的：两者的 ErrKind 注释
				// 早就写着「请求级错误，不罚号不轮转，透传原文」，但本 case 漏了它们 ⇒ 落到
				// default 做 NoteError 计数，等于拿客户端的问题惩罚健康账号。本地拒绝型渠道
				// （monkeycode / loomy / raccoon / minimax）最容易触发这条路径。
				transparentError(w, status, respBody)
				return
			case provider.ErrModelBlocked:
				// (账号,模型) 级负缓存避让 —— 兑现 provider.go:193 的注释承诺。
				//
				// ⚠️ 2026-10-18 修正：本 case 原先与 ErrWafBlock/ErrAccountFault 合并，
				// 走 Pool.Cooldown(uid) **冻结整个账号**。后果是「一个模型在上游不存在」
				// 会拖垮同一账号下的所有模型：officeace 里 glm-5.3-flash 上游回 404，
				// 三个账号接连被 model_blocked 冻结 60s，整条渠道 503。
				// 而 provider.go:193 对 ErrModelBlocked 的定义明明写着「(账号,模型) 负缓存避让」。
				// 现在改为只冻这一个 (账号,模型)；CooldownModel 内部在「≥2 个模型同时冷」
				// 时才升级为整号冷却（见 pool.go 的同款注释），那时才是真正的账号问题。
				escalated := rt.Pool.CooldownModel(acct.UID, model, h.cfg.SoftCooldown, kind.String())
				log.Printf("model cooldown platform=%s uid=%s model=%s escalated=%v reason=%s",
					rt.Kind, acct.UID, model, escalated, kind)
				transparentError(w, status, respBody)
				return
			case provider.ErrWafBlock, provider.ErrAccountFault:
				// 账号级风控/授权故障：软冷却，不累计错误计数。
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, kind.String())
				if kind != provider.ErrAccountFault {
					transparentError(w, status, respBody)
					return
				}
			case provider.ErrClient:
				// 请求内容/形态类 4xx（如 workbuddy 11148 工具调用与结果不配对）：
				// 错误跟着请求体走，换号必然同样拒收——不累计错误计数。否则一个
				// 坏会话的客户端重试风暴会在几分钟内把全池账号记满冷却（阈值默认 3），
				// 面板呈现「渠道自己挂了」假象，且诱发无意义的模型降级链触发。
				// 仅轮转以兼容「同模型不同账号权益差异」场景；候选耗尽后透传最后
				// 错误（与上游分类器注释「BadParams：不罚号但仍轮转」对齐）。
			case provider.ErrQueued:
				// 上游把「排队」当 4xx 下发（同 10605 的另一种形态）：只冷却「号×模型」，不罚整号。
				// 冷却时长取响应里的 retryAfter/重置点，换号可能命中不同队列。
				rt.Pool.CooldownModel(acct.UID, model, h.softRateCooldown(string(respBody)), "model queued")
				log.Printf("model queued (http %d) platform=%s uid=%s model=%s", status, rt.Kind, acct.UID, model)
			case provider.ErrServer:
				// 上游 5xx = 服务端故障，与账号健康无关 ⇒ **不计入账号错误**。
				//
				// ⚠️ 2026-10-18 翻转（原先只有声明 NoCooldownOnServerError 的渠道豁免）：
				// 实测事故（lobsterai）—— 上游对带 developer 角色的请求返回 500，
				// ErrServer 可轮换 ⇒ 一次客户端请求打 3 个账号各 +1 errCount，
				// 而 err_threshold 默认 3 ⇒ **重试一次就把三个健康账号全冷却 10 分钟**，
				// 期间整条渠道 503。账号本身完全健康（积分/签到/session 全正常）。
				//
				// ⚠️ 2026-10-20 再进一步：**连换号也不做了**（ErrServer 从 Rotatable 除名）。
				// 实测 workbuddyai 11134「the model provider is temporarily unavailable」
				// 在同一波里 4 个号全 500，且每个号内部客户端已自行重试 3 次（日志
				// 「http 500，800ms 后重试 (2/3)」）⇒ 一次请求实际打 9 次，全部白打。
				// 结论：5xx 直接透传原状态码/原文，把等待与重试交给客户端。
				//
				// 逃生门：NoCooldownOnServerError 字段保留，但语义不变（仍表示「不豁免」），
				// 仅用于审计/文档层面标注渠道差异；真正的计数开关由 CountServerErrorAsAccountFault
				// 单独表达，当前无一渠道开启。
				if rt.CountServerErrorAsAccountFault {
					rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
					log.Printf("upstream server error platform=%s uid=%s status=%d（渠道显式要求计入账号错误）",
						rt.Kind, acct.UID, status)
				} else {
					log.Printf("upstream server error platform=%s uid=%s status=%d（不计入账号错误、不换号，直接透传）",
						rt.Kind, acct.UID, status)
				}
			default:
				rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			// 账号级错误（限流/欠费/登录态死/账号故障/排队等）：换下一个账号重试本请求，
			// 全部候选耗尽后才把最后一个上游错误透传给客户端（-p4 前此处直接 return，
			// 导致客户端看到本可避免的 429/402）。内容类错误在上方 case 中已提前返回；
			// 5xx 与传输层错误也是直接透传（换号必撞同一面墙，见各自分支注释）。
			if kind.Rotatable() {
				log.Printf("rotate platform=%s uid=%s kind=%s status=%d body=%s",
					rt.Kind, acct.UID, kind, status, truncateBody(respBody))
				lastErr = fmt.Errorf("upstream %s (http %d): %s", kind, status, truncateBody(respBody))
				lastStatus = status
				lastBody = respBody
				lastKind = kind
				continue
			}
			transparentError(w, status, respBody)
			return
		}
		defer rc.Close()
		// 轮转内换号成功：清掉前面 status>=400 分支写进统计行的状态码。
		// 否则「A 号 429、B 号成功」的请求会在运行统计/请求日志里记成 429——
		// 出口 defer 只在 Status==0 时补 200，而该字段一旦被写就不会自愈。
		// 模型饱和（14003/6004）是多账号渠道的高频形态，统计失真会让面板看不出
		// 降级是否生效，故与请求内降级路径的 statRow.Status 清零同源处理。
		statRow.Status = 0
		rt.Pool.NoteSuccess(acct.UID)
		h.stickySuccess(rt)
		if stream {
			usage, serr := rt.Upstream.Stream(w, rc, dispModel)
			// 排队提示（qodercn 10605 isQueued）：上游把「模型拥塞」当成 HTTP 200 里的
			// 一帧业务错误下发，此刻 **一字节未写** ⇒ 冷却「号×该模型」后换下一个号。
			//
			// 若不识别：这帧没有 choices，会被当普通 chunk 透传 ⇒ 客户端只有一个空 delta，
			// 界面停在「正在等待模型…」，直到网关 180s 后掐断（2026-10-20 实录：
			// 180.3/180.7/180.8s 三次，每轮扣掉 3 分钟，且零 token 计费）。
			var qe *provider.Queued
			if errors.As(serr, &qe) && !sw.written {
				h.stickyClear(rt)
				rt.Pool.CooldownModel(acct.UID, model, qe.Cooldown(), "model queued")
				clearSSEHeaders(w) // StreamCapture 已写进 header 的 SSE 头必须撤掉
				log.Printf("model queued platform=%s uid=%s model=%s %s（换下一个账号，冷却 %s）",
					rt.Kind, acct.UID, model, qe.Error(), qe.Cooldown())
				lastErr = qe
				lastStatus = http.StatusTooManyRequests
				lastBody = queuedBody(qe)
				lastKind = provider.ErrQueued
				continue
			}
			// 其余「首帧即错误、一字节未写」的分类错误（典型：qodercom 的 envelope 403 里
			// 包着 code 110/112 额度耗尽帧 → ErrHardCredit）：此刻换号还来得及，不应当作
			// 「已写流的残留错误」只罚不换（那样 5 个号全耗尽时客户端只能干等下次请求）。
			// 与写后分支的区别仅在「能不能换」：已写出字节的流无法撤回，只能罚号 + 透传。
			if serr != nil && !sw.written && len(rt.Pool.List()) > 1 {
				var sec provider.StreamErrorClassifier
				if errors.As(serr, &sec) && sec.Kind().Rotatable() {
					kind := sec.Kind()
					h.stickyClear(rt)
					switch kind {
					case provider.ErrHardCredit:
						rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.hardCooldown(), "stream 402 权益不足")
					case provider.ErrSoftRate:
						rt.Pool.CooldownModel(acct.UID, model, h.softRateCooldown(serr.Error()), "stream 429 限流")
					case provider.ErrSessionDead:
						rt.Pool.MarkSessionDead(acct.UID, serr.Error())
					case provider.ErrAccountFault:
						rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "stream account fault")
					default:
						rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
					}
					clearSSEHeaders(w)
					log.Printf("stream error before write platform=%s uid=%s model=%s kind=%s（换下一个账号）err=%v",
						rt.Kind, acct.UID, model, kind, serr)
					lastErr = serr
					lastKind = kind
					// 额度耗尽透 402（付费墙语义，客户端/用户一看就知道要充值/换号），
					// 其余保留上游原始状态码；body 用原文，保留上游错误码与文案。
					lastStatus = http.StatusPaymentRequired
					if pe := (*provider.Error)(nil); errors.As(serr, &pe) && pe.Status >= 400 {
						lastStatus = pe.Status
					}
					lastBody = []byte(serr.Error())
					continue
				}
			}
			cu := cacheUsageOf(usage) // 缓存命中（上游未上报时 reported=false，面板显示「—」）
			// 记 token 流水（成功请求口径；usage 缺失记 0 token 仅计请求数）。
			// 记账失败不干扰主流程——锦上添花功能。
			if h.ledger != nil {
				pt, ct, src := usageTokens(usage)
				h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
					Model: ledgerModelOf(acctModel), PT: pt, CT: ct, CR: cu.cached, CRC: cu.reported, Src: src})
			}
			// 运行统计：模型维度记账（有 usage 才计，对齐 sidecar 的 u.found 语义）。
			// 吞吐仅在流式下有意义：genSec = 总耗时 − TTFB；引擎内对 ≤50ms 的样本只计 TTFB。
			if h.stats != nil {
				pt, ct, _ := usageTokens(usage)
				if pt > 0 || ct > 0 {
					gen := time.Since(sw.start).Seconds() - float64(sw.ttfbMs)/1000
					h.stats.AddUsage(acctModel, pt, ct, usageCredit(usage), sw.ttfbMs, gen, cu.cached, cu.reported, time.Now())
					statRow.Tok = ct
					if gen > 0 {
						statRow.TokPerSec = float64(ct) / gen
					}
				}
				statRow.Credit = usageCredit(usage)
			}
			if serr != nil {
				// 思考链死循环（发癫）命中：模型/请求级问题，不罚账号（换号必然复现），
				// 错误帧已透传给客户端，由下游 agent 自行重试。仅记日志。
				var lge *loopguard.Error
				if errors.As(serr, &lge) {
					log.Printf("reasoning loop intercepted platform=%s uid=%s model=%s kind=%s reason=%s",
						rt.Kind, acct.UID, clientModel, lge.Hit.Kind, lge.Hit.Reason)
					return
				}
				// 上游中断/空流已尽力透传，错误仅在日志可见。
				// 必须带耗时与 TTFB（R58）：只有这个数字能把几种原因分开——
				// 耗时 ≈ 空闲阈值（idle.DefaultTimeout 300s / 各渠道 IdleReader 90s）
				// 且 TTFB 远小于它 ⇒ 看门狗掐的；耗时远小于阈值 ⇒ 上游自己断的。
				// 原先只记 err，两种完全同形，定位只能靠猜。
				log.Printf("stream relay end platform=%s uid=%s model=%s elapsed=%.1fs ttfb=%dms err=%v",
					rt.Kind, acct.UID, clientModel, time.Since(sw.start).Seconds(), sw.ttfbMs, serr)
				// 流内业务错误（traework SOLO 的 event:error，如 3004 限流）：
				// HTTP 200 与响应头早已发出，无法在同一请求内换号重试；唯一补救是把
				// 中招账号冷却掉——下次请求挑号会自动换到别的账号（粘性路由见
				// pickWithSticky 对 status.Cooling 的检查，故无需显式 stickyClear）。
				var sec provider.StreamErrorClassifier
				if errors.As(serr, &sec) {
					kind := sec.Kind()
					switch kind {
					case provider.ErrSoftRate:
						// 单号渠道（当前仅一个账号，如 loomy/小浣熊）：冷却 = 整条渠道下线，
						// 反不如透传——客户端已通过 SSE 收到上游原始限流错误（3004/429），
						// 会自行退避重试；与非流式「单账号不罚账号」口径一致。
						// SingleAccount 渠道（oczen）官方已有完整豁免语义，保持官方原样不重复覆盖。
						if len(rt.Pool.List()) <= 1 && !rt.SingleAccount {
							log.Printf("stream error passthrough platform=%s uid=%s kind=%s（单号渠道不罚账号，客户端自行重试）",
								rt.Kind, acct.UID, kind)
							break
						}
						rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "stream business error: "+serr.Error())
						log.Printf("stream error cooled platform=%s uid=%s kind=%s cooldown=%s",
							rt.Kind, acct.UID, kind, h.cfg.SoftCooldown)
					case provider.ErrHardCredit:
						if len(rt.Pool.List()) <= 1 {
							// 单号渠道：余额/权益不足透传原文，客户端/用户直接看到真实原因（如充值），
							// 而不是被 503 掩盖成「渠道不可用」。账号本身不动，仅冷却「账号×该模型」，
							// 下一次请求挑号即「模型全冷」，自动走降级链顶替。
							rt.Pool.CooldownModel(acct.UID, model, h.hardCooldown(), "stream 402 权益不足")
							log.Printf("stream error model-cooldown platform=%s uid=%s model=%s kind=%s（单号渠道仅冷却该模型，原文透传）",
								rt.Kind, acct.UID, model, kind)
							break
						}
						rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.hardCooldown(), "stream business error: "+serr.Error())
						log.Printf("stream error cooled platform=%s uid=%s kind=%s cooldown=%s",
							rt.Kind, acct.UID, kind, h.hardCooldown())
					case provider.ErrSessionDead:
						// 登录态失效：标记 SessionDead（挑号跳过 + 面板徽标），不再混入停用（#134）
						rt.Pool.MarkSessionDead(acct.UID, serr.Error())
					default:
						// 其余流内错误（如请求级拒绝）不罚账号，只记日志。
						log.Printf("stream error ignored platform=%s uid=%s kind=%s",
							rt.Kind, acct.UID, kind)
					}
				}
			}
			return
		}
		resp, err := rt.Upstream.Aggregate(rc, dispModel)
		if err != nil {
			// 流内嵌错误（如 qwenwork 402 权益不足）此时已带分类：模型级冷却，
			// 下一次请求挑号即「模型全冷」，自动走降级链顶替；本次仍返回错误。
			var asec provider.StreamErrorClassifier
			if errors.As(err, &asec) && asec.Kind() == provider.ErrHardCredit {
				rt.Pool.CooldownModel(acct.UID, model, h.hardCooldown(), "402 权益不足")
				log.Printf("model cooldown (aggregate 402) platform=%s uid=%s model=%s cooldown=%s",
					rt.Kind, acct.UID, model, h.hardCooldown())
			}
			statRow.Status = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		if h.ledger != nil {
			u, _ := resp["usage"].(map[string]any)
			pt, ct, src := usageTokens(u)
			cu := cacheUsageOf(u)
			h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
				Model: ledgerModelOf(acctModel), PT: pt, CT: ct, CR: cu.cached, CRC: cu.reported, Src: src})
		}
		writeJSON(w, http.StatusOK, resp)
		// 运行统计：必须在写回之后——此刻 statWriter 才记到首字节，TTFB 为真值；
		// 一次性写回 gen≈0 由引擎 50ms 守卫兜底（只计 TTFB 不计吞吐——非流式本无吞吐样本，
		// 若在写回前取值会把 ttfb=0/gen=端到端总时长喂进引擎，污染 AvgTTFB 与吞吐均值）。
		if h.stats != nil {
			u, _ := resp["usage"].(map[string]any)
			pt, ct, _ := usageTokens(u)
			if pt > 0 || ct > 0 {
				gen := time.Since(sw.start).Seconds() - float64(sw.ttfbMs)/1000
				cu := cacheUsageOf(u)
				h.stats.AddUsage(acctModel, pt, ct, usageCredit(u), sw.ttfbMs, gen, cu.cached, cu.reported, time.Now())
				statRow.Tok = ct
			}
			statRow.Credit = usageCredit(u)
		}
		return
	}
	return h.exhausted(w, rt, model, statRow, lastStatus, lastBody, lastErr, lastKind)
}

// exhausted 候选耗尽时的出口：优先透传最后一个上游错误，否则给出可操作的 503 诊断。
//
// 从 serveRotation 拆出（结构重排，零行为变更）。**本函数是 6 条 503 文案 + 1 条「交回调用方」
// 的唯一归属地**：原文 10 个写响应出口里，E10 的 6 条 503 与 E11 的 done=false 全在这里，
// 拆出后数量不变（这是「谁写响应、写几次」的守恒基线）。
//
// done=false 表示不在此透传、交回 chatCompletions 判定方案 B（积分耗尽/模型饱和时按降级链
// 重入一次，R94）；done=true 表示响应已写完。
func (h *Handler) exhausted(w http.ResponseWriter, rt *Runtime, model string, statRow *statsEngine.ReqRow,
	lastStatus int, lastBody []byte, lastErr error, lastKind provider.ErrKind) rotationResult {
	// 渠道没有可用账号：区分「未绑定账号」与「全部冷却/禁用」，给出可操作的引导。
	// 客户端（如 Claude Code/Codex）只看到这条错误，必须足以让用户知道去面板做什么。
	// 候选耗尽但轮换中有上游错误：优先透传最后一个上游原始响应（保持上游语义，
	// 如 429+6004 重置时间；本次请求确实逐号尝试过，包装成 503 反而丢信息）。
	if lastStatus > 0 && lastBody != nil {
		// 候选耗尽：不在此透传——交回调用方判定（方案 B 积分耗尽时按降级链重入一次，
		// 否则原样透传；透传语义见上方注释）。
		return rotationResult{done: false, lastKind: lastKind, lastStatus: lastStatus, lastBody: lastBody}
	}
	statRow.Status = http.StatusServiceUnavailable
	sts := rt.Pool.List()
	now := time.Now()
	var bound, disabled, cooling, sessionDead, modelAvail int
	var earliest time.Time // 全池对该模型最早的解冻时刻（账号级与模型级冷却取最小）
	consider := func(t time.Time) {
		if t.After(now) && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
	}
	for _, s := range sts {
		if s.Disabled {
			disabled++
			continue
		}
		// ⚠️ 2026-10-18 补 sessionDead：登录态失效的账号原先**被计入 bound（可用账号）**，
		// 于是 503 文案说「其余余额耗尽或出错」，把「需要重新登录」这个唯一可行动的
		// 结论彻底掩盖掉（MonkeyCode 实测：用户看到的是"余额耗尽"，实际要重登）。
		// pool.healthy() 早已把 sessionDead 视为不健康（pool.go:103），此处统计口径对齐它。
		if s.SessionDead {
			sessionDead++
			continue
		}
		if s.Cooling {
			cooling++
			consider(s.Until)
			continue
		}
		bound++
		if t, ok := s.ModelCooling[model]; ok {
			consider(t)
		} else {
			modelAvail++
		}
	}
	if len(sts) == 0 {
		if noLoginChannel(rt.Kind) {
			writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
				fmt.Sprintf("渠道 %s 尚未就绪（虚拟账号未装配）：请重启程序", rt.Kind))
			return rotationResult{done: true}
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			fmt.Sprintf("渠道 %s 尚未绑定账号：请在面板「账号管理」中添加 %s 账号（或把模型改为已接入渠道，如 workbuddy/glm-5.2）",
				rt.Kind, rt.Kind))
		return rotationResult{done: true}
	}
	// 账号均健康、但请求模型在全部账号上都处于模型级限流冷却：
	// 明确告知解冻时间，避免误以为账号故障去重登（2026-09-23 全池 6004 事故的教训）。
	//
	// 措辞「冷却」而不写「限流」：同一窗口也可能来自上游排队（ErrQueued 给「号×模型」
	// 打 30s 冷却，2026-10-20），写死「限流」会让用户以为被限速了。
	// 同时补 Retry-After：首次请求遇到全号排队时回的是 429 + Retry-After: 30，
	// 冷却窗口内的后续请求应当给同量级的重试信号，而不是让 agent 把 503 当“服务坏了”硬退避。
	if bound > 0 && modelAvail == 0 {
		if !earliest.IsZero() {
			w.Header().Set("Retry-After", formatRetryAfter(earliest))
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			fmt.Sprintf("渠道 %s 的 %d 个账号均健康，但模型 %s 已在全部账号上冷却（上游限流或排队）：最早约 %s 解冻（到点自动恢复，无需重新登录），也可先切换其他模型",
				rt.Kind, bound, model, formatUnfreeze(earliest)))
		return rotationResult{done: true}
	}
	if bound == 0 {
		// 匿名渠道没有「重新登录」这个概念，给出针对性提示（否则会误导用户去找登录入口）。
		hint := fmt.Sprintf("%s 账号需重新登录（日志会有 refresh token is invalid）", rt.Kind)
		if noLoginChannel(rt.Kind) {
			hint = "该渠道无需登录，稍后重试即可（若持续失败请查看日志）"
		}
		// 粘贴 Key 型渠道（cline）没有「重新登录」这个动作：Key 不过期也不轮换，
		// 被禁用只会是「Key 无效/已被删除」，唯一补救是换一把有数 Key 粘进来。
		// 照搬「需重新登录」会把用户引到一个不存在的入口（R41 同族的文案误导）。
		if keyPasteChannel(rt.Kind) {
			hint = fmt.Sprintf("请在面板粘贴一把有效的 %s API Key（sk_…，在 app.cline.bot → Account / API Keys 创建）", rt.Kind)
		}
		// 多数是限流冷却而非登录态问题：给出最早解冻时间，重登提示仅在无任何冷却时兜底。
		if cooling > 0 && !earliest.IsZero() {
			hint = fmt.Sprintf("最早约 %s 解冻（多为限流冷却，到点自动恢复；若长时间未恢复再检查登录态）", formatUnfreeze(earliest))
		}
		// 登录态失效优先于限流文案：这是唯一「用户自己动手就能恢复」的原因，
		// 被限流文案盖住会让人白等（2026-10-18 MonkeyCode 实测的误导）。
		if sessionDead > 0 && !noLoginChannel(rt.Kind) && !keyPasteChannel(rt.Kind) {
			hint = fmt.Sprintf("%d 个账号登录态已失效，需重新登录后才能恢复", sessionDead)
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			fmt.Sprintf("渠道 %s 的 %d 个账号当前全部不可用（禁用 %d / 冷却 %d / 登录态失效 %d）：可在面板查看原因；%s",
				rt.Kind, len(sts), disabled, cooling, sessionDead, hint))
		return rotationResult{done: true}
	}
	msg := fmt.Sprintf("渠道 %s 暂无可用账号（共 %d 个：禁用 %d / 冷却 %d / 登录态失效 %d；其余余额耗尽或出错）",
		rt.Kind, len(sts), disabled, cooling, sessionDead)
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	return rotationResult{done: true} // 诊断引导已把响应写完
}
