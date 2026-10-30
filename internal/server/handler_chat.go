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
	resolved := h.resolveAlias(peek.Model)
	rt, model, err := h.runtimeForModel(resolved)

	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	clientModel := peek.Model // 客户端请求的原始模型名（含 channel/ 前缀），回填进响应
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
	fbID, primaryAvail := h.pickModelFallback(rt, model, false)
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
	body = h.applyEffort(body, clientModel)

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
	for fbAttempt := 0; ; fbAttempt++ {
		res := h.serveRotation(w, sw, rt, model, body, clientModel, dispModel, acctModel, &statRow, peek.Stream)
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
		if fbAttempt == 0 && fallbackWorthy(res.lastKind) && !rt.SingleAccount {
			// 请求内重入只跨渠道（skipSameChannel=true）：同渠道兄弟模型走的是同一批
			// 账号，若本轮 429 其实是账号级限流（traework/qwenwork/qodercn/qodercom
			// 等渠道把任意 429 都归 ErrSoftRate），再打一遍同一批账号必然二次撞墙；
			// 且该账号第二个模型也撞墙后，pool 会把「模型级冷却」升级为「账号级冷却」
			// ——冷却时长取已有条目的最晚到期时刻（不延长），变的是粒度：可用容量从
			// 「1 个模型格子」缩到「0，整号对所有模型下线」。
			// 入口降级不设此限：那里「该模型已全冷」是既有事实，换同渠道兄弟模型
			// 正是降级链配置的本意（上游配额按模型计，模型 A 冷不代表模型 B 冷）。
			if fb2, _ := h.pickModelFallback(rt, model, true); fb2 != "" {
				if rt2, m2, err2 := h.runtimeForModel(fb2); err2 == nil {
					nb, err3 := rewriteModel(body, m2)
					if err3 == nil {
						log.Printf("model fallback platform=%s from=%s to=%s", rt.Kind, clientModel, fb2)
						// 响应头必须在任何 WriteHeader 之前（此刻未写过任何字节）；
						// 改写成功才发——失败走透传时不得残留降级头（S1）。
						w.Header().Set("X-Model-Fallback", clientModel+" -> "+fb2)
						body = nb
						body = h.applyEffort(body, clientModel) // 与入口同参数；键已存在不碰（双注入守卫）
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
// 只认两类「本渠道对该模型已无承载能力」的错误：
//   - ErrHardCredit：积分耗尽 → 账号进 12h 硬冷却，本渠道容量已翻 false；
//   - ErrSoftRate：模型饱和（14003 Model busy / 6004 usage exceeds frequency
//     limit）→ 全部账号对该模型都在模型级冷却，轮转已无号可选。
//
// 其余错误不换模型：内容类/请求类错误换模型必然复现（换模型只是白烧备胎额度），
// 5xx/登录态问题换模型也治不了本，原样透传让客户端与运维看到真实原因。
func fallbackWorthy(kind provider.ErrKind) bool {
	return kind == provider.ErrHardCredit || kind == provider.ErrSoftRate
}
