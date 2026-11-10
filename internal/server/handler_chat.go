// 对话入口：请求解析 → 别名/改写/思考强度 → 单次轮转（serveRotation）→ 方案 B 请求内降级重入。
//
// 从 handler.go 拆出（结构重排，零行为变更）。本文件对应「一次请求的入口与外部骨架」，
// serveRotation 的轮转细节在 handler_rotation.go。
//
// R94 的降级重入循环（for fbAttempt）**必须留在本文件**：它只在「换号已用尽」后出手，
// 与轮转是两个维度；搬进 rotation 会让 rotation 同时承担两个维度，正是三维归因要避免的。
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/provider"
	// 别名 statsEngine：与运行统计引擎包名一致（本包无同名局部变量，别名仅为与 app 包对齐可读）
	statsEngine "wild-work/internal/stats"
)

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := ReadBodyLimited(r)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", err.Error())
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		}
		return
	}
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	// 解析错误不再静默（issue #30）：截断/损坏的 body 若吞掉错误会误报 invalid_model
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	// 别名解析（`compat.model_map`）：必须发生在 runtimeForModel **之前**，且解析结果
	// 必须写回传给它 —— runtimeForModel 返回的 upstream 模型名随后被 pickModelFallback
	// 用来匹配 rule.From（那里比的是真模型的上游名），若继续拿别名去匹配，降级链对
	// 别名请求**永远不命中**（MS-006 设计口径 4）。
	// clientModel 仍取客户端原值：响应 model 字段「回填请求名」语义不变（MS-003 已确认）。
	// model_map 为空/nil 时 resolveAlias 是恒等变换 ⇒ 旧行为逐字不变（口径 5）。
	// resolved 保留给记账用（见下方 canonModel）：别名请求必须记到**真模型**名下，
	// 否则 stats.channelOfModel 对裸名返回 ok=false，平台「今日 Token」列会静默漏计。
	isAuto := isAutoModel(peek.Model) // 保留名：客户端原值优先于 model_map（见下）
	resolved := h.resolveAlias(peek.Model)
	// 自动选路（R165）：请求 model 写成 "auto" 时，由打分引擎按「免费优先 + 质价比」
	// 当场算出一个具体的 `<channel>/<model>`。必须发生在 runtimeForModel 之前
	// （后者只认带前缀的全路径）。
	//
	// `auto` 是**保留名**，判定取客户端的原始值 `peek.Model` 而非 resolveAlias 的结果：
	// resolveAlias 会先查 model_map，配置里一条 `auto -> xxx` 就能把整套自动选路静默架空
	// （这是最容易在面板里"手滑"造成的一类失效）。故别名表里的 `auto` 键一律被忽略。
	// 场景路由写入端也拒收该键（app.SetSceneRouting），两道闸一起挡。
	//
	// 失败语义（三种，全部可诊断、绝不静默走错渠道）：
	//   ① 未接入/未启用自动选路 ⇒ 400 提示改回显式模型名；
	//   ② 已启用但候选被硬门全部剔除 ⇒ 503 + 指向 /api/auto/preview；
	//   ③ 命中 ⇒ 覆盖 resolved，并记下备胎链供降级段使用。
	var autoChain []string // 展示用（写进 X-Model-Auto-Chain，长度 = fallback_depth）
	var autoOrder []string // 降级用（R166：全部候选的跨渠道全序，不受 depth 限制）
	autoOrigin := false
	if isAuto {
		chosen, chain, order, ok := h.autoDecide(autoRequestFromBody(body))
		switch {
		case !ok && !h.hasAutoResolver():
			writeOpenAIError(w, http.StatusBadRequest, "invalid_model",
				"model \"auto\" 未启用：请在面板「自动选路」开启，或改用显式的 <channel>/<model>")
			return
		case !ok:
			writeOpenAIError(w, http.StatusServiceUnavailable, "no_candidate",
				"自动选路当前没有可用候选（渠道全灭/模型能力不匹配/全部被排除）："+
					"可在面板「自动选路」预览查看每个候选被剔除的原因")
			return
		}
		resolved = chosen
		autoChain = chain
		autoOrder = order
		autoOrigin = true
		// 响应头必须在任何 WriteHeader 之前；sw 透传 Header。
		w.Header().Set("X-Model-Origin", "auto")
		w.Header().Set("X-Model-Auto-Chosen", chosen)
		if len(autoChain) > 1 {
			w.Header().Set("X-Model-Auto-Chain", strings.Join(autoChain, ","))
		}
		log.Printf("model auto chosen=%s chain=%s", chosen, strings.Join(chain, ","))
	} else if order := autoOrderFromHeader(r); len(order) > 0 {
		// 兼容层路径（R167）：`/v1/responses` / `/v1/messages` 已经把 auto 解析成裸模型名
		// 并重新投递到本层，这里的 peek.Model 不再等于 "auto"。网关把那次决策的**全序**
		// 经内部头带进来，本层用同一条全序降级——两条路径行为一致（否则 Codex/Claude
		// 客户端这条路上 auto 只会在静态链上撞空、直接透传上游错误）。
		// 只影响**降级次序**，不参与选路：首选仍是 body 里那个具体模型。
		autoOrder = order
	}
	rt, model, err := h.runtimeForModel(resolved)

	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	clientModel := peek.Model // 客户端请求的原始模型名（含 channel/ 前缀），回填进响应
	// effortModel 思考强度映射的查表键：普通请求 = 客户端原始模型名（面板按客户端写法配档）；
	// auto 请求 = 实际选中的模型全路径——客户端只写了 "auto"，拿它查表永远不命中，
	// 而用户配的档位是按具体模型配的。
	effortModel := clientModel
	if autoOrigin {
		effortModel = resolved
	}
	// 诊断：记录上下文构成摘要（config.body_log 关闭时立即返回，无实际开销）。
	// 必须放在 rewriteModel 之前——要记的是客户端发来的原始体，而非改写后的。
	if h.ledger != nil {
		h.ledger.TraceBody(rt.Kind.String(), clientModel, body)
	}
	// 统计/流水的归一 ID：新旧写法（cline-free/x 与 cline/cline-free/x）归一到同一串，
	// 否则同一模型会在用量榜/请求日志里裂成两行（R59）；其余渠道为恒等变换。
	// 基准取**解析后**的 resolved 而非 clientModel（MS-006 MINOR-1）：别名 `fast` 若按
	// clientModel 归一，记账值就是裸名 `fast` ⇒
	//   ① stats/usage.go:172 的 channelOfModel 对裸名返回 ok=false，stats.go:801 只累加
	//      ok==true 的行 ⇒ 别名请求的 token 从平台「今日 Token」列静默消失；
	//   ② 同一条路径里降级分支（下方 acctModel=备胎）记的是真模型，两套口径并存。
	// 取 resolved 后统一为「实际执行模型」：resolved 命中别名时是全路径；未命中/空表时
	// resolved 与 clientModel 完全相同 ⇒ 记账值与改造前逐字等价（零迁移）。
	canonModel := provider.CanonicalModelID(resolved)
	// 模型降级链：请求模型在本渠道已无账号可承载（no_healthy_account 的两种形态——
	// 「模型全冷」或「整渠道全灭」，pickWithSticky 必败）时，按用户配置的链找第一个
	// 仍有可用账号的备胎模型顶上；备胎可跨渠道，渠道全灭时不至于直接 503。
	// 仅此场景触发，正常路由零开销；不粘滞——每个请求都按原模型重新判定，上游解冻即自动切回。
	// fbApplied=实际备胎全路径（用于把响应 model 字段/X-Model-Fallback 头告知 agent）；
	// dispModel=写进响应的模型名（无降级=客户端请求名，保持官方「回填请求名」语义；降级=备胎全路径，
	// agent 读 chunk.model / resp.model 即可感知实际执行模型）。
	var fbApplied string
	// skipSameChannel=false：入口降级不限制渠道（见请求内重入点的理由）。
	// auto 请求优先走决策链（本次请求算出的跨渠道次序），链走空才回落到配置的静态降级链。
	var fbID string
	var primaryAvail bool
	if len(autoOrder) > 0 {
		fbID, primaryAvail = h.pickAutoFallback(rt, model, autoOrder, false, false)
	} else {
		fbID, primaryAvail = h.pickModelFallback(rt, model, false)
	}
	// 口径说明：降级期间 ledger/stats 的 token 流水按**实际执行模型**记账
	// （statRow.Model、AddUsage(acctModel)、ledgerModelOf(acctModel)）——谁实际工作、
	// 谁实际消耗 token/积分，就记在谁名下（2026-10-16 口径变更，推翻原「按客户端
	// 请求名记账」设计）。acctModel=记账归属模型（无降级=canonModel；降级=备胎的
	// 规范对外 ID，归一函数同构，新旧写法仍归同一行）；原请求名→备胎的对照由
	// 日志行 + 面板「降级记录」区块 + X-Model-Fallback 头/响应 model 字段承担。
	acctModel := canonModel
	if fbID != "" {
		if rt2, m2, err := h.runtimeForModel(fbID); err == nil {
			log.Printf("model fallback platform=%s from=%s to=%s", rt.Kind, clientModel, fbID)
			// 响应头标注实际备胎（必须在任何 WriteHeader 之前；sw 透传 Header）
			w.Header().Set("X-Model-Fallback", clientModel+" -> "+fbID)
			rt, model = rt2, m2
			fbApplied = fbID
			acctModel = provider.CanonicalModelID(fbID)
		}
	}
	dispModel := clientModel
	// auto 请求把实际执行模型写进响应（客户端应据此感知选了什么）；
	// 选路来源由 X-Model-Origin / X-Model-Auto-Chosen 头承担。
	if autoOrigin {
		dispModel = resolved
	}
	if fbApplied != "" {
		dispModel = fbApplied
	}
	// 降级事件喂入运行统计（面板「降级记录」区块）：降级命中记一条；未降级且
	// 原模型容量在位时拿本请求当恢复探针（本请求后续成败不影响，
	// 恢复语义=「重新有可用账号」而非「重新请求成功」）。
	if h.stats != nil {
		switch {
		case fbApplied != "":
			h.stats.NoteFallback(clientModel, fbApplied, time.Now())
		case primaryAvail:
			h.stats.NoteFallbackRecovery(clientModel, time.Now())
		}
	}
	body, err = rewriteModel(body, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 思考强度注入：客户端未传 reasoning_effort 且面板配置命中（按客户端请求模型 ID 匹配，
	// 含 `*` 全局默认）时补上档位。未配置 = 零开销直通（applyEffort 内部快检）；
	// 注入失败仅记日志、原 body 照发，绝不因此中断请求。
	body = h.applyEffort(body, effortModel)

	// 运行统计插桩：每客户端请求一行（最终结果；中间换号尝试不逐行——对齐 sidecar 口径）。
	// TTFB 由 statWriter 记首字节，耗时/吞吐在出口由 defer 收口；
	// Status 未显式赋值 = 成功写回（200）。
	var statRow statsEngine.ReqRow
	statRow.Model = acctModel
	if peek.Stream {
		statRow.Mode = "流"
	} else {
		statRow.Mode = "非流"
	}
	sw := &statWriter{ResponseWriter: w, start: time.Now()}
	w = sw // 此后所有写出（流式/非流式/透传错误）统一经 sw 计时
	if h.stats != nil {
		defer func() {
			now := time.Now()
			statRow.Time = now.Format("01-02 15:04:05")
			statRow.TTFBMs = sw.ttfbMs
			statRow.TotalSec = now.Sub(sw.start).Seconds()
			if statRow.Status == 0 {
				statRow.Status = http.StatusOK
			}
			h.stats.AddRequestRow(statRow)
		}()
	}

	// 轮转选号 + 上游交互 + 耗尽出口整体平移至 serveRotation（方案 B：轮转耗尽且最后
	// 错误是积分耗尽时，按降级链当场切备胎、以备胎重入同一轮转段——抽方法使重入=
	// 再次调用；成功/错误出口与记账 defer 全留在本函数，「一个客户端请求一行」口径
	// 不变）。serveRotation 保持纯轮转语义；降级状态同步（模型改写/档位注入/记账
	// 归属/响应头）由本函数在重入前完成，与入口降级完全同构。
	// 请求内降级的最大重入次数：普通请求沿用历史的 1 次（零行为变化）；
	// auto 请求放宽到 3 次（R166）——全序里有几十个候选，只重入一次意味着
	// 「首选与第一备胎同时额度见底」就把 503 直接砸给客户端，而后面明明还有能用的。
	// 上限仍是常数而非 len(autoOrder)：每次重入都是一轮真实的轮转 + 上游调用，
	// 无限重入会让一个请求在上游全线限流时打满轮转，代价远大于收益。
	maxFallbackAttempts := 1
	if len(autoOrder) > 0 {
		maxFallbackAttempts = 3
	}
	for fbAttempt := 0; ; fbAttempt++ {
		res := h.serveRotation(w, sw, rt, model, body, clientModel, dispModel, acctModel, &statRow, peek.Stream, len(autoOrder) > 0)
		if res.done {
			return
		}
		// 候选耗尽待透传（res.lastStatus>0）：方案 B 判定——对两类「本渠道确实
		// 顶不住」的错误就地换模型：
		//   ① ErrHardCredit（积分耗尽）：账号刚进 12h 硬冷却，本渠道容量已翻 false；
		//   ② ErrSoftRate（模型饱和）：14003「Model busy」/6004「usage exceeds
		//      frequency limit」——上游明确按模型限额并建议「switch to the other
		//      models」，本渠道全部账号对该模型都在模型级冷却，轮转已无可选号。
		// 为什么 ② 必须纳入（2026-10-17 生产实录）：14003 被 Classify 判软限流后
		// 只冷「账号×模型」60s，而请求内降级原先只认 ErrHardCredit ⇒ 饱和时段的
		// **第一个**请求必然把 429 原样抛给客户端，第二个请求才由入口降级链顶上
		// （app.log 20:46:00 吃 429、20:46:01 降级成功，同一分钟后续 8 个请求全绿）。
		// 账号级 429 也会因此换模型：比让客户端直接失败更友好，代价是可能多烧
		// 备胎额度——已由用户裁决接受。
		// 仅重入一次（防备胎再降备胎死循环）；单账号渠道豁免（透传是惩罚豁免
		// 不变量的设计约束）；此刻未写过任何字节（轮转失败路径天然成立——所有
		// 写出都发生在成功流式/非流式分支内），降级头可安全补发；无规则/备胎无
		// 容量 → fb2 为空，维持原样透传，与现状一致。
		// 单账号渠道原本一律不重入（R22：透传是惩罚豁免的设计约束）。auto 请求
		// 放开这一条（R167）：重入点是**另一个渠道**的模型（skipSameChannel=true），
		// 不是在唯一账号上反复捶，既不产生惩罚也不违反 R22。
		if fbAttempt < maxFallbackAttempts && fallbackWorthy(res.lastKind) &&
			(len(autoOrder) > 0 || !rt.SingleAccount) {
			fb2 := ""
			if len(autoOrder) > 0 {
				fb2, _ = h.pickAutoFallback(rt, model, autoOrder, true, rt.SingleAccount)
			} else {
				fb2, _ = h.pickModelFallback(rt, model, true)
			}
			if fb2 != "" {
				if rt2, m2, err2 := h.runtimeForModel(fb2); err2 == nil {
					nb, err3 := rewriteModel(body, m2)
					if err3 == nil {
						log.Printf("model fallback platform=%s from=%s to=%s", rt.Kind, clientModel, fb2)
						// 响应头必须在任何 WriteHeader 之前（此刻未写过任何字节）；
						// 改写成功才发——失败走透传时不得残留降级头（S1）。
						w.Header().Set("X-Model-Fallback", clientModel+" -> "+fb2)
						body = nb
						body = h.applyEffort(body, effortModel) // 与入口同参数；键已存在不碰（双注入守卫）
						rt, model = rt2, m2
						dispModel = fb2
						acctModel = provider.CanonicalModelID(fb2) // 记账归属=备胎（0021/PR #148 口径）
						statRow.Model = acctModel
						statRow.Status = 0 // 429 轮转已污染；出口 defer 靠 Status==0 记 200
						if h.stats != nil {
							h.stats.NoteFallback(clientModel, fb2, time.Now()) // 请求内降级也喂降级记录
						}
						continue
					}
					log.Printf("model fallback body rewrite failed platform=%s to=%s err=%v（维持原样透传）", rt.Kind, fb2, err3)
				}
			}
		}
		if res.lastKind == provider.ErrQueued {
			// 全号排队：给合规的重试信号（与 body 里的 retry_after_seconds 一致）。
			w.Header().Set("Retry-After", "30")
		}
		transparentError(w, res.lastStatus, res.lastBody)
		return
	}
}

// rotationResult：serveRotation 的判定结果。done=true=响应已完整写回（成功/请求级
// 错误/耗尽诊断引导），调用方直接收尾；done=false 且 lastStatus>0=候选耗尽待透传，
// 由调用方决定原样透传还是按降级链请求内重入（fallbackWorthy 判定）。
type rotationResult struct {
	done       bool
	lastKind   provider.ErrKind
	lastStatus int
	lastBody   []byte
}

// fallbackWorthy 报告「轮转耗尽后是否值得就地换模型」。
//
// 判据只有一条：**本渠道/本账号对该模型已无承载能力**（换模型确实可能换来能力）。
// 2026-11-10 扩面（R167）——用户报「auto 里的模型额度用完或不能用了不降级」，
// 原实现只认 402/429 两种，于是「模型在这个渠道下线/不存在」这类最典型的
// 「不能用了」恰好一个都不降级，被原样透传给客户端。现认五类：
//   - ErrHardCredit：402 积分/权益耗尽 → 账号进 12h 硬冷却，本渠道容量已翻 false；
//   - ErrSoftRate：429 模型饱和（14003 Model busy / 6004 usage exceeds frequency
//     limit）→ 全部账号对该模型都在模型级冷却，轮转已无号可选；
//   - ErrModelBlocked：11102「该后端无此模型」→ 该 (账号,模型) 组合本就不成立；
//   - ErrNotFound：404 模型/端点不存在 → 换号已试尽，继续重试没有意义；
//   - ErrAccountFault：11140/14017 账号级授权/配额故障 → 整号已失去承载该模型的能力。
//
// 仍**不**换模型的两类（各自有独立理由，别顺手扩）：
//   - 内容/请求类（ErrContentBlocked / ErrPromptTooLong / ErrImageInvalid /
//     ErrBadParams / ErrPassthrough）：换模型必然复现，只会白烧备胎额度；
//   - ErrServer（5xx）与 ErrSessionDead（登录态）：换模型治不了本。5xx 有 2026-10-20
//     实测（同一波里 4 个号全 500，换号只是把 1 次失败放大成 9 次）；SessionDead 需要
//     人工重登，把它换成"看起来成功的另一个模型"会让运维彻底看不见该去重登哪个号。
func fallbackWorthy(kind provider.ErrKind) bool {
	switch kind {
	case provider.ErrHardCredit, // 402 积分/权益耗尽（额度用完的常规形态）
		provider.ErrSoftRate,     // 429 模型饱和（14003 / 6004）
		provider.ErrModelBlocked, // 11102 该后端无此模型 → 模型在这个渠道"不存在"
		provider.ErrNotFound,     // 404 模型/端点不存在（换号已试尽，剩下一层才是模型面）
		provider.ErrAccountFault: // 11140/14017 账号授权/配额故障（整号失去该模型承载）
		return true
	}
	return false
}
